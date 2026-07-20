package loomgtp

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/loom/core/app/voip"
	loomdp "github.com/bgrewell/loom/core/datapath"
	"github.com/bgrewell/loom/core/netpath/dgram"
	"github.com/bgrewell/loom/core/rtp"
	"github.com/bgrewell/loom/core/rtp/codec"

	"github.com/bgrewell/orbit/internal/datapath"
)

// delayRx wraps the bridge's rxDatapath so every `every`-th frame (after a
// warmup) is DELIVERED late while its arrival timestamp — stamped into the
// demux ring at push time, i.e. at the socket read in production — is
// untouched: a demux ring whose consumer dequeues late under load, the exact
// shape the MetaConn seam exists for. Polls are forced to one frame so each
// delayed frame is its own delivery.
type delayRx struct {
	inner *rxDatapath
	after int
	every int
	delay time.Duration

	mu sync.Mutex
	n  int
}

func (d *delayRx) Name() string               { return d.inner.Name() }
func (d *delayRx) Caps() loomdp.Capabilities  { return d.inner.Caps() }
func (d *delayRx) Close() error               { return d.inner.Close() }
func (d *delayRx) RxRelease(f []loomdp.Frame) { d.inner.RxRelease(f) }

func (d *delayRx) RxPoll(int) ([]loomdp.Frame, error) {
	frames, err := d.inner.RxPoll(1)
	if len(frames) > 0 {
		d.mu.Lock()
		d.n++
		k := d.n
		d.mu.Unlock()
		if k > d.after && k%d.every == 0 {
			time.Sleep(d.delay)
		}
	}
	return frames, err
}

// TestArrivalMetaJitterOverBridge is the orbit-side regression for the loom
// v0.11 MetaConn fix (the voip rxLoop now anchors RFC 3550 A.8 jitter at the
// datapath's arrival stamp): arrival timestamps must flow demux ring →
// rxDatapath Frame.Meta → dgram MetaConn → voip receiver stats. A paced RTP
// stream whose dequeue is artificially delayed (25ms on every 2nd frame)
// must show near-floor jitter when the ring stamps arrivals; the control run
// with the stamps removed shows the delays as tens of ms of spurious jitter
// — proving the low reading comes from the carried timestamps, not from the
// delays being invisible. Mirrors loom's TestArrivalMetaJitter against the
// orbit bridge.
func TestArrivalMetaJitterOverBridge(t *testing.T) {
	ueAddr := netip.MustParseAddr("192.168.100.5")
	run := func(t *testing.T, stampArrival bool) float64 {
		t.Helper()
		ring := datapath.NewRing(512)
		drx := &delayRx{inner: newRx(ring, nil), after: 20, every: 2, delay: 25 * time.Millisecond}
		nw, err := dgram.New(newRawTx(nullUplink{}, DefaultInnerMTU), drx, ueAddr, DefaultInnerMTU)
		if err != nil {
			t.Fatal(err)
		}
		defer nw.Close()

		// Answerer over the bridge: it latches the injected stream's source
		// and scores reception; its own transmit goes to the null uplink.
		ans, err := voip.NewMediaSession(nw, voip.MediaConfig{Codec: codec.Codec{Name: "pcmu"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); _ = ans.Run(ctx) }()

		// Paced downlink: a media-clock RTP stream every ptime, pushed into
		// the demux ring the way the Demux reader does — payload + arrival.
		c, err := codec.ByName("pcmu")
		if err != nil {
			t.Fatal(err)
		}
		pk := rtp.NewPacketizer(c)
		payload := make([]byte, 160) // pcmu @ 20ms
		buf := make([]byte, 512)
		port := ans.LocalAddr().Port()
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(c.Ptime)
			defer tick.Stop()
			for i := 0; i < 400; i++ {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				n := pk.Next(buf, payload)
				pktIP, err := datapath.BuildUDPPacket(net.IPv4(10, 0, 0, 9), net.IP(ueAddr.AsSlice()), 4000, port, buf[:n])
				if err != nil {
					t.Error(err)
					return
				}
				arrival := time.Now()
				if !stampArrival {
					arrival = time.Time{} // control: no wire stamp → dequeue time rules
				}
				ring.Push(pktIP, arrival)
			}
		}()

		deadline := time.Now().Add(15 * time.Second)
		for ans.CumulativeMetrics().RxPackets < 150 {
			if time.Now().After(deadline) {
				t.Fatalf("answerer received only %d packets in 15s", ans.CumulativeMetrics().RxPackets)
			}
			time.Sleep(50 * time.Millisecond)
		}
		j := ans.CumulativeMetrics().JitterMs
		close(stop)
		wg.Wait()
		cancel()
		<-done
		return j
	}

	meta := run(t, true)
	plain := run(t, false)
	if meta >= 8 {
		t.Errorf("JitterMs with ring arrival stamps = %.2f, want < 8 (dequeue delay must not count as jitter)", meta)
	}
	if plain <= 10 {
		t.Errorf("JitterMs without arrival stamps = %.2f, want > 10 (control: the injected delays must be visible at dequeue)", plain)
	}
}
