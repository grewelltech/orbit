// Package tlspin is ORBIT's TLS without a CA: the server's self-signed
// certificate (orbit serve --tls auto) and the client's pin of its public
// key (ADR-0008 §4). A pin is the SHA-256 of the certificate's
// SubjectPublicKeyInfo, written sha256/<base64> (curl's --pinnedpubkey
// takes the same hash as sha256//<base64>).
package tlspin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Fingerprint is a certificate's SPKI pin, sha256/<base64>.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256/" + base64.StdEncoding.EncodeToString(sum[:])
}

// VerifyPin is a tls.Config VerifyPeerCertificate that accepts the peer
// only when its leaf certificate's SPKI matches pin: the pin is the whole
// verification, since a self-signed certificate has no CA to check.
func VerifyPin(pin string) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("the server sent no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("the server's certificate does not parse: %w", err)
		}
		got := Fingerprint(leaf)
		if subtle.ConstantTimeCompare([]byte(got), []byte(pin)) != 1 {
			return fmt.Errorf("the server's key %s does not match the pinned %s; if the server's key changed on purpose, run orbit server trust again", got, pin)
		}
		return nil
	}
}

// Pair is a --tls auto certificate and key.
type Pair struct {
	Cert tls.Certificate
	Leaf *x509.Certificate
	// Repaired says why a new pair replaced what was on disk ("" when the
	// pair was loaded, or made for the first time).
	Repaired string
}

// SelfSigned loads the self-signed certificate and key from dir
// (cert.pem, key.pem), creating them on first use: ECDSA P-256, valid for
// ten years, for hosts (IPs and names). The pair is written key first,
// cert last, each through a temp file, fsync and rename, so a crash leaves
// either the old state or the whole pair; if exactly one of the two is
// found, both are made anew and Repaired says so. The key is 0600, a loaded
// key that group or others can read is refused, and neither file is read
// or written through a symlink.
func SelfSigned(dir string, hosts []string) (Pair, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for _, p := range []string{certPath, keyPath, dir} {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return Pair{}, fmt.Errorf("%s is a symlink: --tls auto keeps its key only in a regular file", p)
		}
	}
	certOK, err := exists(certPath)
	if err != nil {
		return Pair{}, fmt.Errorf("--tls auto: %w", err)
	}
	keyOK, err := exists(keyPath)
	if err != nil {
		return Pair{}, fmt.Errorf("--tls auto: %w", err)
	}
	repaired := ""
	switch {
	case certOK && keyOK:
		if st, err := os.Stat(keyPath); err == nil && st.Mode().Perm()&0o077 != 0 {
			return Pair{}, fmt.Errorf("--tls auto: %s is readable by others (mode %o); chmod 600 it", keyPath, st.Mode().Perm())
		}
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return Pair{}, fmt.Errorf("--tls auto: %s: %w", dir, err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return Pair{}, err
		}
		return Pair{Cert: pair, Leaf: leaf}, nil
	case keyOK:
		repaired = "found key.pem without cert.pem (an interrupted first start); made a new pair"
	case certOK:
		repaired = "found cert.pem without key.pem; made a new pair"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Pair{}, err
	}
	certPEM, keyPEM, err := generate(hosts)
	if err != nil {
		return Pair{}, err
	}
	if err := writeAtomic(keyPath, keyPEM, 0o600); err != nil {
		return Pair{}, err
	}
	if err := writeAtomic(certPath, certPEM, 0o644); err != nil {
		return Pair{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Pair{}, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Pair{}, err
	}
	return Pair{Cert: pair, Leaf: leaf, Repaired: repaired}, nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// generate makes a self-signed certificate and its key, PEM-encoded.
func generate(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "orbit serve"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// writeAtomic writes b to path at mode through a new temp file in the same
// directory, fsynced, then renamed over path (which replaces a link at
// path, never writes through it), then fsyncs the directory.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
