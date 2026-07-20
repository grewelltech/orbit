package loomgtp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	loomdp "github.com/bgrewell/loom/core/datapath"
	"github.com/bgrewell/loom/core/netpath"
	"github.com/bgrewell/loom/core/netpath/dgram"

	"github.com/bgrewell/orbit/internal/datapath"
)

// DefaultInnerMTU bounds inner IP packets when the caller has no
// tunnel-derived MTU: a 1500-byte N3 link minus outer IPv4 (20) + outer UDP
// (8) + GTP-U header with extensions (8..16), with slack (design §2.2). The
// Tunnel does not expose a path MTU today, so this constant is the default;
// once it does, NetworkFor's innerMTU parameter is where the computed value
// plugs in.
const DefaultInnerMTU = 1400

// rxPollWindow bounds a blocking RxPoll so loom's receive loops can observe
// cancellation between polls — the same discipline as loom's own blocking
// backends (UDPListener's recvDeadline).
const rxPollWindow = 200 * time.Millisecond

// timeoutError is the net.Error (Timeout()==true) RxPoll returns when the
// poll window elapses with no frame, per loom's RxDatapath contract.
type timeoutError struct{}

func (timeoutError) Error() string   { return "orbit-gtp rx: poll window elapsed" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var errPollTimeout net.Error = timeoutError{}

// rxDatapath implements loom's datapath.RxDatapath over one demuxed downlink
// lane: a datapath.Ring fed by the session Demux's single socket reader
// (design §6 — consumers never read the N3 socket directly). Frames are the
// inner IP packets exactly as they left the GTP-U decap, and each carries the
// arrival timestamp the demux reader stamped at the socket read into
// Frame.Meta.Nanos — the way loom's own datapaths stamp receive time — so
// dgram's MetaConn/ReadFromMeta can report receive time, not dequeue time.
// Since loom v0.11 the voip MediaSession's receive loop consumes that
// MetaConn timestamp, so RFC 3550 A.8 jitter through this bridge anchors at
// the wire arrival — demux-ring wait, dgram conn-channel queueing, and
// goroutine handoffs no longer masquerade as network jitter (pinned by
// TestArrivalMetaJitterOverBridge).
//
// Ring payloads are already private copies made by the demux reader, so
// polled frames are simply handed out and RxRelease has nothing to reclaim.
type rxDatapath struct {
	ring    *datapath.Ring
	release func() // unsubscribes the lane from its UERx; nil = just close the ring

	frames []loomdp.Frame // reused scratch, valid until the next RxPoll (borrow contract)

	closeOnce sync.Once
}

// newRx wraps a demuxed downlink ring as a loom RxDatapath. release, if
// non-nil, is invoked once on Close to hand the lane back to its UERx.
func newRx(ring *datapath.Ring, release func()) *rxDatapath {
	return &rxDatapath{ring: ring, release: release}
}

func (r *rxDatapath) Name() string { return "orbit-gtp" }
func (r *rxDatapath) Caps() loomdp.Capabilities {
	return loomdp.Capabilities{RawL3: true} // frames are complete IP packets
}

// RxPoll returns up to max downlink packets, blocking up to rxPollWindow for
// the first (then draining whatever else is already queued). A quiet window
// returns errPollTimeout (net.Error, Timeout()==true); a closed lane returns
// net.ErrClosed once drained.
func (r *rxDatapath) RxPoll(max int) ([]loomdp.Frame, error) {
	if max <= 0 {
		return nil, nil
	}
	f, err := r.ring.Read(rxPollWindow)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errPollTimeout
		}
		return nil, err // net.ErrClosed once the lane is closed and drained
	}
	r.frames = r.frames[:0]
	r.frames = append(r.frames, toLoomFrame(f))
	// Drain without blocking: only what is already buffered.
	for len(r.frames) < max && r.ring.Len() > 0 {
		f, err := r.ring.Read(0)
		if err != nil {
			break
		}
		r.frames = append(r.frames, toLoomFrame(f))
	}
	return r.frames, nil
}

// RxRelease implements RxDatapath. Ring payloads are private copies (the
// demux reader copied them out of its socket buffer), so there is nothing to
// return to a pool; only the scratch slice is reused on the next poll.
func (r *rxDatapath) RxRelease([]loomdp.Frame) {}

// Close hands the lane back (closing the ring), waking a blocked RxPoll.
func (r *rxDatapath) Close() error {
	r.closeOnce.Do(func() {
		if r.release != nil {
			r.release()
			return
		}
		r.ring.Close()
	})
	return nil
}

// toLoomFrame converts one demuxed downlink frame, preserving the arrival
// timestamp taken at the socket read (Meta.Nanos, ADR-0020; 0 if unstamped).
func toLoomFrame(f datapath.Frame) loomdp.Frame {
	out := loomdp.Frame{Data: f.Payload, Len: len(f.Payload)}
	if !f.Arrival.IsZero() {
		out.Meta.Nanos = f.Arrival.UnixNano()
	}
	return out
}

// NetworkFor bridges one UE session onto loom's netpath seam (design §6): it
// returns a dgram netpath.Network whose outbound packets — complete inner
// IPv4+UDP packets with real headers and checksums, built by loom — go up the
// session's GTP-U tunnel, and whose downlink is the session's demuxed
// wildcard UDP lane (rx.SubscribeUDPAll; the ICMP latency probe and any
// per-port lanes keep their own subscriptions). VoIP and other UDP apps
// dial/listen through the returned Network; TCP apps (http, video) ride the
// per-gNB GNBStack instead (stack.go — the engine's Session.appNetwork picks
// per app) and are refused here by dgram with ErrTCPUnsupported.
//
// up and rx come from the session's data plane (engine Session.dataplane():
// the *datapath.UETunnel view of the per-gNB SharedTunnel and its
// Demux-registered UERx lane), ueIP is the UE's
// PDU-session IPv4 address, and innerMTU bounds inner packets (<= 0 uses
// DefaultInnerMTU; pass a tunnel-derived value when one exists).
//
// The Network owns the bridge datapaths: closing it releases the wildcard
// UDP lane. Closing it does NOT close the tunnel or the demux.
func NetworkFor(up Uplink, rx *datapath.UERx, ueIP net.IP, innerMTU int) (netpath.Network, error) {
	if up == nil || rx == nil {
		return nil, errors.New("loomgtp: Uplink and UERx are required")
	}
	ip4 := ueIP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("loomgtp: UE IP %v is not IPv4 (the dgram bridge is IPv4-only)", ueIP)
	}
	local, ok := netip.AddrFromSlice(ip4)
	if !ok {
		return nil, fmt.Errorf("loomgtp: invalid UE IP %v", ueIP)
	}
	if innerMTU <= 0 {
		innerMTU = DefaultInnerMTU
	}
	ring := rx.SubscribeUDPAll()
	rxdp := newRx(ring, func() { rx.UnsubscribeUDPAll(ring) })
	n, err := dgram.New(newRawTx(up, innerMTU), rxdp, local, innerMTU)
	if err != nil {
		_ = rxdp.Close() // hand the lane back
		return nil, err
	}
	return n, nil
}
