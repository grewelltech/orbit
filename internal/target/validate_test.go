package target

import (
	"strings"
	"testing"
)

// Resolve checks the format of every field that ends up set, and names the
// path, the value (masked when secret), and the layer and source that set
// it. Only the winning value is checked: a bad value a higher layer
// replaced does not fail.
func TestResolveValidatesValues(t *testing.T) {
	const key = "0123456789abcdef0123456789ABCDEF"
	tests := []struct {
		name    string
		path    string
		value   any
		wantErr string // "" = valid
	}{
		{name: "mcc ok", path: "core.plmn.mcc", value: "001"},
		{name: "mcc short", path: "core.plmn.mcc", value: "1", wantErr: `core.plmn.mcc: "1" is not 3 digits (env $SRC)`},
		{name: "mcc letters", path: "core.plmn.mcc", value: "0a1", wantErr: "is not 3 digits"},
		{name: "mnc 2", path: "core.plmn.mnc", value: "01"},
		{name: "mnc 3", path: "core.plmn.mnc", value: "001"},
		{name: "mnc 4", path: "core.plmn.mnc", value: "0001", wantErr: "is not 2 or 3 digits"},
		{name: "supi ok", path: "subscribers[].first_supi", value: "001010000000001"},
		{name: "supi with prefix", path: "subscribers[].first_supi", value: "imsi-001010000000001"},
		{name: "supi letters", path: "subscribers[].first_supi", value: "00101x", wantErr: "is not an IMSI"},
		{name: "ki ok", path: "subscribers[].ki", value: key},
		{name: "ki short, masked", path: "subscribers[].ki", value: "abc", wantErr: "subscribers[default].ki: <redacted> is not 32 hex digits"},
		{name: "opc not hex, masked", path: "subscribers[].opc", value: strings.Repeat("z", 32), wantErr: "<redacted> is not 32 hex digits"},
		{name: "slice ok", path: "core.slices", value: []Slice{{SST: 1, SD: "010203"}}},
		{name: "sst too big", path: "core.slices", value: []Slice{{SST: 256}}, wantErr: "SST 256 is more than 255"},
		{name: "sd not hex", path: "core.slices", value: []Slice{{SST: 1, SD: "01020x"}}, wantErr: `SD "01020x" is not 6 hex digits`},
		{name: "tac ok", path: "core.tac", value: "16777215"},
		{name: "tac 25 bits", path: "core.tac", value: "16777216", wantErr: "more than 24 bits"},
		{name: "amf ok", path: "core.amf", value: "192.0.2.10:38412"},
		{name: "amf v6", path: "core.amf", value: "[2001:db8::1]:38412"},
		{name: "amf no port", path: "core.amf", value: "192.0.2.10", wantErr: `core.amf: "192.0.2.10" is not host:port`},
		{name: "amf bad port", path: "core.amf", value: "192.0.2.10:99999", wantErr: "has no valid port"},
		{name: "responder control", path: "app.responders[].control", value: "nohost", wantErr: "is not host:port"},
		{name: "responder data ip", path: "app.responders[].data_ip", value: "203.0.113.300", wantErr: "is not an IP address"},
		{name: "n2 pool ip, range, cidr", path: "ran.n2_source_ips", value: []any{"192.0.2.50", "192.0.2.50-192.0.2.59", "192.0.2.0/28"}},
		{name: "n2 pool backwards range", path: "ran.n2_source_ips", value: []any{"192.0.2.59-192.0.2.50"}, wantErr: `entry "192.0.2.59-192.0.2.50" is not an address`},
		{name: "n2 pool with a port", path: "ran.n2_source_ips", value: []any{"192.0.2.50:2152"}, wantErr: "is not an address"},
		{name: "n3 ip and ip:port", path: "ran.n3_ips", value: []any{"198.51.100.50", "198.51.100.51:2152"}},
		{name: "n3 garbage", path: "ran.n3_ips", value: []any{"n3-host"}, wantErr: "is not an address"},
		{name: "gnb bits ok", path: "ran.gnb_ids.bits", value: "24"},
		{name: "gnb bits unset", path: "ran.gnb_ids.bits", value: "0"},
		{name: "gnb bits 21", path: "ran.gnb_ids.bits", value: "21", wantErr: "not between 22 and 32"},
		{name: "gnb bits 33", path: "ran.gnb_ids.bits", value: "33", wantErr: "not between 22 and 32"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(Input{Layer: LayerEnv, Settings: []Setting{{Path: tc.path, Value: tc.value, Source: Source{Name: "$SRC"}}}})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "(env $SRC)") {
				t.Errorf("err %v does not name the layer and source", err)
			}
			if IsSecret(tc.path) && strings.Contains(err.Error(), tc.value.(string)) {
				t.Errorf("err %v shows the secret", err)
			}
		})
	}

	// A bad value a higher layer replaced is not checked.
	_, err := Resolve(
		Input{Layer: LayerTarget, Settings: []Setting{set("core.plmn.mcc", "1", "t")}},
		Input{Layer: LayerFlag, Settings: []Setting{set("core.plmn.mcc", "001", "--mcc")}},
	)
	if err != nil {
		t.Errorf("an overridden bad value failed: %v", err)
	}
}

// A gate's name comes from its metric and scope, so a run-file gate and a
// --slo-* flag on the same metric merge into one gate, the flag winning.
func TestGatesMergeByDerivedName(t *testing.T) {
	if GateName("", "slo-reg-p99") != "slo-reg-p99" || GateName("pop", "slo-reg-p99") != "pop/slo-reg-p99" {
		t.Fatalf("GateName = %q, %q", GateName("", "slo-reg-p99"), GateName("pop", "slo-reg-p99"))
	}
	b, ok := FlagBinding("orbit load", "slo-reg-p99")
	if !ok || b.Path != "gates["+GateName("", "slo-reg-p99")+"].slo.threshold" {
		t.Fatalf("binding = %+v", b)
	}
	r, err := Resolve(
		Input{Layer: LayerRunFile, Settings: []Setting{
			set("gates["+GateName("", "slo-reg-p99")+"].slo.threshold", "500ms", "run.yaml"),
			set("gates["+GateName("pop", "slo-reg-p99")+"].slo.threshold", "800ms", "run.yaml"),
		}},
		Input{Layer: LayerFlag, Settings: []Setting{set(b.Path, "300ms", "--slo-reg-p99")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var gates []string
	for p := range r.Run {
		if strings.HasSuffix(p, ".name") && strings.HasPrefix(p, "gates[") {
			gates = append(gates, p)
		}
	}
	if len(gates) != 2 {
		t.Errorf("gates = %v, want the run-wide one (merged) and pop's", gates)
	}
	if got := r.Run["gates[slo-reg-p99].slo.threshold"]; got != "300ms" {
		t.Errorf("run-wide threshold = %v, want the flag's 300ms", got)
	}
	if p := prov(t, r, "gates[slo-reg-p99].slo.threshold"); len(p.Overridden) != 1 || p.Overridden[0].Value != "500ms" {
		t.Errorf("provenance = %+v", p)
	}
	if got := r.Run["gates[pop/slo-reg-p99].slo.threshold"]; got != "800ms" {
		t.Errorf("pop's threshold = %v, want 800ms untouched", got)
	}
}
