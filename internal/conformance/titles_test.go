package conformance

import (
	"context"
	"strings"
	"testing"
	"time"
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

// Every built-in check's group, derived from its primary reference.
func TestBuiltinGroups(t *testing.T) {
	want := map[string]string{
		"NGAP-UE-CTX-RELEASE-ON-REQUEST":         "NGAP / UE Context Management Procedures / UE Context Release Request (NG-RAN node initiated)",
		"NAS-DEREG-ACCEPT-NOT-SWITCH-OFF":        "NAS / 5GMM specific procedures / De-registration procedure",
		"NGAP-CTX-RELEASE-AFTER-SWITCH-OFF":      "NGAP / UE Context Management Procedures / UE Context Release (AMF initiated)",
		"GTPU-END-MARKER-ON-PATH-SWITCH":         `TS 23.501 / User Plane Management / Functionality of Sending of "End marker"`,
		"GTPU-UNKNOWN-TEID-ERRIND":               "GTPv1-U / Tunnel Management Messages / Error Indication",
		"GTPU-ECHO-RESPONSE":                     "GTPv1-U / Path Management Messages / Echo Request",
		"N3-DL-QFI-CONTAINER":                    "GTPv1-U / GTP-U Extension Header / PDU Session Container",
		"NGAP-PATH-SWITCH-ACK-TRANSFER":          "NGAP / UE Mobility Management Procedures / Path Switch Request",
		"NGAP-HANDOVER-COMMAND-TRANSFER":         "NGAP / UE Mobility Management Procedures / Handover Preparation",
		"NGAP-HANDOVER-CANCEL-ACK":               "NGAP / UE Mobility Management Procedures / Handover Cancellation",
		"NAS-SERVICE-REQUEST-REACTIVATES":        "NAS / 5GMM connection management procedures / Service request procedure",
		"NAS-SMC-IMEISV-REQUEST":                 "NAS / 5GMM common procedures / Security mode control procedure",
		"NAS-INITIAL-MSG-CONTAINER-WINS":         "NAS / 5GMM common procedures / Security mode control procedure",
		"NAS-INITIAL-MSG-NO-CONTAINER":           "NAS / 5GMM common procedures / Security mode control procedure",
		"NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED":  "NAS / 5GMM common procedures / Security mode control procedure",
		"NAS-REG-REJECTED-NSSAI":                 "NAS / 5GMM specific procedures / Registration procedure",
		"NAS-PDU-UNKNOWN-DNN":                    "NAS / UE-requested 5GSM procedures / UE-requested PDU session establishment procedure",
		"NAS-PDU-RELEASE-UE-REQUESTED":           "NAS / UE-requested 5GSM procedures / UE-requested PDU session release procedure",
		"NAS-PDU-SESSION-ESTABLISHMENT":          "NAS / UE-requested 5GSM procedures / UE-requested PDU session establishment procedure",
		"NAS-INITIAL-REGISTRATION":               "NAS / 5GMM specific procedures / Registration procedure",
		"NAS-REGISTRATION-AFTER-RELEASE":         "NAS / 5GMM specific procedures / Registration procedure",
		"NAS-RETRANSMIT-NEW-COUNT":               "NAS / NAS security / Handling of NAS COUNT and NAS sequence number",
		"NAS-T3550-ACCEPT-RETRANSMIT":            "NAS / 5GMM specific procedures / Registration procedure",
		"NAS-T3560-AUTH-RETRANSMIT":              "NAS / 5GMM common procedures / Primary authentication and key agreement procedure",
		"NGAP-UNKNOWN-UE-SURVIVES":               "NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Handling of AP ID",
		"NGAP-NG-SETUP":                          "NGAP / Interface Management Procedures / NG Setup",
		"NGAP-DUPLICATE-RAN-NODE-ID":             "NGAP / Interface Management Procedures / NG Setup",
		"NGAP-NGSETUP-MISSING-TALIST":            "NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Missing IE or IE group",
		"NGAP-NGSETUP-UNKNOWN-PLMN":              "NGAP / Interface Management Procedures / NG Setup",
		"NGAP-PAGING-ON-DL-DATA":                 "NAS / 5GMM connection management procedures / Paging procedure",
		"NGAP-PATH-SWITCH-SECCAP-MISMATCH":       "TS 33.501 / Procedures for AS algorithm selection / Xn-handover",
		"NGAP-RAN-CONFIG-UPDATE-ACK":             "NGAP / Interface Management Procedures / RAN Configuration Update",
		"NGAP-RAN-CONFIG-UPDATE-KEEPS-TA":        "NGAP / Interface Management Procedures / RAN Configuration Update",
		"NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX": "NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Transfer Syntax Error",
		"NGAP-NG-RESET-ACK":                      "NGAP / Interface Management Procedures / NG Reset",
		"NAS-SMC-NO-NIA0-REPLAYED-CAPS":          "TS 33.501 / Requirements on the AMF / Signalling data integrity",
		"NAS-INTEGRITY-FAIL-DISCARD":             "NAS / NAS security / Integrity protection of NAS signalling messages",
		"NAS-REPLAY-DISCARD":                     "TS 33.501 / NAS security mechanisms / NAS integrity activation",
		"NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL":     "NAS / NAS security / Handling of NAS COUNT and NAS sequence number",
		"NAS-BADMAC-REGISTRATION-AFTER-SMC":      "NAS / NAS security / Integrity protection of NAS signalling messages",
	}
	tests := NewRegistry().Tests()
	if len(tests) != len(want) {
		t.Errorf("%d checks, %d groups listed here", len(tests), len(want))
	}
	for _, c := range tests {
		for _, rel := range []Release{0, 15, 16, 17, 18, 19, 20} {
			g, err := GroupOf(c, rel)
			if err != nil {
				t.Errorf("%s at %s: %v", c.ID(), rel, err)
				continue
			}
			if g.String() != want[c.ID()] {
				t.Errorf("%s at %s: group %q, want %q", c.ID(), rel, g, want[c.ID()])
			}
		}
	}
}

// The table holds exactly what the checks' groups need: each clause is a
// built-in check's cited primary clause, or the procedure or family a
// group is read from.
func TestClauseTitlesHoldNothingUnused(t *testing.T) {
	used := map[string]bool{}
	for _, c := range NewRegistry().Tests() {
		r, _ := PrimaryRef(c)
		s, _ := specTitlesOf(r.Spec)
		num := clauseNumber(r.Clause)
		used[r.Spec+" "+num] = true
		_, leaf, _ := s.nearest(num, levelLeaf, LatestRelease)
		_, family, _ := s.nearest(parentClause(leaf), levelFamily, LatestRelease)
		used[r.Spec+" "+leaf], used[r.Spec+" "+family] = true, true
	}
	for _, s := range clauseTitles {
		for _, c := range s.Clauses {
			if !used[s.Spec+" "+c.Clause] {
				t.Errorf("%s %s is in the table but no check's group uses it", s.Spec, c.Clause)
			}
		}
	}
}

// A group is read in the run's target release, and in the latest release
// the reference was read in when the cited clause does not exist in the
// target, the target is unset, or the reference was not read in it.
func TestGroupOfRelease(t *testing.T) {
	ref := func(spec, clause string, rels ...Release) SpecRef {
		r := SpecRef{Spec: spec, Clause: clause, Strength: Shall, Since: 15, WordingUnchanged: true, Primary: true}
		for _, rel := range rels {
			r.Verified = append(r.Verified, SpecVersion{Release: rel})
		}
		return r
	}
	table := []specTitles{{Spec: "TS 24.501", Label: "NAS", Clauses: []clauseTitle{
		{"6.4", levelFamily, []titleSpan{{15, 19, "UE-requested 5GSM procedures"}}},
		{"6.4.1", levelLeaf, []titleSpan{{15, 16, "Old wording"}, {17, 19, "New wording"}}},
		{"6.4.1.4.1", levelClause, []titleSpan{{16, 19, "General"}}},
	}}}
	cases := []struct {
		name    string
		ref     SpecRef
		rel     Release
		want    string
		wantErr string
	}{
		{name: "the target's title", ref: ref("TS 24.501", "6.4.1.4.1", 15, 16, 17, 18, 19), rel: 16, want: "NAS / UE-requested 5GSM procedures / Old wording"},
		{name: "unset: the latest", ref: ref("TS 24.501", "6.4.1.4.1", 15, 16, 17, 18, 19), rel: 0, want: "NAS / UE-requested 5GSM procedures / New wording"},
		{name: "clause absent in the target", ref: ref("TS 24.501", "6.4.1.4.1 (6.4.1.4 in Rel-15)", 15, 16, 17, 18, 19), rel: 15, want: "NAS / UE-requested 5GSM procedures / New wording"},
		{name: "a target newer than any read", ref: ref("TS 24.501", "6.4.1.4.1", 15, 16, 17, 18, 19), rel: 20, want: "NAS / UE-requested 5GSM procedures / New wording"},
		{name: "a target the reference was not read in", ref: ref("TS 24.501", "6.4.1.4.1", 15, 16), rel: 18, want: "NAS / UE-requested 5GSM procedures / Old wording"},
		{name: "no entry", ref: ref("TS 24.501", "6.4.2", 19), wantErr: "clause 6.4.2 has no entry"},
		{name: "no spec", ref: ref("TS 29.244", "7.5", 19), wantErr: "TS 29.244 has no part"},
		{name: "no release read", ref: ref("TS 24.501", "6.4.1.4.1", 15), wantErr: "has no title in a release the reference was read in"},
		{name: "no leaf above", ref: ref("TS 24.501", "6.4", 19), wantErr: "no procedure or titled clause at or above 6.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := groupOfRef(table, tc.ref, tc.rel)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case g.String() != tc.want:
				t.Errorf("group %q, want %q", g, tc.want)
			}
		})
	}
	if _, err := GroupOf(provTest{id: "X", refs: []SpecRef{{Spec: "TS 24.501", Clause: "6.4.1"}}}, 0); err == nil || !strings.Contains(err.Error(), "no reference is marked primary") {
		t.Errorf("no primary: err = %v", err)
	}
}

func TestGroupOfNoFamily(t *testing.T) {
	table := []specTitles{{Spec: "TS 24.501", Label: "NAS", Clauses: []clauseTitle{{"6.4.1", levelLeaf, allReleases("Leaf")}}}}
	ref := SpecRef{Spec: "TS 24.501", Clause: "6.4.1", Primary: true, Verified: []SpecVersion{{Release: 19}}}
	if _, err := groupOfRef(table, ref, 0); err == nil || !strings.Contains(err.Error(), "no procedure family above 6.4.1") {
		t.Errorf("err = %v", err)
	}
}

// A run stamps each result with its check's group; a check without a
// primary reference has none.
func TestRunStampsGroup(t *testing.T) {
	r := &Registry{}
	r.Register(provTest{id: "GROUPED", refs: []SpecRef{goodRef()}, result: Result{Verdict: Pass}})
	r.Register(fakeTest{"LOOSE", Procedural, Pass, ""})
	got := r.Run(context.Background(), Env{TargetRelease: 17}, time.Second)
	if len(got) != 2 {
		t.Fatalf("%d results", len(got))
	}
	if want := "NGAP / Handling of Unknown, Unforeseen and Erroneous Protocol Data / Missing IE or IE group"; got[0].Group != want {
		t.Errorf("group %q, want %q", got[0].Group, want)
	}
	if got[1].Group != "" {
		t.Errorf("a check without a primary reference has group %q", got[1].Group)
	}
}
