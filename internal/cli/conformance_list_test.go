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
// by --group and --check, with each one's group) with how long each can take at
// --per-test-timeout, and the total. An unknown --check ID is refused
// before anything is sent, on a run the server makes too. Without --list,
// --amf is still required.
func TestConformanceList(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantNot []string
		wantErr string
	}{
		{name: "all checks", args: []string{"--list"},
			want: []string{"CHECK", "CLASS", "UP TO", "PHASE", "TAGS", "GROUP", "negative,security", "NGAP / Interface Management Procedures / NG Reset", "stranding", "disruptive", "NAS-T3560-AUTH-RETRANSMIT", "3m10s", "NAS-T3550-ACCEPT-RETRANSMIT", "2m25s", "NGAP-NG-RESET-ACK", "in total at --per-test-timeout 15s"}},
		{name: "one family", args: []string{"--list", "--group", "nas/5gmm common procedures"},
			want: []string{"NAS-T3560-AUTH-RETRANSMIT", "NAS-SMC-IMEISV-REQUEST", "5 checks, up to"}, wantNot: []string{"NGAP-NG-RESET-ACK", "NAS-T3550-ACCEPT-RETRANSMIT"}},
		{name: "two groups, one by specification number", args: []string{"--list", "--group", "TS 29.281", "--group", "NGAP / Interface Management Procedures / NG Reset"},
			want: []string{"GTPU-ECHO-RESPONSE", "NGAP-NG-RESET-ACK", "4 checks, up to"}, wantNot: []string{"NAS-T3560-AUTH-RETRANSMIT"}},
		{name: "a group titled in the target release", args: []string{"--list", "--group", "NAS / UE-requested 5GSM procedures", "--target-release", "Rel-15"},
			want: []string{"NAS-PDU-UNKNOWN-DNN", "UE-requested PDU session establishment procedure"}},
		{name: "a tag", args: []string{"--list", "--tag", "TIMING"},
			want: []string{"NAS-T3550-ACCEPT-RETRANSMIT", "NAS-REGISTRATION-AFTER-RELEASE", "4 checks, up to"}, wantNot: []string{"NGAP-NG-RESET-ACK"}},
		{name: "a tag within a group", args: []string{"--list", "--tag", "security", "--group", "TS 33.501"},
			want: []string{"NAS-SMC-NO-NIA0-REPLAYED-CAPS", "3 checks, up to"}, wantNot: []string{"NAS-INTEGRITY-FAIL-DISCARD"}},
		{name: "unknown tag", args: []string{"--list", "--tag", "negative-ie"}, wantErr: `--tag: unknown tag "negative-ie": want one of negative, security, timing`},
		{name: "unknown group", args: []string{"--list", "--group", "PFCP"}, wantErr: `--group: no check is in group "PFCP"`},
		{name: "a category is refused", args: []string{"--list", "--category", "gtpu"},
			wantErr: `--category: categories were replaced by groups (ADR-0011): select with --group <path or prefix> (run file: conformance.groups) or --check <ID> (conformance.checks); "gtpu" is now the checks GTPU-UNKNOWN-TEID-ERRIND,GTPU-ECHO-RESPONSE,N3-DL-QFI-CONTAINER`},
		{name: "a category is refused on a server run", args: []string{"--amf", "127.0.0.1:1", "--category", "procedural"}, wantErr: `"procedural" is now the checks`},
		{name: "longer per-test timeout", args: []string{"--list", "--group", "NGAP / Interface Management Procedures", "--per-test-timeout", "40s"},
			want: []string{"NGAP-NG-RESET-ACK", "40s", "at --per-test-timeout 40s"}},
		{name: "one check", args: []string{"--list", "--check", "NGAP-NG-RESET-ACK"},
			want: []string{"NGAP-NG-RESET-ACK", "1 checks, up to 15s in total"}, wantNot: []string{"NAS-T3560-AUTH-RETRANSMIT"}},
		{name: "two checks, repeated and comma-separated, any case", args: []string{"--list", "--check", "ngap-ng-reset-ack,NAS-T3560-AUTH-RETRANSMIT", "--check", "NAS-T3550-ACCEPT-RETRANSMIT"},
			want: []string{"NGAP-NG-RESET-ACK", "NAS-T3560-AUTH-RETRANSMIT", "NAS-T3550-ACCEPT-RETRANSMIT", "3 checks, up to"}, wantNot: []string{"NGAP-UNKNOWN-UE-SURVIVES"}},
		{name: "a check outside the group", args: []string{"--list", "--group", "NAS", "--check", "NGAP-NG-RESET-ACK"},
			want: []string{"0 checks"}, wantNot: []string{"NGAP-NG-RESET-ACK "}},
		{name: "unknown check", args: []string{"--list", "--check", "NGAP-NO-SUCH-CHECK"}, wantErr: `--check: unknown check "NGAP-NO-SUCH-CHECK"; --list names the checks`},
		{name: "unknown check on a server run", args: []string{"--amf", "127.0.0.1:1", "--check", "NGAP-NO-SUCH-CHECK"}, wantErr: `--check: unknown check "NGAP-NO-SUCH-CHECK"`},
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

// --list prints the checks in the order a run takes them: the check a core
// may not survive last, after the ones that leave a UE context behind.
func TestConformanceListShowsRunOrder(t *testing.T) {
	root := New("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"conformance", "--list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, line := range strings.Split(out.String(), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) == 0 {
			break
		}
		listed = append(listed, f[0]+" "+f[4])
	}
	want := conformance.NewRegistry().Select()
	if len(listed) != len(want) {
		t.Fatalf("listed %d checks, want %d:\n%s", len(listed), len(want), out.String())
	}
	for i, c := range want {
		if listed[i] != c.ID()+" "+conformance.PhaseOf(c).String() {
			t.Errorf("line %d is %q, want %s in phase %s", i, listed[i], c.ID(), conformance.PhaseOf(c))
		}
	}
	if last := listed[len(listed)-1]; last != "NGAP-PATH-SWITCH-SECCAP-MISMATCH disruptive" {
		t.Errorf("the list ends with %q", last)
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
// a short check gets none, and done stops the lines. The preflight is a
// line per step.
func TestAnnouncer(t *testing.T) {
	reg := conformance.NewRegistry()
	if err := reg.Groups(0, "NAS / 5GMM specific procedures"); err != nil {
		t.Fatal(err)
	}
	nas := reg.Select()
	var w syncBuffer
	a := newAnnouncer(&w, 20*time.Millisecond)
	a.suite(nas, conformance.DefaultPerTestTimeout)
	a.preflight(conformance.Preflight{{Step: conformance.StepNGSetup, Status: conformance.PreflightOK}, {Step: conformance.StepRegistration, Status: conformance.PreflightFailed}})
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
		"Running 5 checks, up to 5m49s in total\n",
		"preflight: NG Setup ok\npreflight: registration failed\n",
		"[1/2] " + nas[0].ID() + ": up to 100ms\n",
		"[2/2] " + nas[1].ID() + ": up to 10ms\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if after < 2 {
		t.Errorf("%d progress lines for the long check, want at least 2:\n%s", after, out)
	}
	if strings.Contains(out, "      "+nas[1].ID()+": ") {
		t.Errorf("the short check got progress lines:\n%s", out)
	}
}
