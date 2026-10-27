package server

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/bgrewell/orbit/internal/scenario"
	"github.com/bgrewell/orbit/internal/target"
)

// A fleet scenario runs start-fleet sends with a target applied carries
// the responder's token in its app cohorts and bulk traffic (as an inline
// token always did); the server's YAML redaction masks every copy of it, as
// the scenario struct marshals today.
func TestAppliedScenarioTokenIsRedacted(t *testing.T) {
	const token = "sekrit-responder-token"
	doc := `version: 1
kind: target
name: lab1
core:
  amf: 192.0.2.10:38412
  plmn: { mcc: "001", mnc: "01" }
subscribers:
  - name: main
    first_supi: "001010000000001"
    ki: 00112233445566778899aabbccddeeff
    opc: 000102030405060708090a0b0c0d0e0f
ran:
  n2_source_ips: [192.0.2.50]
app:
  responders:
    - { name: n6, control: 203.0.113.20:9551, token: ` + token + `, data_ip: 203.0.113.21 }
`
	files, err := target.Parse("lab1.yaml", []byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	r, err := target.Resolve(target.DefaultInput(), files[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	fleet := `kind: fleet
topology:
  gnbs: { count: 1 }
fleet: { count: 2, pdu_session: true }
behaviors:
  traffic:
    mix:
      - { app: http, share: 1.0 }
    bulk: { ues: 1, uplink_rate: 1Mbps }
`
	f, err := scenario.ParseFleetWithTarget([]byte(fleet), &r.Target)
	if err != nil {
		t.Fatal(err)
	}
	f.Credentials = scenario.Credentials{}
	b, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), token) != 2 {
		t.Fatalf("the marshalled scenario should carry the token twice (cohort and bulk):\n%s", b)
	}
	if red := redactSecretsInYAML(string(b)); strings.Contains(red, token) {
		t.Errorf("redaction left the token:\n%s", red)
	}
}
