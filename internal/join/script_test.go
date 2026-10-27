package join

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/tlspin"
)

var update = flag.Bool("update", false, "rewrite the golden script")

const (
	testToken = Prefix + "AAAAAAAAAAAAAAAAAAAAAA"
	testPin   = "sha256/47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	testSHA   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func testData() ScriptData {
	return ScriptData{ServerURL: "https://192.0.2.10:8412", Pin: testPin, Token: testToken, Name: "app1",
		SHA256: map[string]string{"amd64": testSHA, "arm64": testSHA}, Spec: testSpec}
}

// The rendered script matches the golden file (go test -update rewrites
// it), and parses as POSIX sh.
func TestScriptGolden(t *testing.T) {
	got, err := Script(testData())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "stub.sh")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("script differs from %s (go test -update to accept):\n%s", golden, got)
	}
	// shellcheck is required in CI ($CI set), so CI cannot lose the check
	// silently; elsewhere it runs when installed.
	switch sc, err := exec.LookPath("shellcheck"); {
	case err == nil:
		if out, err := exec.Command(sc, "-s", "sh", golden).CombinedOutput(); err != nil {
			t.Errorf("shellcheck: %v\n%s", err, out)
		}
	case os.Getenv("CI") != "":
		t.Error("shellcheck is not installed; CI must run it")
	default:
		t.Log("shellcheck is not installed: the script was not linted")
	}
	// Every POSIX sh on the host parses it.
	for _, shell := range []string{"sh", "dash", "busybox"} {
		p, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		args := []string{"-n", golden}
		if shell == "busybox" {
			args = append([]string{"sh"}, args...)
		}
		if out, err := exec.Command(p, args...).CombinedOutput(); err != nil {
			t.Errorf("%s -n: %v\n%s", shell, err, out)
		}
	}
}

// The script's hardening: strict mode and a private umask before anything
// runs, a private work directory removed on exit, and the binary checked
// with sha256sum (which must exist) before it is made executable or run.
func TestScriptHardening(t *testing.T) {
	b, err := Script(testData())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	at := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("script lacks %q", sub)
		}
		return i
	}
	firstCommand := at("[ \"$(uname -s)\"")
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{name: "set -eu first", before: "\tset -eu\n", after: "ORBIT_SERVER="},
		{name: "umask 077 before anything runs", before: "\tumask 077\n", after: "[ \"$(uname -s)\""},
		{name: "main checks its call before anything runs", before: "[ \"$ORBIT_LAST\" = --run ] ||", after: "[ \"$(uname -s)\""},
		{name: "sha256sum required before the download", before: "command -v sha256sum >/dev/null 2>&1 || die", after: "curl -fsS"},
		{name: "a private work directory before the download", before: "WORK=$(mktemp -d)", after: "curl -fsS"},
		{name: "removed on exit", before: "trap 'rm -rf \"$WORK\"' EXIT", after: "curl -fsS"},
		{name: "the check before chmod", before: "[ \"$GOT\" = \"$WANT\" ] || die", after: "chmod 0700"},
		{name: "the check before running it", before: "[ \"$GOT\" = \"$WANT\" ] || die", after: "\"$WORK/orbit\" version"},
		{name: "downloaded into the work directory", before: "-o \"$WORK/orbit\"", after: "sha256sum \"$WORK/orbit\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if at(tc.before) > at(tc.after) {
				t.Errorf("%q comes after %q", tc.before, tc.after)
			}
		})
	}
	if at("set -eu") > firstCommand || at("umask 077") > firstCommand {
		t.Error("strict mode or the umask comes after the first command")
	}
	// Everything runs from main, called with --run on the last line only:
	// outside main there are comments and the call, and nothing else.
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if last := lines[len(lines)-1]; last != `main "$@" --run` {
		t.Errorf("last line = %q, want the call of main", last)
	}
	inMain := false
	for _, l := range lines[:len(lines)-1] {
		switch {
		case l == "main() {":
			inMain = true
		case l == "}" && inMain:
			inMain = false
		case !inMain && l != "" && !strings.HasPrefix(l, "#"):
			t.Errorf("outside main: %q", l)
		}
	}

	// Only the work directory's verified orbit is run or made executable.
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if strings.Contains(l, "chmod") && !strings.Contains(l, "\"$WORK/orbit\"") {
			t.Errorf("chmod of something else: %q", l)
		}
	}
}

// Every value is checked for its shape before it is rendered; whatever
// could carry shell syntax is refused.
func TestScriptRefusesBadValues(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*ScriptData)
		wantErr string
	}{
		{name: "an http URL", mut: func(d *ScriptData) { d.ServerURL = "http://192.0.2.10:8412" }, wantErr: "want https"},
		{name: "a URL with a path", mut: func(d *ScriptData) { d.ServerURL = "https://192.0.2.10:8412/x';id;'" }, wantErr: "want https"},
		{name: "a URL with a query", mut: func(d *ScriptData) { d.ServerURL = "https://h:1?$(id)" }, wantErr: "want https"},
		{name: "a pin with a quote", mut: func(d *ScriptData) { d.Pin = "sha256/'$(id)'" }, wantErr: "pin"},
		{name: "a token with a quote", mut: func(d *ScriptData) { d.Token = Prefix + "AAAAAAAAAAAAAAAAAAAA'x" }, wantErr: "token"},
		{name: "a name with a quote", mut: func(d *ScriptData) { d.Name = "a'b" }, wantErr: "name"},
		{name: "a sha with a quote", mut: func(d *ScriptData) { d.SHA256["amd64"] = "'$(id)'" }, wantErr: "sha256"},
		{name: "an unknown arch", mut: func(d *ScriptData) { d.SHA256["riscv64"] = testSHA }, wantErr: "no arch"},
		{name: "no binary", mut: func(d *ScriptData) { d.SHA256 = nil }, wantErr: "no orbit binary"},
		{name: "a pool with shell", mut: func(d *ScriptData) { d.Spec.UEPools = []string{"10.0.0.0/8 $(id)"} }, wantErr: "not a CIDR"},
		{name: "upf n6 with shell", mut: func(d *ScriptData) { d.Spec.UPFN6 = "1.2.3.4;id" }, wantErr: "upf_n6"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testData()
			d.SHA256 = map[string]string{"amd64": testSHA, "arm64": testSHA}
			tc.mut(&d)
			if _, err := Script(d); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// shquote's word is the string itself to sh, whatever it holds.
func TestShquote(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, s := range []string{"", "plain", "it's", "''", `a\b`, "$(id)", "`id`", "a b\nc", "'; rm -rf / #"} {
		out, err := exec.Command(sh, "-c", "printf %s "+shquote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("shquote(%q) gives %q (%v)", s, out, err)
		}
	}
}

// The stub run for real: against a TLS server pinned by its key it
// downloads the binary into a private directory, verifies it and runs it,
// then removes the directory; a binary whose SHA-256 differs is never made
// executable or run; with no sha256sum nothing is downloaded.
func TestScriptRuns(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("the script runs on linux amd64/arm64")
	}
	for _, tool := range []string{"sh", "curl", "sha256sum", "mktemp", "cut", "chmod", "rm", "uname"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	good := []byte("#!/bin/sh\ntouch '" + marker + "'\n")
	bad := []byte("#!/bin/sh\ntouch '" + marker + "'\n# tampered\n")
	sum := sha256.Sum256(good)
	served := good
	fetched := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/join/"+testToken+"/bin/"+runtime.GOARCH {
			http.NotFound(w, r)
			return
		}
		fetched++
		_, _ = w.Write(served)
	}))
	defer srv.Close()
	d := testData()
	d.ServerURL = srv.URL
	d.Pin = tlspin.Fingerprint(srv.Certificate())
	d.SHA256 = map[string]string{runtime.GOARCH: hex.EncodeToString(sum[:])}
	script, err := Script(d)
	if err != nil {
		t.Fatal(err)
	}
	run := func(path string, shell ...string) (string, error) {
		tmp := t.TempDir()
		if len(shell) == 0 {
			shell = []string{"sh"}
		}
		cmd := exec.Command(shell[0], append(shell[1:], "-s")...)
		cmd.Stdin = strings.NewReader(string(script))
		cmd.Env = []string{"PATH=" + path, "TMPDIR=" + tmp}
		out, err := cmd.CombinedOutput()
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Errorf("the work directory was left behind: %v", left)
		}
		return string(out), err
	}

	t.Run("verified, then run", func(t *testing.T) {
		out, err := run(os.Getenv("PATH"))
		if err != nil || !strings.Contains(out, "verified orbit") || !strings.Contains(out, "nothing on this host was changed") {
			t.Fatalf("err %v\n%s", err, out)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Error("the verified binary did not run")
		}
	})
	// The same run under each POSIX sh on the host.
	for _, shell := range [][]string{{"dash"}, {"busybox", "sh"}} {
		p, err := exec.LookPath(shell[0])
		if err != nil {
			continue
		}
		t.Run("verified, then run, under "+strings.Join(shell, " "), func(t *testing.T) {
			_ = os.Remove(marker)
			out, err := run(os.Getenv("PATH"), append([]string{p}, shell[1:]...)...)
			if err != nil || !strings.Contains(out, "verified orbit") {
				t.Fatalf("err %v\n%s", err, out)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Error("the verified binary did not run")
			}
		})
	}
	t.Run("a download cut short runs nothing", func(t *testing.T) {
		// Cut at every line end (and mid-line) of the script, and in
		// the final call: whatever arrives runs no command of the
		// script's: no download, no verified line, nothing written.
		var cuts []int
		for i, c := range script {
			if c == '\n' && i < len(script)-1 {
				cuts = append(cuts, i, i-3)
			}
		}
		last := strings.LastIndex(string(script), `main "$@" --run`)
		for i := last; i < len(script)-1; i++ {
			cuts = append(cuts, i)
		}
		for _, n := range cuts {
			if n <= 0 {
				continue
			}
			_ = os.Remove(marker)
			before := fetched
			tmp := t.TempDir()
			cmd := exec.Command("sh", "-s")
			cmd.Stdin = strings.NewReader(string(script[:n]))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmp}
			out, _ := cmd.CombinedOutput()
			if fetched != before || strings.Contains(string(out), "verified orbit") {
				t.Fatalf("cut at %d of %d ran the script (%d fetches):\n%s", n, len(script), fetched-before, out)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("cut at %d of %d ran the binary", n, len(script))
			}
			if left, _ := os.ReadDir(tmp); len(left) != 0 {
				t.Fatalf("cut at %d of %d wrote %v", n, len(script), left)
			}
		}
	})
	t.Run("a different binary is refused", func(t *testing.T) {
		_ = os.Remove(marker)
		served = bad
		defer func() { served = good }()
		out, err := run(os.Getenv("PATH"))
		if err == nil || !strings.Contains(out, "refusing to run it") {
			t.Fatalf("err %v\n%s", err, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("an unverified binary ran")
		}
	})
	t.Run("no sha256sum: nothing downloaded", func(t *testing.T) {
		bin := t.TempDir()
		for _, tool := range []string{"sh", "curl", "mktemp", "cut", "chmod", "rm", "uname"} {
			p, _ := exec.LookPath(tool)
			if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
		before := fetched
		out, err := run(bin)
		if err == nil || !strings.Contains(out, "sha256sum is required") || fetched != before {
			t.Fatalf("err %v, %d fetches\n%s", err, fetched-before, out)
		}
	})
	t.Run("a wrong pin is refused", func(t *testing.T) {
		d := d
		d.Pin = testPin
		s, err := Script(d)
		if err != nil {
			t.Fatal(err)
		}
		before := fetched
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(string(s))
		out, err := cmd.CombinedOutput()
		if err == nil || fetched != before {
			t.Fatalf("a wrong pin downloaded: err %v\n%s", err, out)
		}
	})
}
