package target

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// singleTarget is one target document, as `orbit target init` writes: its
// secrets are ${VAR} placeholders, so it exports without an environment.
const singleTarget = `version: 1
kind: target
name: tb1
core:
    amf: 10.0.0.1:38412
    plmn:
        mcc: "001"
        mnc: "01"
    tac: 1
    slices:
        - sst: 1
subscribers:
    - name: main
      first_supi: "001010000000001"
      ki: ${ORBIT_KI}
      opc: ${ORBIT_OPC}
ran:
    n2_source_ips:
        - 10.0.0.5
    n3_ips:
        - 10.0.0.5
`

// listTarget holds two targets as one YAML list, the only shape that is not
// a single-target file.
const listTarget = `- version: 1
  kind: target
  name: tb1
  core:
    amf: 10.0.0.1:38412
    plmn: {mcc: "001", mnc: "01"}
    slices:
      - {sst: 1}
- version: 1
  kind: target
  name: tb2
  core:
    amf: 10.0.0.2:38412
    plmn: {mcc: "001", mnc: "01"}
    slices:
      - {sst: 1}
`

// writeConfigTarget writes doc to <ConfigDir>/targets/<file>.yaml.
func writeConfigTarget(t *testing.T, loc Locations, file, doc string) string {
	t.Helper()
	p := filepath.Join(loc.ConfigDir, "targets", file+".yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeLocal writes doc to <Dir>/orbit.yaml.
func writeLocal(t *testing.T, loc Locations, doc string) string {
	t.Helper()
	p := filepath.Join(loc.Dir, "orbit.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newLoc(t *testing.T) Locations {
	t.Helper()
	dir := t.TempDir()
	return Locations{Dir: dir, ConfigDir: filepath.Join(dir, "config")}
}

func TestExportRaw(t *testing.T) {
	t.Run("config-dir file comes back byte for byte with its placeholders", func(t *testing.T) {
		loc := newLoc(t)
		p := writeConfigTarget(t, loc, "tb1", singleTarget)
		raw, path, err := ExportRaw("tb1", loc)
		if err != nil {
			t.Fatalf("ExportRaw: %v", err)
		}
		if string(raw) != singleTarget {
			t.Errorf("raw mismatch:\n got %q\nwant %q", raw, singleTarget)
		}
		if path != p {
			t.Errorf("path = %q, want %q", path, p)
		}
		for _, want := range []string{"${ORBIT_KI}", "${ORBIT_OPC}"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("export dropped %s", want)
			}
		}
	})

	t.Run("a single-target orbit.yaml exports and names its own file", func(t *testing.T) {
		loc := newLoc(t)
		p := writeLocal(t, loc, singleTarget)
		raw, path, err := ExportRaw("tb1", loc)
		if err != nil {
			t.Fatalf("ExportRaw: %v", err)
		}
		if string(raw) != singleTarget || path != p {
			t.Errorf("got (%q, %q), want (singleTarget, %q)", raw, path, p)
		}
	})

	t.Run("orbit.yaml shadows the config directory", func(t *testing.T) {
		loc := newLoc(t)
		local := writeLocal(t, loc, singleTarget)
		writeConfigTarget(t, loc, "tb1", strings.Replace(singleTarget, "10.0.0.1", "10.9.9.9", 1))
		_, path, err := ExportRaw("tb1", loc)
		if err != nil {
			t.Fatalf("ExportRaw: %v", err)
		}
		if path != local {
			t.Errorf("path = %q, want the shadowing orbit.yaml %q", path, local)
		}
	})

	t.Run("a name only in the config dir is found past orbit.yaml", func(t *testing.T) {
		loc := newLoc(t)
		writeLocal(t, loc, listTarget) // defines tb1 and tb2, not tb3
		p := writeConfigTarget(t, loc, "tb3", strings.Replace(singleTarget, "name: tb1", "name: tb3", 1))
		_, path, err := ExportRaw("tb3", loc)
		if err != nil {
			t.Fatalf("ExportRaw: %v", err)
		}
		if path != p {
			t.Errorf("path = %q, want %q", path, p)
		}
	})

	t.Run("a target in a list is refused, naming the file", func(t *testing.T) {
		loc := newLoc(t)
		local := writeLocal(t, loc, listTarget)
		_, _, err := ExportRaw("tb1", loc)
		if err == nil || !strings.Contains(err.Error(), "list") || !strings.Contains(err.Error(), local) {
			t.Fatalf("err = %v, want a refusal naming %s and 'list'", err, local)
		}
	})

	t.Run("a multi-document file is refused", func(t *testing.T) {
		loc := newLoc(t)
		// The first document is the named target, but a second follows: the
		// file's own bytes are not a single-target file the dashboard accepts.
		writeConfigTarget(t, loc, "tb1", singleTarget+"\n---\n"+singleTarget)
		_, _, err := ExportRaw("tb1", loc)
		if err == nil || !strings.Contains(err.Error(), "second YAML document") {
			t.Fatalf("err = %v, want a second-document refusal", err)
		}
	})

	t.Run("a config-dir file whose target is named differently is refused", func(t *testing.T) {
		loc := newLoc(t)
		// File tb1.yaml, but the document inside is named other.
		writeConfigTarget(t, loc, "tb1", strings.Replace(singleTarget, "name: tb1", "name: other", 1))
		_, _, err := ExportRaw("tb1", loc)
		if err == nil || !strings.Contains(err.Error(), "want the one target") {
			t.Fatalf("err = %v, want 'want the one target'", err)
		}
	})

	t.Run("an unknown target is ErrNotFound", func(t *testing.T) {
		loc := newLoc(t)
		_, _, err := ExportRaw("nope", loc)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("an empty name is refused", func(t *testing.T) {
		loc := newLoc(t)
		if _, _, err := ExportRaw("", loc); err == nil {
			t.Fatal("ExportRaw(\"\") returned no error")
		}
	})
}
