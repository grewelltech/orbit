package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// provTest is a configurable check for the provenance rules.
type provTest struct {
	id     string
	refs   []SpecRef
	class  Verdict
	min    Release // 0 = derive from refs
	result Result
	ran    *bool
}

func (p provTest) ID() string            { return p.id }
func (p provTest) Category() Category    { return Procedural }
func (p provTest) SpecRefs() []SpecRef   { return p.refs }
func (p provTest) VerdictClass() Verdict { return p.class }
func (p provTest) Run(context.Context, Env) Result {
	if p.ran != nil {
		*p.ran = true
	}
	return p.result
}

// provTestMin is provTest with a declared MinRelease.
type provTestMin struct{ provTest }

func (p provTestMin) MinRelease() Release { return p.min }

func goodRef() SpecRef {
	return SpecRef{Spec: "TS 38.413", Clause: "10.3.5", Strength: Shall, Since: 15, Verified: ts38413Verified, WordingUnchanged: true}
}

// The gate: every registered check carries verified provenance that meets
// every rule, and has a provenance file with its quoted normative text.
func TestEveryCheckHasVerifiedProvenance(t *testing.T) {
	tests := NewRegistry().Tests()
	if len(tests) < 4 {
		t.Fatalf("only %d checks registered", len(tests))
	}
	for _, c := range tests {
		t.Run(c.ID(), func(t *testing.T) {
			if err := ValidateTest(c); err != nil {
				t.Error(err)
			}
			path := filepath.Join("..", "..", "docs", "conformance", "provenance", c.ID()+".md")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no provenance file: %v", err)
			}
			for _, r := range c.SpecRefs() {
				if !strings.Contains(string(b), r.Spec+" "+r.Clause) {
					t.Errorf("provenance file does not cover %s %s", r.Spec, r.Clause)
				}
			}
			if !strings.Contains(string(b), "https://www.3gpp.org/ftp/Specs/archive/") {
				t.Error("provenance file cites no 3GPP archive URL")
			}
		})
	}
}

// ValidateTest rejects every way a reference or a check can fall short.
func TestValidateTestRejects(t *testing.T) {
	without := func(rel Release) []SpecVersion {
		var out []SpecVersion
		for _, v := range ts38413Verified {
			if v.Release != rel {
				out = append(out, v)
			}
		}
		return out
	}
	mod := func(f func(r *SpecRef)) []SpecRef { r := goodRef(); f(&r); return []SpecRef{r} }
	tests := []struct {
		name    string
		test    Test
		wantErr string
	}{
		{name: "valid", test: provTest{id: "OK", refs: []SpecRef{goodRef()}, class: Fail}},
		{name: "Since before Rel-15 needs only its own release plus 15..19", test: provTest{id: "OLD", class: Fail,
			refs: []SpecRef{{Spec: "TS 29.281", Clause: "7.3.1", Strength: Shall, Since: 8, Verified: ts29281Verified, WordingUnchanged: true}}}},
		{name: "no refs", test: provTest{id: "X", class: Fail}, wantErr: "no spec references"},
		{name: "missing a release in the range", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Verified = without(17) })}, wantErr: "no verified version for Rel-17"},
		{name: "missing the latest release", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Verified = without(19) })}, wantErr: "no verified version for Rel-19"},
		{name: "missing the Since release", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Since = 14 })}, wantErr: "from the Since release Rel-14"},
		{name: "version not V-form", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) {
			r.Verified = append([]SpecVersion{{Release: 15, Version: "15.0", Archive: "38413-f00.zip"}}, r.Verified...)
		})}, wantErr: "is not V<n>.<n>.<n>"},
		{name: "version from another release", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) {
			r.Verified = append([]SpecVersion{{Release: 16, Version: "V15.0.0", Archive: "38413-f00.zip"}}, r.Verified...)
		})}, wantErr: "belongs to Rel-15"},
		{name: "archive not a 3GPP file", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) {
			r.Verified = append([]SpecVersion{{Release: 15, Version: "V15.0.0", Archive: "https://example.org/x.pdf"}}, r.Verified...)
		})}, wantErr: "archive"},
		{name: "empty spec", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Spec = "" })}, wantErr: "empty Spec"},
		{name: "dagger placeholder", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Clause = "10.3†" })}, wantErr: "exact paragraph"},
		{name: "section sign", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Clause = "§10.3.5" })}, wantErr: "exact paragraph"},
		{name: "unknown strength", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Strength = "must" })}, wantErr: "strength"},
		{name: "neither unchanged nor changed", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.WordingUnchanged = false })}, wantErr: "neither WordingUnchanged"},
		{name: "change outside the range", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Changed = []SpecChange{{Release: 20, Note: "x"}} })}, wantErr: "change entry"},
		{name: "FAIL-class without a shall", test: provTest{id: "X", class: Fail, refs: mod(func(r *SpecRef) { r.Strength = Should })}, wantErr: "must cite at least one shall"},
		{name: "should-only check declared DEVIATION", test: provTest{id: "OK", class: Deviation, refs: mod(func(r *SpecRef) { r.Strength = Should })}},
		{name: "verdict class neither", test: provTest{id: "X", class: Pass, refs: []SpecRef{goodRef()}}, wantErr: "VerdictClass must be"},
		{name: "declared MinRelease beyond Latest", test: provTestMin{provTest{id: "X", class: Fail, refs: []SpecRef{goodRef()}, min: 20}}, wantErr: "MinRelease Rel-20"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTest(tc.test)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateTest: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// One rendering, the form the design names.
func TestSpecRefString(t *testing.T) {
	tests := []struct {
		ref  SpecRef
		want string
	}{
		{goodRef(), "TS 38.413 10.3.5 (Rel-15 onward, wording unchanged through Rel-19)"},
		{SpecRef{Spec: "TS 24.501", Clause: "5.4.1.3.7 item c)", Since: 15, Changed: []SpecChange{{17, "clause renumbered from 5.4.1.3.6"}}},
			"TS 24.501 5.4.1.3.7 item c) (Rel-15 onward; Rel-17: clause renumbered from 5.4.1.3.6)"},
		{SpecRef{Spec: "TS 29.281", Clause: "7.3.1", Since: 8, Changed: []SpecChange{{12, "a"}, {15, "b"}}},
			"TS 29.281 7.3.1 (Rel-8 onward; Rel-12: a; Rel-15: b)"},
	}
	for _, tc := range tests {
		if got := tc.ref.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
	if got := RenderRefs([]SpecRef{tests[0].ref, tests[2].ref}); got != tests[0].want+"; "+tests[2].want {
		t.Errorf("RenderRefs = %q", got)
	}
}

func TestParseRelease(t *testing.T) {
	tests := []struct {
		in      string
		want    Release
		wantErr bool
	}{
		{"", 0, false}, {"unset", 0, false}, {"Rel-15", 15, false}, {"rel15", 15, false}, {"R17", 17, false},
		{"15", 15, false}, {"Rel-20", 20, false}, {" Rel-16 ", 16, false},
		{"Rel-", 0, true}, {"fifteen", 0, true}, {"0", 0, true}, {"Rel-123", 0, true}, {"15.1", 0, true},
	}
	for _, tc := range tests {
		got, err := ParseRelease(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ParseRelease(%q) = %v, %v; want %v, err %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
	b, _ := json.Marshal(struct{ A, B Release }{0, 17})
	if string(b) != `{"A":"unset","B":"Rel-17"}` {
		t.Errorf("JSON = %s", b)
	}
	var back struct{ A, B Release }
	if err := json.Unmarshal(b, &back); err != nil || back.A != 0 || back.B != 17 {
		t.Errorf("JSON round trip = %+v, %v", back, err)
	}
}

// A check that requires a newer release than the target SKIPs without
// running and says why; at or below the target, or with no target, it runs
// and its verdict stands: SKIP can never mask a FAIL the target covers.
func TestRunOneTargetRelease(t *testing.T) {
	ref17 := SpecRef{Spec: "TS 24.501", Clause: "5.4.1.3.7", Strength: Shall, Since: 17, WordingUnchanged: true}
	tests := []struct {
		name        string
		target      Release
		wantRan     bool
		wantVerdict Verdict
		wantDetail  string
	}{
		{name: "older target skips", target: 15, wantRan: false, wantVerdict: Skip, wantDetail: "requires Rel-17 (TS 24.501 5.4.1.3.7); target release is Rel-15"},
		{name: "equal target runs", target: 17, wantRan: true, wantVerdict: Fail},
		{name: "newer target runs", target: 18, wantRan: true, wantVerdict: Fail},
		{name: "unset target runs", target: 0, wantRan: true, wantVerdict: Fail},
		{name: "target beyond verification runs and says so", target: 20, wantRan: true, wantVerdict: Fail, wantDetail: "wording verified through Rel-19"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			c := provTest{id: "NEW-CHECK", refs: []SpecRef{ref17}, class: Fail, ran: &ran,
				result: Result{Verdict: Fail, Observed: "no reject", Detail: "core accepted it"}}
			res := runOne(context.Background(), c, Env{TargetRelease: tc.target}, time.Second)
			if ran != tc.wantRan || res.Verdict != tc.wantVerdict {
				t.Fatalf("ran=%v verdict=%s, want ran=%v verdict=%s", ran, res.Verdict, tc.wantRan, tc.wantVerdict)
			}
			if tc.wantDetail != "" && !strings.Contains(res.Detail, tc.wantDetail) {
				t.Errorf("detail %q lacks %q", res.Detail, tc.wantDetail)
			}
			want := "requires Rel-17 or later; " + ref17.String()
			if !strings.HasSuffix(res.Detail, want) {
				t.Errorf("detail %q does not end with the requirement line %q", res.Detail, want)
			}
			if res.ID != "NEW-CHECK" || res.MinRelease != 17 || res.SpecRef != ref17.String() || len(res.SpecRefs) != 1 {
				t.Errorf("result not stamped: %+v", res)
			}
		})
	}
}

// A Deviation-class check can never report FAIL; a declared MinRelease sets
// the requirement line.
func TestRunOneVerdictClassAndDeclaredMin(t *testing.T) {
	ref8 := SpecRef{Spec: "TS 29.281", Clause: "7.3.1", Strength: Shall, Since: 8, WordingUnchanged: true}
	c := provTestMin{provTest{id: "GTPU-X", refs: []SpecRef{ref8}, class: Deviation, min: 15, result: Result{Verdict: Fail}}}
	res := runOne(context.Background(), c, Env{}, time.Second)
	if res.Verdict != Deviation {
		t.Errorf("verdict = %s, want DEVIATION for a Deviation-class check", res.Verdict)
	}
	if res.MinRelease != 15 || !strings.HasPrefix(res.Detail, "requires Rel-15 or later; TS 29.281 7.3.1 (Rel-8 onward") {
		t.Errorf("MinRelease %s, detail %q", res.MinRelease, res.Detail)
	}
	if res := runOne(context.Background(), c, Env{TargetRelease: 14}, time.Second); res.Verdict != Skip {
		t.Errorf("target Rel-14 below the declared Rel-15: verdict %s, want SKIP", res.Verdict)
	}
}

// The committed checks table is what the registry renders; regenerate with
// go generate ./internal/conformance.
func TestChecksDocIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "conformance", "checks.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != ChecksMarkdown(NewRegistry().Tests()) {
		t.Errorf("%s is stale: run go generate ./internal/conformance", path)
	}
}

// The JSON summary records the target release, "unset" when none was given.
func TestSummaryJSONTargetRelease(t *testing.T) {
	for _, tc := range []struct {
		target Release
		want   string
	}{{0, `"TargetRelease":"unset"`}, {16, `"TargetRelease":"Rel-16"`}} {
		s := Summarize(nil)
		s.TargetRelease = tc.target
		b, err := json.Marshal(s)
		if err != nil || !strings.Contains(string(b), tc.want) {
			t.Errorf("summary JSON %s (%v) lacks %s", b, err, tc.want)
		}
	}
}
