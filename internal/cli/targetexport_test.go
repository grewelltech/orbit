package cli

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgrewell/orbit/internal/target"
)

// errWriter fails every write, standing in for a full disk on a redirect.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("disk full") }

// cliFixture reads one of the byte-for-byte files `orbit target init` wrote,
// the same files web/src/data/targets/roundtrip.test.ts imports unchanged, so
// the CLI and the dashboard agree on what a target file is.
func cliFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "target", "testdata", "roundtrip", "cli", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// exportLoc returns locations with an empty config dir and a helper that
// writes a target file into it, as `orbit target init` would.
func exportLoc(t *testing.T) (target.Locations, func(name string, doc []byte)) {
	t.Helper()
	dir := t.TempDir()
	loc := target.Locations{Dir: dir, ConfigDir: filepath.Join(dir, "config")}
	write := func(name string, doc []byte) {
		p := filepath.Join(loc.ConfigDir, "targets", name+".yaml")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, doc, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return loc, write
}

// The default output is the stored file, byte for byte, with every ${VAR}
// kept: `orbit target export <name> > <name>.yaml` writes a file the Targets
// page imports unchanged.
func TestTargetExportDefaultIsTheFile(t *testing.T) {
	clearOrbitEnv(t)
	loc, write := exportLoc(t)
	file := cliFixture(t, "keys-as-vars")
	write("keys-as-vars", file)

	r := runOrbit(t, loc, "target", "export", "keys-as-vars")
	if r.execErr != nil {
		t.Fatalf("export: %v\n%s", r.execErr, r.err)
	}
	if r.out != string(file) {
		t.Errorf("output is not the stored file byte for byte:\n got %q\nwant %q", r.out, file)
	}
	for _, want := range []string{"${ORBIT_KI}", "${ORBIT_OPC}"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("export resolved %s instead of keeping it", want)
		}
	}
}

// A file written with keys in clear (as --write-keys does) is exported as it
// is: export never masks, unlike `target show`.
func TestTargetExportKeepsClearKeys(t *testing.T) {
	clearOrbitEnv(t)
	loc, write := exportLoc(t)
	file := cliFixture(t, "keys-written")
	write("keys-written", file)

	r := runOrbit(t, loc, "target", "export", "keys-written")
	if r.execErr != nil {
		t.Fatalf("export: %v\n%s", r.execErr, r.err)
	}
	if r.out != string(file) {
		t.Errorf("output is not the stored file byte for byte")
	}
	if !strings.Contains(r.out, "5122250214c33e723a5dd523fc145fc0") {
		t.Error("export masked a literal key; it must emit the file as written")
	}
	if strings.Contains(r.out, target.Masked) {
		t.Errorf("export emitted %q; it must not mask", target.Masked)
	}
}

// --link prints <dashboard>/targets#import=<base64url>, and the payload is the
// file base64url-encoded with no padding — exactly what the web app decodes in
// web/src/data/targets/importlink.ts (decodeBase64Url). base64.RawURLEncoding
// is that encoding, and the web test's b64url() helper computes the same bytes.
func TestTargetExportLink(t *testing.T) {
	clearOrbitEnv(t)
	t.Setenv("ORBIT_SERVER", "") // the default base is then the loopback dashboard

	loc, write := exportLoc(t)
	file := cliFixture(t, "keys-as-vars")
	write("keys-as-vars", file)

	cases := []struct {
		name     string
		args     []string
		wantBase string
	}{
		{"default base is the loopback dashboard", nil, "http://127.0.0.1:8412"},
		{"--dashboard overrides the base", []string{"--dashboard", "https://orbit.lab:9000"}, "https://orbit.lab:9000"},
		{"--server names the dashboard origin", []string{"--server", "https://ran1:8412"}, "https://ran1:8412"},
		{"a trailing slash on the base is trimmed", []string{"--dashboard", "http://d:8412/"}, "http://d:8412"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"target", "export", "keys-as-vars", "--link"}, tc.args...)
			r := runOrbit(t, loc, args...)
			if r.execErr != nil {
				t.Fatalf("export --link: %v\n%s", r.execErr, r.err)
			}
			got := strings.TrimRight(r.out, "\n")
			prefix := tc.wantBase + "/targets#import="
			if !strings.HasPrefix(got, prefix) {
				t.Fatalf("link = %q, want prefix %q", got, prefix)
			}
			enc := strings.TrimPrefix(got, prefix)
			// The exact encoding the web decoder expects.
			if want := base64.RawURLEncoding.EncodeToString(file); enc != want {
				t.Errorf("payload encoding = %q, want %q", enc, want)
			}
			decoded, err := base64.RawURLEncoding.DecodeString(enc)
			if err != nil {
				t.Fatalf("payload is not base64url: %v", err)
			}
			if string(decoded) != string(file) {
				t.Errorf("payload decodes to %q, want the stored file", decoded)
			}
		})
	}
}

// A single-target orbit.yaml exports byte for byte too.
func TestTargetExportLocalFile(t *testing.T) {
	clearOrbitEnv(t)
	loc := targetDir(t, lab1NoUE)
	r := runOrbit(t, loc, "target", "export", "lab1")
	if r.execErr != nil {
		t.Fatalf("export: %v\n%s", r.execErr, r.err)
	}
	if r.out != lab1NoUE {
		t.Errorf("output is not ./orbit.yaml byte for byte:\n got %q\nwant %q", r.out, lab1NoUE)
	}
}

// A target that is one entry of a multi-target list is refused with guidance,
// not a lossy partial export.
func TestTargetExportListRefused(t *testing.T) {
	clearOrbitEnv(t)
	const list = `- version: 1
  kind: target
  name: lab1
  core:
    amf: 10.0.0.1:38412
    plmn: {mcc: "001", mnc: "01"}
    slices:
      - {sst: 1}
- version: 1
  kind: target
  name: lab2
  core:
    amf: 10.0.0.2:38412
    plmn: {mcc: "001", mnc: "01"}
    slices:
      - {sst: 1}
`
	loc := targetDir(t, list)
	r := runOrbit(t, loc, "target", "export", "lab1")
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "list") {
		t.Fatalf("err = %v, want a refusal mentioning a list", r.execErr)
	}
}

// An unknown target fails, naming where it looked.
func TestTargetExportNotFound(t *testing.T) {
	clearOrbitEnv(t)
	loc, _ := exportLoc(t)
	r := runOrbit(t, loc, "target", "export", "nope")
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "nope") {
		t.Fatalf("err = %v, want an error naming the missing target", r.execErr)
	}
}

// A failing stdout write is returned (not swallowed), so a redirect to a full
// disk fails loudly instead of leaving a truncated file and exit 0.
func TestTargetExportStdoutWriteError(t *testing.T) {
	clearOrbitEnv(t)
	loc, write := exportLoc(t)
	write("keys-as-vars", cliFixture(t, "keys-as-vars"))

	root := New("test", WithTargetLocations(loc))
	root.SetOut(errWriter{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"target", "export", "keys-as-vars"})
	if err := root.Execute(); err == nil {
		t.Fatal("export reported success despite a failing stdout write")
	}
}
