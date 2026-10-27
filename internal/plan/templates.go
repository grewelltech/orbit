package plan

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bgrewell/orbit/internal/target"
)

// templateFS holds the built-in run files `orbit test` starts from
// (ADR-0008 §6): address-free, so the target gives every address.
//
//go:embed templates/*.yaml
var templateFS embed.FS

// Templates are the built-in templates' names, sorted.
func Templates() []string {
	entries, _ := templateFS.ReadDir("templates")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out)
	return out
}

// TemplateText is a template's run file as written, for --list and docs.
func TemplateText(name string) (string, error) {
	b, err := templateFS.ReadFile(path.Join("templates", name+".yaml"))
	if err != nil {
		return "", fmt.Errorf("no template %q: want one of %s", name, strings.Join(Templates(), ", "))
	}
	return string(b), nil
}

// Template is a template as a run file: the run-file layer of `orbit
// test`, its fields' source "template <name>". A template expands no
// ${ENV}: it has none.
func Template(name string) (*target.RunFile, error) {
	text, err := TemplateText(name)
	if err != nil {
		return nil, err
	}
	return target.ParseRunFile("template "+name, []byte(text), func(string) (string, bool) { return "", false })
}
