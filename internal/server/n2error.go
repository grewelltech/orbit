package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/sctp"
)

// n2Error maps the error of an RPC that opens an N2 association (RunNGSetup,
// RegisterUE, Handover, XnHandover) to one Connect code, so the same failure
// reads the same from every RPC:
//
//   - a handover target whose identity is invalid even after inheriting is
//     the caller's input: InvalidArgument;
//   - a dial the caller cancelled: Canceled;
//   - any other dial failure: Unavailable, the AMF cannot be reached from
//     this host. That includes an INIT nobody answered, whether the kernel
//     gave up first or the dial's bound did: the code says what the AMF did,
//     not which timer won;
//   - anything else, including a deadline that passes after a successful
//     dial (NG Setup or registration timing out): Internal.
//
// A dial failure is a *sctp.DialError anywhere in the chain, so it is found
// through the engine's wrapping ("SCTP dial: %w", "dial target gNB: %w").
// It returns nil for a nil error.
func n2Error(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, engine.ErrInvalidTarget) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	var de *sctp.DialError
	if errors.As(err, &de) {
		if errors.Is(de, context.Canceled) {
			return connect.NewError(connect.CodeCanceled, err)
		}
		return connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}
