package conformance

import (
	"fmt"
	"slices"
	"strings"
)

// Tag is a kind of test (ADR-0011 rule 9), from a closed set. Tags are
// optional, for filtering only: they never affect a verdict, and a check's
// group says what it is about, not its tags.
type Tag string

const (
	// TagNegative is a check whose stimulus is malformed, unknown or
	// unexpected input: a missing IE, an unknown identifier, a bad MAC, a
	// message out of turn.
	TagNegative Tag = "negative"
	// TagSecurity is a check whose stimulus is a security procedure or
	// exercises a protection: integrity, replay, the protection of the
	// initial NAS message, the UE security capabilities.
	TagSecurity Tag = "security"
	// TagTiming is a check whose stimulus is a timer or the time between
	// messages: a timer expiry it waits out, an arrival-time sweep.
	TagTiming Tag = "timing"
)

// tags is the closed set, in the order they are listed.
var tags = []Tag{TagNegative, TagSecurity, TagTiming}

// Tags returns the closed set of tags.
func Tags() []Tag { return slices.Clone(tags) }

// ParseTag reads a tag, in any case.
func ParseTag(s string) (Tag, error) {
	t := Tag(strings.ToLower(strings.TrimSpace(s)))
	if !slices.Contains(tags, t) {
		names := make([]string, len(tags))
		for i, x := range tags {
			names[i] = string(x)
		}
		return "", fmt.Errorf("unknown tag %q: want one of %s", s, strings.Join(names, ", "))
	}
	return t, nil
}

// Tagger is a check that declares its tags. A check that does not has
// none.
type Tagger interface {
	Tags() []Tag
}

// TagsOf is a check's declared tags, nil for none.
func TagsOf(t Test) []Tag {
	if tg, ok := t.(Tagger); ok {
		return tg.Tags()
	}
	return nil
}

// validateTags refuses a tag outside the closed set and a tag declared
// twice.
func validateTags(t Test) error {
	seen := map[Tag]bool{}
	for _, tag := range TagsOf(t) {
		if !slices.Contains(tags, tag) {
			return fmt.Errorf("%s: tag %q is not one of the closed set", t.ID(), tag)
		}
		if seen[tag] {
			return fmt.Errorf("%s: tag %q declared twice", t.ID(), tag)
		}
		seen[tag] = true
	}
	return nil
}

// Tagged narrows every later Select to the checks with at least one of
// tags, on top of Only and Groups. A tag outside the closed set is an
// error and leaves the registry as it was; no tags lifts the restriction.
func (r *Registry) Tagged(names ...string) error {
	if len(names) == 0 {
		r.tags = nil
		return nil
	}
	want := make([]Tag, 0, len(names))
	for _, n := range names {
		t, err := ParseTag(n)
		if err != nil {
			return err
		}
		want = append(want, t)
	}
	r.tags = want
	return nil
}

// hasAnyTag reports whether t declares one of want.
func hasAnyTag(t Test, want []Tag) bool {
	for _, tag := range TagsOf(t) {
		if slices.Contains(want, tag) {
			return true
		}
	}
	return false
}

// The tags of the built-in checks, by what each one's stimulus is.

func (errorIndicationOnUnknownUE) Tags() []Tag    { return []Tag{TagNegative} }
func (ngSetupMissingMandatoryIE) Tags() []Tag     { return []Tag{TagNegative} }
func (ngSetupUnknownPLMN) Tags() []Tag            { return []Tag{TagNegative} }
func (duplicateRANNodeID) Tags() []Tag            { return []Tag{TagNegative} }
func (ranConfigUpdateTransferSyntax) Tags() []Tag { return []Tag{TagNegative} }
func (gtpuUnknownTEID) Tags() []Tag               { return []Tag{TagNegative} }
func (nasPDUUnknownDNN) Tags() []Tag              { return []Tag{TagNegative} }
func (nasRegRejectedNSSAI) Tags() []Tag           { return []Tag{TagNegative} }

func (nasSMCNoNIA0) Tags() []Tag                      { return []Tag{TagSecurity} }
func (nasSMCIMEISVRequest) Tags() []Tag               { return []Tag{TagNegative, TagSecurity} }
func (nasInitialMsgContainerWins) Tags() []Tag        { return []Tag{TagNegative, TagSecurity} }
func (nasInitialMsgNoContainer) Tags() []Tag          { return []Tag{TagNegative, TagSecurity} }
func (nasInitialMsgNonCleartextAccepted) Tags() []Tag { return []Tag{TagNegative, TagSecurity} }
func (nasIntegrityFailDiscard) Tags() []Tag           { return []Tag{TagNegative, TagSecurity} }
func (nasReplayDiscard) Tags() []Tag                  { return []Tag{TagNegative, TagSecurity} }
func (nasCountNotAdvanced) Tags() []Tag               { return []Tag{TagNegative, TagSecurity} }
func (nasBadMACRegistration) Tags() []Tag             { return []Tag{TagNegative, TagSecurity} }
func (ngapPathSwitchSecCapMismatch) Tags() []Tag      { return []Tag{TagNegative, TagSecurity} }
func (nasRetransmitNewCount) Tags() []Tag             { return []Tag{TagSecurity, TagTiming} }

func (nasT3550AcceptRetransmit) Tags() []Tag          { return []Tag{TagTiming} }
func (nasT3560AuthRetransmit) Tags() []Tag            { return []Tag{TagTiming} }
func (nasRegistrationAfterRelease) Tags() []Tag       { return []Tag{TagTiming} }
func (nasRegistrationDuringOtherRelease) Tags() []Tag { return []Tag{TagTiming} }
