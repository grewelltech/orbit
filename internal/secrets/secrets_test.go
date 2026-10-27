package secrets

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// Every field in the API named like a secret (ki, opc, token, k, password,
// secret, *_token, …) carries the secret option, or secret_yaml for a YAML
// document: a new secret field cannot ship unmarked and so unguarded.
func TestEverySecretNamedFieldIsMarked(t *testing.T) {
	n := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("orbit.v1", func(fd protoreflect.FileDescriptor) bool {
		var visit func(msgs protoreflect.MessageDescriptors)
		visit = func(msgs protoreflect.MessageDescriptors) {
			for i := 0; i < msgs.Len(); i++ {
				md := msgs.Get(i)
				if md.IsMapEntry() {
					continue // a map's synthetic entry: its "key" is the map key
				}
				for j := 0; j < md.Fields().Len(); j++ {
					f := md.Fields().Get(j)
					n++
					if IsSecretName(string(f.Name())) && !Marked(f) && !MarkedYAML(f) {
						t.Errorf("%s is named like a secret but not marked [(orbit.v1.secret) = true]", f.FullName())
					}
				}
				visit(md.Messages())
			}
		}
		visit(fd.Messages())
		return true
	})
	if n < 100 {
		t.Fatalf("walked only %d fields; the registry walk is broken", n)
	}
}

// Find reports each non-empty marked field at any depth, and each secret
// in a marked YAML document; an empty one is no secret.
func TestFind(t *testing.T) {
	tests := []struct {
		name string
		m    interface{ ProtoReflect() protoreflect.Message }
		want []string
	}{
		{name: "register with keys", m: &orbitv1.RegisterRequest{Supi: "x", Credentials: &orbitv1.Credentials{Ki: "k", Opc: "o"}},
			want: []string{"credentials.ki", "credentials.opc"}},
		{name: "register, empty keys", m: &orbitv1.RegisterRequest{Supi: "x", Credentials: &orbitv1.Credentials{}}},
		{name: "a run's load spec", m: &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Load{Load: &orbitv1.LoadRunSpec{Credentials: &orbitv1.Credentials{Ki: "k"}}}},
			want: []string{"load.credentials.ki"}},
		{name: "a responder token", m: &orbitv1.StartAppRequest{Supi: "x", Token: "t"}, want: []string{"token"}},
		{name: "keys in a fleet scenario", m: &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{
			ScenarioYaml: "kind: fleet\ncredentials: { ki: 00112233445566778899aabbccddeeff, opc: x }\n"}}},
			want: []string{"fleet.scenario_yaml: credentials.ki", "fleet.scenario_yaml: credentials.opc"}},
		{name: "a fleet scenario with placeholders only", m: &orbitv1.FleetRunSpec{ScenarioYaml: "credentials:\n  ki: ${ORBIT_KI}\n"}},
		{name: "nothing", m: &orbitv1.ListRequest{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Find(tc.m)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Find = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestYAMLSecrets(t *testing.T) {
	tests := []struct {
		name, doc string
		want      []string
	}{
		{name: "block", doc: "credentials:\n  ki: abc\n  opc: def\n", want: []string{"credentials.ki", "credentials.opc"}},
		{name: "flow", doc: "credentials: { ki: abc, opc: def }\n", want: []string{"credentials.ki", "credentials.opc"}},
		{name: "a list item's first key", doc: "mix:\n  - token: abc\n    app: http\n", want: []string{"mix.token"}},
		{name: "a *_token key", doc: "loom_token: abc\n", want: []string{"loom_token"}},
		{name: "placeholders and empties", doc: "ki: ${ORBIT_KI}\nopc: \"\"\ntoken:\n"},
		{name: "a fleet's echo target is no secret", doc: "behaviors:\n  latency: { target: 192.0.2.30 }\n"},
		{name: "unparsable falls back to the pattern", doc: "credentials: { ki: abc\n  token: x: y: [\n", want: []string{"ki", "token"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := YAMLSecrets(tc.doc)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("YAMLSecrets = %q, want %q", got, tc.want)
			}
		})
	}
}

// RedactYAML masks secret values in block, list-item and flow style, and
// leaves other text alone.
func TestRedactYAML(t *testing.T) {
	doc := "credentials:\n  ki: aaaa\n  opc: bbbb\nflow: { ki: cccc, opc: dddd }\nmix:\n  - token: eeee\n    app: http\nloom_token: ffff\ntrack: keepme\nname: keepme\n"
	got := RedactYAML(doc)
	for _, s := range []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff"} {
		if strings.Contains(got, s) {
			t.Errorf("%q survived redaction:\n%s", s, got)
		}
	}
	if strings.Count(got, "keepme") != 2 || strings.Count(got, Masked) != 6 {
		t.Errorf("redaction over- or under-masked:\n%s", got)
	}
}
