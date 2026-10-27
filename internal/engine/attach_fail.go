package engine

import (
	"context"
	"fmt"
	"time"
)

// failDeregisterTimeout bounds the Deregistration fail sends on an attach
// that is already failing, so cleanup never stretches the error return.
const failDeregisterTimeout = 2 * time.Second

// fail is the attach loop's error return. When the UE had already
// registered (a requested PDU session then failed or timed out), it first
// sends a switch-off Deregistration under the attach's security context:
// the caller gets no Session for a failed attach, so nobody else could
// deregister it, and the core would keep a registered UE with no handle.
func (s *attachState) fail(err error) error {
	if !s.registered {
		return err
	}
	sess := &Session{SUPI: s.ueCfg.Sub.SUPI, gnbCfg: s.gnbCfg, conn: s.conn, sec: s.sec,
		amfID: s.amfID, ranID: s.ranID, guti: s.guti}
	// The attach's own context may be what expired.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), failDeregisterTimeout)
	defer cancel()
	if derr := sess.deregister(ctx); derr != nil {
		s.log.WarnContext(ctx, "attach failed after registration; deregistering the UE failed", "supi", sess.SUPI, "err", derr)
		return fmt.Errorf("%w (the UE was registered, and deregistering it failed: %v)", err, derr)
	}
	s.log.InfoContext(ctx, "attach failed after registration; UE deregistered", "supi", sess.SUPI, "err", err)
	s.event(StateDeregistered, "deregistered after the attach failed: "+err.Error())
	return err
}
