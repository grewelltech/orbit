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
	pair, leaf, err := SelfSigned(dir, []string{"10.0.0.5", "orbit.lab", "", "::1"})
	if err != nil {
		t.Fatal(err)
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
	_, again, err := SelfSigned(dir, []string{"192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(again) != Fingerprint(leaf) {
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
			_, _, err := SelfSigned(dir, []string{"127.0.0.1"})
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
	if _, _, err := SelfSigned(dir, nil); err == nil || !strings.Contains(err.Error(), "--tls auto") {
		t.Fatalf("err = %v, want a --tls auto error naming the directory", err)
	}
}

func TestFingerprintAndVerifyPin(t *testing.T) {
	_, leaf, err := SelfSigned(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := SelfSigned(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
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
