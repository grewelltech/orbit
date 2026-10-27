package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

const (
	testKi  = "5122250214c33e723a5dd523fc145fc0"
	testOPc = "981d464c7c52eb6e5036234984ad0bcf"
)

// lab1 is a target with every field the commands below read.
const lab1 = `version: 1
kind: target
name: lab1
core:
  amf: 192.0.2.10:38412
  plmn: { mcc: "001", mnc: "01" }
  tac: 7
  slices:
    - { sst: 1, sd: "010203" }
  dnn: internet
subscribers:
  - name: main
    first_supi: "001010000000001"
    ki: ${ORBIT_TEST_KI}
    opc: 981d464c7c52eb6e5036234984ad0bcf
ran:
  n2_source_ips: [192.0.2.50-192.0.2.59]
  n3_ips: [198.51.100.50]
  gnb_ids: { first: 100, bits: 24 }
`

// lab1NoUE is lab1 without its subscriber pool, so the PLMN can change
// without the pool's SUPI falling outside it.
var lab1NoUE = strings.Replace(lab1, `subscribers:
  - name: main
    first_supi: "001010000000001"
    ki: ${ORBIT_TEST_KI}
    opc: 981d464c7c52eb6e5036234984ad0bcf
`, "", 1)

// targetDir writes orbit.yaml holding doc and returns locations that find
// it (and nothing in a user config directory).
func targetDir(t *testing.T, doc string) target.Locations {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return target.Locations{Dir: dir, ConfigDir: filepath.Join(dir, "config")}
}

type orbitRun struct {
	root          *cobra.Command
	out, err      string
	execErr       error
	cmdPathOfArgs []string
}

// flag is the final text of --name on the command args ran.
func (r orbitRun) flag(t *testing.T, name string) string {
	t.Helper()
	c, _, err := r.root.Find(r.cmdPathOfArgs)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Flags().Lookup(name)
	if f == nil {
		t.Fatalf("%s has no --%s", c.CommandPath(), name)
	}
	return f.Value.String()
}

func runOrbit(t *testing.T, loc target.Locations, args ...string) orbitRun {
	t.Helper()
	root := New("test", WithTargetLocations(loc))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return orbitRun{root: root, out: out.String(), err: errOut.String(), execErr: err, cmdPathOfArgs: args}
}

// clearOrbitEnv unsets the ORBIT_* variables the resolver reads, so a
// developer's shell cannot change a result.
func clearOrbitEnv(t *testing.T) {
	t.Helper()
	for _, b := range target.Bindings {
		if b.Env != "" {
			t.Setenv(b.Env, "")
			if err := os.Unsetenv(b.Env); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("ORBIT_TEST_KI", testKi)
}

// conformance with --target-release Rel-14 SKIPs every check without
// dialling and prints the network it would present, so a target's values
// can be read off the output.
var skipAll = []string{"conformance", "--target-release", "Rel-14"}

// -t fills every flag the target sets and the command binds, and a
// required flag (--amf) is satisfied by the target.
func TestTargetFillsFlags(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, append(skipAll, "-t", "lab1")...)
	if r.execErr != nil {
		t.Fatalf("conformance -t lab1: %v\n%s", r.execErr, r.out)
	}
	if !strings.Contains(r.out, "network: plmn=001/01 tac=7 snssai=1/010203 dnn=internet") {
		t.Errorf("network line:\n%s", r.out)
	}
	want := map[string]string{
		"amf": "192.0.2.10:38412", "mcc": "001", "mnc": "01", "tac": "7", "sst": "1", "sd": "010203",
		"dnn": "internet", "bind": "192.0.2.50", "gnb-base": "100", "supi": "001010000000001",
		"ki": testKi, "opc": testOPc, "gnb-n3": "198.51.100.50", "n3-bind": "198.51.100.50:2152",
	}
	for name, v := range want {
		if got := r.flag(t, name); got != v {
			t.Errorf("--%s = %q, want %q", name, got, v)
		}
	}
	if r.err != "" {
		t.Errorf("stderr without --explain: %q", r.err)
	}
}

// With no target named and no --explain the resolver does not run: flag
// values and errors are as before T2.
func TestNoTargetChangesNothing(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, "conformance", "--target-release", "Rel-14", "--mcc", "001", "--mnc", "01")
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), `required flag(s) "amf" not set`) {
		t.Fatalf("err = %v, want the required --amf error", r.execErr)
	}
	r = runOrbit(t, loc, "conformance", "--target-release", "Rel-14", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01")
	if r.execErr != nil || r.flag(t, "tac") != "1" || r.flag(t, "bind") != "" {
		t.Fatalf("err = %v, tac %s, bind %q", r.execErr, r.flag(t, "tac"), r.flag(t, "bind"))
	}
}

// $ORBIT_TARGET names the target when -t does not, and -t wins over it.
func TestOrbitTargetEnv(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	t.Setenv("ORBIT_TARGET", "lab1")
	r := runOrbit(t, loc, skipAll...)
	if r.execErr != nil || r.flag(t, "amf") != "192.0.2.10:38412" {
		t.Fatalf("$ORBIT_TARGET: err %v, amf %q", r.execErr, r.flag(t, "amf"))
	}
	r = runOrbit(t, loc, append(skipAll, "-t", "nope")...)
	if !errors.Is(r.execErr, target.ErrNotFound) {
		t.Fatalf("-t nope over $ORBIT_TARGET: err = %v, want ErrNotFound", r.execErr)
	}
}

// Precedence is the resolver's, field by field: a flag beats the target,
// and so does $ORBIT_*. The --mcc/--mnc pair: the target supplying both
// passes; a lone --mnc flag pairs with the target's --mcc.
func TestTargetPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		args     []string
		wantPLMN string
	}{
		{name: "target supplies both", wantPLMN: "plmn=001/01"},
		{name: "lone --mnc flag pairs with the target mcc", args: []string{"--mnc", "02"}, wantPLMN: "plmn=001/02"},
		{name: "flags beat the target", args: []string{"--mcc", "208", "--mnc", "93"}, wantPLMN: "plmn=208/93"},
		{name: "env beats the target", env: map[string]string{"ORBIT_MCC": "999", "ORBIT_MNC": "70"}, wantPLMN: "plmn=999/70"},
		{name: "flag beats env beats target", env: map[string]string{"ORBIT_MCC": "999"}, args: []string{"--mnc", "02"}, wantPLMN: "plmn=999/02"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOrbitEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			r := runOrbit(t, targetDir(t, lab1NoUE), append(append(skipAll, "-t", "lab1", "--explain"), tc.args...)...)
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			if !strings.Contains(r.out, "network: "+tc.wantPLMN+" ") {
				t.Errorf("network line, want %s:\n%s", tc.wantPLMN, r.out)
			}
		})
	}
}

// When env wins, the flag is left for the command's own $ORBIT_* fallback
// and --explain shows the env value with the target's as overridden.
func TestExplainEnvOverTarget(t *testing.T) {
	clearOrbitEnv(t)
	t.Setenv("ORBIT_MCC", "999")
	r := runOrbit(t, targetDir(t, lab1NoUE), append(skipAll, "-t", "lab1", "--explain")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	c, _, _ := r.root.Find(r.cmdPathOfArgs)
	if f := c.Flags().Lookup("mcc"); f.Changed {
		t.Errorf("--mcc was set (%q): an env winner is left to the command's $ORBIT_MCC fallback", f.Value)
	}
	if !strings.Contains(r.out, "network: plmn=999/01 ") {
		t.Errorf("network line, want the env MCC:\n%s", r.out)
	}
	line := lineWith(r.err, "core.plmn.mcc ")
	for _, want := range []string{"999", "env $ORBIT_MCC", "target ", "=001", "--mcc"} {
		if !strings.Contains(line, want) {
			t.Errorf("explain row %q lacks %q\n%s", line, want, r.err)
		}
	}
}

func lineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// --explain names the target, shows each bound field's value, layer and
// flag, masks secrets, says how a pool became one address, and lists the
// target fields the command does not use.
func TestExplain(t *testing.T) {
	clearOrbitEnv(t)
	doc := lab1 + "app:\n  responders:\n    - { name: r1, control: 203.0.113.20:9551, token: sekrit-token }\n"
	r := runOrbit(t, targetDir(t, doc), append(skipAll, "-t", "lab1", "--explain", "--tac", "9")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	e := r.err
	for _, want := range []string{
		"orbit conformance: target lab1",
		"FIELD", "SET BY", "OVERRODE",
		"core.tac", "flag --tac", "default built-in=1; target",
		`--bind=192.0.2.50 (from "192.0.2.50-192.0.2.59")`,
		`--n3-bind=198.51.100.50:2152 (from "198.51.100.50")`,
		"subscribers[main].ki", target.Masked,
		"not used by orbit conformance: app.responders[r1].control, app.responders[r1].token",
	} {
		if !strings.Contains(e, want) {
			t.Errorf("explain lacks %q:\n%s", want, e)
		}
	}
	for _, secret := range []string{testKi, testOPc, "sekrit-token"} {
		if strings.Contains(e, secret) || strings.Contains(r.out, secret) {
			t.Errorf("output shows a secret %q", secret)
		}
	}
}

// --explain without a target still shows where each setting came from.
func TestExplainWithoutTarget(t *testing.T) {
	clearOrbitEnv(t)
	t.Setenv("ORBIT_MCC", "001")
	r := runOrbit(t, targetDir(t, lab1), append(skipAll, "--explain", "--amf", "127.0.0.1:1", "--mnc", "01")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	for _, want := range []string{"target (none)", "core.amf", "flag --amf", "env $ORBIT_MCC", "core.tac", "default built-in"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("explain lacks %q:\n%s", want, r.err)
		}
	}
}

// A command that binds no field refuses -t and --explain, and ignores
// $ORBIT_TARGET so a shell export never breaks it.
func TestTargetOnCommandWithoutFields(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, "version", "-t", "lab1")
	if r.execErr == nil || r.execErr.Error() != "--target does not apply to orbit version: it takes no target fields" {
		t.Errorf("version -t: %v", r.execErr)
	}
	r = runOrbit(t, loc, "version", "--explain")
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "takes no target fields") {
		t.Errorf("version --explain: %v", r.execErr)
	}
	t.Setenv("ORBIT_TARGET", "nope")
	r = runOrbit(t, loc, "version")
	if r.execErr != nil || !strings.Contains(r.out, "orbit test") {
		t.Errorf("version under $ORBIT_TARGET: %v %q", r.execErr, r.out)
	}
}

// On ue latency and ue traffic --target is the echo destination, as
// before: -t is refused with a reason, never taken as a run target.
func TestEchoTargetCommandsKeepTheirFlag(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	for _, c := range [][]string{{"ue", "latency"}, {"ue", "traffic"}} {
		r := runOrbit(t, loc, append(c, "-t", "lab1", "--supi", "001010000000001", "--server", "http://127.0.0.1:1")...)
		want := "-t is not available on orbit " + strings.Join(c, " ") + ": its --target is the destination"
		if r.execErr == nil || !strings.Contains(r.execErr.Error(), want) {
			t.Errorf("%v -t: err = %v, want %q", c, r.execErr, want)
		}
		r = runOrbit(t, loc, append(c, "--target", "192.0.2.99:7", "--supi", "001010000000001", "--server", "http://127.0.0.1:1")...)
		if got := r.flag(t, "target"); got != "192.0.2.99:7" {
			t.Errorf("%v --target = %q", c, got)
		}
		if r.execErr != nil && strings.Contains(r.execErr.Error(), "target") && strings.Contains(r.execErr.Error(), "not found") {
			t.Errorf("%v --target resolved as a run target: %v", c, r.execErr)
		}
	}
}

// The deprecated --local on cell ngsetup is bound with --bind, so a user's
// --local beats a target's N2 pool instead of losing to it.
func TestNGSetupLocalBeatsTarget(t *testing.T) {
	clearOrbitEnv(t)
	r := runOrbit(t, targetDir(t, lab1), "cell", "ngsetup", "-t", "lab1", "--local", "192.0.2.77:0", "--server", "http://127.0.0.1:1")
	if got := r.flag(t, "bind"); got != "" {
		t.Errorf("--bind = %q: --local was given", got)
	}
	r = runOrbit(t, targetDir(t, lab1), "cell", "ngsetup", "-t", "lab1", "--server", "http://127.0.0.1:1")
	if got := r.flag(t, "bind"); got != "192.0.2.50" {
		t.Errorf("--bind = %q, want the target's", got)
	}
	if got := r.flag(t, "local"); got != "" {
		t.Errorf("--local = %q: a deprecated alias is never filled from a target", got)
	}
	if got := r.flag(t, "gnb-id"); got != "100" {
		t.Errorf("--gnb-id = %q, want the target's first gNB ID", got)
	}
}

// Every numeric flag bound to a path refuses a leading zero, target or
// not, with the target file's message; 0x, 0o and plain 0 pass.
func TestNumericFlagsLeadingZero(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	base := []string{"conformance", "--target-release", "Rel-14", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01"}
	tests := []struct {
		args    []string
		wantErr string
	}{
		{args: []string{"--tac", "0010"}, wantErr: `invalid argument "0010" for "--tac" flag: 0010 has a leading zero; write 10, or 0x… for hex`},
		{args: []string{"--sst", "01"}, wantErr: "01 has a leading zero"},
		{args: []string{"--gnb-base", "0400"}, wantErr: "0400 has a leading zero; write 400"},
		{args: []string{"--tac", "0x0a"}},
		{args: []string{"--tac", "0o12"}},
		{args: []string{"--tac", "10"}},
		{args: []string{"--sst", "0"}},
	}
	for _, tc := range tests {
		r := runOrbit(t, loc, append(base, tc.args...)...)
		if tc.wantErr == "" {
			if r.execErr != nil {
				t.Errorf("%v: %v", tc.args, r.execErr)
			}
			continue
		}
		if r.execErr == nil || !strings.Contains(r.execErr.Error(), tc.wantErr) {
			t.Errorf("%v: err = %v, want %q", tc.args, r.execErr, tc.wantErr)
		}
	}
	r := runOrbit(t, loc, "load", "--count", "0010")
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "0010 has a leading zero") {
		t.Errorf("load --count 0010: %v", r.execErr)
	}
}

// Every bound numeric flag on every command carries the rule.
func TestEveryBoundNumericFlagIsStrict(t *testing.T) {
	var walk func(c *cobra.Command)
	n := 0
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if b, ok := target.FlagBinding(c.CommandPath(), f.Name); ok && b.Numeric() {
				n++
				if _, ok := f.Value.(strictNumber); !ok {
					t.Errorf("%s --%s is numeric and bound but not strict", c.CommandPath(), f.Name)
				}
			}
		})
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(New("test"))
	if n < 10 {
		t.Errorf("only %d bound numeric flags found", n)
	}
}

func TestFirstAddress(t *testing.T) {
	tests := []struct {
		flag     string
		list     []string
		want     string
		wantNote string
	}{
		{flag: "bind", list: []string{"192.0.2.50"}, want: "192.0.2.50"},
		{flag: "bind", list: []string{"192.0.2.50:0"}, want: "192.0.2.50:0"},
		{flag: "bind", list: []string{"192.0.2.50", "192.0.2.51"}, want: "192.0.2.50", wantNote: `from "192.0.2.50", the first of 2`},
		{flag: "bind", list: []string{"192.0.2.50-192.0.2.59"}, want: "192.0.2.50", wantNote: `from "192.0.2.50-192.0.2.59"`},
		{flag: "bind", list: []string{"192.0.2.0/28"}, want: "192.0.2.1", wantNote: `from "192.0.2.0/28"`},
		{flag: "bind", list: []string{"192.0.2.7/32"}, want: "192.0.2.7", wantNote: `from "192.0.2.7/32"`},
		{flag: "bind", list: []string{"2001:db8::/64"}, want: "2001:db8::", wantNote: `from "2001:db8::/64"`},
		{flag: "bind", list: []string{"[fe80::1%eth0]:0"}, want: "[fe80::1%eth0]:0"},
		{flag: "gnb-n3", list: []string{"198.51.100.50:2152"}, want: "198.51.100.50", wantNote: `from "198.51.100.50:2152"`},
		{flag: "n3-bind", list: []string{"198.51.100.50"}, want: "198.51.100.50:2152", wantNote: `from "198.51.100.50"`},
		{flag: "n3-bind", list: []string{"198.51.100.50:3000"}, want: "198.51.100.50:3000"},
	}
	for _, tc := range tests {
		got, note, err := firstAddress(tc.flag, tc.list)
		if err != nil || got != tc.want || note != tc.wantNote {
			t.Errorf("firstAddress(%s, %v) = %q, %q, %v; want %q, %q", tc.flag, tc.list, got, note, err, tc.want, tc.wantNote)
		}
	}
	if _, _, err := firstAddress("bind", []string{"host"}); err == nil {
		t.Error("a non-address entry was accepted")
	}
}

// The roadmap's done-when for T2: conformance against a target with -t and
// no other flags gives the same result as today's long command line,
// against the mock AMF over real SCTP.
func TestConformanceTargetMatchesLongCommandLine(t *testing.T) {
	clearOrbitEnv(t)
	ki, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	doc := `version: 1
kind: target
name: lab1
core:
  amf: ` + front.Addr + `
  plmn: { mcc: "208", mnc: "93" }
  tac: 1
  slices:
    - { sst: 1, sd: "010203" }
  dnn: internet
ran:
  n2_source_ips: [127.0.0.1-127.0.0.9]
  gnb_ids: { first: 100 }
`
	loc := targetDir(t, doc)

	common := []string{"conformance", "--json", "--category", "procedural", "--per-test-timeout", "5s"}
	long := runOrbit(t, loc, append(common, "--amf", front.Addr, "--mcc", "208", "--mnc", "93", "--tac", "1",
		"--sst", "1", "--sd", "010203", "--dnn", "internet", "--bind", "127.0.0.1", "--gnb-base", "100")...)
	short := runOrbit(t, loc, append(common, "-t", "lab1")...)
	type result struct{ ID, Verdict, Observed string }
	parse := func(name string, r orbitRun) ([]result, string, string) {
		var sum struct {
			Results  []result
			Network  json.RawMessage
			N2Source string
		}
		if err := json.Unmarshal([]byte(r.out), &sum); err != nil {
			t.Fatalf("%s: JSON: %v (err %v)\n%s", name, err, r.execErr, r.out)
		}
		return sum.Results, string(sum.Network), sum.N2Source
	}
	lr, ln, ls := parse("long", long)
	sr, sn, ss := parse("-t", short)
	if len(lr) == 0 {
		t.Fatal("no results")
	}
	if !reflect.DeepEqual(lr, sr) || ln != sn || ls != ss {
		t.Errorf("-t differs from the long command line:\nlong %s %s %+v\n-t   %s %s %+v", ln, ls, lr, sn, ss, sr)
	}
	if (long.execErr == nil) != (short.execErr == nil) {
		t.Errorf("exit differs: long %v, -t %v", long.execErr, short.execErr)
	}
}

// Conformance refuses UE flags without --supi only when the user gave
// them: a target's keys and N3 address are its environment and simply go
// unused when its UE is turned off (--supi "").
func TestConformanceUEFlagsFromTargetAreNotRefused(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, append(skipAll, "-t", "lab1", "--supi", "")...)
	if r.execErr != nil {
		t.Errorf("target keys and N3 without a SUPI: %v", r.execErr)
	}
	r = runOrbit(t, loc, append(skipAll, "-t", "lab1", "--supi", "", "--ki", testKi)...)
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "need --supi") {
		t.Errorf("a user's --ki without --supi: err = %v, want the refusal", r.execErr)
	}
}

// An explicit empty --bind clears the field and wins over the target: the
// N2 source is route-selected, not the target's first source IP, and
// --explain says it was cleared.
func TestEmptyBindClearsTarget(t *testing.T) {
	clearOrbitEnv(t)
	r := runOrbit(t, targetDir(t, lab1), append(skipAll, "-t", "lab1", "--bind", "", "--explain")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	if got := r.flag(t, "bind"); got != "" {
		t.Errorf("--bind = %q, want route-selected (empty)", got)
	}
	if strings.Contains(r.out, "n2-source=192.0.2.50") {
		t.Errorf("the target's source was used:\n%s", r.out)
	}
	row := lineWith(r.err, "ran.n2_source_ips ")
	if !strings.Contains(row, "(cleared)") || !strings.Contains(row, "cleared by flag --bind") || !strings.Contains(row, "192.0.2.50-192.0.2.59") {
		t.Errorf("explain row %q, want cleared by flag --bind over the target pool", row)
	}
}

// ue register, the most-used path, gives the same result with -t and no
// other flags as with today's explicit flag line: a real server, the mock
// AMF behind a real SCTP listener, a fresh pair for each run.
func TestUERegisterTargetMatchesLongCommandLine(t *testing.T) {
	clearOrbitEnv(t)
	register := func(name string, args ...string) (string, error) {
		ki, _ := auth.ParseHexKey("Ki", testKi)
		opc, _ := auth.ParseHexKey("OPc", testOPc)
		amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
			MCC: "208", MNC: "93", SST: 1, SD: "010203"})
		if err != nil {
			t.Fatal(err)
		}
		front := sctpfront.Start(t, amf)
		srv := httptest.NewServer(server.New(observability.NewLogger(io.Discard, slog.LevelError), "test",
			observability.NewRegistry(), server.Options{StateDir: ""}))
		t.Cleanup(srv.Close)
		doc := `version: 1
kind: target
name: lab1
core:
  amf: ` + front.Addr + `
  plmn: { mcc: "208", mnc: "93" }
  slices:
    - { sst: 1, sd: "010203" }
subscribers:
  - name: main
    first_supi: "208930000000001"
    ki: ` + testKi + `
    opc: ` + testOPc + `
ran:
  n2_source_ips: [127.0.0.1-127.0.0.9]
  gnb_ids: { first: 7 }
`
		args = append([]string{"--server", srv.URL, "ue", "register"}, args...)
		for i, a := range args {
			args[i] = strings.ReplaceAll(a, "$AMF", front.Addr)
		}
		r := runOrbit(t, targetDir(t, doc), args...)
		// The AMF-UE-NGAP-ID and the ephemeral source port vary run to run.
		out := regexp.MustCompile(`AMF-UE-NGAP-ID \d+`).ReplaceAllString(r.out, "AMF-UE-NGAP-ID N")
		out = regexp.MustCompile(`(n2-source=[0-9.]+):\d+`).ReplaceAllString(out, "$1:P")
		return out, r.execErr
	}
	long, lerr := register("long", "--amf", "$AMF", "--mcc", "208", "--mnc", "93", "--sst", "1", "--sd", "010203",
		"--supi", "208930000000001", "--ki", testKi, "--opc", testOPc, "--bind", "127.0.0.1", "--gnb-id", "7")
	short, serr := register("-t", "-t", "lab1")
	if lerr != nil {
		t.Fatalf("long command line: %v\n%s", lerr, long)
	}
	if !strings.Contains(long, "registered=true") || !strings.Contains(long, "n2-source=127.0.0.1:P") {
		t.Fatalf("long command line did not register through 127.0.0.1:\n%s", long)
	}
	if serr != nil || short != long {
		t.Errorf("-t differs from the long command line (err %v):\nlong:\n%s\n-t:\n%s", serr, long, short)
	}
}
