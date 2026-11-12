package conformance

import (
	"strings"
	"testing"
)

func TestMatchGroup(t *testing.T) {
	g := Group{Spec: "NAS", Family: "5GMM common procedures", Procedure: "Security mode control procedure"}
	cases := []struct {
		prefix string
		want   bool
	}{
		{"NAS", true},
		{"nas", true},
		{"TS 24.501", true},
		{"ts 24.501 / 5gmm common procedures", true},
		{"NAS/5GMM common procedures", true},
		{" NAS /  5GMM common procedures / ", true},
		{"NAS / 5GMM common procedures / Security mode control procedure", true},
		{"NA", false},
		{"NAS / 5GMM common", false},
		{"NAS / 5GMM specific procedures", false},
		{"NAS / 5GMM common procedures / Security mode control procedure / more", false},
		{"", false},
		{"/", false},
		{"5GMM common procedures", false},
	}
	for _, tc := range cases {
		if got := MatchGroup(g, "TS 24.501", tc.prefix); got != tc.want {
			t.Errorf("MatchGroup(%q) = %v, want %v", tc.prefix, got, tc.want)
		}
	}
}

func TestRegistryGroups(t *testing.T) {
	cases := []struct {
		name     string
		prefixes []string
		wantIDs  []string
		wantErr  string
	}{
		{name: "none: every check", wantIDs: nil},
		{name: "a specification", prefixes: []string{"GTPv1-U"}, wantIDs: []string{"GTPU-UNKNOWN-TEID-ERRIND", "GTPU-ECHO-RESPONSE", "N3-DL-QFI-CONTAINER"}},
		{name: "a specification by number", prefixes: []string{"TS 29.281"}, wantIDs: []string{"GTPU-UNKNOWN-TEID-ERRIND", "GTPU-ECHO-RESPONSE", "N3-DL-QFI-CONTAINER"}},
		{name: "a procedure", prefixes: []string{"ngap/interface management procedures/ng reset"}, wantIDs: []string{"NGAP-NG-RESET-ACK"}},
		{name: "two groups", prefixes: []string{"NGAP / Interface Management Procedures / NG Reset", "GTPv1-U / Path Management Messages"},
			wantIDs: []string{"GTPU-ECHO-RESPONSE", "NGAP-NG-RESET-ACK"}},
		{name: "an unknown group", prefixes: []string{"NGAP", "PFCP"}, wantErr: `no check is in group "PFCP"`},
		{name: "an empty group", prefixes: []string{" / "}, wantErr: `empty group " / "`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			err := r.Groups(0, tc.prefixes...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if len(r.Select()) != len(r.Tests()) {
					t.Error("a refused selection narrowed the registry")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sel := r.Select()
			if tc.wantIDs == nil {
				if len(sel) != len(r.Tests()) {
					t.Errorf("%d selected, want all %d", len(sel), len(r.Tests()))
				}
				return
			}
			var got []string
			for _, c := range sel {
				got = append(got, c.ID())
			}
			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("selected %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

// A selection narrows by every kind it gives at once.
func TestSelectionApply(t *testing.T) {
	r := NewRegistry()
	if err := (Selection{Checks: []string{"NGAP-NG-RESET-ACK", "GTPU-ECHO-RESPONSE"}, Groups: []string{"NGAP"}}).Apply(r); err != nil {
		t.Fatal(err)
	}
	if sel := r.Select(); len(sel) != 1 || sel[0].ID() != "NGAP-NG-RESET-ACK" {
		t.Errorf("selected %v", sel)
	}
	if err := (Selection{Checks: []string{"NO-SUCH"}}).Apply(NewRegistry()); err == nil {
		t.Error("an unknown check was accepted")
	}
	if err := (Selection{Groups: []string{"NO-SUCH"}}).Apply(NewRegistry()); err == nil {
		t.Error("an unknown group was accepted")
	}
}

func TestGroupPaths(t *testing.T) {
	paths := GroupPaths(NewRegistry().Tests(), 0)
	if len(paths) < 10 || paths[0] > paths[len(paths)-1] {
		t.Fatalf("paths %v", paths)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Errorf("%q twice", p)
		}
		seen[p] = true
	}
}

// A category is refused with what replaced it, never taken as an alias.
func TestCategoriesRemoved(t *testing.T) {
	if err := CategoriesRemoved(nil, 0); err != nil {
		t.Errorf("no categories: %v", err)
	}
	err := CategoriesRemoved([]string{"gtpu", "timing", "bogus"}, 18)
	if err == nil {
		t.Fatal("categories accepted")
	}
	for _, want := range []string{
		"categories were replaced by groups (ADR-0011)", "--group", "--check",
		`"gtpu" is now the checks GTPU-UNKNOWN-TEID-ERRIND,GTPU-ECHO-RESPONSE,N3-DL-QFI-CONTAINER, in the groups "GTPv1-U / GTP-U Extension Header / PDU Session Container", "GTPv1-U / Path Management Messages / Echo Request", "GTPv1-U / Tunnel Management Messages / Error Indication"`,
		`"timing" held no checks`, `"bogus" was never a category`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	// Every check a category held is still registered.
	known := map[string]bool{}
	for _, c := range NewRegistry().Tests() {
		known[c.ID()] = true
	}
	for cat, ids := range legacyCategories {
		for _, id := range ids {
			if !known[id] {
				t.Errorf("category %s names %s, which is not registered", cat, id)
			}
		}
	}
}
