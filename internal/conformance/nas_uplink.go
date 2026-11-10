package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

const (
	// uplinkRegisterWait bounds the registration that precedes a check's
	// hand-driven uplink NAS messages.
	uplinkRegisterWait = 10 * time.Second
	// uplinkAnswerWait bounds the wait for the answer to a valid request.
	uplinkAnswerWait = 10 * time.Second
	// uplinkQuietWait is how long a message the AMF shall discard is
	// watched for an answer.
	uplinkQuietWait = 5 * time.Second
	// uplinkSettleWait is how long the downlink is left to settle after an
	// answer, so a second message for the same request is not taken for an
	// answer to the next one.
	uplinkSettleWait = time.Second
	// uplinkCleanupWait bounds the wait for the UE Context Release Command
	// after the deregistration that ends a check.
	uplinkCleanupWait = 3 * time.Second
)

// uplinkWaits are the bounds of a check that drives uplink NAS messages by
// hand; a zero field takes its default (tests shorten them).
type uplinkWaits struct {
	register, answer, quiet, settle, cleanup time.Duration
}

func (w uplinkWaits) withDefaults() uplinkWaits {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&w.register, uplinkRegisterWait)
	def(&w.answer, uplinkAnswerWait)
	def(&w.quiet, uplinkQuietWait)
	def(&w.settle, uplinkSettleWait)
	def(&w.cleanup, uplinkCleanupWait)
	return w
}

// sessionRequest is one PDU Session Establishment Request a check built: its
// PDU session identity and procedure transaction identity, and the
// protected NAS PDU (a UL NAS Transport) that carries it.
type sessionRequest struct {
	psi, pti uint8
	pdu      []byte
}

// smReply is one answer to a PDU session request: a 5GSM message in a DL
// NAS Transport (pti set), or a PDU Session Resource Setup Request for the
// session (setup).
type smReply struct {
	psi, pti uint8
	setup    bool
	name     string
}

// downlinkLog is what a watch saw.
type downlinkLog struct {
	sm []smReply
	// mm are the 5GMM message types of the other NAS messages, in order.
	mm []uint8
	// undecoded are the NAS PDUs that did not verify or decode.
	undecoded []string
	// released is a UE Context Release Command; lost is why the
	// association went, nil while it is up.
	released bool
	lost     error
}

// answer is the first reply to r in the log: a 5GSM message with r's PDU
// session identity and procedure transaction identity (the un-forwarded
// request returned by the AMF is one), or a PDU Session Resource Setup
// Request for the session.
func (l downlinkLog) answer(r sessionRequest) *smReply {
	for i, a := range l.sm {
		if a.psi == r.psi && (a.setup || a.pti == r.pti) {
			return &l.sm[i]
		}
	}
	return nil
}

// has reports whether the log holds a 5GMM message of type mt.
func (l downlinkLog) has(mt uint8) bool {
	for _, t := range l.mm {
		if t == mt {
			return true
		}
	}
	return false
}

// gone reports whether the core released the UE or the association went.
func (l downlinkLog) gone() bool { return l.released || l.lost != nil }

// goneNote says how the UE went.
func (l downlinkLog) goneNote() string {
	if l.released {
		return "the AMF sent a UE Context Release Command"
	}
	return fmt.Sprintf("the association was lost (%v)", l.lost)
}

// uplinkDriver registers the check's UE and then drives its NAS signalling
// connection by hand through an engine.NASProbe: fresh PDU Session
// Establishment Requests, sent as built, altered or again, and the answers.
type uplinkDriver struct {
	h     *UEHarness
	p     *engine.NASProbe
	waits uplinkWaits
	// psi and pti are the last identities handed out; each request takes
	// the next of both, so an answer names its request.
	psi, pti uint8
	// established are the PDU sessions the core set up, for the cleanup.
	established []uint8
	gone        bool
	// ngKSI is the NAS key set identifier of the UE's security context,
	// from the Security Mode Command.
	ngKSI uint8
}

// errNIA0Selected is the registration error of a core that selected 5G-IA0:
// the UE harness refuses it, and integrity and replay protection do not
// apply under it (TS 24.501 4.4.3.2, 4.4.3.3).
var errNIA0Selected = errors.New("the core selected 5G-IA0")

// nia0Skip is the SKIP of a check that needs integrity protection.
func nia0Skip(r Result) Result {
	r.Verdict, r.Observed = Skip, "the core selected 5G-IA0"
	r.Detail = "\"Integrity verification is not applicable when 5G-IA0 is used\" (TS 24.501 4.4.3.3), and neither is replay protection (4.4.3.2); the UE harness refuses 5G-IA0, so the UE did not register"
	return r
}

// newUplinkDriver registers h's UE (no PDU session) and opens a NAS probe
// on it. errNIA0Selected is the error when the Security Mode Command
// selected 5G-IA0. On an error nothing stays open.
func newUplinkDriver(ctx context.Context, h *UEHarness, waits uplinkWaits) (*uplinkDriver, error) {
	waits = waits.withDefaults()
	rctx, cancel := context.WithTimeout(ctx, waits.register)
	defer cancel()
	var smc []byte
	_, err := h.RegisterWithProbe(rctx, &engine.AttachProbe{
		OnDownlink: func(ev engine.DownlinkEvent) {
			if ev.NASType == f5nas.MsgTypeSecurityModeCommand && smc == nil {
				smc = append([]byte(nil), ev.NAS...)
			}
		},
	}, func(cfg *engine.UEConfig) { cfg.GNBN3Addr = h.env.UE.GNBN3 })
	selected, smcErr := parseSMC(smc)
	if err != nil {
		if errors.Is(err, engine.ErrUnsupportedAlgorithms) && smcErr == nil && selected.integrity == nas.NIA0 {
			return nil, errNIA0Selected
		}
		return nil, fmt.Errorf("registration: %w", err)
	}
	p, err := h.ProbeNAS(ctx)
	if err != nil {
		return nil, err
	}
	return &uplinkDriver{h: h, p: p, waits: waits, psi: h.pdu.PDUSessionID, ngKSI: selected.ngKSI}, nil
}

// request builds a PDU Session Establishment Request for a fresh PDU
// session identity and procedure transaction identity on the harness's DNN
// and slice, and protects it at the UE's next uplink NAS COUNT. It is not
// sent.
func (d *uplinkDriver) request() (sessionRequest, error) {
	if d.psi >= 15 {
		return sessionRequest{}, errors.New("out of PDU session identities")
	}
	d.psi++
	d.pti++
	params := d.h.pdu
	params.PDUSessionID, params.PTI = d.psi, d.pti
	sm, err := ue.BuildPDUSessionEstablishmentRequest(params)
	if err != nil {
		return sessionRequest{}, err
	}
	transport, err := ue.BuildULNASTransportForPDUSession(sm, params)
	if err != nil {
		return sessionRequest{}, err
	}
	pdu, err := d.p.Protect(transport)
	if err != nil {
		return sessionRequest{}, err
	}
	return sessionRequest{psi: d.psi, pti: d.pti, pdu: pdu}, nil
}

// sendRequest builds a fresh request and sends it.
func (d *uplinkDriver) sendRequest() (sessionRequest, error) {
	r, err := d.request()
	if err != nil {
		return r, err
	}
	return r, d.p.Send(r.pdu)
}

// mobilityRegistration builds a REGISTRATION REQUEST for mobility
// registration updating, naming the UE by its 5G-GUTI and its security
// context by ngKSI, with the UE's security capability and the harness's
// slice as requested NSSAI and no EPS NAS message container. It is
// protected at the UE's next uplink NAS COUNT and not sent.
func (d *uplinkDriver) mobilityRegistration() ([]byte, error) {
	guti := d.p.GUTI()
	if len(guti) == 0 {
		return nil, errors.New("the Registration Accept assigned no 5G-GUTI to name the UE by")
	}
	requested, err := ue.BuildRequestedNSSAI([]ue.SNSSAI{d.h.pdu.Slice()})
	if err != nil {
		return nil, err
	}
	caps := ue.SecurityCapability()
	if d.h.caps != nil {
		caps = *d.h.caps
	}
	plain, err := ue.BuildRegistrationRequest(guti, caps, requested, ue.WithMobilityRegistration(d.ngKSI))
	if err != nil {
		return nil, err
	}
	msg, err := nas.DecodePlain(plain)
	if err != nil {
		return nil, err
	}
	return d.p.Protect(msg)
}

// corruptMAC returns nasPDU with its message authentication code (octets 3
// to 6 of the security protected message) inverted.
func corruptMAC(nasPDU []byte) []byte {
	out := append([]byte(nil), nasPDU...)
	for i := 2; i < 6 && i < len(out); i++ {
		out[i] ^= 0xff
	}
	return out
}

// watch reads the UE's downlink for up to wait, or until done reports true
// for what has been seen, the core releases the UE or the association goes.
// The error is ctx's.
func (d *uplinkDriver) watch(ctx context.Context, wait time.Duration, done func(downlinkLog) bool) (downlinkLog, error) {
	var l downlinkLog
	deadline := time.Now().Add(wait)
	for {
		if done != nil && done(l) {
			return l, nil
		}
		dl, err := d.p.Next(ctx, time.Until(deadline))
		switch {
		case errors.Is(err, engine.ErrProbeQuiet):
			return l, nil
		case errors.Is(err, engine.ErrAssociationLost):
			l.lost, d.gone = err, true
			return l, nil
		case err != nil:
			return l, err
		}
		if dl.Released {
			l.released, d.gone = true, true
			return l, nil
		}
		for _, psi := range dl.Sessions {
			l.sm = append(l.sm, smReply{psi: psi, setup: true, name: "PDU Session Resource Setup Request"})
			d.noteEstablished(psi)
		}
		for _, ev := range dl.NAS {
			if ev.Err != nil {
				l.undecoded = append(l.undecoded, fmt.Sprintf("%v (NAS %x)", ev.Err, ev.Raw))
				continue
			}
			sm, ok := dlN1SMFrom(ev.Msg)
			if !ok || len(sm.payload) < 4 || sm.payload[0] != nasMessage.Epd5GSSessionManagementMessage {
				l.mm = append(l.mm, ev.Msg.GmmHeader.GetMessageType())
				continue
			}
			info := readSM(sm.payload)
			l.sm = append(l.sm, smReply{psi: sm.payload[1], pti: sm.payload[2], name: info.name})
			if info.kind == smAccept {
				d.noteEstablished(sm.payload[1])
			}
		}
	}
}

func (d *uplinkDriver) noteEstablished(psi uint8) {
	for _, e := range d.established {
		if e == psi {
			return
		}
	}
	d.established = append(d.established, psi)
}

// awaitAnswer watches for the answer to r for the answer wait.
func (d *uplinkDriver) awaitAnswer(ctx context.Context, r sessionRequest) (downlinkLog, error) {
	return d.watch(ctx, d.waits.answer, func(l downlinkLog) bool { return l.answer(r) != nil })
}

// total is the longest a check with n answered requests and one discarded
// message can take, with the liveness probe.
func (w uplinkWaits) total(n int) time.Duration {
	w = w.withDefaults()
	return w.register + time.Duration(n)*(w.answer+w.settle) + w.quiet + w.cleanup + 6*time.Second
}

// cleanup ends the UE and says what it did: a PDU Session Release Request
// for each session the core set up (best effort, unanswered), a switch-off
// Deregistration Request with a bounded wait for the UE Context Release
// Command, and then the association is dropped whatever came back. A UE the
// core already released, or whose association went, is only dropped.
func (d *uplinkDriver) cleanup() string {
	defer d.p.Close()
	if d.gone {
		return "cleanup: the UE was already released, so the association was only dropped"
	}
	var steps []string
	var released []string
	for _, psi := range d.established {
		d.pti++
		sm, err := ue.BuildPDUSessionReleaseRequest(psi, d.pti)
		if err == nil {
			var m *f5nas.Message
			if m, err = ue.BuildULNASTransportForSM(sm, psi); err == nil {
				var pdu []byte
				if pdu, err = d.p.Protect(m); err == nil {
					err = d.p.Send(pdu)
				}
			}
		}
		if err != nil {
			steps = append(steps, fmt.Sprintf("PDU Session Release Request for session %d not sent (%v)", psi, err))
			continue
		}
		released = append(released, fmt.Sprint(psi))
	}
	if len(released) > 0 {
		steps = append(steps, "PDU Session Release Request sent for session "+strings.Join(released, ", "))
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.waits.cleanup+time.Second)
	defer cancel()
	switch ok, err := d.p.Deregister(ctx, d.waits.cleanup); {
	case err != nil:
		steps = append(steps, fmt.Sprintf("deregistration failed (%v); the association was dropped, and the core may keep a half-registered UE until its own timers expire", err))
	case ok:
		steps = append(steps, "Deregistration Request sent and the core released the UE context")
	default:
		steps = append(steps, fmt.Sprintf("Deregistration Request sent, no UE Context Release Command within %s; the association was dropped, and the core may keep a half-registered UE until its own timers expire", d.waits.cleanup))
	}
	return "cleanup: " + strings.Join(steps, "; ")
}
