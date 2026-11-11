package conformance

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/bgrewell/orbit/internal/engine"
)

// A check's setup is the procedures it needs before its stimulus: its NG
// Setup, its UE's registration, the UE's PDU session. The run's preflight
// (ADR-0011 rule 6) has shown that each kind of setup works against this
// core with this configuration, so a setup step that does not complete
// inside a check is attributed in one place, here, and the same way for
// every check: to the core (BLOCKED), unless the fault is plainly local
// (ERROR). A check carries no attribution of its own.

// procedureError is a setup error that names the procedure that did not
// complete.
type procedureError struct {
	proc Procedure
	err  error
}

func (e *procedureError) Error() string { return e.err.Error() }
func (e *procedureError) Unwrap() error { return e.err }

// during marks err as the failure of procedure p; nil stays nil.
func during(p Procedure, err error) error {
	if err == nil {
		return nil
	}
	return &procedureError{proc: p, err: err}
}

// localError is a setup error that is this host's doing whatever its cause:
// a socket of the data path that could not be opened or bound.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

// setupProcedure is the procedure a setup error names: the one it was
// marked with (during), the NG Setup for an attach that failed before it
// began (engine.ErrNGSetup), else the registration.
func setupProcedure(err error) Procedure {
	var pe *procedureError
	switch {
	case errors.As(err, &pe):
		return pe.proc
	case errors.Is(err, engine.ErrNGSetup):
		return ProcNGSetup
	}
	return ProcRegistration
}

// localFaults are the socket errors that are this host's or the testbed's
// doing, not the core's: an address that cannot be bound, a missing
// permission or protocol, no route.
var localFaults = []syscall.Errno{
	syscall.EADDRINUSE, syscall.EADDRNOTAVAIL, syscall.EACCES, syscall.EPERM, syscall.EINVAL,
	syscall.EAFNOSUPPORT, syscall.EPROTONOSUPPORT, syscall.ENETUNREACH, syscall.ENETDOWN,
	syscall.EMFILE, syscall.ENFILE,
}

// localFault reports whether a setup error says nothing about the core: the
// run was cancelled, a data-path socket could not be opened (localError),
// or the error is one of localFaults.
func localFault(err error) bool {
	var le *localError
	if errors.As(err, &le) || errors.Is(err, context.Canceled) {
		return true
	}
	for _, errno := range localFaults {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// SetupFailed returns r for a setup step of the check that did not
// complete with err: ERROR when the fault is local (localFault), otherwise
// BLOCKED on the procedure the error names (setupProcedure), which the
// check must declare as a need. observed says what failed, in the check's
// words ("UE registration failed").
func (r Result) SetupFailed(observed string, err error) Result {
	if localFault(err) {
		return r.Errored(observed, err.Error())
	}
	p := setupProcedure(err)
	detail := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		detail = fmt.Sprintf("%s (the core did not complete %s before the check's time ran out)", detail, p.article())
	}
	return r.BlockedBy(p, observed, detail)
}
