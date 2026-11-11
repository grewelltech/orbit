package conformance

import (
	"fmt"
	"sort"
	"strings"
)

// Selection is what narrows a run to some of the registered checks: check
// IDs, group paths or prefixes of them, and tags (ADR-0011 rule 9). Each kind
// given narrows the run on top of the others; within a kind, a check named
// by any entry is selected. The zero Selection selects every check.
type Selection struct {
	// Checks are check IDs, in any case (Registry.Only).
	Checks []string
	// Groups are group paths or prefixes of them, in any case
	// (Registry.Groups), read in Release.
	Groups []string
	// Release is the release the group titles are read in: the run's
	// target release, 0 for the latest (GroupOf).
	Release Release
	// Tags are tags, in any case (Registry.Tagged).
	Tags []string
	// Pack names a core regression pack whose checks join the run, and
	// Profile is the target's core.profile it is held to (Registry.UsePack).
	Pack, Profile string
}

// Apply narrows r to the selection. An entry that names no registered
// check is an error, and leaves r as it was.
func (s Selection) Apply(r *Registry) error {
	if err := r.UsePack(s.Pack, s.Profile); err != nil {
		return err
	}
	if err := r.Only(s.Checks...); err != nil {
		return err
	}
	if err := r.Groups(s.Release, s.Groups...); err != nil {
		return err
	}
	return r.Tagged(s.Tags...)
}

// Groups narrows every later Select, and so every run and listing, to the
// checks whose group (GroupOf, in release rel) has one of prefixes as a
// path prefix (MatchGroup), on top of Only. A prefix no registered check's
// group matches is an error and leaves the registry as it was; no prefixes
// lifts the restriction.
func (r *Registry) Groups(rel Release, prefixes ...string) error {
	if len(prefixes) == 0 {
		r.groups = nil
		return nil
	}
	for _, p := range prefixes {
		if len(groupSegments(p)) == 0 {
			return fmt.Errorf("empty group %q", p)
		}
		matched := false
		for _, t := range r.tests {
			if inGroups(t, rel, []string{p}) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("no check is in group %q; `orbit conformance --list` shows each check's group", p)
		}
	}
	r.groups, r.groupRelease = append([]string(nil), prefixes...), rel
	return nil
}

// inGroups reports whether t's group in release rel matches any of
// prefixes. A check without a group matches none.
func inGroups(t Test, rel Release, prefixes []string) bool {
	g, err := GroupOf(t, rel)
	if err != nil {
		return false
	}
	spec, _ := PrimaryRef(t)
	for _, p := range prefixes {
		if MatchGroup(g, spec.Spec, p) {
			return true
		}
	}
	return false
}

// MatchGroup reports whether prefix selects group g: its segments,
// separated by "/" with the spaces around them ignored, equal g's first
// segments without regard to case. The first segment may also be the
// specification's number (spec, "TS 38.413" for "NGAP"). "NGAP",
// "nas/5GMM common procedures" and the full path all select a check of
// "NAS / 5GMM common procedures / Security mode control procedure" or
// "NGAP / ..." respectively; "NG" selects nothing.
func MatchGroup(g Group, spec, prefix string) bool {
	segs := groupSegments(prefix)
	have := []string{g.Spec, g.Family, g.Procedure}
	if len(segs) == 0 || len(segs) > len(have) {
		return false
	}
	for i, s := range segs {
		if strings.EqualFold(s, have[i]) || (i == 0 && spec != "" && strings.EqualFold(s, spec)) {
			continue
		}
		return false
	}
	return true
}

// groupSegments splits a group path or prefix at "/", trimming each
// segment; empty segments (a trailing "/") are dropped.
func groupSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// GroupPaths returns the distinct groups of tests in release rel, sorted.
func GroupPaths(tests []Test, rel Release) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tests {
		g, err := GroupOf(t, rel)
		if err != nil || seen[g.String()] {
			continue
		}
		seen[g.String()] = true
		out = append(out, g.String())
	}
	sort.Strings(out)
	return out
}

// legacyCategories are the checks of each category the suite had before
// groups replaced categories (ADR-0011 rule 9), for the message that
// refuses a category (CategoriesRemoved).
var legacyCategories = map[string][]string{
	"procedural":  {"NGAP-UE-CTX-RELEASE-ON-REQUEST", "NAS-DEREG-ACCEPT-NOT-SWITCH-OFF", "NGAP-CTX-RELEASE-AFTER-SWITCH-OFF", "NAS-SERVICE-REQUEST-REACTIVATES", "NAS-PDU-RELEASE-UE-REQUESTED", "NAS-PDU-SESSION-ESTABLISHMENT", "NGAP-NG-SETUP", "NGAP-DUPLICATE-RAN-NODE-ID", "NGAP-PAGING-ON-DL-DATA", "NGAP-RAN-CONFIG-UPDATE-ACK", "NGAP-RAN-CONFIG-UPDATE-KEEPS-TA", "NGAP-NG-RESET-ACK"},
	"mobility":    {"GTPU-END-MARKER-ON-PATH-SWITCH", "NGAP-PATH-SWITCH-ACK-TRANSFER", "NGAP-HANDOVER-COMMAND-TRANSFER", "NGAP-HANDOVER-CANCEL-ACK"},
	"gtpu":        {"GTPU-UNKNOWN-TEID-ERRIND", "GTPU-ECHO-RESPONSE", "N3-DL-QFI-CONTAINER"},
	"security":    {"NAS-SMC-IMEISV-REQUEST", "NAS-INITIAL-MSG-CONTAINER-WINS", "NAS-INITIAL-MSG-NO-CONTAINER", "NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED", "NAS-RETRANSMIT-NEW-COUNT", "NGAP-PATH-SWITCH-SECCAP-MISMATCH", "NAS-SMC-NO-NIA0-REPLAYED-CAPS", "NAS-INTEGRITY-FAIL-DISCARD", "NAS-REPLAY-DISCARD", "NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL", "NAS-BADMAC-REGISTRATION-AFTER-SMC"},
	"nas":         {"NAS-REG-REJECTED-NSSAI", "NAS-PDU-UNKNOWN-DNN", "NAS-INITIAL-REGISTRATION", "NAS-REGISTRATION-AFTER-RELEASE", "NAS-T3550-ACCEPT-RETRANSMIT", "NAS-T3560-AUTH-RETRANSMIT"},
	"negative-ie": {"NGAP-UNKNOWN-UE-SURVIVES", "NGAP-NGSETUP-MISSING-TALIST", "NGAP-NGSETUP-UNKNOWN-PLMN", "NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX"},
	"timing":      {},
}

// legacyTags are the tags that name the kind of test a category stood for.
var legacyTags = map[string]Tag{"negative-ie": TagNegative, "security": TagSecurity, "timing": TagTiming}

// CategoriesRemoved is the error for a selection by category, which groups
// replaced (ADR-0011 rule 9): nil for no categories, else one that names,
// for each category given, the groups its checks are in now (in release
// rel) and the checks themselves, so a script or run file can be moved to
// --group or --check. A category is never taken as an alias for groups:
// the groups hold other checks too.
func CategoriesRemoved(categories []string, rel Release) error {
	if len(categories) == 0 {
		return nil
	}
	reg := NewRegistry()
	byID := map[string]Test{}
	for _, t := range reg.Tests() {
		byID[t.ID()] = t
	}
	var parts []string
	for _, c := range categories {
		ids, ok := legacyCategories[strings.ToLower(strings.TrimSpace(c))]
		switch {
		case !ok:
			parts = append(parts, fmt.Sprintf("%q was never a category", c))
		case len(ids) == 0:
			part := fmt.Sprintf("%q held no checks", c)
			if tag, ok := legacyTags[strings.ToLower(strings.TrimSpace(c))]; ok {
				part += fmt.Sprintf("; --tag %s selects the checks of that kind", tag)
			}
			parts = append(parts, part)
		default:
			var tests []Test
			for _, id := range ids {
				if t, ok := byID[id]; ok {
					tests = append(tests, t)
				}
			}
			part := fmt.Sprintf("%q is now the checks %s, in the groups %s",
				c, strings.Join(ids, ","), quoteAll(GroupPaths(tests, rel)))
			if tag, ok := legacyTags[strings.ToLower(strings.TrimSpace(c))]; ok {
				part += fmt.Sprintf("; --tag %s selects every check of that kind", tag)
			}
			parts = append(parts, part)
		}
	}
	return fmt.Errorf("categories were replaced by groups (ADR-0011): select with --group <path or prefix> (run file: conformance.groups) or --check <ID> (conformance.checks); %s",
		strings.Join(parts, "; "))
}

// quoteAll renders each string quoted, comma-separated.
func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
