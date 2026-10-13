package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A pin only narrows: it applies when older than the run target, the run
// target applies otherwise, and an unset target gates nothing, pins included.
func TestEnvSpecTarget(t *testing.T) {
	pfcp16 := map[string]Release{"TS 29.244": 16}
	tests := []struct {
		name   string
		target Release
		pins   map[string]Release
		spec   string
		want   Release
	}{
		{name: "unset target with a pin", target: 0, pins: pfcp16, spec: "TS 29.244", want: 0},
		{name: "pin below the target", target: 18, pins: pfcp16, spec: "TS 29.244", want: 16},
		{name: "pin equal to the target", target: 16, pins: pfcp16, spec: "TS 29.244", want: 16},
		{name: "pin above the target", target: 15, pins: pfcp16, spec: "TS 29.244", want: 15},
		{name: "unpinned spec", target: 18, pins: pfcp16, spec: "TS 38.413", want: 18},
		{name: "nil pins", target: 18, pins: nil, spec: "TS 29.244", want: 18},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := Env{TargetRelease: tc.target, SpecTargets: tc.pins}
			if got := e.SpecTarget(tc.spec); got != tc.want {
				t.Errorf("SpecTarget(%q) = %s, want %s", tc.spec, got, tc.want)
			}
		})
	}
}

// Only the pins that narrow the run are reported, at their effective release.
func TestEnvNarrowingSpecTargets(t *testing.T) {
	pins := map[string]Release{"TS 29.244": 16, "TS 29.281": 19}
	tests := []struct {
		name   string
		target Release
		pins   map[string]Release
		want   map[string]Release
	}{
		{name: "unset", target: 0, pins: pins, want: map[string]Release{}},
		{name: "one narrows", target: 18, pins: pins, want: map[string]Release{"TS 29.244": 16}},
		{name: "none narrow", target: 15, pins: pins, want: map[string]Release{}},
		{name: "nil pins", target: 18, pins: nil, want: map[string]Release{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Env{TargetRelease: tc.target, SpecTargets: tc.pins}.NarrowingSpecTargets()
			if got == nil || len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// runOne gates each requirement on its spec's effective target. A pinned
// spec SKIPs with a detail naming the reference and the pin; the run target
// keeps its own text; a descriptive reference never causes a skip.
func TestRunOneSpecTargets(t *testing.T) {
	pins := map[string]Release{"TS 29.244": 16}
	pfcp := func(since Release) SpecRef {
		return SpecRef{Spec: "TS 29.244", Clause: "5.3.2", Strength: Shall, Since: since, WordingUnchanged: true}
	}
	ngap := func(since Release, s Strength) SpecRef {
		return SpecRef{Spec: "TS 38.413", Clause: "10.3.5", Strength: s, Since: since, WordingUnchanged: true}
	}
	tests := []struct {
		name       string
		refs       []SpecRef
		target     Release
		wantRan    bool
		wantDetail string
	}{
		{name: "pinned spec above its pin skips", refs: []SpecRef{pfcp(17)}, target: 18,
			wantDetail: "requires Rel-17 (TS 29.244 5.3.2); target for TS 29.244 is Rel-16"},
		{name: "pinned spec at its pin runs", refs: []SpecRef{pfcp(16)}, target: 18, wantRan: true},
		{name: "unset target ignores the pin", refs: []SpecRef{pfcp(17)}, target: 0, wantRan: true},
		{name: "unpinned spec at the target runs", refs: []SpecRef{ngap(18, Shall)}, target: 18, wantRan: true},
		{name: "unpinned spec above the target skips with the run-target text", refs: []SpecRef{ngap(19, Shall)}, target: 18,
			wantDetail: "requires Rel-19 (TS 38.413 10.3.5); target release is Rel-18"},
		{name: "should is gated like shall", refs: []SpecRef{ngap(15, Shall), {Spec: "TS 29.244", Clause: "8.2.31", Strength: Should, Since: 17, WordingUnchanged: true}}, target: 18,
			wantDetail: "requires Rel-17 (TS 29.244 8.2.31); target for TS 29.244 is Rel-16"},
		{name: "descriptive ref never skips", refs: []SpecRef{ngap(15, Shall), ngap(19, Descriptive), {Spec: "TS 29.244", Clause: "7.4", Strength: Descriptive, Since: 18, WordingUnchanged: true}}, target: 18, wantRan: true},
		{name: "run target wins over a pin", refs: []SpecRef{pfcp(17), ngap(19, Shall)}, target: 18,
			wantDetail: "target release is Rel-18"},
		{name: "a check citing only descriptive refs is gated on them", refs: []SpecRef{ngap(19, Descriptive)}, target: 18,
			wantDetail: "requires Rel-19 (TS 38.413 10.3.5); target release is Rel-18"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			c := provTest{id: "NEW-CHECK", refs: tc.refs, ran: &ran, result: Result{Verdict: Pass}}
			res := runOne(context.Background(), c, Env{TargetRelease: tc.target, SpecTargets: pins}, time.Second)
			if ran != tc.wantRan {
				t.Fatalf("ran = %v, want %v (verdict %s, detail %q)", ran, tc.wantRan, res.Verdict, res.Detail)
			}
			if !tc.wantRan && res.Verdict != Skip {
				t.Errorf("verdict = %s, want SKIP", res.Verdict)
			}
			if tc.wantDetail != "" && !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("detail %q lacks %q", res.Detail, tc.wantDetail)
			}
			if want := requirementLine(c); !strings.HasSuffix(res.Detail, want) {
				t.Errorf("detail %q does not end with the requirement line", res.Detail)
			}
		})
	}
}

// A declared MinRelease above the run target still SKIPs with the run-target
// text, whatever the pins say.
func TestRunOneDeclaredMinAboveTarget(t *testing.T) {
	ref := SpecRef{Spec: "TS 29.281", Clause: "7.3.1", Strength: Shall, Since: 8, WordingUnchanged: true}
	c := provTestMin{provTest{id: "GTPU-X", refs: []SpecRef{ref}, min: 17}}
	res := runOne(context.Background(), c, Env{TargetRelease: 16, SpecTargets: map[string]Release{"TS 29.281": 15}}, time.Second)
	if res.Verdict != Skip || !strings.Contains(res.Detail, "requires Rel-17 (TS 29.281 7.3.1); target release is Rel-16") {
		t.Errorf("verdict %s detail %q", res.Verdict, res.Detail)
	}
}

// The text line names the narrowing pins in a stable order; the JSON summary
// carries them as releases, an empty object when there are none.
func TestSummaryTargets(t *testing.T) {
	tests := []struct {
		name     string
		target   Release
		pins     map[string]Release
		wantLine string
		wantJSON string
	}{
		{name: "pinned", target: 18, pins: map[string]Release{"TS 29.244": 16}, wantLine: "Rel-18 (TS 29.244 at Rel-16)", wantJSON: `"SpecTargets":{"TS 29.244":"Rel-16"}`},
		{name: "two pins sorted", target: 18, pins: map[string]Release{"TS 29.281": 17, "TS 29.244": 16}, wantLine: "Rel-18 (TS 29.244 at Rel-16, TS 29.281 at Rel-17)"},
		{name: "no pins", target: 18, pins: map[string]Release{}, wantLine: "Rel-18", wantJSON: `"SpecTargets":{}`},
		{name: "unset", target: 0, pins: map[string]Release{}, wantLine: "unset", wantJSON: `"SpecTargets":{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Summarize(nil)
			s.TargetRelease, s.SpecTargets = tc.target, tc.pins
			if got := s.TargetLine(); got != tc.wantLine {
				t.Errorf("TargetLine = %q, want %q", got, tc.wantLine)
			}
			if tc.wantJSON == "" {
				return
			}
			b, err := json.Marshal(s)
			if err != nil || !strings.Contains(string(b), tc.wantJSON) {
				t.Errorf("summary JSON %s (%v) lacks %s", b, err, tc.wantJSON)
			}
		})
	}
}

// A summary nobody set pins on still carries an empty SpecTargets, never null.
func TestSummarizeSpecTargetsNeverNull(t *testing.T) {
	b, err := json.Marshal(Summarize(nil))
	if err != nil || !strings.Contains(string(b), `"SpecTargets":{}`) {
		t.Errorf("summary JSON %s (%v) lacks an empty SpecTargets", b, err)
	}
}

// The defaults are Rel-18 with TS 29.244 pinned to Rel-16, and each call
// returns a map the caller may change without touching the next one.
func TestDefaultTargets(t *testing.T) {
	if DefaultTargetRelease != 18 {
		t.Errorf("DefaultTargetRelease = %s, want Rel-18", DefaultTargetRelease)
	}
	a := DefaultSpecTargets()
	if len(a) != 1 || a["TS 29.244"] != 16 {
		t.Fatalf("DefaultSpecTargets = %v, want TS 29.244 at Rel-16", a)
	}
	if got := defaultTargetLine(); got != "Rel-18 (TS 29.244 at Rel-16)" {
		t.Errorf("defaultTargetLine = %q", got)
	}
	a["TS 29.244"] = 1
	if b := DefaultSpecTargets(); b["TS 29.244"] != 16 {
		t.Errorf("DefaultSpecTargets shares its map: %v", b)
	}
}

// defaultSkips lists the registered checks allowed to SKIP at the default
// targets, each with the reason. It is empty: the defaults must not silently
// shrink the suite, so a check that defaults to SKIP is listed on purpose.
var defaultSkips = map[string]string{}

// Every registered check runs at the default targets, apart from those
// listed in defaultSkips.
func TestNoCheckSkipsAtDefaultTargets(t *testing.T) {
	e := Env{TargetRelease: DefaultTargetRelease, SpecTargets: DefaultSpecTargets()}
	for _, c := range NewRegistry().Tests() {
		why, skip := skipReason(c, e)
		if _, listed := defaultSkips[c.ID()]; skip != listed {
			t.Errorf("%s: skips at the defaults = %v (%s), listed in defaultSkips = %v", c.ID(), skip, why, listed)
		}
	}
}

// A TS 29.244 requirement newer than Rel-16 SKIPs at the defaults, naming
// the pin; at Rel-16 it runs.
func TestRunOneDefaultPFCPPin(t *testing.T) {
	e := Env{TargetRelease: DefaultTargetRelease, SpecTargets: DefaultSpecTargets()}
	for _, tc := range []struct {
		since   Release
		wantRan bool
	}{{17, false}, {16, true}} {
		ran := false
		ref := SpecRef{Spec: "TS 29.244", Clause: "5.3.2", Strength: Shall, Since: tc.since, WordingUnchanged: true}
		c := provTest{id: "PFCP-X", refs: []SpecRef{ref}, ran: &ran, result: Result{Verdict: Pass}}
		res := runOne(context.Background(), c, e, time.Second)
		if ran != tc.wantRan {
			t.Errorf("Since %s: ran = %v, want %v (%q)", tc.since, ran, tc.wantRan, res.Detail)
		}
		if !tc.wantRan && !strings.Contains(res.Detail, "target for TS 29.244 is Rel-16") {
			t.Errorf("Since %s: detail %q does not name the pin", tc.since, res.Detail)
		}
	}
}

// MinRelease falls back to the requirement floor: a descriptive reference
// newer than the check's shall does not raise it, so the check runs at a
// target between the two and its requirement line names the shall's release.
func TestMinReleaseRequirementFloor(t *testing.T) {
	refs := []SpecRef{
		{Spec: "TS 38.413", Clause: "10.3.5", Strength: Shall, Since: 15, WordingUnchanged: true},
		{Spec: "TS 38.413", Clause: "9.2.6.1", Strength: Descriptive, Since: 17, WordingUnchanged: true},
	}
	ran := false
	c := provTest{id: "FLOOR", refs: refs, ran: &ran, result: Result{Verdict: Pass}}
	if got := MinRelease(c); got != 15 {
		t.Errorf("MinRelease = %s, want Rel-15", got)
	}
	res := runOne(context.Background(), c, Env{TargetRelease: 16}, time.Second)
	if !ran || res.Verdict != Pass {
		t.Fatalf("ran = %v verdict %s at target Rel-16, want it to run and PASS", ran, res.Verdict)
	}
	if res.MinRelease != 15 || !strings.Contains(res.Detail, "requires Rel-15 or later") {
		t.Errorf("MinRelease %s, detail %q", res.MinRelease, res.Detail)
	}
	// The run-target SKIP names only the requirement that binds.
	newer := []SpecRef{
		{Spec: "TS 38.413", Clause: "10.6", Strength: Shall, Since: 18, WordingUnchanged: true},
		{Spec: "TS 38.413", Clause: "9.2.3.4", Strength: Descriptive, Since: 19, WordingUnchanged: true},
	}
	res = runOne(context.Background(), provTest{id: "NEWER", refs: newer}, Env{TargetRelease: 17}, time.Second)
	if res.Verdict != Skip || !strings.Contains(res.Detail, "requires Rel-18 (TS 38.413 10.6); target release is Rel-17") {
		t.Errorf("verdict %s detail %q", res.Verdict, res.Detail)
	}
}
