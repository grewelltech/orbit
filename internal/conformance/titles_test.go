package conformance

import (
	"strings"
	"testing"
)

func TestClauseTitleTableIsValid(t *testing.T) {
	if err := validateTitles(clauseTitles); err != nil {
		t.Fatal(err)
	}
}

func TestValidateTitlesRefuses(t *testing.T) {
	read := []SpecVersion{{Release: 18, Version: "V18.11.0", Archive: "38413-ib0.zip"}, {Release: 19, Version: "V19.4.0", Archive: "38413-j40.zip"}}
	spec := func(cs ...clauseTitle) []specTitles {
		return []specTitles{{Spec: "TS 38.413", Label: "NGAP", Read: read, Clauses: cs}}
	}
	cases := []struct {
		name    string
		table   []specTitles
		wantErr string
	}{
		{name: "valid", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{18, 19, "Interface Management Procedures"}}})},
		{name: "a release not read", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{17, 19, "X"}}}), wantErr: "Rel-17 is not in the read list"},
		{name: "overlapping spans", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{18, 19, "X"}, {19, 19, "Y"}}}), wantErr: "two titles for Rel-19"},
		{name: "a reversed span", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{19, 18, "X"}}}), wantErr: "reversed"},
		{name: "a slash in a title", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{18, 19, "A/B"}}}), wantErr: "holds a /"},
		{name: "an empty title", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{18, 19, " "}}}), wantErr: "empty"},
		{name: "no title", table: spec(clauseTitle{"8.7", levelFamily, nil}), wantErr: "no title"},
		{name: "a clause twice", table: spec(clauseTitle{"8.7", levelFamily, []titleSpan{{18, 19, "X"}}}, clauseTitle{"8.7", levelFamily, []titleSpan{{18, 19, "X"}}}), wantErr: "listed twice"},
		{name: "a bad archive", table: []specTitles{{Spec: "TS 38.413", Label: "NGAP", Read: []SpecVersion{{Release: 18, Archive: "x.zip"}}}}, wantErr: "archive file"},
		{name: "an empty label", table: []specTitles{{Spec: "TS 38.413", Read: read}}, wantErr: "label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTitles(tc.table)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Every built-in check's primary reference is in the table, so a group can
// be derived for each.
func TestClauseTitlesCoverEveryPrimaryReference(t *testing.T) {
	for _, c := range NewRegistry().Tests() {
		for _, r := range c.SpecRefs() {
			if !r.Primary {
				continue
			}
			s, ok := specTitlesOf(r.Spec)
			if !ok {
				t.Errorf("%s: %s has no part in the clause-title table", c.ID(), r.Spec)
				continue
			}
			if _, ok := s.clause(clauseNumber(r.Clause)); !ok {
				t.Errorf("%s: %s %s has no entry in the clause-title table", c.ID(), r.Spec, clauseNumber(r.Clause))
			}
		}
	}
}

func TestClauseTitleLookups(t *testing.T) {
	cases := []struct {
		clause, wantNum, wantParent string
	}{
		{"5.6.1.4.1 (5.6.1.4 in Rel-15) Uplink data status a)", "5.6.1.4.1", "5.6.1.4"},
		{"10.3.5 Reject IE bullet 1", "10.3.5", "10.3"},
		{"10", "10", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		num := clauseNumber(tc.clause)
		if num != tc.wantNum || parentClause(num) != tc.wantParent {
			t.Errorf("%q: number %q parent %q, want %q %q", tc.clause, num, parentClause(num), tc.wantNum, tc.wantParent)
		}
	}
	nas, _ := specTitlesOf("TS 24.501")
	c, ok := nas.clause("5.6.1.4.1")
	if !ok {
		t.Fatal("5.6.1.4.1 missing")
	}
	if _, ok := c.in(15); ok {
		t.Error("5.6.1.4.1 has a Rel-15 title; the clause has no subclauses in Rel-15")
	}
	if got, _ := c.in(18); got != "UE is not using 5GS services with control plane CIoT 5GS optimization" {
		t.Errorf("Rel-18 title %q", got)
	}
	if _, ok := specTitlesOf("TS 00.000"); ok {
		t.Error("an unknown specification has a part")
	}
}
