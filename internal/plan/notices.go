package plan

import (
	"fmt"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// OverrideNotices are the fields an env var set over a file's value: an
// entry whose layer is ENV and which replaced a TARGET or RUN_FILE value
// (ADR-0008 §3, widened from "run file" to "file layers": a run file can
// set only one env-bound field, and the case worth catching is the shell
// beating the target, a forgotten `export ORBIT_MCC`). Each names the
// highest file value it beat. A flag over a file is meant and gives none;
// an env value with no file under it (the defaults only) gives none.
// Values are as provenance holds them, so a secret's are masked.
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
		out = append(out, &orbitv1.OverrideNotice{
			Path:            p.GetPath(),
			Winner:          p.GetSource(),
			Value:           p.GetValue(),
			Overridden:      beat.GetSource(),
			OverriddenValue: beat.GetValue(),
		})
	}
	return out
}

// NoticeText is a notice as the CLI prints it: the field, the env value
// and the file value it beat, with where each came from. An env value the
// same as the file's is still a notice (an export left in the shell is
// what it shows), marked "(same value)".
func NoticeText(n *orbitv1.OverrideNotice) string {
	text := fmt.Sprintf("%s: %s (%s) overrode %s (%s)", n.GetPath(), n.GetWinner().GetDetail(), n.GetValue(), n.GetOverridden().GetDetail(), n.GetOverriddenValue())
	if n.GetValue() == n.GetOverriddenValue() {
		text += " (same value)"
	}
	return text
}
