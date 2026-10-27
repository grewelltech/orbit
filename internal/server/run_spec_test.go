package server

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/secrets"
)

// put masks every marked field of the spec it keeps (secrets.Mask), keeps
// the values for the archive scrub, and leaves the caller's spec alone: it
// is still driving a live run.
func TestSpecStoreMasksEveryMarkedField(t *testing.T) {
	const ki, opc = "00112233445566778899aabbccddeeff", "000102030405060708090a0b0c0d0e0f"
	tests := []struct {
		name  string
		req   *orbitv1.StartRunRequest
		creds func(*orbitv1.RunArchive) *orbitv1.Credentials
		want  []string
	}{
		{
			name: "load",
			req: &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Load{Load: &orbitv1.LoadRunSpec{
				AmfAddress: "10.0.0.1:38412", Credentials: &orbitv1.Credentials{Ki: ki, Opc: opc}}}},
			creds: func(a *orbitv1.RunArchive) *orbitv1.Credentials { return a.GetLoadSpec().GetCredentials() },
			want:  []string{opc, ki},
		},
		{
			name: "fleet: request credentials and the scenario's",
			req: &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{
				ScenarioYaml: "kind: fleet\nloom: {token: s3cret-bearer}\n",
				Credentials:  &orbitv1.Credentials{Ki: ki, Opc: opc}}}},
			creds: func(a *orbitv1.RunArchive) *orbitv1.Credentials { return a.GetFleetSpec().GetCredentials() },
			want:  []string{opc, ki, "s3cret-bearer"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpecStore()
			s.put("run-1", tc.req)
			held, ok := s.take("run-1")
			if !ok {
				t.Fatal("spec not stored")
			}
			if c := tc.creds(held.archive); c.GetKi() != redacted || c.GetOpc() != redacted {
				t.Errorf("credentials = %v, want masked", c)
			}
			if found := secrets.Find(held.archive); len(found) != 0 {
				t.Errorf("the held spec still has secrets: %v", found)
			}
			if !reflect.DeepEqual(held.secrets, sortedCopy(tc.want)) {
				t.Errorf("secret values = %q, want %q", held.secrets, sortedCopy(tc.want))
			}
			if secrets.Find(tc.req) == nil {
				t.Error("put masked the caller's spec")
			}
		})
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// redactArchive masks marked fields anywhere in an archive and scrubs the
// request's secret values wherever else they landed: an event, an error, a
// scenario comment.
func TestRedactArchiveScrubsByValue(t *testing.T) {
	const ki = "00112233445566778899aabbccddeeff"
	a := &orbitv1.RunArchive{
		Run:    &orbitv1.Run{RunId: "run-1", Error: "auth failed for key " + ki},
		Events: []*orbitv1.RunEvent{{Message: "ki=" + ki}},
		Spec: &orbitv1.RunArchive_FleetSpec{FleetSpec: &orbitv1.FleetRunSpec{
			ScenarioYaml: "kind: fleet # lab key " + ki + "\n",
			Credentials:  &orbitv1.Credentials{Opc: "000102030405060708090a0b0c0d0e0f"},
		}},
	}
	redactArchive(a, []string{ki})
	b, err := proto.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{ki, "000102030405060708090a0b0c0d0e0f"} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("%q survived in the archive", s)
		}
	}
	if !strings.Contains(a.GetRun().GetError(), "auth failed for key "+redacted) {
		t.Errorf("error = %q", a.GetRun().GetError())
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
	got := secrets.RedactYAML(doc)
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

func TestRedactedScenarioStillParses(t *testing.T) {
	// YAML is whitespace-significant; a redactor that breaks it produces a
	// document that no longer parses. The redactor re-encodes the parsed
	// document, so the result keeps its structure, not its indentation.
	got := secrets.RedactYAML("credentials:\n    ki: abc\n    supi: imsi-001010000000001\n")
	var m struct {
		Credentials map[string]string `yaml:"credentials"`
	}
	if err := yaml.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("redacted document does not parse: %v\n%s", err, got)
	}
	if m.Credentials["ki"] != redacted || m.Credentials["supi"] != "imsi-001010000000001" {
		t.Errorf("redacted document = %+v, want ki masked and supi kept", m.Credentials)
	}
}

func TestSpecStoreForgetsAfterTake(t *testing.T) {
	// The map would otherwise hold a scenario document and keys per run for
	// the life of the process.
	s := newSpecStore()
	s.put("run-1", &orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{ScenarioYaml: "kind: fleet"}},
	})
	if _, ok := s.take("run-1"); !ok {
		t.Fatal("spec not stored")
	}
	if _, ok := s.take("run-1"); ok {
		t.Error("spec retained after being taken")
	}
	if len(s.specs) != 0 {
		t.Errorf("store still holds %d specs", len(s.specs))
	}
}

func TestSpecStoreIgnoresSpeclessRequest(t *testing.T) {
	s := newSpecStore()
	s.put("run-1", &orbitv1.StartRunRequest{})
	if _, ok := s.take("run-1"); ok {
		t.Error("stored a spec for a request that had none")
	}
}
