package scenario

import (
	"testing"
)

const sample = `
name: t
core:
  amf: 1.2.3.4:38412
  plmn: {mcc: "208", mnc: "93"}
  slice: {sst: 1, sd: "010203"}
  dnn: internet
credentials:
  ki: ${TEST_KI}
  opc: ffee
gnbs:
  - {id: 1, name: gnb-1, n3: 10.0.0.1}
  - {id: 2, name: gnb-2, n3: 10.0.0.2, bind: 10.0.0.2:0}
ues:
  - {supi: "208930100007500", gnb: gnb-1, pdu_session: true}
  - range: {base: "208930100007501", count: 3}
    gnb: gnb-2
steps:
  - register: all
  - ping: {ue: "208930100007500", dst: 8.8.8.8}
  - handover: {ue: "208930100007500", to: gnb-2, type: xn}
  - wait: 2s
`

func TestParseAndResolve(t *testing.T) {
	t.Setenv("TEST_KI", "deadbeef")
	sc, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Credentials.Ki != "deadbeef" {
		t.Errorf("${ENV} not expanded: %q", sc.Credentials.Ki)
	}

	ues, err := sc.ResolveUEs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ues) != 4 {
		t.Fatalf("want 4 UEs (1 + range of 3), got %d", len(ues))
	}
	if ues[1].SUPI != "208930100007501" || ues[3].SUPI != "208930100007503" {
		t.Errorf("range expansion wrong: %s..%s", ues[1].SUPI, ues[3].SUPI)
	}
	if ues[1].GNB.Name != "gnb-2" {
		t.Errorf("UE not bound to its gNB: %+v", ues[1])
	}

	if len(sc.Steps) != 4 || sc.Steps[0].Action != "register" || sc.Steps[3].Action != "wait" {
		t.Fatalf("steps parsed wrong: %+v", sc.Steps)
	}
	if got := sc.Steps[3].str(); got != "2s" {
		t.Errorf("wait value = %q, want 2s", got)
	}
	var p handoverParams
	if err := sc.Steps[2].decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.To != "gnb-2" || p.Type != "xn" {
		t.Errorf("handover decode = %+v", p)
	}
}

func TestValidateErrors(t *testing.T) {
	if _, err := Parse([]byte(`core: {}`)); err == nil {
		t.Error("want error for missing core.amf")
	}
	if _, err := Parse([]byte("core: {amf: x}\nues:\n  - {supi: \"1\", gnb: nope}\n")); err == nil {
		t.Error("want error for UE referencing an unknown gNB")
	}
	if _, err := Parse([]byte("core: {amf: x}\ngnbs:\n  - {name: g}\n  - {name: g}\n")); err == nil {
		t.Error("want error for duplicate gNB name")
	}
}
