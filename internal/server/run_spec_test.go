package server

import (
	"strings"
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func TestRedactLoadSpecDropsCredentials(t *testing.T) {
	in := &orbitv1.LoadRunSpec{
		AmfAddress: "10.0.0.1:38412",
		Credentials: &orbitv1.Credentials{
			Ki:  "00112233445566778899aabbccddeeff",
			Opc: "000102030405060708090a0b0c0d0e0f",
		},
	}
	out := redactLoadSpec(in)
	if out.GetCredentials() != nil {
		t.Error("credentials survived into the archive; a report is meant to be shareable")
	}
	if out.GetAmfAddress() != "10.0.0.1:38412" {
		t.Error("redaction dropped more than the credentials")
	}
	// The original must be untouched — it is still driving a live run.
	if in.GetCredentials() == nil {
		t.Error("redaction mutated the caller's spec")
	}
}

func TestRedactSecretsInYAML(t *testing.T) {
	doc := `kind: fleet
name: nightly

credentials:
  ki: 00112233445566778899aabbccddeeff
  opc: 000102030405060708090a0b0c0d0e0f

core:
  amf: 10.0.0.1:38412
loom:
  token: s3cret-bearer
`
	got := redactSecretsInYAML(doc)
	for _, secret := range []string{
		"00112233445566778899aabbccddeeff",
		"000102030405060708090a0b0c0d0e0f",
		"s3cret-bearer",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived redaction:\n%s", secret, got)
		}
	}
	// Everything else must survive verbatim — the document is kept so runs can
	// be compared, which needs it intact.
	for _, keep := range []string{"kind: fleet", "name: nightly", "amf: 10.0.0.1:38412"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q, which is not a secret", keep)
		}
	}
	if strings.Count(got, redacted) != 3 {
		t.Errorf("want 3 redactions, got %d:\n%s", strings.Count(got, redacted), got)
	}
}

func TestRedactSecretsPreservesIndentation(t *testing.T) {
	// YAML is whitespace-significant; a redactor that reflows it produces a
	// document that no longer parses.
	got := redactSecretsInYAML("credentials:\n    ki: abc\n")
	if !strings.Contains(got, "    ki: ") {
		t.Errorf("indentation lost: %q", got)
	}
}

func TestSpecStoreForgetsAfterTake(t *testing.T) {
	// The map would otherwise hold a scenario document per run for the life of
	// the process.
	s := newSpecStore()
	s.put("run-1", &orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{ScenarioYaml: "kind: fleet"}},
	})
	if got := s.take("run-1"); got == nil {
		t.Fatal("spec not stored")
	}
	if got := s.take("run-1"); got != nil {
		t.Error("spec retained after being taken")
	}
	if len(s.specs) != 0 {
		t.Errorf("store still holds %d specs", len(s.specs))
	}
}

func TestSpecStoreIgnoresSpeclessRequest(t *testing.T) {
	s := newSpecStore()
	s.put("run-1", &orbitv1.StartRunRequest{})
	if got := s.take("run-1"); got != nil {
		t.Error("stored a spec for a request that had none")
	}
}
