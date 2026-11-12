package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
)

// The check warns once when the server's version differs, and says nothing
// when it matches, when the server cannot answer or reports no version.
func TestVersionCheckRun(t *testing.T) {
	tests := []struct {
		name     string
		server   string
		err      error
		wantWarn bool
	}{
		{name: "older server", server: "v0.1.0", wantWarn: true},
		{name: "dev server", server: "dev", wantWarn: true},
		{name: "same version", server: "v0.2.0"},
		{name: "no answer", err: errors.New("unavailable")},
		{name: "empty version", server: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var w bytes.Buffer
			vc := versionCheckFrom(withVersionCheck(context.Background(), "v0.2.0", &w))
			calls := 0
			info := func(context.Context) (string, error) { calls++; return tc.server, tc.err }
			vc.run(context.Background(), "https://lab:8412", info)
			vc.run(context.Background(), "https://lab:8412", info)
			if calls != 1 {
				t.Errorf("GetInfo called %d times, want 1", calls)
			}
			got := w.String()
			if tc.wantWarn {
				want := "warning: the server at https://lab:8412 runs orbit " + tc.server + " and this CLI is orbit v0.2.0;"
				if !strings.HasPrefix(got, want) || strings.Count(got, "\n") != 1 {
					t.Errorf("warning = %q, want one line starting %q", got, want)
				}
			} else if got != "" {
				t.Errorf("warning = %q, want none", got)
			}
		})
	}
}

// A context without a check checks nothing.
func TestVersionCheckAbsent(t *testing.T) {
	if versionCheckFrom(context.Background()) != nil {
		t.Fatal("background context carries a version check")
	}
}

// End to end through the CLI root against a real server: a command against
// a server on another version warns once on stderr and leaves stdout alone;
// one on the same version says nothing.
func TestCLIWarnsOnServerVersionMismatch(t *testing.T) {
	clientConfig(t)
	tests := []struct {
		name, server string
		wantWarn     bool
	}{
		{name: "server not restarted after an upgrade", server: "v0.1.0", wantWarn: true},
		{name: "same version", server: "v0.2.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), tc.server,
				observability.NewRegistry(), server.Options{}))
			t.Cleanup(srv.Close)
			root := New("v0.2.0")
			var out, errw bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errw)
			root.SetArgs([]string{"--server", srv.URL, "system", "info"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "server version: "+tc.server) {
				t.Errorf("stdout = %q", out.String())
			}
			if strings.Contains(out.String(), "warning") {
				t.Errorf("stdout carries the warning: %q", out.String())
			}
			n := strings.Count(errw.String(), "warning: the server at "+srv.URL+" runs orbit "+tc.server)
			if tc.wantWarn && n != 1 {
				t.Errorf("stderr = %q, want the warning once", errw.String())
			}
			if !tc.wantWarn && errw.String() != "" {
				t.Errorf("stderr = %q, want nothing", errw.String())
			}
		})
	}
}
