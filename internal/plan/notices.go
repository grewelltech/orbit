package plan

import (
	"fmt"
	"strings"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/target"
)

// OverrideNotices are the fields an env var set over a file's value: an
// entry whose layer is ENV and which replaced a TARGET or RUN_FILE value
// (ADR-0008 §3, widened from "run file" to "file layers": a run file can
// set only one env-bound field, and the case worth catching is the shell
// beating the target, a forgotten `export ORBIT_MCC`). Each names the
// highest file value it beat. A flag over a file is meant and gives none;
// an env value with no file under it (the defaults only) gives none; and an
// env value that beat a file value expanded from that same ${VAR} gives none,
// since the two are equal by construction (expandedVia). Values are as
// provenance holds them, so a secret's are masked.
func OverrideNotices(prov []*orbitv1.FieldProvenance) []*orbitv1.OverrideNotice {
	var out []*orbitv1.OverrideNotice
	for _, p := range prov {
		if p.GetSource().GetLayer() != orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV {
			continue
		}
		var beat *orbitv1.ProvenanceOverride
		for _, o := range p.GetOverridden() {
			switch o.GetSource().GetLayer() {
			case orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET, orbitv1.ProvenanceLayer_PROVENANCE_LAYER_RUN_FILE:
				beat = o // lowest first: the last file entry is the highest
			}
		}
		if beat == nil {
			continue
		}
		// The env var beat a file value that was itself expanded from that
		// same var (target lab1.yaml:6 via ${ORBIT_KI}, with $ORBIT_KI set):
		// then the two values are equal by construction and the "override" is
		// the variable beating its own expansion, not a stale shell export
		// beating a different file value. There is nothing to warn about, so
		// no notice.
		if expandedVia(p.GetSource(), beat.GetSource()) {
			continue
		}
		out = append(out, &orbitv1.OverrideNotice{
			Path:            p.GetPath(),
			Winner:          p.GetSource(),
			Value:           p.GetValue(),
			Overridden:      beat.GetSource(),
			OverriddenValue: beat.GetValue(),
			SameValue:       sameValue(p.GetPath(), p.GetValue(), beat.GetValue()),
		})
	}
	return out
}

// expandedVia reports whether the file value the env beat was expanded from
// the very ${VAR} the env layer sets. The winner is an env source, whose
// detail is "env $VAR" (resolve.go Source.String: the layer and the var name,
// with no line or via of its own); the beaten file value's detail ends
// "via ${VAR}" when it was expanded from that variable. When they name the
// same variable the env value and the file value are identical by
// construction — the file read $VAR to get its value, and the env layer is
// $VAR — so the override is not one worth reporting.
func expandedVia(winner, beaten *orbitv1.ProvenanceSource) bool {
	if winner.GetLayer() != orbitv1.ProvenanceLayer_PROVENANCE_LAYER_ENV {
		return false
	}
	// An env source's detail is "env " + its variable name, which the resolver
	// writes with a leading "$" (mapping.go); accept it with or without.
	name, ok := strings.CutPrefix(winner.GetDetail(), "env ")
	if !ok {
		return false
	}
	name = strings.TrimPrefix(name, "$")
	if name == "" {
		return false
	}
	return strings.Contains(beaten.GetDetail(), "${"+name+"}")
}

// sameValue reports whether an env value equals the file value it beat,
// for a field that is not secret only. A secret's values are masked in
// provenance (and in the archive, which is all a past run has), so two
// masks say nothing of the values: a different ORBIT_KI over a target's
// ki would read as harmless. Nor is a value with the mask in it: two
// different values that each held the same request secret both scrub to
// the same text.
func sameValue(path, value, overridden string) bool {
	if target.IsSecret(path) || strings.Contains(value, secrets.Masked) || strings.Contains(overridden, secrets.Masked) {
		return false
	}
	return value == overridden
}

// NoticeText is a notice as the CLI prints it: the field, the env value
// and the file value it beat, with where each came from. An env value the
// same as the file's is still a notice (an export left in the shell is
// what it shows), marked "(same value)" when the notice says so
// (same_value), never from comparing the values, which may be masks.
func NoticeText(n *orbitv1.OverrideNotice) string {
	text := fmt.Sprintf("%s: %s (%s) overrode %s (%s)", n.GetPath(), n.GetWinner().GetDetail(), n.GetValue(), n.GetOverridden().GetDetail(), n.GetOverriddenValue())
	if n.GetSameValue() {
		text += " (same value)"
	}
	return text
}
