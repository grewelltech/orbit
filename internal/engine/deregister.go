package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
)

// DefaultDeregistrationAcceptWait is how long a normal deregistration waits
// for the Deregistration Accept when the caller gives no bound: 15 s, the
// default of the UE's T3521 (TS 24.501 Table 10.2.1).
const DefaultDeregistrationAcceptWait = 15 * time.Second

// DeregisterOption adjusts Manager.DeregisterWith.
type DeregisterOption func(*deregisterOptions)

// deregisterOptions is what the options of one DeregisterWith call select.
type deregisterOptions struct {
	normal      bool
	acceptWait  time.Duration
	releaseWait time.Duration
}

// WithReleaseWait bounds this call's wait for the UE Context Release
// Command at d instead of the Manager's linger (3 s); zero or less keeps
// the linger.
func WithReleaseWait(d time.Duration) DeregisterOption {
	return func(o *deregisterOptions) { o.releaseWait = d }
}

// WithNormalDeregistration makes the deregistration a normal one (the
// De-registration type's switch-off bit is 0), which the AMF answers with a
// Deregistration Accept (TS 24.501 5.5.2.2.2). The call waits up to
// acceptWait for it (zero or less: DefaultDeregistrationAcceptWait), then for
// the UE Context Release Command as a switch-off does. A core that answers
// with neither is sent a switch-off Deregistration Request before the
// association closes, so it is not left holding the UE as registered.
func WithNormalDeregistration(acceptWait time.Duration) DeregisterOption {
	return func(o *deregisterOptions) {
		o.normal = true
		o.acceptWait = acceptWait
	}
}

// UEContextRelease is a UE CONTEXT RELEASE COMMAND the core sent on a UE's
// association: when it arrived and the UE NGAP IDs it named (TS 38.413
// 9.2.2.5: the ID pair, or the AMF UE NGAP ID alone).
type UEContextRelease struct {
	At time.Time
	// HasAMFUENGAPID and HasRANUENGAPID report which IDs the command
	// carried; the values are zero when absent.
	HasAMFUENGAPID, HasRANUENGAPID bool
	AMFUENGAPID, RANUENGAPID       int64
}

// DeregisterResult reports what the core answered to a Deregistration
// Request.
type DeregisterResult struct {
	// SwitchOff is the De-registration type sent.
	SwitchOff bool
	// AMFUENGAPID and RANUENGAPID identify the UE-associated connection the
	// request went out on.
	AMFUENGAPID, RANUENGAPID int64
	// SentAt is when the Deregistration Request was sent.
	SentAt time.Time
	// Accepted reports a Deregistration Accept, read through the UE's NAS
	// security context, at AcceptAt. A switch-off deregistration never
	// waits for one. AcceptErr says why a normal deregistration saw none.
	Accepted  bool
	AcceptAt  time.Time
	AcceptErr error
	// Release is the UE CONTEXT RELEASE COMMAND that followed, answered
	// with a UE CONTEXT RELEASE COMPLETE; nil with ReleaseErr saying why
	// none was seen.
	Release    *UEContextRelease
	ReleaseErr error
	// SwitchOffFallback reports that a normal deregistration drew neither
	// an Accept nor a release, and a switch-off Deregistration Request was
	// sent after it; Release and ReleaseErr then describe its answer.
	SwitchOffFallback bool
}

// errNoDeregistrationAccept is how a normal deregistration reports that the
// core sent no Deregistration Accept.
var errNoDeregistrationAccept = errors.New("the core sent no Deregistration Accept")

// awaitRelease waits on the UE's association for the UE Context Release
// Command that answers its deregistration, and answers it with UE Context
// Release Complete. The wait ends with parent, or after linger, as
// errNoUEContextRelease; other downlink PDUs are skipped (and logged at
// debug).
func (s *Session) awaitRelease(parent context.Context, linger time.Duration, log *slog.Logger) (*UEContextRelease, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, linger)
	defer cancel()
	for {
		pdu, err := s.readPDU(ctx)
		if parent.Err() != nil {
			return nil, fmt.Errorf("%w: waited %s: %v", errNoUEContextRelease, time.Since(start).Round(time.Millisecond), parent.Err())
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w within %s", errNoUEContextRelease, linger)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errNoUEContextRelease, err)
		}
		rel, ok := releaseCommandOf(pdu)
		if !ok {
			if log != nil {
				code, _ := procedureCode(pdu)
				log.Debug("switch-off deregistration: skipping a downlink PDU while waiting for the release",
					"supi", s.SUPI, "procedure", procedureName(code), "present", pdu.Present)
			}
			continue
		}
		if err := s.answerRelease(pdu); err != nil {
			return nil, err
		}
		return rel, nil
	}
}

// answerRelease answers the UE Context Release Command cmd (completeRelease).
func (s *Session) answerRelease(cmd *ngapType.NGAPPDU) error {
	if err := s.completeRelease(cmd); err != nil {
		return fmt.Errorf("UE Context Release Complete not sent: %w", err)
	}
	return nil
}

// releaseCommandOf reads pdu as a UE CONTEXT RELEASE COMMAND, stamped now:
// ok is false for any other PDU.
func releaseCommandOf(pdu *ngapType.NGAPPDU) (*UEContextRelease, bool) {
	code, ok := procedureCode(pdu)
	if !ok || code != ngapType.ProcedureCodeUEContextRelease || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage {
		return nil, false
	}
	rel := &UEContextRelease{At: time.Now()}
	cmd := pdu.InitiatingMessage.Value.UEContextReleaseCommand
	if cmd == nil {
		return rel, true
	}
	for _, ie := range cmd.ProtocolIEs.List {
		ids := ie.Value.UENGAPIDs
		if ie.Id.Value != ngapType.ProtocolIEIDUENGAPIDs || ids == nil {
			continue
		}
		switch {
		case ids.Present == ngapType.UENGAPIDsPresentUENGAPIDPair && ids.UENGAPIDPair != nil:
			rel.HasAMFUENGAPID, rel.AMFUENGAPID = true, ids.UENGAPIDPair.AMFUENGAPID.Value
			rel.HasRANUENGAPID, rel.RANUENGAPID = true, ids.UENGAPIDPair.RANUENGAPID.Value
		case ids.Present == ngapType.UENGAPIDsPresentAMFUENGAPID && ids.AMFUENGAPID != nil:
			rel.HasAMFUENGAPID, rel.AMFUENGAPID = true, ids.AMFUENGAPID.Value
		}
	}
	return rel, true
}

// awaitDeregistrationAccept waits for the answer to a normal deregistration
// and records it in res: the Deregistration Accept, within acceptWait, then
// the UE Context Release Command, within linger of the Accept. A release
// that comes without an Accept ends the wait, since no Accept follows it.
// When neither comes, the core may still hold the UE as registered, so a
// switch-off Deregistration Request follows and its release is awaited for
// linger (res.SwitchOffFallback). The waits read through readPDU, so the
// read the Accept wait gives up on is the one the next wait resumes.
func (s *Session) awaitDeregistrationAccept(parent context.Context, acceptWait, linger time.Duration, log *slog.Logger, res *DeregisterResult) {
	if acceptWait <= 0 {
		acceptWait = DefaultDeregistrationAcceptWait
	}
	res.AcceptErr = s.awaitAnswer(parent, acceptWait, true, log, res)
	switch {
	case res.Release != nil || res.ReleaseErr != nil || parent.Err() != nil:
	case res.Accepted:
		res.ReleaseErr = s.awaitAnswer(parent, linger, false, log, res)
	default:
		res.SwitchOffFallback = true
		if err := s.deregister(parent, true); err != nil {
			res.ReleaseErr = fmt.Errorf("%w: the switch-off Deregistration Request that followed was not sent: %v", errNoUEContextRelease, err)
			return
		}
		res.ReleaseErr = s.awaitAnswer(parent, linger, false, log, res)
	}
	if res.Release == nil && res.ReleaseErr == nil {
		res.ReleaseErr = fmt.Errorf("%w: %v", errNoUEContextRelease, parent.Err())
	}
}

// awaitAnswer reads the association for up to wait: until the Deregistration Accept
// when wantAccept is set, until the UE Context Release Command (which it
// completes and records in res.Release) otherwise. A release always ends
// it. The error says why what it waited for did not come: the Accept, as
// errNoDeregistrationAccept, or the release, as errNoUEContextRelease. A
// failure to send the Release Complete is recorded in res.ReleaseErr.
func (s *Session) awaitAnswer(parent context.Context, wait time.Duration, wantAccept bool, log *slog.Logger, res *DeregisterResult) error {
	missing := errNoUEContextRelease
	if wantAccept {
		missing = errNoDeregistrationAccept
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, wait)
	defer cancel()
	for {
		pdu, err := s.readPDU(ctx)
		switch {
		case err == nil:
		case parent.Err() != nil:
			return fmt.Errorf("%w: waited %s: %v", missing, time.Since(start).Round(time.Millisecond), parent.Err())
		case ctx.Err() != nil:
			return fmt.Errorf("%w within %s", missing, wait)
		default:
			return fmt.Errorf("%w: %v", missing, err)
		}
		if rel, ok := releaseCommandOf(pdu); ok {
			if err := s.answerRelease(pdu); err != nil {
				res.ReleaseErr = err
			} else {
				res.Release = rel
			}
			if wantAccept {
				return fmt.Errorf("%w: a UE Context Release Command came instead", missing)
			}
			return nil
		}
		if wantAccept && s.isDeregistrationAccept(pdu, log) {
			res.Accepted, res.AcceptAt = true, time.Now()
			return nil
		}
	}
}

// isDeregistrationAccept reports whether pdu is a Downlink NAS Transport
// carrying a Deregistration Accept (UE originating), read through the
// session's security context when it is protected. A NAS PDU that does not
// decode is logged and is not one.
func (s *Session) isDeregistrationAccept(pdu *ngapType.NGAPPDU, log *slog.Logger) bool {
	dl, err := gnb.ParseDownlink(pdu)
	if err != nil || dl.Procedure != ngapType.ProcedureCodeDownlinkNASTransport || len(dl.NASPDU) < 2 {
		return false
	}
	var msg *f5nas.Message
	if f5nas.GetSecurityHeaderType(dl.NASPDU) == f5nas.SecurityHeaderTypePlainNas {
		msg, err = nas.DecodePlain(dl.NASPDU)
	} else {
		msg, err = s.sec.DecodeSecure(dl.NASPDU)
	}
	if err != nil {
		if log != nil {
			log.Warn("deregistration: a downlink NAS PDU did not decode", "supi", s.SUPI, "err", err)
		}
		return false
	}
	return msg.GmmMessage != nil && msg.GmmHeader.GetMessageType() == f5nas.MsgTypeDeregistrationAcceptUEOriginatingDeregistration
}
