package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// A UE CONTEXT RELEASE REQUEST over the mock pair. When the AMF releases
// the UE, the command is answered, the UE has no NG connection left (a
// second request is refused), and its deregistration goes out as an initial
// NAS message whose release is answered under the IDs that release names.
// When the AMF ignores the request the UE is as it was and deregisters over
// its NG connection, the wait having left no read behind.
func TestRequestContextReleaseOverMockPair(t *testing.T) {
	tests := []struct {
		name          string
		net24         int
		opts          []mockamf.Option
		wantReleased  bool
		wantIdleDereg int64
		wantCompletes int64
	}{
		{name: "released", net24: testnet.EngineReleaseOnReq, wantReleased: true, wantIdleDereg: 1, wantCompletes: 2},
		{name: "request ignored", net24: testnet.EngineReleaseIgnored, opts: []mockamf.Option{mockamf.WithIgnoreUEContextReleaseRequest()}, wantCompletes: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false, append(tc.opts, mockamf.WithGUTI())...)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100007540"
			sess := p.register(t, m, supi)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			start := time.Now()
			res, err := m.RequestContextRelease(ctx, supi, 400*time.Millisecond)
			if err != nil {
				t.Fatalf("RequestContextRelease: %v", err)
			}
			if res.Released != tc.wantReleased || (tc.wantReleased && (res.After <= 0 || !strings.HasPrefix(res.Cause, "nas("))) {
				t.Errorf("result %+v, want released %v with its NAS cause", res, tc.wantReleased)
			}
			if !tc.wantReleased && time.Since(start) < 400*time.Millisecond {
				t.Errorf("returned after %s, before the wait for the command ran out", time.Since(start))
			}
			if got := p.amf.ReleaseRequests(); got != 1 {
				t.Errorf("mock AMF saw %d UE Context Release Requests, want 1", got)
			}
			if sess.N3LocalAddr() != nil && tc.wantReleased {
				t.Error("the data path is still open after the release")
			}
			_, err = m.RequestContextRelease(ctx, supi, 100*time.Millisecond)
			if tc.wantReleased && (err == nil || !strings.Contains(err.Error(), "no NG connection")) {
				t.Errorf("second request on a released UE = %v, want it refused", err)
			}
			if !tc.wantReleased && err != nil {
				t.Errorf("second request on a UE the AMF kept = %v", err)
			}

			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			if err := m.Deregister(dctx, supi); err != nil {
				t.Errorf("Deregister: %v", err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for p.amf.ReleaseCompletes() < tc.wantCompletes && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := p.amf.Deregistrations(); got != 1 {
				t.Errorf("mock AMF saw %d deregistrations, want 1", got)
			}
			if got := p.amf.IdleDeregistrations(); got != tc.wantIdleDereg {
				t.Errorf("mock AMF saw %d deregistrations in an Initial UE Message, want %d", got, tc.wantIdleDereg)
			}
			sent, got := p.amf.DeregistrationReleaseIDs(), p.amf.ReleaseCompleteIDs()
			if len(sent) != 1 || int64(len(got)) != tc.wantCompletes || got[len(got)-1] != sent[0] {
				t.Errorf("Release Completes %v, want %d, the last for the switch-off's release %v", got, tc.wantCompletes, sent)
			}
			if n := p.amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
		})
	}
}

// A UE that is not registered has no context to release.
func TestRequestContextReleaseUnknownUE(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.RequestContextRelease(context.Background(), "208930100009999", time.Second); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("RequestContextRelease = %v, want a not-registered error", err)
	}
}
