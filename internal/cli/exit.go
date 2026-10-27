package cli

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

// Exit codes (ADR-0008 open question 5). A failed gate keeps the 1 that
// `orbit load`'s SLO breach and `orbit conformance`'s gate have always
// exited with; a run that did not complete, and one that never started,
// get their own, so CI can tell "fix the core" from "fix the lab".
const (
	// ExitPass is a run that completed with every gate passed (or none).
	ExitPass = 0
	// ExitGateFailed is a run that completed with a gate failed.
	ExitGateFailed = 1
	// ExitDidNotComplete is a run that failed, was cancelled, or had a
	// workload that did not complete, or has no report.
	ExitDidNotComplete = 2
	// ExitCouldNotStart is a run that never started: refused (an invalid
	// plan, a lease another run holds), no server, or a usage error.
	ExitCouldNotStart = 3
)

// ExitMeaning is an exit code in words, for stderr.
func ExitMeaning(code int) string {
	switch code {
	case ExitPass:
		return "the run completed and every gate passed"
	case ExitGateFailed:
		return "a gate failed"
	case ExitDidNotComplete:
		return "the run did not complete"
	case ExitCouldNotStart:
		return "the run could not start"
	}
	return "unknown"
}

// ExitError is an error with the exit code it ends the command with.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// exitWith wraps err with an exit code; nil stays nil. A server's
// INVALID_ARGUMENT or NOT_FOUND is bad input whatever the stage (a wrong
// run ID, an invalid request): could not start, never "did not complete".
func exitWith(code int, err error) error {
	if err == nil {
		return nil
	}
	if badInput(err) {
		code = ExitCouldNotStart
	}
	return &ExitError{Code: code, Err: err}
}

// badInput reports whether err is the server refusing the request itself.
func badInput(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeNotFound:
		return true
	}
	return false
}

// gateFailed is the error a command returns for a failed gate.
func gateFailed(format string, args ...any) error {
	return &ExitError{Code: ExitGateFailed, Err: fmt.Errorf(format, args...)}
}

// ExitCode is the exit code err ends the command with: its ExitError's, or
// ExitCouldNotStart for any other error (a command fails that way before
// its run is underway: usage, no server, a refused start).
func ExitCode(err error) int {
	if err == nil {
		return ExitPass
	}
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return ExitCouldNotStart
}
