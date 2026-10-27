package join

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"text/template"

	"github.com/bgrewell/orbit/internal/target"
)

// Arches are the app-host architectures a join serves a binary for.
var Arches = []string{"amd64", "arm64"}

//go:embed script.sh.tmpl
var scriptText string

var scriptTmpl = template.Must(template.New("join").Funcs(template.FuncMap{"q": shquote}).Parse(scriptText))

// DefaultBindPort is the port the responder's control plane listens on when
// the script picks its bind (USAGE's `orbit responder --bind ...:9551`).
const DefaultBindPort = 9551

// defaultPath is the PATH a join script sets for itself: never the caller's
// or sudo's.
const defaultPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// The shapes a script's values must have before they are rendered.
var (
	nonceRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	tokenRE  = regexp.MustCompile(`^` + Prefix + `[A-Za-z0-9_-]{22}$`)
	pinRE    = regexp.MustCompile(`^sha256/[A-Za-z0-9+/]{43}=$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ScriptData is what a join script is rendered from.
type ScriptData struct {
	// ServerURL is https://<management address>, the script's server.
	ServerURL string
	// Pin is the server's key, sha256/<base64> (tlspin.Fingerprint).
	Pin   string
	Token string
	Name  string
	// SHA256 is each arch's orbit binary's SHA-256, lowercase hex; an
	// arch without one is refused by the script.
	SHA256 map[string]string
	Spec   Spec
	// Nonce makes this render's truncation sentinel: 32 lowercase hex
	// digits. Empty draws a fresh one from crypto/rand, so every render's
	// differs; tests fix it.
	Nonce string

	// root prefixes every file the script reads or writes ("" in
	// production); path replaces its PATH. Only this package's tests set
	// them, so nothing at run time can point a root script elsewhere.
	root string
	path string
}

// Script renders the join script for d. Every value is checked for its
// shape first and then single-quoted (shquote), so nothing reaches the
// script as shell syntax.
func Script(d ScriptData) ([]byte, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return render(d)
}

// render renders d without validating it. Script validates first; tests
// call render directly to show the script itself refuses what validation
// would have stopped.
func render(d ScriptData) ([]byte, error) {
	sha := map[string]string{}
	for _, a := range Arches {
		sha[a] = d.SHA256[a]
	}
	if d.Nonce == "" {
		var n [16]byte
		if _, err := rand.Read(n[:]); err != nil {
			return nil, fmt.Errorf("nonce: %w", err)
		}
		d.Nonce = hex.EncodeToString(n[:])
	}
	path := d.path
	if path == "" {
		path = defaultPath
	}
	pools := make([]string, len(d.Spec.UEPools))
	for i, p := range d.Spec.UEPools {
		pools[i] = masked(p)
	}
	d.Spec.N6Subnet = masked(d.Spec.N6Subnet)
	if a, err := netip.ParseAddr(d.Spec.UPFN6); err == nil {
		d.Spec.UPFN6 = a.String()
	}
	host, _, err := net.SplitHostPort(d.Spec.Management)
	if err != nil {
		host = d.Spec.Management
	}
	isIP := "no"
	if _, err := netip.ParseAddr(host); err == nil {
		isIP = "yes"
	}
	var b bytes.Buffer
	err = scriptTmpl.Execute(&b, struct {
		ScriptData
		CurlPin  string
		UEPools  string
		SHA256   map[string]string
		End      string
		Path     string
		Root     string
		MgmtHost string
		MgmtIsIP string
		BindPort string
	}{d, "sha256//" + strings.TrimPrefix(d.Pin, "sha256/"), strings.Join(pools, " "), sha,
		"--orbit-end-" + d.Nonce, path, d.root, host, isIP, fmt.Sprint(DefaultBindPort)})
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// masked is the CIDR p with its host bits cleared, or p as given when it
// does not parse (only render, unvalidated, sees one).
func masked(p string) string {
	if pr, err := netip.ParsePrefix(p); err == nil {
		return pr.Masked().String()
	}
	return p
}

func (d ScriptData) validate() error {
	u, err := url.Parse(d.ServerURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("server URL %q: want https://host:port", d.ServerURL)
	}
	if !pinRE.MatchString(d.Pin) {
		return errors.New("pin: want sha256/<base64>")
	}
	if !tokenRE.MatchString(d.Token) {
		return errors.New("token: not a join token")
	}
	if d.Name != "" && !target.ValidName(d.Name) {
		return ErrName
	}
	for a, h := range d.SHA256 {
		if !isArch(a) {
			return fmt.Errorf("sha256: no arch %q", a)
		}
		if !sha256RE.MatchString(h) {
			return fmt.Errorf("sha256 for %s: want 64 lowercase hex digits", a)
		}
	}
	if len(d.SHA256) == 0 {
		return errors.New("sha256: no orbit binary to serve")
	}
	if d.Nonce != "" && !nonceRE.MatchString(d.Nonce) {
		return errors.New("nonce: want 32 lowercase hex digits")
	}
	return d.Spec.Validate()
}

func isArch(a string) bool {
	for _, x := range Arches {
		if a == x {
			return true
		}
	}
	return false
}

// shquote is s as one POSIX shell word: single-quoted, with each single
// quote in s closed, escaped with a backslash and reopened.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
