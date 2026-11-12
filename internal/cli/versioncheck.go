package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// versionInfoTimeout bounds the one GetInfo call a version check makes, so a
// slow server cannot hold up the command it is checking for.
const versionInfoTimeout = 3 * time.Second

// versionCheck compares this CLI's version with the API server's, once per
// command. A server keeps running the binary it started with, so after an
// upgrade the CLI and an `orbit serve` that was not restarted disagree, and
// the server's (older) behaviour is what the command gets.
type versionCheck struct {
	client string
	w      io.Writer
	once   sync.Once
}

type versionCheckKey struct{}

// withVersionCheck returns ctx carrying a version check for a CLI at version
// client that warns on w.
func withVersionCheck(ctx context.Context, client string, w io.Writer) context.Context {
	return context.WithValue(ctx, versionCheckKey{}, &versionCheck{client: client, w: w})
}

// versionCheckFrom returns the check ctx carries, or nil.
func versionCheckFrom(ctx context.Context) *versionCheck {
	vc, _ := ctx.Value(versionCheckKey{}).(*versionCheck)
	return vc
}

// run asks the server at base for its version through info and writes one
// warning line when it differs from the CLI's. It does so at most once; a
// server that cannot answer, or answers with no version, is not reported.
func (vc *versionCheck) run(ctx context.Context, base string, info func(context.Context) (string, error)) {
	vc.once.Do(func() {
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), versionInfoTimeout)
		defer cancel()
		server, err := info(ictx)
		if err != nil || server == "" || server == vc.client {
			return
		}
		// A failed stderr write is no reason to fail the command.
		_, _ = fmt.Fprintf(vc.w, "warning: the server at %s runs orbit %s and this CLI is orbit %s; "+
			"a server keeps running the binary it started with, so restart it (or upgrade it) to match\n",
			base, server, vc.client)
	})
}

// versionInterceptor runs the context's version check after the first call
// to the server at base, using h and opts for the GetInfo call. opts must not
// include a versionInterceptor, so the check's own call does not check again.
type versionInterceptor struct {
	base string
	h    *http.Client
	opts []connect.ClientOption
}

func (v versionInterceptor) info(ctx context.Context) (string, error) {
	res, err := orbitv1connect.NewSystemServiceClient(v.h, v.base, v.opts...).GetInfo(ctx,
		connect.NewRequest(&orbitv1.GetInfoRequest{}))
	if err != nil {
		return "", err
	}
	return res.Msg.GetVersion(), nil
}

// WrapUnary checks after the call returns, so the check never delays or
// fails the call itself.
func (v versionInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		if vc := versionCheckFrom(ctx); vc != nil {
			vc.run(ctx, v.base, v.info)
		}
		return res, err
	}
}

// WrapStreamingClient checks when the stream is opened.
func (v versionInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if vc := versionCheckFrom(ctx); vc != nil {
			vc.run(ctx, v.base, v.info)
		}
		return conn
	}
}

// WrapStreamingHandler is a no-op: the interceptor is client-side only.
func (v versionInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
