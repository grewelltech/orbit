package mockamf

import (
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/ngap/ngapType"
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

// WithCrashAfterRelease makes the mock crash once it has released a UE
// (WithT3560's release, or ReleaseNoCommonIntegrity's): from then on any association closes on its next message, so a fresh
// NG Setup fails at once.
func WithCrashAfterRelease() Option { return func(a *AMF) { a.crashAfterRelease = true } }

// WithNoAuthenticationRequest leaves the Initial UE Message unanswered: the
// mock never challenges the UE.
func WithNoAuthenticationRequest() Option { return func(a *AMF) { a.noAuthRequest = true } }

// WithAuthenticationReject answers the Initial UE Message with an
// Authentication Reject instead of an Authentication Request.
func WithAuthenticationReject() Option { return func(a *AMF) { a.authReject = true } }

// t3560Timer is one UE's running T3560; stop ends it when the Authentication
// Response arrives or the association goes.
type t3560Timer struct {
	once sync.Once
	done chan struct{}
}

func (t *t3560Timer) stop() {
	if t != nil {
		t.once.Do(func() { close(t.done) })
	}
}

// startT3560 runs T3560 for st on its own goroutine, retransmitting ar (the
// encoded Authentication Request) on p. Writes to p are safe from any
// goroutine; a failed write (the association closed) ends the timer.
func (a *AMF) startT3560(p *pipe, st *ueState, ar []byte) *t3560Timer {
	t := &t3560Timer{done: make(chan struct{})}
	amfID, ranID := st.amfID, st.ranID
	go func() {
		for expiry := 1; ; expiry++ {
			wait := a.t3560
			if expiry > a.t3560Retries && a.t3560ReleaseAfter > 0 {
				wait = a.t3560ReleaseAfter
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
			if expiry <= a.t3560Retries {
				if a.send(p, buildDownlinkNASTransport(amfID, ranID, ar)) != nil {
					return
				}
				continue
			}
			if !a.t3560NoRelease {
				_ = a.send(p, buildUEContextReleaseCommand(amfID, ranID))
				if a.crashAfterRelease {
					// Not closing p here: a pipe read can pick the close
					// over the queued release. The next message on any
					// association ends it (serve).
					a.crashed.Store(true)
				}
			}
			return
		}
	}()
	return t
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
