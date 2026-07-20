// Session-side wiring of the per-gNB netstack bridge (design §2.2/§6,
// Phase 6): which loom netpath.Network an app session rides, the lazy
// AddAddress on a session's first TCP app, RemoveAddress + uplink-route
// cleanup on UE release, and the inter-gNB handover address move.
package engine

import (
	"fmt"
	"net"
	"time"

	"github.com/bgrewell/loom/core/netpath"

	"github.com/bgrewell/orbit/internal/loomgtp"
)

// StateTCPConnsReset is the correlation-visible event emitted when an
// inter-gNB data-path move aborted live TCP connections: the UE's address
// moved between gNB netstacks and gVisor closes conns outright when their
// address leaves a stack (loom netstack.RemoveAddress semantics). It reaches
// app-session streams through the hub like the handover phase events.
const StateTCPConnsReset = "TCP_CONNS_RESET"

// appNetwork returns the loom netpath.Network that carries one app's traffic
// for this session — the app-protocol dimension of the loomgtp bridge
// (design §6 end): UDP apps (voip) ride the lightweight dgram network over
// the session's demuxed tunnel, so fleet voice never pays the gVisor cost;
// TCP apps (http, video) ride the per-gNB gVisor netstack via a source-bound
// Stack.Network(ueIP) view. Networks returned for UDP apps are per-call and
// closed by the app session; TCP networks are retargetable facades whose
// underlying UE address stays on the gNB stack until UE release.
func (s *Session) appNetwork(app string, ueIP net.IP) (netpath.Network, error) {
	switch app {
	case "voip":
		_, rx, err := s.dataplane()
		if err != nil {
			return nil, err
		}
		return loomgtp.NetworkFor(s, rx, ueIP, 0)
	case "http", "video":
		return s.tcpNetwork(ueIP)
	default:
		return nil, fmt.Errorf("app %q has no network mapping (udp: voip; tcp: http, video)", app)
	}
}

// tcpNetwork returns a netstack-backed Network for this UE, attaching the
// UE's PDU-session address to its gNB's shared gVisor stack on first use
// (lazy per-gNB stack creation + AddAddress — design Phase 6 lifecycle). The
// returned StackNetwork facade follows inter-gNB handovers (rebindLocked
// retargets it onto the target gNB's stack); closing it releases only its
// own conns, never the address.
func (s *Session) tcpNetwork(ueIP net.IP) (netpath.Network, error) {
	s.dpMu.Lock()
	defer s.dpMu.Unlock()
	if s.ue == nil {
		// Same lazy open as dataplane(), inlined because we already hold dpMu.
		ref, ue, err := s.openViewLocked(nil, nil)
		if err != nil {
			return nil, err
		}
		s.n3ref, s.ue, s.rx, s.rxTEID = ref, ue, ue.Lane(), s.Result.DLTEID
		s.ueUp.Store(ue)
	}
	if s.nsBridge != nil && !s.nsAddr.Equal(ueIP) {
		return nil, fmt.Errorf("UE %s netstack address is %s; cannot serve %s", s.SUPI, s.nsAddr, ueIP)
	}
	if s.nsBridge == nil {
		br, err := s.n3ref.netstack()
		if err != nil {
			return nil, err
		}
		if err := br.Attach(ueIP, s, s.rx); err != nil {
			return nil, err
		}
		s.nsBridge = br
		s.nsAddr = append(net.IP(nil), ueIP...)
	}
	view, err := s.nsBridge.Network(s.nsAddr)
	if err != nil {
		return nil, err
	}
	fac := loomgtp.NewStackNetwork(view, s.dropStackNet)
	s.nsNetsMu.Lock()
	if s.nsNets == nil {
		s.nsNets = make(map[*loomgtp.StackNetwork]struct{})
	}
	s.nsNets[fac] = struct{}{}
	s.nsNetsMu.Unlock()
	return fac, nil
}

// dropStackNet is the facade's onClose hook: forget a facade the app closed.
func (s *Session) dropStackNet(f *loomgtp.StackNetwork) {
	s.nsNetsMu.Lock()
	delete(s.nsNets, f)
	s.nsNetsMu.Unlock()
}

// takeStackNets snapshots and clears the live facade set.
func (s *Session) takeStackNets() []*loomgtp.StackNetwork {
	s.nsNetsMu.Lock()
	out := make([]*loomgtp.StackNetwork, 0, len(s.nsNets))
	for f := range s.nsNets {
		out = append(out, f)
	}
	s.nsNets = nil
	s.nsNetsMu.Unlock()
	return out
}

// teardownNetstackLocked is the UE-release half of the netstack lifecycle:
// RemoveAddress (aborting any live conns — never a silent blackhole), the
// ueIP→uplink route cleanup, and the facades' close. Callers hold dpMu; the
// gNB stack itself stays up for the gNB's other UEs and closes with the
// shared tunnel (n3Pool.release → sharedN3.closeNetstack).
func (s *Session) teardownNetstackLocked() {
	if s.nsBridge == nil {
		return
	}
	s.nsBridge.Detach(s.nsAddr)
	s.nsBridge, s.nsAddr = nil, nil
	for _, f := range s.takeStackNets() {
		_ = f.Close()
	}
}

// moveNetstackLocked relocates the UE's netstack address after an inter-gNB
// data-path move: RemoveAddress on the source gNB's stack (gVisor ABORTS
// conns that were live on the address during the move window — loom
// RemoveAddress semantics, stated honestly: TCP apps must reconnect, and the
// reconnect lands on the target stack through the retargeted facades),
// AddAddress + uplink-route registration on the target gNB's stack, and a
// TCP_CONNS_RESET correlation event when live conns were actually aborted.
// An address with no live conns moves silently — TCP sees nothing at all.
// Intra-gNB handovers never come here: the TEID swap happens below the
// stack, so established connections survive and see only delay/loss.
//
// Callers hold dpMu, with s.n3ref/s.rx already pointing at the target gNB.
// On a re-attach failure the TCP plane degrades to closeDataPath semantics
// (facades closed, apps see errors) while the UDP path stays live.
func (s *Session) moveNetstackLocked() {
	if s.nsBridge == nil {
		return
	}
	oldBr := s.nsBridge
	newBr, err := s.n3ref.netstack()
	if err == nil && newBr == oldBr {
		return // same bridge (not actually a stack move)
	}
	oldBr.Detach(s.nsAddr) // aborts conns live during the move window
	if err == nil {
		err = newBr.Attach(s.nsAddr, s, s.rx)
	}
	if err != nil {
		s.nsBridge, s.nsAddr = nil, nil
		for _, f := range s.takeStackNets() {
			_ = f.Close()
		}
		s.notifyEvent(StateTCPConnsReset,
			fmt.Sprintf("inter-gNB move could not re-attach the UE to the target gNB netstack: %v; TCP networks closed", err))
		return
	}
	s.nsBridge = newBr
	resets := 0
	s.nsNetsMu.Lock()
	facs := make([]*loomgtp.StackNetwork, 0, len(s.nsNets))
	for f := range s.nsNets {
		facs = append(facs, f)
	}
	s.nsNetsMu.Unlock()
	for _, f := range facs {
		view, verr := newBr.Network(s.nsAddr)
		if verr != nil {
			continue
		}
		resets += f.Retarget(view)
	}
	if resets > 0 {
		// "socket(s)", not "connection(s)": Retarget's count is the facade's
		// live-tracked set, which includes listeners and packet conns
		// alongside established conns — a lone idle listener crossing the
		// move is 1 socket, and the event must not overstate it as an
		// aborted connection.
		s.notifyEvent(StateTCPConnsReset,
			fmt.Sprintf("inter-gNB handover moved the UE address between gNB netstacks; %d live TCP socket(s) (conns and listeners) aborted (gVisor closes sockets whose address leaves the stack) — apps reconnect via the target gNB", resets))
	}
}

// notifyEvent publishes a correlation-visible session event through the
// Manager's hub (nil-safe for sessions built outside a Manager).
func (s *Session) notifyEvent(state, detail string) {
	if s.notify == nil {
		return
	}
	s.notify(StateEvent{SUPI: s.SUPI, State: state, Detail: detail, Time: time.Now()})
}
