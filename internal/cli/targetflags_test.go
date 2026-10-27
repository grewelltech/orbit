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
	// Without --explain there is no provenance table; stderr carries only
	// the conformance progress lines.
	if strings.Contains(r.err, "SET BY") {
		t.Errorf("provenance on stderr without --explain: %q", r.err)
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
		`--n3-bind=198.51.100.50:2152 (from "198.51.100.50", port 2152 added)`,
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
// --local beats a target's N2 pool instead of losing to it (it fills --bind,
// as any flag on the same field does), and a target never fills --local.
func TestNGSetupLocalBeatsTarget(t *testing.T) {
	clearOrbitEnv(t)
	r := runOrbit(t, targetDir(t, lab1), "cell", "ngsetup", "-t", "lab1", "--local", "192.0.2.77:0", "--server", "http://127.0.0.1:1")
	if got := r.flag(t, "bind"); got != "192.0.2.77:0" {
		t.Errorf("--bind = %q: the user's --local beats the target's pool", got)
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
		{flag: "gnb-n3", list: []string{"198.51.100.50:2152"}, want: "198.51.100.50", wantNote: `from "198.51.100.50:2152", port dropped`},
		{flag: "n3-bind", list: []string{"198.51.100.50"}, want: "198.51.100.50:2152", wantNote: `from "198.51.100.50", port 2152 added`},
		{flag: "n3-bind", list: []string{"198.51.100.50", "198.51.100.51"}, want: "198.51.100.50:2152", wantNote: `from "198.51.100.50", the first of 2, port 2152 added`},
		{flag: "n3-bind", list: []string{"198.51.100.50:3000"}, want: "198.51.100.50:3000"},
		{flag: "gnb-n3", list: []string{"198.51.100.50-198.51.100.59"}, want: "198.51.100.50", wantNote: `from "198.51.100.50-198.51.100.59"`},
		{flag: "n3-bind", list: []string{"198.51.100.64/28"}, want: "198.51.100.65:2152", wantNote: `from "198.51.100.64/28", port 2152 added`},
		{flag: "bind", list: []string{"localhost"}, want: "localhost"},
		{flag: "bind", list: []string{"localhost:0"}, want: "localhost:0"},
		{flag: "n3-bind", list: []string{"lab-n3"}, want: "lab-n3:2152", wantNote: `from "lab-n3", port 2152 added`},
	}
	for _, tc := range tests {
		got, note, err := firstAddress(tc.flag, tc.list)
		if err != nil || got != tc.want || note != tc.wantNote {
			t.Errorf("firstAddress(%s, %v) = %q, %q, %v; want %q, %q", tc.flag, tc.list, got, note, err, tc.want, tc.wantNote)
		}
	}
	if _, _, err := firstAddress("bind", []string{"bad_host!"}); err == nil {
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

// Only a target-filled --n3-bind gets the GTP-U port: one typed bare is
// passed on exactly as given, and the GTP-U checks report it as today.
func TestTypedN3BindIsNotRewritten(t *testing.T) {
	clearOrbitEnv(t)
	r := runOrbit(t, targetDir(t, lab1), append(skipAll, "-t", "lab1", "--n3-bind", "198.51.100.77")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	if got := r.flag(t, "n3-bind"); got != "198.51.100.77" {
		t.Errorf("--n3-bind = %q, want it as typed", got)
	}
}

// Two flags bound to one field (--gnb-n3, --n3-bind: the gNB's N3 address)
// take the resolved field: the one the user did not type is filled from the
// one they did, shaped to it; both typed keep their own values; neither
// typed, the target fills both. --explain names the flag a value came from.
func TestFlagsSharingAFieldFillEachOther(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		gnbN3, n3Bind   string
		explainContains string
	}{
		{name: "neither typed: the target fills both", gnbN3: "198.51.100.50", n3Bind: "198.51.100.50:2152"},
		{name: "--n3-bind typed fills --gnb-n3", args: []string{"--n3-bind", "192.0.2.7:2152"}, gnbN3: "192.0.2.7", n3Bind: "192.0.2.7:2152",
			explainContains: "--gnb-n3=192.0.2.7 (from flag --n3-bind via ran.n3_ips)"},
		{name: "--gnb-n3 typed fills --n3-bind", args: []string{"--gnb-n3", "192.0.2.8"}, gnbN3: "192.0.2.8", n3Bind: "192.0.2.8:2152",
			explainContains: "--n3-bind=192.0.2.8:2152 (from flag --gnb-n3 via ran.n3_ips, port 2152 added)"},
		{name: "both typed keep their own", args: []string{"--gnb-n3", "192.0.2.8", "--n3-bind", "192.0.2.9:3000"}, gnbN3: "192.0.2.8", n3Bind: "192.0.2.9:3000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearOrbitEnv(t)
			r := runOrbit(t, targetDir(t, lab1), append(append(skipAll, "-t", "lab1", "--explain"), tc.args...)...)
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			if got := r.flag(t, "gnb-n3"); got != tc.gnbN3 {
				t.Errorf("--gnb-n3 = %q, want %q", got, tc.gnbN3)
			}
			if got := r.flag(t, "n3-bind"); got != tc.n3Bind {
				t.Errorf("--n3-bind = %q, want %q", got, tc.n3Bind)
			}
			if tc.explainContains != "" && !strings.Contains(r.err, tc.explainContains) {
				t.Errorf("explain lacks %q:\n%s", tc.explainContains, r.err)
			}
		})
	}
	// --explain alone only reports: with no target, a typed --n3-bind does
	// not fill --gnb-n3.
	clearOrbitEnv(t)
	r := runOrbit(t, targetDir(t, lab1), "conformance", "--target-release", "Rel-14", "--amf", "127.0.0.1:1",
		"--mcc", "001", "--mnc", "01", "--n3-bind", "192.0.2.7:2152", "--explain")
	if r.execErr != nil || r.flag(t, "gnb-n3") != "" {
		t.Errorf("--explain without a target filled --gnb-n3 = %q (err %v)", r.flag(t, "gnb-n3"), r.execErr)
	}
}

// loadTarget is a target for load against front, with pool as its N2
// source pool.
func loadTarget(amf, pool string) string {
	return "version: 1\nkind: target\nname: lab1\ncore:\n  amf: " + amf + "\n  plmn: { mcc: \"208\", mnc: \"93\" }\n" +
		"  slices:\n    - { sst: 1, sd: \"010203\" }\nsubscribers:\n  - name: main\n    first_supi: \"208930000000001\"\n" +
		"    ki: " + testKi + "\n    opc: " + testOPc + "\nran:\n  n2_source_ips: [" + pool + "]\n"
}

// A range pool on load's per-gNB --bind list gives one address per gNB,
// in order; the tester's live reproduction, which had dialled from the
// literal "127.0.0.2-127.0.0.9", end to end against the mock AMF over real
// SCTP.
func TestLoadTargetRangePoolOnePerGNB(t *testing.T) {
	clearOrbitEnv(t)
	ki, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	r := runOrbit(t, targetDir(t, loadTarget(front.Addr, "127.0.0.2-127.0.0.9")), "load", "-t", "lab1", "--count", "2", "--gnb-count", "2", "--explain")
	if r.execErr != nil {
		t.Fatalf("load -t: %v\n%s\n%s", r.execErr, r.out, r.err)
	}
	if !regexp.MustCompile(`n2-source: 127\.0\.0\.2:\d+ 127\.0\.0\.3:\d+`).MatchString(r.out) || !strings.Contains(r.out, "2/2 attached") {
		t.Errorf("load output:\n%s", r.out)
	}
	if !strings.Contains(r.err, "--bind=127.0.0.2,127.0.0.3 (first 2 of 127.0.0.2-127.0.0.9)") {
		t.Errorf("explain:\n%s", r.err)
	}
}

// The pool rule on load's and runs start-load's --bind: first N for N
// gNBs; one address shared; too few is an error naming both counts.
func TestPoolBindListFlags(t *testing.T) {
	tests := []struct {
		name, pool string
		gnbs       string
		want       string
		wantErr    string
	}{
		{name: "range, one gNB", pool: "127.0.0.2-127.0.0.9", gnbs: "1", want: "[127.0.0.2]"},
		{name: "cidr, three gNBs", pool: "192.0.2.0/29", gnbs: "3", want: "[192.0.2.1,192.0.2.2,192.0.2.3]"},
		{name: "mixed list", pool: "192.0.2.50, 192.0.2.60-192.0.2.61", gnbs: "3", want: "[192.0.2.50,192.0.2.60,192.0.2.61]"},
		{name: "one address is shared", pool: "192.0.2.50", gnbs: "4", want: "[192.0.2.50]"},
		{name: "too short", pool: "127.0.0.2-127.0.0.4", gnbs: "4", wantErr: "ran.n2_source_ips has 3 addresses, orbit %s needs 4 (one per gNB)"},
		{name: "past the cap", pool: "10.0.0.0/8", gnbs: "65537", wantErr: "65537 addresses is more than a pool expands to (65536)"},
	}
	for _, cmd := range [][]string{{"load"}, {"runs", "start-load"}} {
		for _, tc := range tests {
			t.Run(strings.Join(cmd, " ")+" "+tc.name, func(t *testing.T) {
				clearOrbitEnv(t)
				// The AMF and server are unreachable: only the flags matter.
				args := append(append([]string{}, cmd...), "-t", "lab1", "--gnb-count", tc.gnbs, "--count", "1", "--server", "http://127.0.0.1:1")
				r := runOrbit(t, targetDir(t, loadTarget("127.0.0.1:1", tc.pool)), args...)
				if tc.wantErr != "" {
					want := strings.Replace(tc.wantErr, "%s", strings.Join(cmd, " "), 1)
					if r.execErr == nil || !strings.Contains(r.execErr.Error(), want) {
						t.Fatalf("err = %v, want %q", r.execErr, want)
					}
					return
				}
				if got := r.flag(t, "bind"); got != tc.want {
					t.Errorf("--bind = %s, want %s (err %v)", got, tc.want, r.execErr)
				}
			})
		}
	}
}

// ueRegisterArgs is a full ue register line against an unreachable server:
// the run ends at the server call, so equal errors mean the CLI accepted
// the same input.
var ueRegisterArgs = []string{"--server", "http://127.0.0.1:1", "ue", "register", "--amf", "192.0.2.10:38412",
	"--mcc", "001", "--mnc", "01", "--supi", "001010000000001", "--ki", testKi, "--opc", testOPc}

// A host name for --bind, which the server resolves, is accepted with and
// without --explain and with -t, as without them; a target pool may hold
// one too.
func TestHostnameBind(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1NoUE)
	base := runOrbit(t, loc, ueRegisterArgs...)
	if base.execErr == nil || !strings.Contains(base.execErr.Error(), "unavailable") {
		t.Fatalf("baseline: %v", base.execErr)
	}
	for _, bind := range []string{"localhost", "localhost:0"} {
		for _, extra := range [][]string{nil, {"--explain"}, {"-t", "lab1"}, {"-t", "lab1", "--explain"}} {
			args := append(append(append([]string{}, ueRegisterArgs...), "--bind", bind), extra...)
			r := runOrbit(t, loc, args...)
			if r.execErr == nil || r.execErr.Error() != base.execErr.Error() {
				t.Errorf("--bind %s %v: err = %v, want the server-call error %v", bind, extra, r.execErr, base.execErr)
			}
			if got := r.flag(t, "bind"); got != bind {
				t.Errorf("--bind %s %v: flag = %q", bind, extra, got)
			}
		}
	}
	r := runOrbit(t, targetDir(t, strings.Replace(lab1NoUE, "[192.0.2.50-192.0.2.59]", "[lab-n2]", 1)),
		"--server", "http://127.0.0.1:1", "ue", "register", "-t", "lab1", "--supi", "001010000000001", "--ki", testKi, "--opc", testOPc)
	if got := r.flag(t, "bind"); got != "lab-n2" {
		t.Errorf("a host name in the pool: --bind = %q (err %v)", got, r.execErr)
	}
}

// --explain with no target changes nothing: for values each command
// accepts, including ones the resolver's own checks reject, the result is
// the same with --explain as without, and a rejected value is a marked row.
func TestExplainChangesNothing(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	conf := []string{"conformance", "--target-release", "Rel-14", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01"}
	tests := []struct {
		name string
		args []string
		mark string // a marked row --explain must show, "" for none
	}{
		{name: "register: host name bind", args: append(append([]string{}, ueRegisterArgs...), "--bind", "localhost:0")},
		{name: "register: SUPI the server judges", args: append(append([]string{}, ueRegisterArgs...), "--supi", "imsi-00101x"),
			mark: `⚠ subscribers[default].first_supi: "imsi-00101x" is not an IMSI`},
		{name: "register: hex gNB ID and TAC", args: append(append([]string{}, ueRegisterArgs...), "--gnb-id", "0x10", "--tac", "0x0a")},
		{name: "register: empty bind", args: append(append([]string{}, ueRegisterArgs...), "--bind", "")},
		{name: "conformance: host name binds", args: append(append([]string{}, conf...), "--bind", "localhost", "--n3-bind", "localhost:2152", "--upf-n3", "upf.example:2152")},
		{name: "conformance: categories and timeout", args: append(append([]string{}, conf...), "--category", "nas,security", "--per-test-timeout", "1500ms")},
		{name: "load: binds and counts", args: []string{"load", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01", "--base-imsi", "001010000000001",
			"--ki", testKi, "--opc", testOPc, "--bind", "localhost,127.0.0.2:0", "--gnb-count", "2", "--count", "1", "--rate", "2.5"}},
		{name: "cell ngsetup: deprecated --local", args: []string{"--server", "http://127.0.0.1:1", "cell", "ngsetup", "--amf", "192.0.2.10:38412",
			"--mcc", "001", "--mnc", "01", "--local", "localhost:0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plain := runOrbit(t, loc, tc.args...)
			explained := runOrbit(t, loc, append(append([]string{}, tc.args...), "--explain")...)
			if errText(plain.execErr) != errText(explained.execErr) || plain.out != explained.out {
				t.Errorf("--explain changed the result:\nwithout: %v\n%s\nwith: %v\n%s", plain.execErr, plain.out, explained.execErr, explained.out)
			}
			if tc.mark != "" && !strings.Contains(explained.err, tc.mark) {
				t.Errorf("explain lacks the marked row %q:\n%s", tc.mark, explained.err)
			}
			if tc.mark == "" && strings.Contains(explained.err, "⚠") {
				t.Errorf("unexpected marked row:\n%s", explained.err)
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	// Ephemeral source ports differ run to run.
	return regexp.MustCompile(`:\d{4,5}\b`).ReplaceAllString(err.Error(), ":P")
}

// A lone --sd with a target keeps the target's SST and the rest of its
// slice list, end to end through a command.
func TestLoneSDKeepsTargetSST(t *testing.T) {
	clearOrbitEnv(t)
	doc := strings.Replace(lab1NoUE, `    - { sst: 1, sd: "010203" }`, `    - { sst: 2, sd: "000002" }
    - { sst: 1 }`, 1)
	r := runOrbit(t, targetDir(t, doc), append(skipAll, "-t", "lab1", "--sd", "000005", "--explain")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	if !strings.Contains(r.out, "snssai=2/000005") {
		t.Errorf("network line, want the target's SST 2 with --sd 000005:\n%s", r.out)
	}
	if got := r.flag(t, "sst"); got != "2" {
		t.Errorf("--sst = %q, want the target's 2", got)
	}
	if row := lineWith(r.err, "core.slices[0].sst "); !strings.Contains(row, "target ") {
		t.Errorf("sst row %q, want set by the target", row)
	}
	if row := lineWith(r.err, "core.slices[0].sd "); !strings.Contains(row, "flag --sd") || !strings.Contains(row, "=000002") {
		t.Errorf("sd row %q, want flag --sd over the target's 000002", row)
	}
}

// A negative duration, which the commands treat as unset, never fails a
// command: not with no target and no --explain (the resolver does not run),
// and not with -t or --explain either (it resolves to 0 ms).
func TestNegativeDurationAccepted(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1)
	conf := []string{"conformance", "--target-release", "Rel-14", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01", "--per-test-timeout=-1s"}
	for _, extra := range [][]string{nil, {"--explain"}, {"-t", "lab1", "--supi", ""}, {"-t", "lab1", "--supi", "", "--explain"}} {
		r := runOrbit(t, loc, append(append([]string{}, conf...), extra...)...)
		if r.execErr != nil {
			t.Errorf("--per-test-timeout=-1s %v: %v", extra, r.execErr)
		}
	}
	r := runOrbit(t, loc, "load", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01", "--base-imsi", "001010000000001",
		"--ki", testKi, "--opc", testOPc, "--count", "1", "--duration=-1s", "--explain")
	if r.execErr != nil && strings.Contains(r.execErr.Error(), "duration") {
		t.Errorf("load --duration=-1s --explain: %v", r.execErr)
	}
}

// A host name in ran.n3_ips fills --n3-bind, which takes one, and leaves
// --gnb-n3 (an IP only) unfilled with a note, so a run that does not need
// it is not failed by it.
func TestN3HostNameLeavesGNBN3Unfilled(t *testing.T) {
	clearOrbitEnv(t)
	doc := strings.Replace(lab1, "n3_ips: [198.51.100.50]", "n3_ips: [ran-n3.example.net]", 1)
	r := runOrbit(t, targetDir(t, doc), append(skipAll, "-t", "lab1", "--supi", "", "--explain")...)
	if r.execErr != nil {
		t.Fatalf("%v\n%s", r.execErr, r.err)
	}
	if got := r.flag(t, "n3-bind"); got != "ran-n3.example.net:2152" {
		t.Errorf("--n3-bind = %q", got)
	}
	if got := r.flag(t, "gnb-n3"); got != "" {
		t.Errorf("--gnb-n3 = %q, want it unfilled", got)
	}
	if !strings.Contains(r.err, "--gnb-n3 (not filled: takes an IP address; the field holds a host name)") {
		t.Errorf("explain lacks the unfilled note:\n%s", r.err)
	}
}

// An empty value only reads as cleared when a flag or env var cleared it:
// a target slice with no SD, or an empty default, is (none).
func TestExplainEmptyValues(t *testing.T) {
	clearOrbitEnv(t)
	doc := strings.Replace(lab1NoUE, `- { sst: 1, sd: "010203" }`, `- { sst: 1 }`, 1)
	r := runOrbit(t, targetDir(t, doc), append(skipAll, "-t", "lab1", "--bind", "", "--explain")...)
	if r.execErr != nil {
		t.Fatal(r.execErr)
	}
	if row := lineWith(r.err, "core.slices[0].sd "); !strings.Contains(row, "(none)") || strings.Contains(row, "cleared") {
		t.Errorf("target slice without SD: %q", row)
	}
	if row := lineWith(r.err, "ran.n2_source_ips "); !strings.Contains(row, "cleared by flag --bind") {
		t.Errorf("--bind \"\": %q", row)
	}
}

// No target, no $ORBIT_TARGET and no --explain means nothing resolves, by
// structure: over every command that binds a field, the hook returns
// before it builds any layer. --explain does start resolving.
func TestNoTargetNeverResolves(t *testing.T) {
	clearOrbitEnv(t)
	var walk func(c *cobra.Command, f func(*cobra.Command))
	walk = func(c *cobra.Command, f func(*cobra.Command)) {
		// Commands that take a scenario file resolve its target themselves
		// (scenarioTarget), outside the flag hook.
		if c.Runnable() && len(boundFlags(c)) > 0 && c.Annotations[targetFileAnnotation] == "" {
			f(c)
		}
		for _, s := range c.Commands() {
			walk(s, f)
		}
	}
	n := 0
	root := New("test", withResolving(func(cmd string) { t.Errorf("%s resolved with no target and no --explain", cmd) }))
	walk(root, func(c *cobra.Command) {
		n++
		if err := c.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		if err := c.PreRunE(c, nil); err != nil {
			t.Errorf("%s: %v", c.CommandPath(), err)
		}
	})
	if n < 8 {
		t.Fatalf("only %d bound commands walked", n)
	}
	// The control: --explain resolves, on every one of them.
	resolved := map[string]bool{}
	walk(New("test", withResolving(func(cmd string) { resolved[cmd] = true })), func(c *cobra.Command) {
		if err := c.ParseFlags([]string{"--explain"}); err != nil {
			t.Fatal(err)
		}
		_ = c.PreRunE(c, nil) // the run itself may fail; only resolving matters
		if !resolved[c.CommandPath()] {
			t.Errorf("%s --explain did not resolve", c.CommandPath())
		}
	})
}

// When a command fails for want of a flag the target could not fill
// (--gnb-n3 takes an IP; the target's N3 entry is a host name), its error
// says why, without --explain.
func TestUnfilledFlagReasonInError(t *testing.T) {
	clearOrbitEnv(t)
	doc := strings.Replace(lab1, "n3_ips: [198.51.100.50]", "n3_ips: [gnb1.lab]", 1)
	r := runOrbit(t, targetDir(t, doc), append(skipAll, "-t", "lab1")...)
	// The file is shown relative to the working directory, absolute here.
	want := regexp.MustCompile(`^--gnb-n3: the mobility checks need the gNB N3 IP with --supi ` +
		`\(ran\.n3_ips\[0\] "gnb1\.lab" is a host name; --gnb-n3 needs an IP, target lab1, \S*orbit\.yaml:18\)$`)
	if r.execErr == nil || !want.MatchString(r.execErr.Error()) {
		t.Errorf("err = %v\nwant  %s", r.execErr, want)
	}
}

// An explicit -t "" clears target selection, so $ORBIT_TARGET is ignored
// and a command with no file layer runs with no target.
func TestEmptyTargetFlagIgnoresEnv(t *testing.T) {
	clearOrbitEnv(t)
	t.Setenv("ORBIT_TARGET", "lab1")
	r := runOrbit(t, targetDir(t, lab1), append(skipAll, "-t", "")...)
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), `required flag(s) "amf" not set`) {
		t.Errorf("-t \"\" under $ORBIT_TARGET: err = %v, want no target (the required --amf)", r.execErr)
	}
}
