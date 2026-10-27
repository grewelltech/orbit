package tlspin

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfSigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	p, err := SelfSigned(dir, []string{"10.0.0.5", "orbit.lab", "", "::1"})
	if err != nil {
		t.Fatal(err)
	}
	pair, leaf := p.Cert, p.Leaf
	if p.Repaired != "" {
		t.Errorf("a first start reports a repair: %s", p.Repaired)
	}
	if len(pair.Certificate) == 0 {
		t.Fatal("no certificate in the pair")
	}
	if got := strings.Join(leaf.DNSNames, ","); got != "orbit.lab" {
		t.Errorf("DNS names = %q, want orbit.lab", got)
	}
	if len(leaf.IPAddresses) != 2 {
		t.Errorf("IP addresses = %v, want 10.0.0.5 and ::1", leaf.IPAddresses)
	}
	if err := leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Errorf("the certificate does not cover its listen IP: %v", err)
	}
	// The key and its directory are the operator's alone (umask can only
	// tighten them).
	for _, name := range []string{"key.pem", "."} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group or other access", name, st.Mode().Perm())
		}
	}

	// A second start reuses the pair, so the pin clients hold survives a
	// restart.
	again, err := SelfSigned(dir, []string{"192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(again.Leaf) != Fingerprint(leaf) || again.Repaired != "" {
		t.Error("a second SelfSigned made a new key; pins would break on every restart")
	}
}

func TestSelfSignedRefusesSymlinks(t *testing.T) {
	tests := []struct {
		name string
		link string // relative to the tls dir; "." = the dir itself
	}{
		{name: "key", link: "key.pem"},
		{name: "cert", link: "cert.pem"},
		{name: "dir", link: "."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "tls")
			target := filepath.Join(root, "elsewhere")
			if tc.link == "." {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, tc.link)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := SelfSigned(dir, []string{"127.0.0.1"})
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("err = %v, want a symlink refusal", err)
			}
			if _, err := os.Stat(filepath.Join(target, "key.pem")); err == nil {
				t.Error("a key was written through the link")
			}
		})
	}
}

func TestSelfSignedBadPair(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("not pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SelfSigned(dir, nil); err == nil || !strings.Contains(err.Error(), "--tls auto") {
		t.Fatalf("err = %v, want a --tls auto error naming the directory", err)
	}
}

func TestFingerprintAndVerifyPin(t *testing.T) {
	p1, err := SelfSigned(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := SelfSigned(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, other := p1.Leaf, p2.Leaf
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	pin := "sha256/" + base64.StdEncoding.EncodeToString(sum[:])
	if got := Fingerprint(leaf); got != pin {
		t.Fatalf("Fingerprint = %s, want %s", got, pin)
	}

	tests := []struct {
		name    string
		raw     [][]byte
		wantErr string
	}{
		{name: "the pinned key", raw: [][]byte{leaf.Raw}},
		{name: "the pinned key, with a chain after it", raw: [][]byte{leaf.Raw, other.Raw}},
		{name: "another key", raw: [][]byte{other.Raw}, wantErr: "does not match the pinned " + pin},
		{name: "another key first", raw: [][]byte{other.Raw, leaf.Raw}, wantErr: "does not match"},
		{name: "no certificate", raw: nil, wantErr: "no certificate"},
		{name: "garbage", raw: [][]byte{[]byte("junk")}, wantErr: "does not parse"},
	}
	verify := VerifyPin(pin)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verify(tc.raw, [][]*x509.Certificate{})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// A half-written pair (one file of the two) is replaced by a new pair and
// reported, rather than wedging every later start; a key others can read
// is refused; no temp files are left behind.
func TestSelfSignedHalfPair(t *testing.T) {
	tests := []struct {
		name, keep string // the file left from a first pair
		mode       os.FileMode
		wantErr    string
		wantRepair string
	}{
		{name: "key.pem without cert.pem", keep: "key.pem", mode: 0o600, wantRepair: "key.pem without cert.pem"},
		{name: "cert.pem without key.pem", keep: "cert.pem", mode: 0o644, wantRepair: "cert.pem without key.pem"},
		{name: "a key others can read", keep: "both", mode: 0o644, wantErr: "readable by others"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			first, err := SelfSigned(dir, []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			switch tc.keep {
			case "key.pem":
				err = os.Remove(filepath.Join(dir, "cert.pem"))
			case "cert.pem":
				err = os.Remove(filepath.Join(dir, "key.pem"))
			case "both":
				err = os.Chmod(filepath.Join(dir, "key.pem"), tc.mode)
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := SelfSigned(dir, []string{"127.0.0.1"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("a half pair wedged the start: %v", err)
			}
			if !strings.Contains(p.Repaired, tc.wantRepair) {
				t.Errorf("Repaired = %q, want %q", p.Repaired, tc.wantRepair)
			}
			if Fingerprint(p.Leaf) == Fingerprint(first.Leaf) {
				t.Error("the pair was not made anew")
			}
			again, err := SelfSigned(dir, []string{"127.0.0.1"})
			if err != nil || Fingerprint(again.Leaf) != Fingerprint(p.Leaf) || again.Repaired != "" {
				t.Errorf("the repaired pair does not load as is: %v", err)
			}
			if st, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("key.pem after the repair: %v %v", st, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Errorf("left %d files, want cert.pem and key.pem only: %v", len(entries), entries)
			}
		})
	}
}
