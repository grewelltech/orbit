package target

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	sentKi  = "5122250214c33e723a5dd523fc145fc0"
	sentOPc = "981d464c7c52eb6e5036234984ad0bcf"
)

// sentTarget is a target with every field set, as a dashboard sends one.
func sentTarget() Target {
	return Target{Version: Version, Kind: Kind, Name: "lab1",
		Core: Core{AMF: "10.0.0.1:38412", PLMN: PLMN{MCC: "001", MNC: "01"}, TAC: 7, Slices: []Slice{{SST: 2, SD: "010203"}},
			DNN: "ims", Profile: "sd-core", UPFN3: "192.0.2.30:2152", UEPools: []string{"10.250.0.0/16"},
			UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24"},
		Subscribers: []Pool{{Name: "main", FirstSUPI: "001010000000001", Count: 3, Ki: sentKi, OPc: sentOPc, RoutingIndicator: "0001"}},
		RAN: RAN{GNBName: "g", N2SourceIPs: []string{"192.0.2.20"}, N3IPs: []string{"192.0.2.21"},
			GNBIDs: GNBIDs{First: 100, Count: 4, Bits: 24}},
		App: App{Responders: []Responder{{Name: "n6", Control: "203.0.113.20:9551", Token: "tok", DataIP: "203.0.113.20"}}}}
}

// Every target field the document can set has a setting in SettingsOf:
// a field added to the schema and not here fails, rather than being
// dropped from what the server checks and uses.
func TestSettingsOfCoversEveryField(t *testing.T) {
	got := map[string]bool{}
	for _, s := range SettingsOf(sentTarget(), SentSource) {
		segs, err := parsePath(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		got[pattern(segs)] = true
	}
	var missing []string
	for p, f := range fields {
		if !f.target || p == "version" || p == "kind" || p == "ran.node" || strings.HasPrefix(p, "core.slices[]") {
			continue
		}
		if !got[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("SettingsOf sets no %v (or sentTarget leaves them empty)", missing)
	}
}

// A sent target resolves as a file with the same fields does: what it
// leaves unset (a zero value) comes from the defaults, and the CLI's
// checks refuse a bad field by its path, in the CLI's words, the value
// masked when it is a key. Nothing is expanded.
func TestResolveSent(t *testing.T) {
	tests := []struct {
		name     string
		change   func(*Target)
		wantPath string // "" = resolves
		want     string // in the reason
		check    func(t *testing.T, r *Resolved)
	}{
		{name: "every field set: as sent", check: func(t *testing.T, r *Resolved) {
			if !reflect.DeepEqual(r.Target, sentTarget()) {
				t.Errorf("got %+v\nwant %+v", r.Target, sentTarget())
			}
		}},
		{name: "unset fields take the defaults", change: func(x *Target) {
			x.Core.TAC, x.Core.Slices, x.Core.DNN, x.Core.Profile = 0, nil, "", ""
		}, check: func(t *testing.T, r *Resolved) {
			c := r.Target.Core
			if c.TAC != 1 || len(c.Slices) != 1 || c.Slices[0] != (Slice{SST: 1}) || c.DNN != "internet" || c.Profile != "strict-3gpp" {
				t.Errorf("core = %+v, want tac 1, slices [1], dnn internet, profile strict-3gpp", c)
			}
		}},
		// The wire cannot carry an explicit 0: tac 0, which a file may
		// write, arrives as unset and takes the default.
		{name: "tac 0 is unset: the default 1", change: func(x *Target) { x.Core.TAC = 0 }, check: func(t *testing.T, r *Resolved) {
			if r.Target.Core.TAC != 1 {
				t.Errorf("tac = %d, want 1", r.Target.Core.TAC)
			}
		}},
		{name: "a bad SD", change: func(x *Target) { x.Core.Slices[0].SD = "xyz" }, wantPath: "core.slices", want: `SD "xyz" is not 6 hex digits`},
		{name: "a bad MCC", change: func(x *Target) { x.Core.PLMN.MCC = "1" }, wantPath: "core.plmn.mcc", want: `"1" is not 3 digits`},
		{name: "a bad pool", change: func(x *Target) { x.Core.UEPools = []string{"10.250.0.0"} }, wantPath: "core.ue_pools", want: `"10.250.0.0"`},
		{name: "a key, masked", change: func(x *Target) { x.Subscribers[0].Ki = "${ORBIT_KI}" }, wantPath: "subscribers[main].ki", want: "<redacted> is not 32 hex digits"},
		{name: "a SUPI outside the PLMN", change: func(x *Target) { x.Subscribers[0].FirstSUPI = "208930000000001" }, wantPath: "subscribers[main].first_supi", want: "is not in PLMN 001/01"},
		{name: "a bad control", change: func(x *Target) { x.App.Responders[0].Control = "nowhere" }, wantPath: "app.responders[n6].control"},
		{name: "gnb_ids bits", change: func(x *Target) { x.RAN.GNBIDs.Bits = 40 }, wantPath: "ran.gnb_ids.bits", want: "not between 22 and 32"},
		{name: "no name", change: func(x *Target) { x.Name = "" }, wantPath: "name", want: "is required"},
		{name: "pools with one name", change: func(x *Target) { x.Subscribers = append(x.Subscribers, x.Subscribers[0]) }, wantPath: "subscribers[main].name", want: `"main" twice`},
		{name: "a pool without a name", change: func(x *Target) { x.Subscribers[0].Name = "" }, wantPath: "subscribers[0].name", want: "is required"},
		{name: "a responder name with a dot", change: func(x *Target) { x.App.Responders[0].Name = "n.6" }, wantPath: "app.responders[0].name"},
		{name: "ran.node", change: func(x *Target) { x.RAN.Node = "edge1" }, wantPath: "ran.node", want: "reserved"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tg := sentTarget()
			if tc.change != nil {
				tc.change(&tg)
			}
			r, err := ResolveSent(tg)
			if tc.wantPath == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.check != nil {
					tc.check(t, r)
				}
				return
			}
			var fe *FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want a FieldError on %s", err, tc.wantPath)
			}
			if fe.Path != tc.wantPath || !strings.Contains(fe.Reason, tc.want) {
				t.Errorf("FieldError %q / %q, want %s / …%s…", fe.Path, fe.Reason, tc.wantPath, tc.want)
			}
			// The form the dashboard reads: "<path>: <reason>", no source.
			if fe.Error() != fe.Path+": "+fe.Reason || strings.Contains(fe.Reason, SentSource.String()) {
				t.Errorf("Error() = %q", fe.Error())
			}
			if strings.Contains(err.Error(), sentKi) || strings.Contains(err.Error(), sentOPc) {
				t.Errorf("a key in the error: %v", err)
			}
		})
	}
}
