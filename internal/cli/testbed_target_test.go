package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/target"
)

// shellFunc is one function's definition, from its name line to the
// closing brace at the start of a line, out of a shell script.
func shellFunc(t *testing.T, script, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\) \{\n.*?^\}\n`).FindString(script)
	if m == "" {
		// One-line definitions: name() { ...; }
		m = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*\}.*\n`).FindString(script)
	}
	if m == "" {
		t.Fatalf("testbed.sh has no %s()", name)
	}
	return m
}

// testbed.sh's target_for writes a target that loads: run with the
// testbed.conf defaults, for both RAN nodes, into a scratch config
// directory, it resolves with the keys and token from the environment,
// and its pools give one N2 and N3 address per simulated gNB.
func TestTestbedWritesALoadableTarget(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testing", "lxd-testbed", "testbed.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	fns := shellFunc(t, script, "ip_in") + shellFunc(t, script, "target_for")
	vars := map[string]string{
		"TESTBED_NET_N2": "10.102.0.0/24", "TESTBED_NET_N3": "10.103.0.0/24",
		"TESTBED_NET_FAR_N2": "10.112.0.0/24", "TESTBED_NET_FAR_N3": "10.113.0.0/24",
		"TESTBED_HOST_RAN": "21", "TESTBED_RAN_GNBS": "10", "NODE_RANF": "orbit-ran-far",
		"TESTBED_MCC": "001", "TESTBED_MNC": "01", "TESTBED_TAC": "1", "TESTBED_SST": "1", "TESTBED_SD": "010203",
		"TESTBED_DNN": "internet", "UPF_N3": "10.103.0.100", "TESTBED_SUB_START": "001010100007500",
		"TESTBED_SUB_COUNT": "1000", "AMF_N2": "10.102.0.10:38412", "RESPONDER": "10.106.0.30:9551", "APP_N6": "10.106.0.30",
	}
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"ORBIT_KI": testKi, "ORBIT_OPC": testOPc, "ORBIT_RESPONDER_TOKEN": "tok"}[k]
		return v, ok
	}
	for node, nets := range map[string][2]string{"orbit-ran-near": {"10.102.0.", "10.103.0."}, "orbit-ran-far": {"10.112.0.", "10.113.0."}} {
		n2, n3 := nets[0], nets[1]
		t.Run(node, func(t *testing.T) {
			cmd := exec.Command(bash, "-c", fns+"\ntarget_for "+node)
			for k, v := range vars {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("target_for: %v", err)
			}
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "targets"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "targets", "lxd.yaml"), out, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := target.Find("lxd", target.Locations{Dir: t.TempDir(), ConfigDir: dir}, env)
			if err != nil {
				t.Fatalf("the testbed's target does not load: %v\n%s", err, out)
			}
			r, err := target.Resolve(target.DefaultInput(), f.Input)
			if err != nil {
				t.Fatal(err)
			}
			tg := r.Target
			if tg.Core.AMF != "10.102.0.10:38412" || tg.Core.PLMN.MCC != "001" || tg.Core.PLMN.MNC != "01" ||
				tg.Subscribers[0].FirstSUPI != "001010100007500" || tg.Subscribers[0].Count != 1000 || tg.Subscribers[0].Ki != testKi ||
				tg.App.Responders[0].Token != "tok" || tg.App.Responders[0].DataIP != "10.106.0.30" {
				t.Errorf("target = %+v", tg)
			}
			if strings.Contains(string(out), testKi) || strings.Contains(string(out), "tok\n") {
				t.Error("the file holds a secret")
			}
			n2s, err := target.ExpandPool(tg.RAN.N2SourceIPs, 20)
			if err != nil || len(n2s) != 10 || n2s[0] != n2+"21" || n2s[9] != n2+"30" {
				t.Errorf("N2 pool = %v (%v)", n2s, err)
			}
			n3s, err := target.ExpandPool(tg.RAN.N3IPs, 20)
			if err != nil || len(n3s) != 10 || n3s[0] != n3+"21" || n3s[9] != n3+"30" {
				t.Errorf("N3 pool = %v (%v)", n3s, err)
			}
		})
	}
}
