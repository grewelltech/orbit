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

// FleetProbe is the fleet template's latency probe in the form the server
// fills when the run starts (from the target's first responder: its
// data_ip, or the registry's for one registered by join). A run file can
// carry it whatever the target, so `orbit run` of an exported file runs
// the probe Start runs.
const FleetProbe = "{latency: {}}"

// fleetFillable reports whether target t gives the fleet template's
// latency probe a target: its first responder has a data_ip, or is one
// registered by join (named only).
func fleetFillable(t target.Target) bool {
	rs := t.App.Responders
	return len(rs) > 0 && (rs[0].DataIP != "" || (rs[0].Control == "" && rs[0].Token == ""))
}

// fillProbe sets rf's fleet behaviors to FleetProbe when template name is
// the fleet and t gives the probe a target; it reports whether it did.
func fillProbe(name string, rf *target.RunFile, t target.Target) bool {
	if name != "fleet" || !fleetFillable(t) {
		return false
	}
	changed := false
	for i, s := range rf.Input.Settings {
		if s.Path == fleetBehaviors {
			rf.Input.Settings[i].Value = FleetProbe
			rf.Input.Settings[i].Source.Name += " (latency target: the first responder's, filled when the run starts)"
			changed = true
		}
	}
	return changed
}

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
	ip, registered := "", false
	if rs := t.App.Responders; len(rs) > 0 {
		ip = rs[0].DataIP
		// A responder registered by join (named only) has its data_ip on
		// the server: the probe goes without a target, which the server
		// fills from its registry.
		registered = ip == "" && rs[0].Control == "" && rs[0].Token == ""
	}
	value, source := fmt.Sprintf("{latency: {target: %q}}", ip), " (latency target: the first responder's data_ip)"
	switch {
	case registered:
		value, source = "{latency: {}}", fmt.Sprintf(" (latency target: responder %s's data_ip, from the server's registry)", t.App.Responders[0].Name)
	case ip == "":
		return false, fmt.Sprintf("fleet template: no responder with a data_ip in target %s; running without the latency probe", t.Name)
	}
	for i, s := range rf.Input.Settings {
		if s.Path == fleetBehaviors {
			rf.Input.Settings[i].Value = value
			rf.Input.Settings[i].Source.Name += source
			changed = true
		}
	}
	return changed, ""
}
