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

// fleetBehaviors is the fleet template's behaviours field.
const fleetBehaviors = "workloads[fleet].behaviors"

// FillTemplate completes template name from the resolved target t: the
// fleet template's latency probe echoes the target's first responder's
// data_ip, as a scenario's omitted latency.target does. A target with
// none leaves the fleet idle until its deadline, and FillTemplate says so
// in note; it never fails. changed reports whether rf changed, so the
// plan must be built again.
func FillTemplate(name string, rf *target.RunFile, t target.Target) (changed bool, note string) {
	if name != "fleet" {
		return false, ""
	}
	ip := ""
	if rs := t.App.Responders; len(rs) > 0 {
		ip = rs[0].DataIP
	}
	if ip == "" {
		return false, fmt.Sprintf("fleet template: no responder in target %s; running without the latency probe", t.Name)
	}
	for i, s := range rf.Input.Settings {
		if s.Path == fleetBehaviors {
			rf.Input.Settings[i].Value = fmt.Sprintf("{latency: {target: %q}}", ip)
			rf.Input.Settings[i].Source.Name += " (latency target: the first responder's data_ip)"
			changed = true
		}
	}
	return changed, ""
}
