package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
)

// ErrProbeQuiet is the error NASProbe.Next returns when no downlink PDU
// arrived within its wait.
var ErrProbeQuiet = errors.New("no downlink within the wait")

// ErrAssociationLost wraps the error NASProbe.Next returns once a read on
// the UE's association failed: the core closed it, or it broke.
var ErrAssociationLost = errors.New("the UE's N2 association is gone")

// NASProbe drives a registered UE's NAS signalling connection by hand, for
// the conformance checks whose stimulus is an altered or replayed uplink
// NAS message (AttachProbe is its counterpart during the attach). The
// caller protects a message at the UE's next uplink NAS COUNT (Protect),
// sends that PDU, an altered copy or an earlier one again (Send), and reads
// what the core sends back (Next).
//
// A probe holds the UE's procedure lock and owns every read on its
// association from ProbeNAS until Close, which ends the UE: the association
// is dropped without further signalling, so a check deregisters first
// (Deregister). A NASProbe is not safe for concurrent use.
type NASProbe struct {
	m       *Manager
	s       *Session
	release func()
	done    chan struct{}
	pdus    <-chan pduResult
	readErr error
	close   sync.Once
}

// ProbeDownlink is one downlink NGAP message a NASProbe read.
type ProbeDownlink struct {
	At time.Time
	// Procedure names the NGAP procedure (see DownlinkEvent.Procedure).
	Procedure string
	// Released is set for a UE CONTEXT RELEASE COMMAND, which the probe
	// answers with UE CONTEXT RELEASE COMPLETE.
	Released bool
	// Sessions are the PDU sessions of a PDU SESSION RESOURCE SETUP
	// REQUEST, which the probe answers with a response giving each a gNB
	// downlink tunnel.
	Sessions []uint8
	// ReleasedSessions are the PDU sessions of a PDU SESSION RESOURCE
	// RELEASE COMMAND, which the probe answers with a response reporting
	// each released.
	ReleasedSessions []uint8
	// ModifiedSessions are the PDU sessions of a PDU SESSION RESOURCE
	// MODIFY REQUEST, which the probe does not answer.
	ModifiedSessions []uint8
	// NAS holds every NAS PDU the message carried, as the UE's NAS
	// security context unwrapped it: the top-level one, or the one of
	// each session of a PDU SESSION RESOURCE SETUP or MODIFY REQUEST.
	NAS []NASEvent
}

// ProbeNAS returns a NASProbe on the registered UE supi, taking its
// procedure lock (waiting for a procedure in flight, or until ctx ends).
func (m *Manager) ProbeNAS(ctx context.Context, supi string) (*NASProbe, error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	if sess.sec == nil {
		return nil, fmt.Errorf("UE %s has no NAS security context", supi)
	}
	release, err := sess.beginProcedure(ctx)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	return &NASProbe{m: m, s: sess, release: release, done: done, pdus: readAssociation(sess.conn, done)}, nil
}

// GUTI is the 5G-GUTI the Registration Accept assigned, as a 5GS mobile
// identity value; nil when it assigned none.
func (p *NASProbe) GUTI() []byte { return append([]byte(nil), p.s.guti...) }

// Protect wraps msg, integrity protected and ciphered, under the UE's NAS
// security context at its next uplink NAS COUNT, which it advances, and
// returns the NAS PDU without sending it.
func (p *NASProbe) Protect(msg *f5nas.Message) ([]byte, error) {
	return p.s.sec.EncodeSecure(msg, nas.SecHdrIntegrityCiphered)
}

// Send sends nasPDU as it is in an UPLINK NAS TRANSPORT.
func (p *NASProbe) Send(nasPDU []byte) error {
	pdu, err := gnb.BuildUplinkNASTransport(p.s.ServingGNB(), p.s.amfID, p.s.ranID, nasPDU)
	if err != nil {
		return err
	}
	return gnb.SendPDU(p.s.conn, ueStream, pdu)
}

// Next returns the UE's next downlink NGAP message, waiting up to wait:
// ErrProbeQuiet when none came, an error wrapping ErrAssociationLost once
// the association is gone, or ctx's error.
func (p *NASProbe) Next(ctx context.Context, wait time.Duration) (ProbeDownlink, error) {
	if p.readErr != nil {
		return ProbeDownlink{}, p.readErr
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ProbeDownlink{}, ctx.Err()
	case <-t.C:
		return ProbeDownlink{}, ErrProbeQuiet
	case r := <-p.pdus:
		if r.err != nil {
			p.readErr = fmt.Errorf("%w: %v", ErrAssociationLost, r.err)
			return ProbeDownlink{}, p.readErr
		}
		return p.take(r.pdu)
	}
}

// take turns one downlink PDU into a ProbeDownlink, answering a release
// command, a PDU session resource setup and a PDU session resource release.
func (p *NASProbe) take(pdu *ngapType.NGAPPDU) (ProbeDownlink, error) {
	s := p.s
	d := ProbeDownlink{At: time.Now(), Procedure: "unknown"}
	code, ok := procedureCode(pdu)
	if ok {
		d.Procedure = procedureName(code)
	}
	if ok && code == ngapType.ProcedureCodeUEContextRelease && pdu.Present == ngapType.NGAPPDUPresentInitiatingMessage {
		d.Released = true
		if err := gnb.SendPDU(s.conn, ueStream, gnb.BuildUEContextReleaseComplete(s.amfID, s.ranID)); err != nil {
			return d, fmt.Errorf("UE Context Release Complete not sent: %w", err)
		}
		return d, nil
	}
	if amfID, ranID, res, err := gnb.ParsePDUSessionResourceSetupRequest(pdu); err == nil {
		teids := map[int64]uint32{}
		teidFor := func(id int64) uint32 {
			if _, ok := teids[id]; !ok {
				teids[id] = allocDLTEID()
			}
			return teids[id]
		}
		n3 := s.gnbN3
		if n3 == "" {
			n3 = "127.0.0.1"
		}
		for _, r := range res {
			d.Sessions = append(d.Sessions, uint8(r.PDUSessionID))
			if len(r.NASPDU) > 0 {
				d.NAS = append(d.NAS, p.decode(d.At, r.NASPDU))
			}
		}
		resp, err := gnb.BuildPDUSessionResourceSetupResponse(amfID, ranID, res, n3, teidFor)
		if err != nil {
			return d, err
		}
		if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
			return d, fmt.Errorf("send PDU Session Resource Setup Response: %w", err)
		}
		return d, nil
	}
	if rel, err := gnb.ParsePDUSessionResourceReleaseCommand(pdu); err == nil {
		for _, id := range rel.PDUSessionIDs {
			d.ReleasedSessions = append(d.ReleasedSessions, uint8(id))
		}
		if len(rel.NASPDU) > 0 {
			d.NAS = append(d.NAS, p.decode(d.At, rel.NASPDU))
		}
		resp, err := gnb.BuildPDUSessionResourceReleaseResponse(rel.AMFUENGAPID, rel.RANUENGAPID, rel.PDUSessionIDs)
		if err != nil {
			return d, err
		}
		if err := gnb.SendPDU(s.conn, ueStream, resp); err != nil {
			return d, fmt.Errorf("send PDU Session Resource Release Response: %w", err)
		}
		return d, nil
	}
	if mod, err := gnb.ParsePDUSessionResourceModifyRequest(pdu); err == nil {
		for _, id := range mod.PDUSessionIDs {
			d.ModifiedSessions = append(d.ModifiedSessions, uint8(id))
		}
		for _, raw := range mod.NASPDUs {
			d.NAS = append(d.NAS, p.decode(d.At, raw))
		}
		return d, nil
	}
	if dl, err := gnb.ParseDownlink(pdu); err == nil && len(dl.NASPDU) > 0 {
		d.NAS = append(d.NAS, p.decode(d.At, dl.NASPDU))
	}
	return d, nil
}

// decode unwraps one downlink NAS PDU under the UE's NAS security context
// (a plain one is decoded as it is).
func (p *NASProbe) decode(at time.Time, raw []byte) NASEvent {
	ev := NASEvent{At: at, Raw: append([]byte(nil), raw...), Type: nasMessageType(raw, p.s.sec.CipheringAlg == nas.NEA0)}
	switch {
	case len(raw) < 2:
		ev.Err = fmt.Errorf("downlink NAS too short: %d bytes", len(raw))
	case f5nas.GetSecurityHeaderType(raw) == f5nas.SecurityHeaderTypePlainNas:
		ev.Msg, ev.Err = nas.DecodePlain(raw)
	default:
		ev.Msg, ev.Plain, ev.Err = p.s.sec.DecodeSecurePlain(raw)
	}
	if ev.Err != nil {
		ev.Msg = nil
	}
	return ev
}

// Deregister sends a switch-off Deregistration Request at the UE's next
// uplink NAS COUNT and waits up to wait for the UE CONTEXT RELEASE COMMAND
// that answers it, reporting whether it came. Other downlink messages are
// skipped.
func (p *NASProbe) Deregister(ctx context.Context, wait time.Duration) (bool, error) {
	if len(p.s.guti) == 0 {
		return false, errors.New("no 5G-GUTI (AMF assigned none in Registration Accept)")
	}
	msg, err := ueBuildDeregistration(p.s.guti, true)
	if err != nil {
		return false, err
	}
	pdu, err := p.Protect(msg)
	if err != nil {
		return false, fmt.Errorf("wrap Deregistration Request: %w", err)
	}
	if err := p.Send(pdu); err != nil {
		return false, err
	}
	deadline := time.Now().Add(wait)
	for {
		d, err := p.Next(ctx, time.Until(deadline))
		switch {
		case errors.Is(err, ErrProbeQuiet):
			return false, nil
		case err != nil:
			return false, err
		case d.Released:
			return true, nil
		}
	}
}

// Close ends the probe and the UE: the session leaves the Manager and its
// data path and association close, with no signalling. Safe to call more
// than once.
func (p *NASProbe) Close() {
	p.close.Do(func() {
		close(p.done)
		supi := p.s.SUPI
		p.m.mu.Lock()
		delete(p.m.sessions, supi)
		p.m.mu.Unlock()
		p.s.markReleased()
		p.m.releaseAppSessions(supi)
		p.s.closeDataPath()
		_ = p.s.conn.Close() // nothing is left to release when the close fails
		p.release()
		p.m.hub.publish(StateEvent{SUPI: supi, State: StateDeregistered, Detail: "association dropped by a NAS probe"})
		p.m.appMetrics.forgetUE(supi)
	})
}

// readAssociation reads downlink PDUs from conn on one goroutine until a
// read fails or done closes, so a wait that runs out never leaves a read
// behind to swallow the next PDU.
func readAssociation(conn gnb.Transport, done <-chan struct{}) <-chan pduResult {
	ch := make(chan pduResult)
	go func() {
		for {
			pdu, err := gnb.ReadPDU(context.Background(), conn)
			select {
			case ch <- pduResult{pdu, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}
