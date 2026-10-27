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

// SelfSigned loads the self-signed certificate and key from dir
// (cert.pem, key.pem), creating them on first use: ECDSA P-256, valid for
// ten years, for hosts (IPs and names). The key file is 0600 and neither
// file is written through a symlink.
func SelfSigned(dir string, hosts []string) (tls.Certificate, *x509.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for _, p := range []string{certPath, keyPath, dir} {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return tls.Certificate{}, nil, fmt.Errorf("%s is a symlink: --tls auto keeps its key only in a regular file", p)
		}
	}
	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		return pair, leaf, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, nil, fmt.Errorf("--tls auto: %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, nil, err
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
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := writeNew(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := writeNew(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, nil, err
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	return pair, leaf, err
}

// writeNew creates path with b, refusing an existing path (or a link).
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
