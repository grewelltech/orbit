package target

import (
	"reflect"
	"testing"
)

// SetText says a resolved value as --set text that converts back to it,
// or reports that --set cannot say it.
func TestSetText(t *testing.T) {
	tests := []struct {
		path   string
		v      any
		want   string
		wantOK bool
	}{
		{"run.name", "nightly", "nightly", true},
		{"workloads[w].attach.count", uint64(500), "500", true},
		{"workloads[w].attach.rate", 7.5, "7.5", true},
		{"until.complete", false, "false", true},
		{"workloads[w].conformance.categories", []string{}, "", true},
		{"workloads[w].conformance.categories", []string{"nas"}, "nas", true},
		{"workloads[w].conformance.categories", []string{"nas", "ngap"}, "", false},
		{"workloads[w].conformance.categories", []string{""}, "", false},
		{"workloads[w].behaviors", map[string]any{"latency": map[string]any{"target": "10.0.0.9"}}, "{latency: {target: 10.0.0.9}}", true},
		{"workloads[w].behaviors", map[string]any{}, "{}", true},
		{"core.amf", "x", "", false}, // a target field: not the run file's
	}
	for _, tc := range tests {
		got, ok, err := SetText(tc.path, tc.v)
		if err != nil || ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("SetText(%s, %v) = %q, %t, %v; want %q, %t", tc.path, tc.v, got, ok, err, tc.want, tc.wantOK)
		}
		if !ok || err != nil {
			continue
		}
		_, f, err := lookup(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		back, err := convert(f, got)
		if err != nil || !reflect.DeepEqual(back, tc.v) {
			t.Errorf("%s: %q converts to %#v (%v), want %#v", tc.path, got, back, err, tc.v)
		}
	}
	if _, _, err := SetText("nope.x", "v"); err == nil {
		t.Error("an unknown path: no error")
	}
}

// A whole value's plain dates stay text, in a file and in --set: an inline
// expected-failures list's added is a YYYY-MM-DD string, not a time.
func TestWholeValueDates(t *testing.T) {
	rf, err := ParseRunFile("run", []byte("kind: run\nworkloads:\n  - name: c\n    conformance: {expected: {entries: [{added: 2026-10-28}]}}\n"),
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"entries": []any{map[string]any{"added": "2026-10-28"}}}
	for _, s := range rf.Input.Settings {
		if s.Path == "workloads[c].conformance.expected" && !reflect.DeepEqual(s.Value, want) {
			t.Errorf("file: %#v, want %#v", s.Value, want)
		}
	}
	got, err := convert(field{typ: tAny}, "{entries: [{added: 2026-10-28}]}")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("--set: %#v (%v), want %#v", got, err, want)
	}
}
