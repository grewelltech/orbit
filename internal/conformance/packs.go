package conformance

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bgrewell/orbit/internal/coreprofile"
)

// Pack is a core regression pack (ADR-0011 rule 1): checks for the
// behaviour of one named core, keyed by the core-compatibility profile a
// target names in core.profile (ADR-0002). A pack is never part of the
// default run: its checks run only in a run that names the pack, and only
// against a target with the pack's profile. A pack check never reports a
// conformance verdict: it may declare only INFO, WARN, SKIP and BLOCKED
// outcomes (packVerdicts), so PASS, FAIL and DEVIATION, which say a cited
// requirement was met or not, come only from the conformance layer. A pack
// check owns no procedure: owners are checks of the default run.
type Pack struct {
	// Name is what a run names the pack by (--pack, conformance.pack).
	Name string
	// Profile is the core.profile the pack is for; never strict-3gpp.
	Profile string
	// Checks are the pack's checks.
	Checks []Test
}

// packVerdicts are the verdicts a pack check may declare.
var packVerdicts = []Verdict{Info, Warn, Skip, Blocked}

// builtinPacks are the packs every registry has. There are none: no check
// of the suite has needed a core's name to state what it tests.
var builtinPacks []Pack

// validatePack checks a pack against the rules of the pack layer.
func validatePack(p Pack) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("a pack has no name")
	}
	switch _, known := coreprofile.Get(p.Profile); {
	case p.Profile == "" || p.Profile == coreprofile.Default().Name:
		return fmt.Errorf("pack %s: profile %q has no pack; a pack is keyed by a core's own profile", p.Name, p.Profile)
	case !known:
		return fmt.Errorf("pack %s: unknown core profile %q (have %v)", p.Name, p.Profile, coreprofile.Names())
	}
	if len(p.Checks) == 0 {
		return fmt.Errorf("pack %s: no checks", p.Name)
	}
	for _, t := range p.Checks {
		outs, _ := declared(t)
		for _, o := range outs {
			if !slices.Contains(packVerdicts, o.Verdict) {
				return fmt.Errorf("pack %s: %s declares outcome %q as %s; a pack check reports no conformance verdict (INFO, WARN, SKIP or BLOCKED only)",
					p.Name, t.ID(), o.ID, o.Verdict)
			}
		}
		if owned := OwnsOf(t); len(owned) > 0 {
			return fmt.Errorf("pack %s: %s owns %s; a procedure's owner is a check of the default run", p.Name, t.ID(), joinProcedures(owned))
		}
	}
	return nil
}

// UsePack lets the checks of the named pack into every later Select, for a
// target whose core.profile is profile. It is an error, leaving the
// registry as it was, when the target has no profile or strict-3gpp (no
// pack applies to it), when no pack has the name, or when the pack is for
// another profile. An empty name lifts a pack named before.
func (r *Registry) UsePack(name, profile string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		r.pack = ""
		return nil
	}
	p, ok := r.packs[name]
	switch {
	case profile == "" || profile == coreprofile.Default().Name:
		shown := profile
		if shown == "" {
			shown = "none"
		}
		return fmt.Errorf("pack %q needs a target with a core profile; the target's is %s, which has no pack", name, shown)
	case !ok:
		return fmt.Errorf("unknown pack %q (have %s)", name, r.packNames())
	case p.Profile != profile:
		return fmt.Errorf("pack %q is for core profile %q; the target's is %q", name, p.Profile, profile)
	}
	r.pack = name
	return nil
}

// PackProfile returns the core.profile the named pack is for, false when
// r has no such pack.
func (r *Registry) PackProfile(name string) (string, bool) {
	p, ok := r.packs[strings.TrimSpace(name)]
	return p.Profile, ok
}

// PackOf names the pack a check of r belongs to, "" for a check of the
// default run.
func (r *Registry) PackOf(t Test) string { return r.packOf[t.ID()] }

// Packs returns the names of r's packs, sorted.
func (r *Registry) Packs() []string {
	out := make([]string, 0, len(r.packs))
	for n := range r.packs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// packNames renders the pack names for an error, "none" when r has none.
func (r *Registry) packNames() string {
	if len(r.packs) == 0 {
		return "none"
	}
	return strings.Join(r.Packs(), ", ")
}

// inRun reports whether t can be selected: a check of the default run, or
// of the pack UsePack named.
func (r *Registry) inRun(t Test) bool {
	p := r.packOf[t.ID()]
	return p == "" || p == r.pack
}
