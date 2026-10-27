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
	"regexp"
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
		SHA256: map[string]string{"amd64": testSHA, "arm64": testSHA}, Spec: testSpec, Nonce: testNonce}
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
		{name: "main checks its call before anything runs", before: "[ \"$ORBIT_LAST\" = '--orbit-end-" + testNonce + "' ] ||", after: "[ \"$(uname -s)\""},
		{name: "its own PATH before anything runs", before: "\tPATH='/usr/sbin:/usr/bin:/sbin:/bin'\n", after: "eval \"ORBIT_LAST"},
		{name: "sha256sum required before the download", before: "command -v sha256sum >/dev/null 2>&1 || die", after: "curl -fsS"},
		{name: "a private work directory before the download", before: "WORK=$(mktemp -d)", after: "curl -fsS"},
		{name: "removed on exit", before: "trap 'rm -rf \"$WORK\"' EXIT", after: "curl -fsS"},
		{name: "the check before chmod", before: "[ \"$GOT\" = \"$WANT\" ] || die", after: "chmod 0700"},
		{name: "the check before running it", before: "[ \"$GOT\" = \"$WANT\" ] || die", after: "\"$WORK/orbit\" version"},
		{name: "the binary first runs after the confirmation", before: "Apply %d changes? [y/N]", after: "\"$WORK/orbit\" version"},
		{name: "made executable after the confirmation", before: "Apply %d changes? [y/N]", after: "chmod 0700"},
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
	// Everything runs from main, called with the sentinel on the last line
	// only: outside main there are comments and the call, and nothing else.
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if last := lines[len(lines)-1]; last != `main "$@" --orbit-end-`+testNonce {
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

	// Nothing changes the host outside the plan: every command that would
	// is a plan step's (cmd), a line of the helper, or written to the apply
	// file, which runs only after the confirmation.
	changers := regexp.MustCompile(`\b(route replace|nft (insert|delete) rule|tables -[NFAI] |useradd |systemctl |install -)`)
	for _, l := range strings.Split(s, "\n") {
		c := strings.TrimSpace(l)
		if strings.HasPrefix(c, "#") || !changers.MatchString(c) {
			continue
		}
		if !strings.HasPrefix(c, "cmd \"") && !strings.HasPrefix(c, "helper \"") && !strings.Contains(c, `>>"$APPLY"`) &&
			!strings.HasPrefix(c, "echo \"-A ORBIT-JOIN") && !strings.HasPrefix(c, "while read -r L; do cmd") && !strings.HasPrefix(c, "LINE=\"") {
			t.Errorf("changes the host outside the plan: %q", c)
		}
	}
	if !strings.Contains(s, ". \"$APPLY\"") || strings.Index(s, ". \"$APPLY\"") < strings.Index(s, "Apply %d changes? [y/N]") {
		t.Error("the apply file does not run after the confirmation")
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

// The script against a real TLS server, with the real curl and sha256sum:
// pinned by the server's key it downloads the binary into a private
// directory, verifies it and runs it before planning; a binary whose SHA-256
// differs is never made executable or run; with no sha256sum nothing is
// downloaded; a wrong pin downloads nothing; and a download cut short runs
// nothing at all.
func TestScriptRuns(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("the script runs on linux amd64/arm64")
	}
	for _, tool := range []string{"sh", "curl", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	sh, _ := exec.LookPath("sh")
	touch, err := exec.LookPath("touch")
	if err != nil {
		t.Skip("no touch")
	}
	good := []byte("#!" + sh + "\n" + touch + " '" + marker + "'\n")
	bad := []byte("#!" + sh + "\n" + touch + " '" + marker + "'\n# tampered\n")
	sum := sha256.Sum256(good)
	served := good
	fetched := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/join/"+testToken+"/bin/amd64" {
			http.NotFound(w, r)
			return
		}
		fetched++
		_, _ = w.Write(served)
	}))
	defer srv.Close()
	server := &scriptServer{url: srv.URL, pin: tlspin.Fingerprint(srv.Certificate()), sha: hex.EncodeToString(sum[:])}
	host := func() fakeHost {
		h := v4Host()
		h.server = server
		return h
	}

	t.Run("verified and planned; a dry run never runs it", func(t *testing.T) {
		_ = os.Remove(marker)
		r := runHost(t, "dash", host(), "--dry-run")
		if r.err != nil || !strings.Contains(r.out, "verified orbit") || !strings.Contains(r.out, "dry run: nothing was changed") {
			t.Fatalf("err %v\n%s", r.err, r.out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("the downloaded binary ran in a dry run")
		}
	})
	t.Run("run once the plan is confirmed", func(t *testing.T) {
		_ = os.Remove(marker)
		r := runHost(t, "dash", host(), "--yes")
		if r.err != nil || !strings.Contains(r.out, "applied") {
			t.Fatalf("err %v\n%s", r.err, r.out)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Error("the verified binary did not run")
		}
	})
	t.Run("a different binary is refused", func(t *testing.T) {
		_ = os.Remove(marker)
		served = bad
		defer func() { served = good }()
		r := runHost(t, "dash", host(), "--dry-run")
		if r.err == nil || !strings.Contains(r.out, "refusing to run it") {
			t.Fatalf("err %v\n%s", r.err, r.out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("an unverified binary ran")
		}
	})
	t.Run("no sha256sum: nothing downloaded", func(t *testing.T) {
		h := host()
		h.drop = []string{"sha256sum"}
		before := fetched
		r := runHost(t, "dash", h, "--dry-run")
		if r.err == nil || !strings.Contains(r.out, "sha256sum is required") || fetched != before {
			t.Fatalf("err %v, %d fetches\n%s", r.err, fetched-before, r.out)
		}
	})
	t.Run("a wrong pin is refused", func(t *testing.T) {
		h := host()
		h.server = &scriptServer{url: srv.URL, pin: testPin, sha: server.sha}
		before := fetched
		r := runHost(t, "dash", h, "--dry-run")
		if r.err == nil || fetched != before {
			t.Fatalf("a wrong pin downloaded: err %v\n%s", r.err, r.out)
		}
	})
	t.Run("a download cut short runs nothing", func(t *testing.T) {
		d := testData()
		d.ServerURL, d.Pin = srv.URL, server.pin
		d.SHA256 = map[string]string{"amd64": server.sha, "arm64": server.sha}
		script, err := Script(d)
		if err != nil {
			t.Fatal(err)
		}
		// Cut at every line end (and mid-line) of the script, and in the
		// final call: whatever arrives runs no command of the script's: no
		// download, no verified line, nothing written.
		var cuts []int
		for i, c := range script {
			if c == '\n' && i < len(script)-1 {
				cuts = append(cuts, i, i-3)
			}
		}
		last := strings.LastIndex(string(script), `main "$@" --orbit-end-`)
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
			cmd := exec.Command("sh", "-s", "--", "--yes")
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
}

// Only the whole call runs main: a call that ends in anything but this
// render's sentinel, an old fixed --run or another render's nonce among
// them, is refused before anything runs.
func TestScriptSentinel(t *testing.T) {
	b, err := Script(testData())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	end := "--orbit-end-" + testNonce
	if !strings.HasSuffix(s, "\nmain \"$@\" "+end+"\n") {
		t.Fatalf("the script does not end with the call of main and its sentinel")
	}
	for _, last := range []string{"--run", "--orbit-end-ffeeddccbbaa99887766554433221100", "--orbit-end-" + testNonce[:31], ""} {
		t.Run("ends "+last, func(t *testing.T) {
			edited := strings.TrimSuffix(s, end+"\n") + last + "\n"
			cmd := exec.Command("sh", "-s")
			cmd.Stdin = strings.NewReader(edited)
			cmd.Env = []string{"PATH=/nonexistent"}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "this script is incomplete") {
				t.Errorf("err %v\n%s", err, out)
			}
		})
	}
	// A nonce of its own for every render.
	d := testData()
	d.Nonce = ""
	x, err := Script(d)
	if err != nil {
		t.Fatal(err)
	}
	y, err := Script(d)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`--orbit-end-[0-9a-f]{32}`)
	if a, b := re.FindString(string(x)), re.FindString(string(y)); a == "" || a == b {
		t.Errorf("two renders' sentinels: %q and %q", a, b)
	}
}
