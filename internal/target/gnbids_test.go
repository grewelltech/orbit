package target

import (
	"strings"
	"testing"
)

// A gNB ID block must fit its ID length: first to first+count-1 (first
// alone with no count) at most 2^bits-1, bits defaulting to 24. Resolve
// refuses one that does not, for the CLI and the server alike; Report
// records it as a problem.
func TestGNBIDsFitTheirBits(t *testing.T) {
	type ids struct{ first, count, bits uint64 }
	set := func(v ids) Input {
		in := Input{Layer: LayerTarget}
		add := func(p string, n uint64) {
			in.Settings = append(in.Settings, Setting{Path: p, Value: n, Source: Source{Layer: LayerTarget, Name: "lab1.yaml", Line: 20}})
		}
		add("ran.gnb_ids.first", v.first)
		if v.count > 0 {
			add("ran.gnb_ids.count", v.count)
		}
		if v.bits > 0 {
			add("ran.gnb_ids.bits", v.bits)
		}
		return in
	}
	for _, tc := range []struct {
		name string
		ids  ids
		want string // "" when it fits
	}{
		{"22 bits, the whole space", ids{0, 1 << 22, 22}, ""},
		{"22 bits, one over", ids{0, 1<<22 + 1, 22}, "0..4194304 does not fit 22 bits (at most 4194303)"},
		{"22 bits, the last ID", ids{1<<22 - 1, 1, 22}, ""},
		{"22 bits, first past the end", ids{1 << 22, 1, 22}, "does not fit 22 bits"},
		{"the default 24, the last ID", ids{1<<24 - 2, 2, 0}, ""},
		{"the default 24, one over", ids{1<<24 - 2, 3, 0}, "16777214..16777216 does not fit 24 bits"},
		{"first alone, within 24", ids{1<<24 - 1, 0, 0}, ""},
		{"first alone, past 24", ids{1 << 24, 0, 0}, "16777216..16777216 does not fit 24 bits"},
		{"32 bits, the last ID", ids{1<<32 - 1, 1, 32}, ""},
		{"32 bits, one over", ids{1<<32 - 1, 2, 32}, "4294967295..4294967296 does not fit 32 bits"},
	} {
		_, err := Resolve(set(tc.ids))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
		// Report records the same, and still resolves.
		res, err := Report(set(tc.ids))
		if err != nil {
			t.Fatalf("%s: Report: %v", tc.name, err)
		}
		var got string
		for _, p := range res.Problems {
			if p.Path == "ran.gnb_ids" {
				got = p.Err
			}
		}
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: Report problem %q, want %q", tc.name, got, tc.want)
		}
	}
}
