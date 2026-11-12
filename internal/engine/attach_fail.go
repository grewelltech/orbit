package engine

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrAfterRegistration marks an attach that failed after the UE registered:
// the Registration Accept arrived, and a PDU session requested with the
// attach did not complete.
var ErrAfterRegistration = errors.New("registered; a requested PDU session did not complete")

// afterRegistrationError is an error that is ErrAfterRegistration and reads
// as the error it wraps.
type afterRegistrationError struct{ err error }

func (e *afterRegistrationError) Error() string { return e.err.Error() }
func (e *afterRegistrationError) Unwrap() error { return e.err }

// Is reports ErrAfterRegistration as a match.
func (e *afterRegistrationError) Is(target error) bool { return target == ErrAfterRegistration }

// failDeregisterTimeout bounds the Deregistration fail sends on an attach
// that is already failing, so cleanup never stretches the error return.
const failDeregisterTimeout = 2 * time.Second

// fail is the attach loop's error return. An error after the UE
// registered, while a requested PDU session was pending, is marked
// ErrAfterRegistration. When the UE had already
// registered (a requested PDU session then failed or timed out), it first
// sends a switch-off Deregistration under the attach's security context:
// the caller gets no Session for a failed attach, so nobody else could
// deregister it, and the core would keep a registered UE with no handle.
//
// A probe-driven attach (ueCfg.Probe, the conformance checks) is left
// alone: its check owns the cleanup. Some end the attach on purpose after
// Registration Accept and then deregister the UE themselves (the unknown-
// DNN check, #183), which an automatic switch-off here would duplicate. The
// probe guard also covers a UE the core released: ErrReleasedByCore only
// arises with a probe set (observe, probe.go).
func (s *attachState) fail(err error) error {
	if s.registered && len(s.wanted) > 0 {
		err = &afterRegistrationError{err: err}
	}
	if !s.registered || s.ueCfg.Probe != nil {
		return err
	}
	sess := &Session{SUPI: s.ueCfg.Sub.SUPI, gnbCfg: s.gnbCfg, conn: s.conn, sec: s.sec,
		amfID: s.amfID, ranID: s.ranID, guti: s.guti}
	// The attach's own context may be what expired.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), failDeregisterTimeout)
	defer cancel()
	if derr := sess.deregister(ctx, true); derr != nil {
		s.log.WarnContext(ctx, "attach failed after registration; deregistering the UE failed", "supi", sess.SUPI, "err", derr)
		return fmt.Errorf("%w (the UE was registered, and deregistering it failed: %v)", err, derr)
	}
	s.log.InfoContext(ctx, "attach failed after registration; UE deregistered", "supi", sess.SUPI, "err", err)
	s.event(StateDeregistered, "deregistered after the attach failed: "+err.Error())
	return err
}
