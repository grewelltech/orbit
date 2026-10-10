package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

// Env.Dial dials from Bind: a malformed one fails in sctp.Dial's
// normalisation, before any socket is opened, naming the input.
func TestEnvDialUsesBind(t *testing.T) {
	_, err := Env{AMFAddr: "127.0.0.1:38412", Bind: "not-an-ip"}.Dial()
	if err == nil || !strings.Contains(err.Error(), `invalid local address "not-an-ip"`) {
		t.Fatalf("err = %v, want the normalisation error for the bind", err)
	}
}

func TestEnvN2Source(t *testing.T) {
	tests := []struct{ bind, want string }{
		{"", ""},
		{"127.0.0.1", "127.0.0.1:0"},
		{"10.0.0.21:38412", "10.0.0.21:38412"},
		{"::1", "[::1]:0"},
		{"not-an-ip", "not-an-ip"}, // unparseable: reported as written
	}
	for _, tc := range tests {
		if got := (Env{Bind: tc.bind}).N2Source(); got != tc.want {
			t.Errorf("Env{Bind: %q}.N2Source() = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

// The JSON summary records the N2 source when one was chosen and omits the
// key when the kernel chose, so older consumers see no new field by default.
func TestSummaryJSONN2Source(t *testing.T) {
	sum := Summarize(nil)
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "N2Source") {
		t.Errorf("empty N2Source serialised: %s", b)
	}
	sum.N2Source = (Env{Bind: "10.0.0.21"}).N2Source()
	b, err = json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["N2Source"] != "10.0.0.21:0" {
		t.Errorf("N2Source in JSON = %v, want 10.0.0.21:0 (got %s)", got["N2Source"], b)
	}
}
