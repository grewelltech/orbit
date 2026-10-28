package target

import "testing"

func TestPLMNHint(t *testing.T) {
	tests := []struct {
		name, cause, want string
	}{
		{name: "unknown PLMN", cause: "misc(4)", want: " (unknown PLMN); core.plmn 001/01 or core.tac 1 is not one the AMF serves"},
		{name: "another cause", cause: "misc(0)", want: ""},
		{name: "no cause", cause: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PLMNHint(tc.cause, "001", "01", 1); got != tc.want {
				t.Errorf("PLMNHint(%q) = %q, want %q", tc.cause, got, tc.want)
			}
		})
	}
}
