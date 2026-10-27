package join

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
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

// The shapes a script's values must have before they are rendered.
var (
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
}

// Script renders the join script for d. Every value is checked for its
// shape first and then single-quoted (shquote), so nothing reaches the
// script as shell syntax.
func Script(d ScriptData) ([]byte, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	sha := map[string]string{}
	for _, a := range Arches {
		sha[a] = d.SHA256[a]
	}
	var b bytes.Buffer
	err := scriptTmpl.Execute(&b, struct {
		ScriptData
		CurlPin string
		UEPools string
		SHA256  map[string]string
	}{d, "sha256//" + strings.TrimPrefix(d.Pin, "sha256/"), strings.Join(d.Spec.UEPools, " "), sha})
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
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
