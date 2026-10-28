package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/bgrewell/loom/control"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/testnet"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// probeLab is a real orbit server and a mock AMF (with a stub UPF when
// withUPF) behind a real SCTP listener, on loopback /24 net.
type probeLab struct {
	server, amf string
	net         string // "127.0.N."
	mock        *mockamf.AMF
}

func newProbeLab(t *testing.T, net int, withUPF bool) probeLab {
	t.Helper()
	base := testnet.Base(net)
	ki, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	cfg := mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"}
	if withUPF {
		upf, err := mockupf.Listen(base+"10:2152", mockupf.Options{})
		testnet.RequireBind(t, base+"10:2152", err)
		t.Cleanup(func() { _ = upf.Close() })
		cfg.UPF, cfg.UPFAddress = upf, base+"10"
	}
	amf, err := mockamf.New(cfg, mockamf.WithGUTI())
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	srv := httptest.NewServer(server.New(observability.NewLogger(io.Discard, slog.LevelError), "test",
		observability.NewRegistry(), server.Options{}))
	t.Cleanup(srv.Close)
	return probeLab{server: srv.URL, amf: front.Addr, net: base, mock: amf}
}

// labTarget is a target for the lab: its AMF, PLMN, slice, pool and
// addresses.
func (l probeLab) target(name, plmn, pool string) string {
	return "version: 1\nkind: target\nname: " + name + "\ncore:\n  amf: " + l.amf + "\n  plmn: " + plmn +
		"\n  slices:\n    - { sst: 1, sd: \"010203\" }\n" + pool +
		"ran:\n  n2_source_ips: [" + l.net + "11]\n  n3_ips: [" + l.net + "11]\n  gnb_ids: { first: 100 }\n"
}

const servedPLMN = `{ mcc: "208", mnc: "93" }`

func poolWith(ki string) string {
	return "subscribers:\n  - name: main\n    first_supi: \"208930000000001\"\n    ki: " + ki + "\n    opc: " + testOPc + "\n"
}

// startProbeLoom runs a real loom agent in-process on the lab's loopback
// net, with token.
func startProbeLoom(t *testing.T, host, token string) string {
	t.Helper()
	lis, err := net.Listen("tcp", host+":0")
	testnet.RequireBind(t, host+":0", err)
	gs := control.NewGRPCServer(control.NewServer("test", control.WithAuthToken(token)))
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// The roadmap's done-when for T3, through the server's probe: the core
// steps pass against the mock AMF over real SCTP, NG Setup through a PDU
// session; --core-only ends there, passing.
func TestTargetProbePasses(t *testing.T) {
	clearOrbitEnv(t)
	lab := newProbeLab(t, testnet.CLIProbePasses, true)
	r := runOrbit(t, targetDir(t, lab.target("lab1", servedPLMN, poolWith(testKi))), "--server", lab.server, "target", "probe", "lab1", "--core-only")
	if r.execErr != nil {
		t.Fatalf("probe: %v\n%s", r.execErr, r.out)
	}
	for _, want := range []string{
		"probe lab1 through " + lab.server + "\n",
		`✓ 1 NG Setup`, `accepted by AMF`, "from " + lab.net + "11:",
		`✓ 2 register`, "208930000000001 registered in",
		`✓ 3 PDU session`, "via UPF " + lab.net + "10",
		"probe lab1: core OK; user plane not checked (--core-only)",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, testKi) || strings.Contains(r.out, testOPc) {
		t.Error("probe output shows a key")
	}
}

// All seven steps against the mock pair and a real loom agent: the core
// and the responder's control port pass, and the mock UPF, which has no
// N6, gets no echo or datagram to the responder: steps 6 and 7 fail with
// what to check, the probe exits 1 (a gate failed), and the loom token is
// in no output.
func TestTargetProbeUserPlane(t *testing.T) {
	clearOrbitEnv(t)
	lab := newProbeLab(t, testnet.CLIProbeUserPlane, true)
	const tok = "orl1_CLIPROBESECRETCLIPROBE"
	ctl := startProbeLoom(t, lab.net+"20", tok)
	app := func(token string) string {
		return "app:\n  responders:\n    - { name: app1, control: \"" + ctl + "\", token: " + token + ", data_ip: " + lab.net + "20 }\n"
	}
	tests := []struct {
		name string
		app  string
		want []string
	}{
		{name: "no N6 behind the mock UPF", app: app(tok), want: []string{
			"✓ 4 responder", "static responder app1 (not registered)", "✓ 5 control", "loom agent at " + ctl + " answered",
			"✗ 6 UE to N6 echo", "no echo from " + lab.net + "20 (3 sent)", "fix: on the app host, the return route",
			"✗ 7 data ports", "no UDP datagram arrived at " + lab.net + "20:",
			"probe lab1: 5 of 7 steps passed (0 skipped)"}},
		{name: "a wrong loom token", app: app("orl1_WRONGWRONGWRONG"), want: []string{
			"✗ 5 control", "the responder refused its loom token", "- 6 UE to N6 echo", "SKIP",
			"probe lab1: 4 of 7 steps passed (2 skipped)"}},
		{name: "no responder", app: "", want: []string{
			"✗ 4 responder", "no responder in lab1", "fix: join an app host: orbit join create -t lab1",
			"probe lab1: 3 of 7 steps passed (3 skipped)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := lab.target("lab1", servedPLMN, poolWith(testKi)) + tc.app
			r := runOrbit(t, targetDir(t, doc), "--server", lab.server, "target", "probe", "lab1", "--timeout", "3s")
			if ExitCode(r.execErr) != ExitGateFailed || !strings.Contains(r.execErr.Error(), "probe lab1 failed at") {
				t.Errorf("err = %v (exit %d), want a failed gate", r.execErr, ExitCode(r.execErr))
			}
			for _, w := range tc.want {
				if !strings.Contains(r.out, w) {
					t.Errorf("output lacks %q:\n%s", w, r.out)
				}
			}
			if strings.Contains(r.out+r.err, "orl1_") {
				t.Error("probe output shows a loom token")
			}
		})
	}
}

// Each step stops the probe with what to check: an unreachable AMF, a PLMN
// the AMF does not serve, a key the core does not hold, no PDU session, no
// pool; each exits 1. A probe that cannot start exits 3.
func TestTargetProbeFailures(t *testing.T) {
	clearOrbitEnv(t)
	lab := newProbeLab(t, testnet.CLIProbeFailures, false)
	tests := []struct {
		name, doc string
		want      []string
	}{
		{name: "unreachable AMF", doc: strings.Replace(lab.target("lab1", servedPLMN, ""), lab.amf, "127.0.0.1:1", 1),
			want: []string{"✗ 1 NG Setup", "AMF 127.0.0.1:1 is not reachable over SCTP from the orbit server", "fix: check core.amf and the N2 route", "0 of 7 steps passed (6 skipped)"}},
		{name: "wrong PLMN", doc: lab.target("lab1", `{ mcc: "001", mnc: "01" }`, ""),
			want: []string{"✗ 1 NG Setup", "the AMF rejected NG Setup: misc(4) (unknown PLMN); core.plmn 001/01 or core.tac 1 is not one the AMF serves"}},
		{name: "wrong key", doc: lab.target("lab1", servedPLMN, poolWith(strings.Repeat("a", 32))),
			want: []string{"✓ 1 NG Setup", "✗ 2 register", "registering 208930000000001 failed:", "(reject cause not reported yet)", "fix: check that subscribers[main] (first_supi, ki, opc) is provisioned in the core", "1 of 7 steps passed (5 skipped)"}},
		{name: "no PDU session accept (no UPF behind the mock)", doc: lab.target("lab1", servedPLMN, poolWith(testKi)),
			want: []string{"✓ 2 register", "✗ 3 PDU session", "no PDU session accept within 2s (DNN internet, slice 1/010203)", "2 of 7 steps passed (4 skipped)"}},
		{name: "no pool", doc: lab.target("lab1", servedPLMN, ""),
			want: []string{"✓ 1 NG Setup", "✗ 2 register", "lab1 has no subscribers", "fix: add subscribers[].first_supi, ki and opc", "1 of 7 steps passed (5 skipped)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runOrbit(t, targetDir(t, tc.doc), "--server", lab.server, "target", "probe", "lab1", "--timeout", "2s")
			if ExitCode(r.execErr) != ExitGateFailed || !strings.Contains(r.execErr.Error(), "probe lab1 failed at") {
				t.Errorf("err = %v (exit %d), want a failed gate", r.execErr, ExitCode(r.execErr))
			}
			for _, w := range tc.want {
				if !strings.Contains(r.out, w) {
					t.Errorf("output lacks %q:\n%s", w, r.out)
				}
			}
		})
	}
	// The PDU step failed after registration: the probe leaves no UE
	// registered at the core. Its register step deregistered once, and the
	// failed attach deregistered once more.
	// The mock reads them asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for lab.mock.Deregistrations() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := lab.mock.Deregistrations(); n < 2 {
		t.Errorf("the AMF saw %d Deregistration Requests across the probes, want the register step's and the failed PDU step's", n)
	}

	// No server: could not start.
	r := runOrbit(t, targetDir(t, lab.target("lab1", servedPLMN, "")), "--server", "http://127.0.0.1:1", "target", "probe", "lab1")
	if ExitCode(r.execErr) != ExitCouldNotStart || !strings.Contains(r.execErr.Error(), "probe lab1 could not start") {
		t.Errorf("no server: err = %v (exit %d), want could not start", r.execErr, ExitCode(r.execErr))
	}
}

// init runs NG Setup through the server and records the N2 source it used;
// the file loads, holds key placeholders, is 0600, and is never written
// over or written after a rejected NG Setup without --force.
func TestTargetInit(t *testing.T) {
	clearOrbitEnv(t)
	lab := newProbeLab(t, testnet.CLITargetInit, false)
	loc := targetDir(t, "") // an empty ./orbit.yaml exists: --here must refuse
	if err := os.Remove(filepath.Join(loc.Dir, "orbit.yaml")); err != nil {
		t.Fatal(err)
	}
	args := []string{"--server", lab.server, "target", "init", "lab2", "--amf", lab.amf, "--mcc", "208", "--mnc", "93", "--sd", "010203",
		"--first-supi", "208930000000001", "--count", "10"}
	r := runOrbit(t, loc, args...)
	if r.execErr != nil {
		t.Fatalf("init: %v\n%s", r.execErr, r.out)
	}
	path := filepath.Join(loc.ConfigDir, "targets", "lab2.yaml")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file %s: %v mode %v", path, err, st)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"amf: " + lab.amf, `mcc: "208"`, `mnc: "93"`, "first_supi: \"208930000000001\"", "ki: ${ORBIT_KI}", "opc: ${ORBIT_OPC}", "n2_source_ips:", "- 127.0.0.1"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("file lacks %q:\n%s", want, b)
		}
	}
	if !strings.Contains(r.out, "gNB ID 1, the first of the default range") || !strings.Contains(r.out, "export them before a probe or a run") {
		t.Errorf("init output:\n%s", r.out)
	}
	t.Setenv("ORBIT_KI", testKi)
	t.Setenv("ORBIT_OPC", testOPc)
	f, err := target.Find("lab2", loc, os.LookupEnv)
	if err != nil || f.Target.Subscribers[0].Ki != testKi || f.Target.Subscribers[0].Count != 10 {
		t.Fatalf("written target does not load: %v %+v", err, f)
	}

	if r := runOrbit(t, loc, args...); r.execErr == nil || !strings.Contains(r.execErr.Error(), "exists: --force overwrites it") {
		t.Errorf("second init: %v", r.execErr)
	}
	// A PLMN the AMF does not serve: nothing written without --force.
	bad := []string{"--server", lab.server, "target", "init", "lab3", "--amf", lab.amf, "--mcc", "001", "--mnc", "01"}
	if r := runOrbit(t, loc, bad...); r.execErr == nil || !strings.Contains(r.execErr.Error(), "AMF rejected NG Setup: misc(4) (unknown PLMN)") ||
		!strings.Contains(r.execErr.Error(), "nothing written") {
		t.Errorf("rejected init: %v", r.execErr)
	}
	if _, err := os.Stat(filepath.Join(loc.ConfigDir, "targets", "lab3.yaml")); err == nil {
		t.Error("a rejected init wrote its file")
	}
	if r := runOrbit(t, loc, append(bad, "--force")...); r.execErr != nil || !strings.Contains(r.out, "writing anyway (--force)") {
		t.Errorf("rejected init --force: %v\n%s", r.execErr, r.out)
	}
	// --here writes ./orbit.yaml, but never over an existing one.
	here := []string{"target", "init", "lab4", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50", "--here"}
	if r := runOrbit(t, loc, here...); r.execErr != nil || !strings.Contains(r.out, "no subscriber pool: `orbit target probe lab4` fails at register") {
		t.Fatalf("init --here: %v\n%s", r.execErr, r.out)
	}
	if r := runOrbit(t, loc, here...); r.execErr == nil || !strings.Contains(r.execErr.Error(), "orbit.yaml exists: add target") {
		t.Errorf("second init --here: %v", r.execErr)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"target", "init", "x", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe"}, "--no-probe needs --bind"},
		{[]string{"target", "init", "x", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50"}, "target init needs --amf"},
		{[]string{"target", "init", "x/y", "--amf", "a:1"}, `target name "x/y"`},
		{[]string{"target", "init", "x", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50", "--write-keys"}, "need --first-supi"},
		{[]string{"target", "init", "x", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--tac", "0010"}, "0010 has a leading zero"},
	} {
		if r := runOrbit(t, loc, c.args...); r.execErr == nil || !strings.Contains(r.execErr.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, r.execErr, c.want)
		}
	}
}

// --write-keys puts the keys in the file instead of the placeholders.
func TestTargetInitWriteKeys(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, "target", "init", "k", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50",
		"--first-supi", "001010000000001", "--write-keys", "--ki", testKi, "--opc", testOPc)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	b, _ := os.ReadFile(filepath.Join(loc.ConfigDir, "targets", "k.yaml"))
	if !strings.Contains(string(b), "ki: "+testKi) || strings.Contains(string(b), "${ORBIT_KI}") {
		t.Errorf("file:\n%s", b)
	}
	if strings.Contains(r.out, testKi) {
		t.Error("init printed the key")
	}
}

// list shows every target in both places, a bad document (named or not)
// with its error, and a config-dir target that ./orbit.yaml shadows.
func TestTargetList(t *testing.T) {
	clearOrbitEnv(t)
	list := "- {version: 1, kind: target, name: good, core: {amf: 192.0.2.1:38412, plmn: {mcc: \"001\", mnc: \"01\"}}}\n" +
		"- {version: 1, kind: target, name: bad, core: {tac: 0010}}\n" +
		"- {version: 1, kind: target, core: {amff: x}}\n"
	loc := targetDir(t, list)
	if err := os.MkdirAll(filepath.Join(loc.ConfigDir, "targets"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"good.yaml":  "{version: 1, kind: target, name: good}\n",
		"other.yaml": "{version: 1, kind: target, name: other, core: {amf: 192.0.2.2:38412}}\n",
	} {
		if err := os.WriteFile(filepath.Join(loc.ConfigDir, "targets", name), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := runOrbit(t, loc, "target", "list")
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	for _, want := range []string{
		"NAME", "good", "192.0.2.1:38412", "001/01", "ok",
		"bad", "error:", "0010 has a leading zero",
		"(unnamed, document 3)", "unknown key core.amff",
		"shadowed by ./orbit.yaml", "other", "192.0.2.2:38412",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("list lacks %q:\n%s", want, r.out)
		}
	}
	r = runOrbit(t, loc, "target", "list", "--json")
	var rows []listRow
	if err := json.Unmarshal([]byte(r.out), &rows); err != nil || len(rows) != 5 {
		t.Fatalf("--json: %v (%d rows)\n%s", err, len(rows), r.out)
	}
	// A malformed $ORBIT_SERVER never touches list or show.
	t.Setenv("ORBIT_SERVER", "not-a-url")
	if r := runOrbit(t, loc, "target", "list"); r.execErr != nil || strings.Contains(r.err, "server:") {
		t.Errorf("list under a bad $ORBIT_SERVER: %v %q", r.execErr, r.err)
	}
	if r := runOrbit(t, loc, "target", "probe", "good"); r.execErr == nil || !strings.Contains(r.execErr.Error(), `invalid server URL "not-a-url"`) {
		t.Errorf("probe under a bad $ORBIT_SERVER: %v", r.execErr)
	}
}

// show prints the target as loaded with keys masked; --resolved prints
// every field's layer, with $ORBIT_* over the target.
func TestTargetShow(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, "target", "show", "lab1")
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	for _, want := range []string{"# target lab1, from ", "orbit.yaml:1", "amf: 192.0.2.10:38412", "ki: " + target.Masked, "opc: " + target.Masked} {
		if !strings.Contains(r.out, want) {
			t.Errorf("show lacks %q:\n%s", want, r.out)
		}
	}
	t.Setenv("ORBIT_KI", strings.Repeat("5", 32))
	r = runOrbit(t, loc, "target", "show", "lab1", "--resolved")
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	if row := lineWith(r.out, "subscribers[main].ki "); !strings.Contains(row, "env $ORBIT_KI") || !strings.Contains(row, "<redacted>") || strings.Contains(row, "5555") {
		t.Errorf("ki row %q: env over the target, masked", row)
	}
	for _, want := range []string{"default < target", "core.dnn", "default built-in", "core.slices[0].sd", "subscribers[main].ki", target.Masked} {
		if !strings.Contains(r.out, want) {
			t.Errorf("show --resolved lacks %q:\n%s", want, r.out)
		}
	}
	for _, out := range []string{r.out, runOrbit(t, loc, "target", "show", "lab1", "--json").out, runOrbit(t, loc, "target", "show", "lab1", "--resolved", "--json").out} {
		if strings.Contains(out, testKi) || strings.Contains(out, testOPc) {
			t.Error("show printed a key")
		}
	}
	if r := runOrbit(t, loc, "target", "show", "nope"); r.execErr == nil || !strings.Contains(r.execErr.Error(), "target not found") {
		t.Errorf("show nope: %v", r.execErr)
	}
}

// INFO(1) from #196's review: typed flags without a target still never
// resolve, so a future reorder cannot bring back MEDIUM-3.
func TestNoTargetNeverResolvesWithTypedFlags(t *testing.T) {
	clearOrbitEnv(t)
	root := New("test", withResolving(func(cmd string) { t.Errorf("%s resolved with no target and no --explain", cmd) }))
	c, _, err := root.Find([]string{"conformance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ParseFlags([]string{"--per-test-timeout=-1s", "--amf", "127.0.0.1:1", "--bind", "localhost"}); err != nil {
		t.Fatal(err)
	}
	if err := c.PreRunE(c, nil); err != nil {
		t.Errorf("PreRunE: %v", err)
	}
}

// Only a terminal is interactive: /dev/null (cron, CI, nohup), a pipe and
// a file are not, so init never prompts on them, and a missing value is an
// error naming its flag.
func TestTargetInitNeverPromptsWithoutATerminal(t *testing.T) {
	clearOrbitEnv(t)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close() }()
	_ = pw.Close()
	file, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for name, in := range map[string]*os.File{"/dev/null": devnull, "a pipe": pr, "a file": file} {
		if interactive(in) {
			t.Errorf("%s counts as a terminal", name)
		}
		loc := targetDir(t, lab1)
		root := New("test", WithTargetLocations(loc))
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetIn(in)
		root.SetArgs([]string{"target", "init", "x1", "--no-probe", "--bind", "127.0.0.2", "--mcc", "208", "--mnc", "93"})
		err := root.Execute()
		if err == nil || err.Error() != "target init needs --amf" || strings.Contains(out.String(), "(host:port): ") {
			t.Errorf("%s: err = %v, output %q; want the --amf error and no prompt", name, err, out.String())
		}
	}
}

// A config-dir file whose target name differs from its file name can be
// selected by neither, so list shows it as an error, not ok.
func TestTargetListNameFileMismatch(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	if err := os.MkdirAll(filepath.Join(loc.ConfigDir, "targets"), 0o700); err != nil {
		t.Fatal(err)
	}
	for file, doc := range map[string]string{"x2.yaml": "{version: 1, kind: target, name: x}\n", "shadow.yaml": "{version: 1, kind: target, name: lab1}\n"} {
		if err := os.WriteFile(filepath.Join(loc.ConfigDir, "targets", file), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := runOrbit(t, loc, "target", "list")
	for _, want := range []string{`name "x" does not match file x2.yaml; rename one of them`, `name "lab1" does not match file shadow.yaml`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("list lacks %q:\n%s", want, r.out)
		}
	}
}

// init never writes through a link: a dangling ./orbit.yaml link with
// --here and a targets/<name>.yaml link to an existing file with --force
// are refused, naming the link, and the link's target is untouched. A
// normal --force overwrite replaces the file (mode 0600, no temp left).
func TestTargetInitRefusesSymlinks(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	if err := os.Remove(filepath.Join(loc.Dir, "orbit.yaml")); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "planted.yaml")
	if err := os.Symlink(elsewhere, filepath.Join(loc.Dir, "orbit.yaml")); err != nil {
		t.Fatal(err)
	}
	base := []string{"target", "init", "lab2", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50",
		"--first-supi", "001010000000001", "--write-keys", "--ki", testKi, "--opc", testOPc}
	r := runOrbit(t, loc, append(base, "--here")...)
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "orbit.yaml is a symlink (to "+elsewhere+")") {
		t.Errorf("--here over a dangling link: %v", r.execErr)
	}
	if _, err := os.Stat(elsewhere); err == nil {
		t.Error("the link's target was created")
	}

	dir := filepath.Join(loc.ConfigDir, "targets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(t.TempDir(), "shared.yaml")
	if err := os.WriteFile(shared, []byte("shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(dir, "lab2.yaml")); err != nil {
		t.Fatal(err)
	}
	r = runOrbit(t, loc, append(base, "--force")...)
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "lab2.yaml is a symlink (to "+shared+")") {
		t.Errorf("--force over a link: %v", r.execErr)
	}
	if b, _ := os.ReadFile(shared); string(b) != "shared\n" {
		t.Errorf("the link's target was written: %q", b)
	}
	if st, _ := os.Stat(shared); st.Mode().Perm() != 0o644 {
		t.Errorf("the link's target mode changed to %v", st.Mode().Perm())
	}

	// A regular file: --force replaces it.
	if err := os.Remove(filepath.Join(dir, "lab2.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lab2.yaml"), []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if r := runOrbit(t, loc, append(base, "--force")...); r.execErr != nil {
		t.Fatal(r.execErr)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "lab2.yaml"))
	st, _ := os.Stat(filepath.Join(dir, "lab2.yaml"))
	if !strings.Contains(string(b), "name: lab2") || st.Mode().Perm() != 0o600 {
		t.Errorf("overwrite: mode %v, content:\n%s", st.Mode().Perm(), b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// show --json uses the YAML key names.
func TestTargetShowJSONKeys(t *testing.T) {
	clearOrbitEnv(t)
	r := runOrbit(t, targetDir(t, lab1), "target", "show", "lab1", "--json")
	for _, want := range []string{`"first_supi"`, `"n2_source_ips"`, `"plmn"`, `"mcc"`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("show --json lacks %s:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out, `"FirstSUPI"`) {
		t.Error("show --json uses Go field names")
	}
}

// --responder adds a static N6 responder to the target init writes.
func TestTargetInitResponder(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	base := []string{"target", "init", "r1", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50"}
	r := runOrbit(t, loc, append(base, "--responder", "n6=203.0.113.20:9551,203.0.113.21", "--responder", "n6b=203.0.113.40:9551")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	f, err := target.Find("r1", loc, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := []target.Responder{{Name: "n6", Control: "203.0.113.20:9551", DataIP: "203.0.113.21"}, {Name: "n6b", Control: "203.0.113.40:9551"}}
	if !reflect.DeepEqual(f.Target.App.Responders, want) {
		t.Errorf("responders = %+v", f.Target.App.Responders)
	}
	if r := runOrbit(t, loc, append(base[:2], "r2", "--amf", "192.0.2.10:38412", "--mcc", "001", "--mnc", "01", "--no-probe", "--bind", "192.0.2.50", "--responder", "203.0.113.20:9551")...); r.execErr == nil ||
		!strings.Contains(r.execErr.Error(), "want name=control[,data_ip]") {
		t.Errorf("a malformed --responder: %v", r.execErr)
	}
}

// stubProbe streams steps, then ends with err.
type stubProbe struct {
	orbitv1connect.UnimplementedTargetServiceHandler
	steps []string // "STATUS" per step, in order
	err   error
}

func (s *stubProbe) Probe(_ context.Context, _ *connect.Request[orbitv1.ProbeRequest], stream *connect.ServerStream[orbitv1.ProbeStep]) error {
	names := []string{"NG Setup", "register", "PDU session", "responder", "control", "UE to N6 echo", "data ports"}
	for i, st := range s.steps {
		if err := stream.Send(&orbitv1.ProbeStep{Index: uint32(i + 1), Name: names[i], Status: st, Detail: "d"}); err != nil {
			return err
		}
	}
	return s.err
}

// The summary and the exit code from the streamed steps: any FAIL is a
// failed gate (exit 1), a step 7 that passes after the echo failed
// included; a stream that breaks mid-way did not complete (exit 2).
func TestTargetProbeSummary(t *testing.T) {
	clearOrbitEnv(t)
	tests := []struct {
		name     string
		steps    []string
		err      error
		coreOnly bool
		args     []string
		wantExit int
		want     string
		wantErr  string
	}{
		{name: "all pass", steps: []string{"OK", "OK", "OK", "OK", "OK", "OK", "OK"}, wantExit: ExitPass, want: "probe lab1: 7 of 7 steps passed (0 skipped)"},
		{name: "echo FAIL, UDP OK: still a failure", steps: []string{"OK", "OK", "OK", "OK", "OK", "FAIL", "OK"}, wantExit: ExitGateFailed,
			want: "probe lab1: 6 of 7 steps passed (0 skipped)"},
		{name: "core only, passing", steps: []string{"OK", "OK", "OK"}, coreOnly: true, wantExit: ExitPass,
			want: "probe lab1: core OK; user plane not checked (--core-only)"},
		{name: "core only, failing", steps: []string{"OK", "FAIL", "SKIP"}, coreOnly: true, wantExit: ExitGateFailed,
			want: "probe lab1: 1 of 3 steps passed (1 skipped)"},
		{name: "the stream breaks", steps: []string{"OK", "OK"}, err: connect.NewError(connect.CodeUnavailable, errors.New("server going away")),
			wantExit: ExitDidNotComplete},
		{name: "refused before a step", err: connect.NewError(connect.CodeFailedPrecondition, errors.New("gNB ID 1 is held by run-7")),
			wantExit: ExitCouldNotStart},
		{name: "--timeout past 2m", args: []string{"--timeout", "3m"}, wantExit: ExitCouldNotStart, wantErr: "at most 2m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle(orbitv1connect.NewTargetServiceHandler(&stubProbe{steps: tc.steps, err: tc.err}))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			args := []string{"--server", srv.URL, "target", "probe", "lab1"}
			if tc.coreOnly {
				args = append(args, "--core-only")
			}
			r := runOrbit(t, targetDir(t, lab1), append(args, tc.args...)...)
			if tc.wantErr != "" && (r.execErr == nil || !strings.Contains(r.execErr.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want %q", r.execErr, tc.wantErr)
			}
			if got := ExitCode(r.execErr); got != tc.wantExit {
				t.Errorf("exit %d (%v), want %d\n%s", got, r.execErr, tc.wantExit, r.out)
			}
			if !strings.Contains(r.out, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, r.out)
			}
		})
	}
}
