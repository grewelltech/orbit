package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/conformance"
)

// `conformance --list` needs no --amf or PLMN: it lists the checks (filtered
// by --category) with how long each can take at --per-test-timeout, and the
// total. Without --list, --amf is still required.
func TestConformanceList(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantNot []string
		wantErr string
	}{
		{name: "all checks", args: []string{"--list"},
			want: []string{"CHECK", "UP TO", "NAS-T3560-AUTH-RETRANSMIT", "3m10s", "NAS-T3550-ACCEPT-RETRANSMIT", "2m25s", "NGAP-NG-RESET-ACK", "in total at --per-test-timeout 15s"}},
		{name: "nas only", args: []string{"--list", "--category", "nas"},
			want: []string{"NAS-T3560-AUTH-RETRANSMIT", "4 checks, up to 6m25s in total"}, wantNot: []string{"NGAP-NG-RESET-ACK"}},
		{name: "longer per-test timeout", args: []string{"--list", "--category", "procedural", "--per-test-timeout", "40s"},
			want: []string{"NGAP-NG-RESET-ACK", "40s", "at --per-test-timeout 40s"}},
		{name: "no --amf without --list", args: nil, wantErr: `required flag(s) "amf" not set`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(append([]string{"conformance"}, tc.args...))
			err := root.Execute()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out.String(), w) {
					t.Errorf("output has %q:\n%s", w, out.String())
				}
			}
		})
	}
}

// syncBuffer is a bytes.Buffer safe for the announcer's ticker goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The announcer prints the suite total, each check before it runs, and a
// progress line every period for a check that can run longer than that;
// a short check gets none, and done stops the lines.
func TestAnnouncer(t *testing.T) {
	reg := conformance.NewRegistry()
	nas := reg.Select(conformance.NAS)
	var w syncBuffer
	a := newAnnouncer(&w, 20*time.Millisecond)
	a.suite(nas, conformance.DefaultPerTestTimeout)
	a.start(nas[0], 100*time.Millisecond, 0, 2)
	time.Sleep(70 * time.Millisecond)
	a.done()
	after := strings.Count(w.String(), "elapsed of up to")
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(w.String(), "elapsed of up to"); n != after {
		t.Errorf("progress lines kept coming after done: %d then %d", after, n)
	}
	a.start(nas[1], 10*time.Millisecond, 1, 2)
	time.Sleep(50 * time.Millisecond)
	a.done()
	out := w.String()
	for _, want := range []string{
		"Running 4 checks, up to 6m25s in total\n",
		"[1/2] " + nas[0].ID() + " (nas): up to 100ms\n",
		"[2/2] " + nas[1].ID() + " (nas): up to 10ms\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if after < 2 {
		t.Errorf("%d progress lines for the long check, want at least 2:\n%s", after, out)
	}
	if strings.Contains(out, nas[1].ID()+": ") {
		t.Errorf("the short check got progress lines:\n%s", out)
	}
}
