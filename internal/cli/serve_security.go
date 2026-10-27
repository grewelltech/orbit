package cli

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/bgrewell/orbit/internal/tlspin"
)

// minTokenLen is the shortest bearer token serve accepts, and
// minTokenDistinct the fewest distinct characters: a cheap refusal of a
// hand-typed pattern, not an entropy estimate.
const (
	minTokenLen      = 32
	minTokenDistinct = 8
)

// serveSecurity is what serve's TLS and token flags resolve to.
type serveSecurity struct {
	tls   *tls.Config // nil = plaintext
	leaf  *x509.Certificate
	token string
	// repaired is why --tls auto replaced a half-written pair ("" = it did
	// not), for serve to log.
	repaired string
}

// listenLoopback reports whether every address listen binds is loopback.
// An unspecified host (":8412", "0.0.0.0", "::") binds every interface, and
// a name binds whatever it resolves to.
func listenLoopback(listen string) (bool, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false, fmt.Errorf("--listen %q: %w", listen, err)
	}
	if host == "" {
		return false, nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().IsLoopback(), nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return false, fmt.Errorf("--listen %q: %w", listen, err)
	}
	for _, s := range addrs {
		if a, err := netip.ParseAddr(s); err != nil || !a.Unmap().IsLoopback() {
			return false, nil
		}
	}
	return len(addrs) > 0, nil
}

// readTokenFile reads a bearer token: the file's first line, at least
// minTokenLen characters with at least minTokenDistinct distinct ones, from
// a file no group or other user can read.
func readTokenFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("--token-file: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("--token-file %s is readable by others (mode %o); chmod 600 it", path, st.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--token-file: %w", err)
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("--token-file %s is empty", path)
	}
	token := strings.TrimSpace(line)
	if len(token) < minTokenLen {
		return "", fmt.Errorf("--token-file %s: the token must be at least %d characters (got %d)", path, minTokenLen, len(token))
	}
	distinct := map[rune]bool{}
	for _, r := range token {
		distinct[r] = true
	}
	if len(distinct) < minTokenDistinct {
		return "", fmt.Errorf("--token-file %s: token file has too little variety; generate one with `openssl rand -hex 32`", path)
	}
	return token, nil
}

// certHosts are the names and IPs a --tls auto certificate covers: the
// listen IP, or every interface address when it binds them all, plus the
// host name and localhost.
func certHosts(listen string) []string {
	host, _, _ := net.SplitHostPort(listen)
	var out []string
	if a, err := netip.ParseAddr(host); err == nil && !a.IsUnspecified() {
		out = append(out, a.String())
	} else if host != "" && err != nil {
		out = append(out, host)
	} else if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				out = append(out, p.Addr().String())
			}
		}
	}
	if name, err := os.Hostname(); err == nil {
		out = append(out, name)
	}
	return append(out, "localhost", "127.0.0.1", "::1")
}

// resolveServeSecurity applies serve's rules (ADR-0008 §4) before anything
// binds: TLS from --tls-cert/--tls-key or --tls auto; a token from
// --token-file. A non-loopback listener needs TLS unless --insecure-listen,
// and a token unless --no-auth. Loopback needs neither.
func resolveServeSecurity(listen, tlsMode, certFile, keyFile, tokenFile, stateDir string, noAuth, insecureListen bool) (serveSecurity, error) {
	var out serveSecurity
	loopback, err := listenLoopback(listen)
	if err != nil {
		return out, err
	}
	switch {
	case tlsMode != "" && tlsMode != "auto":
		return out, fmt.Errorf("--tls %q: the only value is auto (or give --tls-cert and --tls-key)", tlsMode)
	case tlsMode == "auto" && (certFile != "" || keyFile != ""):
		return out, errors.New("--tls auto and --tls-cert/--tls-key are alternatives; give one")
	case (certFile == "") != (keyFile == ""):
		return out, errors.New("--tls-cert and --tls-key go together")
	}
	var pair tls.Certificate
	switch {
	case tlsMode == "auto":
		if stateDir == "" {
			return out, errors.New("--tls auto keeps its certificate in --state-dir; give one (not \"none\")")
		}
		p, err := tlspin.SelfSigned(filepath.Join(stateDir, "tls"), certHosts(listen))
		if err != nil {
			return out, err
		}
		pair, out.leaf, out.repaired = p.Cert, p.Leaf, p.Repaired
	case certFile != "":
		if pair, err = tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return out, fmt.Errorf("--tls-cert/--tls-key: %w", err)
		}
		if out.leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return out, err
		}
	}
	if out.leaf != nil {
		out.tls = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	}
	if tokenFile != "" {
		if out.token, err = readTokenFile(tokenFile); err != nil {
			return out, err
		}
	}
	if !loopback {
		if out.tls == nil && !insecureListen {
			return out, fmt.Errorf("--listen %s is not loopback: serve it with TLS (--tls auto, or --tls-cert/--tls-key), or pass --insecure-listen to serve plaintext anyway", listen)
		}
		if out.token == "" && !noAuth {
			return out, fmt.Errorf("--listen %s is not loopback: give a bearer token (--token-file), or pass --no-auth to serve it to anyone who can reach it", listen)
		}
	}
	return out, nil
}
