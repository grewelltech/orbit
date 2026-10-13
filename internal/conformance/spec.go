package conformance

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Release is a 3GPP release number (15 is Rel-15). The zero value means
// "unset" where a release is optional, as for a run's target release.
type Release int

// LatestRelease is the newest release every SpecRef has been verified
// through in the 3GPP archive.
const LatestRelease Release = 19

// DefaultTargetRelease is the target release a run uses when none is given:
// Rel-18, the release SD-Core's NGAP, NAS and charts are built against.
// DefaultSpecTargets narrows it where SD-Core is older. A check that requires
// a newer release SKIPs; the target never decides what passes. The evidence
// and caveats are in docs/conformance/target-release.md.
const DefaultTargetRelease Release = 18

// DefaultSpecTargets returns the per-specification pins a run uses with
// DefaultTargetRelease, as a fresh map the caller may change: TS 29.244
// (PFCP) at Rel-16, the release SD-Core's UPF and its PFCP library implement.
// A pin only narrows the run target (see Env.SpecTarget).
func DefaultSpecTargets() map[string]Release {
	return map[string]Release{"TS 29.244": 16}
}

// String renders "Rel-15", or "unset" for the zero value.
func (r Release) String() string {
	if r == 0 {
		return "unset"
	}
	return fmt.Sprintf("Rel-%d", int(r))
}

// MarshalText renders the release as String does, so JSON carries "Rel-15"
// or "unset" rather than a bare number.
func (r Release) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// UnmarshalText accepts what ParseRelease accepts.
func (r *Release) UnmarshalText(b []byte) error {
	v, err := ParseRelease(string(b))
	if err != nil {
		return err
	}
	*r = v
	return nil
}

var releaseRE = regexp.MustCompile(`^(?i:rel-?|r)?(\d{1,2})$`)

// ParseRelease reads a release written as "Rel-15", "rel15", "R15" or "15";
// "" and "unset" are the zero value. Releases run from 1 upwards; a target
// newer than LatestRelease is accepted (every check then runs, with its
// wording verified only through LatestRelease).
func ParseRelease(s string) (Release, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "unset") {
		return 0, nil
	}
	m := releaseRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid release %q: want Rel-<n> or <n>, e.g. Rel-17", s)
	}
	n, _ := strconv.Atoi(m[1])
	if n < 1 {
		return 0, fmt.Errorf("invalid release %q: release numbers start at 1", s)
	}
	return Release(n), nil
}

// Strength is how binding a cited clause is.
type Strength string

const (
	// Shall is a normative requirement; only a check citing one may FAIL.
	Shall Strength = "shall"
	// Should is a recommendation; a check resting on it reports DEVIATION.
	Should Strength = "should"
	// Descriptive is definitional text (a message's IE table, a procedure
	// description) cited for context; it never carries a verdict alone.
	Descriptive Strength = "descriptive"
)

// SpecVersion is one version of a specification whose wording was read in
// the 3GPP archive.
type SpecVersion struct {
	Release Release `json:"release"`
	Version string  `json:"version"` // "V16.12.0"
	Archive string  `json:"archive"` // "29244-gc0.zip"
}

// SpecChange is one release in which a clause's wording or number differs
// from the release it first appeared in.
type SpecChange struct {
	Release Release `json:"release"`
	Note    string  `json:"note"`
}

// SpecRef is one normative source a check asserts, with its provenance. The
// quoted normative text lives in docs/conformance/provenance/<CHECK-ID>.md;
// code carries what a verdict needs.
type SpecRef struct {
	Spec     string   `json:"spec"`   // "TS 29.244"
	Clause   string   `json:"clause"` // exact paragraph: "8.2.31", "10.3.5 Reject IE bullet 1"
	Strength Strength `json:"strength"`
	// Since is the earliest release in which the requirement appears; it may
	// be earlier than Rel-15 (GTP-U dates from Rel-8).
	Since Release `json:"since"`
	// Verified lists every version whose wording was read: one from the
	// Since release, and the latest version of every release from
	// max(Since, 15) through LatestRelease.
	Verified []SpecVersion `json:"verified"`
	// Changed lists releases whose wording or clause number differs from
	// the Since release, one line each.
	Changed []SpecChange `json:"changed,omitempty"`
	// WordingUnchanged states that the text is the same through
	// LatestRelease; it is never inferred from an empty Changed.
	WordingUnchanged bool `json:"wording_unchanged"`
}

// String renders the reference on one line, the same way everywhere (CLI,
// JSON, the API and the generated docs):
//
//	TS 38.413 10.3.5 (Rel-15 onward, wording unchanged through Rel-19)
//	TS 29.281 7.3.1 (Rel-8 onward; Rel-15: "PDU Session" added in V15.4.0)
func (r SpecRef) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%s onward", r.Spec, r.Clause, r.Since)
	if r.WordingUnchanged {
		fmt.Fprintf(&b, ", wording unchanged through %s", LatestRelease)
	}
	for _, c := range r.Changed {
		fmt.Fprintf(&b, "; %s: %s", c.Release, c.Note)
	}
	b.WriteString(")")
	return b.String()
}

// RenderRefs joins a check's references with "; ".
func RenderRefs(refs []SpecRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.String()
	}
	return strings.Join(parts, "; ")
}

// minReleaser is implemented by a check whose applicability is newer than
// its refs' Since, because it asserts the newer wording of a clause. The
// declaration is a statement about the whole check, so it gates on the run
// target only; per-specification pins narrow through the refs' Since (see
// skipReason).
type minReleaser interface{ MinRelease() Release }

// MinRelease is the oldest release a check applies to: its own declaration
// if it has one, else the highest Since among its shall and should
// references (requirementRefs: descriptive ones count only when the check
// cites nothing stronger). It is the floor ValidateTest and skipReason use,
// so the "requires Rel-N or later" line agrees with what gates the check.
func MinRelease(t Test) Release {
	if m, ok := t.(minReleaser); ok {
		return m.MinRelease()
	}
	var max Release
	for _, r := range requirementRefs(t.SpecRefs()) {
		if r.Since > max {
			max = r.Since
		}
	}
	return max
}

// requirementLine is the provenance line every result's detail ends with:
// the release the check requires and its rendered references.
func requirementLine(t Test) string {
	return fmt.Sprintf("requires %s or later; %s", MinRelease(t), RenderRefs(t.SpecRefs()))
}

var (
	versionRE = regexp.MustCompile(`^V(\d+)\.\d+\.\d+$`)
	archiveRE = regexp.MustCompile(`^\d{5}-[0-9a-z]{3}\.zip$`)
)

// ValidateTest checks a test's provenance and verdict class against the
// rules every registered check must meet; the invariant test runs it over
// the registry, so a check without verified provenance cannot merge.
func ValidateTest(t Test) error {
	refs := t.SpecRefs()
	if len(refs) == 0 {
		return fmt.Errorf("%s: no spec references", t.ID())
	}
	hasShall := false
	// floor is the newest Since among the refs that carry a requirement
	// (shall or should), or among all refs when none does: a check cannot
	// apply to a release older than a requirement it asserts.
	var floor, floorAll Release
	for i, r := range refs {
		where := fmt.Sprintf("%s ref %d (%s %s)", t.ID(), i+1, r.Spec, r.Clause)
		if err := validateRef(r); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if r.Strength == Shall {
			hasShall = true
		}
		if r.Since > floorAll {
			floorAll = r.Since
		}
		if (r.Strength == Shall || r.Strength == Should) && r.Since > floor {
			floor = r.Since
		}
	}
	if floor == 0 {
		floor = floorAll
	}
	switch t.VerdictClass() {
	case Fail:
		if !hasShall {
			return fmt.Errorf("%s: a FAIL-class check must cite at least one shall; declare VerdictClass Deviation", t.ID())
		}
	case Deviation:
	default:
		return fmt.Errorf("%s: VerdictClass must be FAIL or DEVIATION, got %q", t.ID(), t.VerdictClass())
	}
	if m := MinRelease(t); m < floor || m > LatestRelease {
		return fmt.Errorf("%s: MinRelease %s outside %s..%s: it must be at least the newest Since among the requirements it asserts", t.ID(), m, floor, LatestRelease)
	}
	return nil
}

func validateRef(r SpecRef) error {
	if strings.TrimSpace(r.Spec) == "" {
		return fmt.Errorf("empty Spec")
	}
	if c := strings.TrimSpace(r.Clause); c == "" || strings.ContainsAny(c, "†§") {
		return fmt.Errorf("clause %q must be an exact paragraph, without § or †", r.Clause)
	}
	switch r.Strength {
	case Shall, Should, Descriptive:
	default:
		return fmt.Errorf("strength %q is not shall, should or descriptive", r.Strength)
	}
	if r.Since < 1 || r.Since > LatestRelease {
		return fmt.Errorf("since %s outside Rel-1..%s", r.Since, LatestRelease)
	}
	have := map[Release]bool{}
	for _, v := range r.Verified {
		m := versionRE.FindStringSubmatch(v.Version)
		if m == nil {
			return fmt.Errorf("version %q is not V<n>.<n>.<n>", v.Version)
		}
		if major, _ := strconv.Atoi(m[1]); Release(major) != v.Release {
			return fmt.Errorf("version %s belongs to Rel-%s, not %s", v.Version, m[1], v.Release)
		}
		if !archiveRE.MatchString(v.Archive) {
			return fmt.Errorf("version %s names archive %q, want a 3GPP archive file like 38413-j40.zip", v.Version, v.Archive)
		}
		have[v.Release] = true
	}
	if !have[r.Since] {
		return fmt.Errorf("no verified version from the Since release %s", r.Since)
	}
	from := r.Since
	if from < 15 {
		from = 15
	}
	for rel := from; rel <= LatestRelease; rel++ {
		if !have[rel] {
			return fmt.Errorf("no verified version for %s (every release from %s to %s is required)", rel, from, LatestRelease)
		}
	}
	if !r.WordingUnchanged && len(r.Changed) == 0 {
		return fmt.Errorf("neither WordingUnchanged nor any Changed entry")
	}
	for _, c := range r.Changed {
		if c.Release < r.Since || c.Release > LatestRelease || strings.TrimSpace(c.Note) == "" {
			return fmt.Errorf("change entry %s %q outside %s..%s or empty", c.Release, c.Note, r.Since, LatestRelease)
		}
	}
	return nil
}
