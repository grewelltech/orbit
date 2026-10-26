package mockamf

import (
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/nas"
)

// Option adjusts a mock AMF's behaviour beyond Config (New).
type Option func(*AMF)

// WithT3560 makes the mock run timer T3560 (TS 24.501 5.4.1.3.7 b): on each
// expiry with no Authentication Response it retransmits the same
// Authentication Request, retries times, and on the expiry after the last
// retransmission it releases the UE with a UE CONTEXT RELEASE COMMAND. A
// conforming AMF uses retries 4; other values model a wrong count. The
// default is no timer: the request is sent once and never repeated.
func WithT3560(d time.Duration, retries int) Option {
	return func(a *AMF) { a.t3560, a.t3560Retries = d, retries }
}

// WithT3560ReleaseAfter sends WithT3560's release d after the last
// retransmission instead of on the next expiry (one T3560 later): a release
// that comes late.
func WithT3560ReleaseAfter(d time.Duration) Option { return func(a *AMF) { a.t3560ReleaseAfter = d } }

// WithT3560NoRelease keeps WithT3560's retransmissions but sends no UE
// CONTEXT RELEASE COMMAND on the last expiry: the AMF aborts locally.
func WithT3560NoRelease() Option { return func(a *AMF) { a.t3560NoRelease = true } }

// WithCrashAfterRelease makes the mock crash once WithT3560 has released a
// UE: from then on any association closes on its next message, so a fresh
// NG Setup fails at once.
func WithCrashAfterRelease() Option { return func(a *AMF) { a.crashAfterRelease = true } }

// WithNoAuthenticationRequest leaves the Initial UE Message unanswered: the
// mock never challenges the UE.
func WithNoAuthenticationRequest() Option { return func(a *AMF) { a.noAuthRequest = true } }

// WithAuthenticationReject answers the Initial UE Message with an
// Authentication Reject instead of an Authentication Request.
func WithAuthenticationReject() Option { return func(a *AMF) { a.authReject = true } }

// WithT3550 makes the mock run timer T3550 (TS 24.501 5.5.1.2.8 c)): on
// each expiry with no Registration Complete it retransmits the Registration
// Accept in a DOWNLINK NAS TRANSPORT, retries times, and on the expiry after
// that aborts silently (the AMF enters 5GMM-REGISTERED; no release). A
// conforming AMF uses retries 4. Only an Accept carrying a 5G-GUTI starts
// T3550 (5.5.1.2.4), so this needs WithGUTI to have any effect. The default
// is no timer.
func WithT3550(d time.Duration, retries int) Option {
	return func(a *AMF) { a.t3550, a.t3550Retries = d, retries }
}

// WithGUTI makes the Registration Accept assign a 5G-GUTI, which a UE
// acknowledges with Registration Complete. The default assigns none.
func WithGUTI() Option { return func(a *AMF) { a.assignGUTI = true } }

// WithCrashAfterT3550Abort makes the mock crash once WithT3550's last
// expiry has aborted the procedure: from then on any association closes on
// its next message, so a fresh NG Setup fails at once.
func WithCrashAfterT3550Abort() Option { return func(a *AMF) { a.crashAfterT3550 = true } }

// retransmitter is one UE's running NAS retransmission timer (T3560 or
// T3550); stop ends it when the awaited reply arrives or the association
// goes.
type retransmitter struct {
	once sync.Once
	done chan struct{}
}

func (t *retransmitter) stop() {
	if t != nil {
		t.once.Do(func() { close(t.done) })
	}
}

// startRetransmit runs a NAS timer of period d on its own goroutine: on
// expiry n it sends resend[n-1], and on the expiry after the last (after
// last instead of d, when last > 0) it calls onLast (if set) and ends.
// Writes to p are safe from any goroutine; a failed write (the association
// closed) ends the timer. resend is encoded up front by the caller, on the
// goroutine that owns the security context.
func (a *AMF) startRetransmit(p *pipe, d, last time.Duration, resend []ngapType.NGAPPDU, onLast func()) *retransmitter {
	t := &retransmitter{done: make(chan struct{})}
	go func() {
		for expiry := 1; ; expiry++ {
			wait := d
			if expiry > len(resend) && last > 0 {
				wait = last
			}
			timer := time.NewTimer(wait)
			select {
			case <-t.done:
				timer.Stop()
				return
			case <-p.closed:
				timer.Stop()
				return
			case <-timer.C:
			}
			// select picks at random when the stop and the expiry are both
			// ready; re-check the stop so nothing goes out after it.
			select {
			case <-t.done:
				return
			default:
			}
			if expiry <= len(resend) {
				if a.send(p, resend[expiry-1]) != nil {
					return
				}
				continue
			}
			if onLast != nil {
				onLast()
			}
			return
		}
	}()
	return t
}

// startT3560 runs T3560 for st, retransmitting ar (the encoded plain
// Authentication Request) and releasing the UE after the last expiry unless
// WithT3560NoRelease.
func (a *AMF) startT3560(p *pipe, st *ueState, ar []byte) *retransmitter {
	amfID, ranID := st.amfID, st.ranID
	resend := make([]ngapType.NGAPPDU, a.t3560Retries)
	for i := range resend {
		resend[i] = buildDownlinkNASTransport(amfID, ranID, ar)
	}
	return a.startRetransmit(p, a.t3560, a.t3560ReleaseAfter, resend, func() {
		if a.t3560NoRelease {
			return
		}
		_ = a.send(p, buildUEContextReleaseCommand(amfID, ranID))
		if a.crashAfterRelease {
			// Not closing p here: a pipe read can pick the close over the
			// queued release. The next message on any association ends it
			// (serve).
			a.crashed.Store(true)
		}
	})
}

// startT3550 runs T3550 for st. Each retransmission is the Registration
// Accept encoded afresh under st.sec (its own NAS COUNT), done here, before
// the timer starts, because the serve goroutine owns st.sec; a retransmission
// left unsent when the Registration Complete arrives only leaves a gap in
// the COUNT, which the UE resynchronises on.
func (a *AMF) startT3550(p *pipe, st *ueState) (*retransmitter, error) {
	resend := make([]ngapType.NGAPPDU, a.t3550Retries)
	for i := range resend {
		acc, err := a.registrationAccept(st)
		if err != nil {
			return nil, err
		}
		wrapped, err := st.sec.EncodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return nil, err
		}
		resend[i] = buildDownlinkNASTransport(st.amfID, st.ranID, wrapped)
	}
	return a.startRetransmit(p, a.t3550, 0, resend, func() {
		if a.crashAfterT3550 {
			a.crashed.Store(true)
		}
	}), nil
}

// registrationAccept builds st's Registration Accept, with a 5G-GUTI when
// WithGUTI is set.
func (a *AMF) registrationAccept(st *ueState) (*f5nas.Message, error) {
	if !a.assignGUTI {
		return buildRegistrationAccept(nil)
	}
	return buildRegistrationAccept(guti5G(a.plmn, uint32(st.amfID)))
}

// guti5G is the 5GS mobile identity value of a 5G-GUTI (TS 24.501
// 9.11.3.4): identity type 5G-GUTI, the PLMN, a fixed AMF Region, Set and
// Pointer, and tmsi as the 5G-TMSI.
func guti5G(plmn [3]byte, tmsi uint32) []byte {
	return []byte{0xf2, plmn[0], plmn[1], plmn[2], 0xca, 0xfe, 0x00,
		byte(tmsi >> 24), byte(tmsi >> 16), byte(tmsi >> 8), byte(tmsi)}
}

// buildAuthenticationReject builds a plain Authentication Reject (TS 24.501
// 8.2.5).
func buildAuthenticationReject() ([]byte, error) {
	m, gmm := plainGmm(f5nas.MsgTypeAuthenticationReject)
	rej := nasMessage.NewAuthenticationReject(f5nas.MsgTypeAuthenticationReject)
	setPlainHeader(&rej.ExtendedProtocolDiscriminator, &rej.SpareHalfOctetAndSecurityHeaderType,
		&rej.AuthenticationRejectMessageIdentity, f5nas.MsgTypeAuthenticationReject)
	gmm.AuthenticationReject = rej
	return m.PlainNasEncode()
}

// buildUEContextReleaseCommand releases the UE identified by its NGAP ID
// pair, cause NAS authentication-failure (TS 38.413 9.2.2.5).
func buildUEContextReleaseCommand(amfID, ranID int64) ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeUEContextRelease
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentUEContextReleaseCommand
	cmd := new(ngapType.UEContextReleaseCommand)
	pdu.InitiatingMessage.Value.UEContextReleaseCommand = cmd

	ids := ngapType.UEContextReleaseCommandIEs{}
	ids.Id.Value = ngapType.ProtocolIEIDUENGAPIDs
	ids.Criticality.Value = ngapType.CriticalityPresentReject
	ids.Value.Present = ngapType.UEContextReleaseCommandIEsPresentUENGAPIDs
	ids.Value.UENGAPIDs = &ngapType.UENGAPIDs{
		Present: ngapType.UENGAPIDsPresentUENGAPIDPair,
		UENGAPIDPair: &ngapType.UENGAPIDPair{
			AMFUENGAPID: ngapType.AMFUENGAPID{Value: amfID},
			RANUENGAPID: ngapType.RANUENGAPID{Value: ranID},
		},
	}
	cause := ngapType.UEContextReleaseCommandIEs{}
	cause.Id.Value = ngapType.ProtocolIEIDCause
	cause.Criticality.Value = ngapType.CriticalityPresentIgnore
	cause.Value.Present = ngapType.UEContextReleaseCommandIEsPresentCause
	cause.Value.Cause = &ngapType.Cause{
		Present: ngapType.CausePresentNas,
		Nas:     &ngapType.CauseNas{Value: ngapType.CauseNasPresentAuthenticationFailure},
	}
	cmd.ProtocolIEs.List = append(cmd.ProtocolIEs.List, ids, cause)
	return pdu
}
