package secrets

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"gopkg.in/yaml.v3"

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
					// A YAML document in a request can embed keys; the walk
					// scans it only when it is marked.
					if strings.HasSuffix(string(f.Name()), "_yaml") && !MarkedYAML(f) {
						t.Errorf("%s holds a YAML document but is not marked [(orbit.v1.secret_yaml) = true]", f.FullName())
					}
					// A map's entries cannot carry the option: the walk treats
					// a secret-named key as the mark, which it can only do
					// for map<string, string> (or a map of messages, walked).
					// Numeric values hold no secret.
					if f.IsMap() && (f.MapValue().Kind() == protoreflect.BytesKind ||
						(f.MapValue().Kind() == protoreflect.StringKind && f.MapKey().Kind() != protoreflect.StringKind)) {
						t.Errorf("%s is a map the secrets walk cannot check; use map<string, string> or a message value", f.FullName())
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
		{name: "a run's load spec", m: &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Load{Load: &orbitv1.LoadRunSpec{Credentials: &orbitv1.Credentials{Ki: "k"}}}}, //nolint:staticcheck // the deprecated arm, kept for API clients
			want: []string{"load.credentials.ki"}},
		{name: "a responder token", m: &orbitv1.StartAppRequest{Supi: "x", Token: "t"}, want: []string{"token"}},
		{name: "keys in a fleet scenario", m: &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{ //nolint:staticcheck // the deprecated arm, kept for API clients
			ScenarioYaml: "kind: fleet\ncredentials: { ki: 00112233445566778899aabbccddeeff, opc: x }\n"}}},
			want: []string{"fleet.scenario_yaml: credentials.ki", "fleet.scenario_yaml: credentials.opc"}},
		{name: "a fleet scenario with placeholders only", m: &orbitv1.FleetRunSpec{ScenarioYaml: "credentials:\n  ki: ${ORBIT_KI}\n"}}, //nolint:staticcheck // the deprecated arm, kept for API clients
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

// shapes are YAML documents holding the secret KEYVAL under a secret key,
// in each style the reviewer found the text redactor missing.
var shapes = []struct {
	name, doc string
	want      []string
}{
	{name: "block", doc: "credentials:\n  ki: KEYVAL\n  opc: KEYVAL2\n", want: []string{"credentials.ki", "credentials.opc"}},
	{name: "flow map", doc: "credentials: { ki: KEYVAL, opc: KEYVAL2 }\n", want: []string{"credentials.ki", "credentials.opc"}},
	{name: "flow list of maps", doc: "mix: [{ token: KEYVAL, app: http }]\n", want: []string{"mix.token"}},
	{name: "a list item's first key", doc: "mix:\n  - token: KEYVAL\n    app: http\n", want: []string{"mix.token"}},
	{name: "a *_token key", doc: "loom_token: KEYVAL\n", want: []string{"loom_token"}},
	{name: "literal block scalar", doc: "credentials:\n  ki: |\n    KEYVAL\n  supi: imsi-001\n", want: []string{"credentials.ki"}},
	{name: "folded block scalar", doc: "credentials:\n  ki: >-\n    KEYVAL\n    KEYVAL2\n", want: []string{"credentials.ki"}},
	{name: "double quoted with a comma and braces", doc: "loom:\n  token: \"KEYVAL,{x}]KEYVAL2\"\n", want: []string{"loom.token"}},
	{name: "single quoted with a comma", doc: "password: 'KEYVAL, KEYVAL2'\n", want: []string{"password"}},
	{name: "quoted in a flow map", doc: "credentials: {ki: \"KEYVAL,}\", opc: 'KEYVAL2'}\n", want: []string{"credentials.ki", "credentials.opc"}},
	{name: "an alias to an anchored key", doc: "shared: &k KEYVAL\ncredentials: {ki: *k}\n", want: []string{"credentials.ki"}},
	{name: "an anchored secret, aliased elsewhere", doc: "credentials:\n  ki: &k KEYVAL\nother: *k\n", want: []string{"credentials.ki"}},
	{name: "a merge key", doc: "base: &c {ki: KEYVAL}\ncredentials:\n  <<: *c\n", want: []string{"base.ki"}},
	{name: "a list under a secret key", doc: "token: [KEYVAL, KEYVAL2]\n", want: []string{"token"}},
	{name: "a second document", doc: "kind: fleet\n---\ncredentials:\n  ki: KEYVAL\n", want: []string{"credentials.ki"}},
}

func TestYAMLSecrets(t *testing.T) {
	tests := append([]struct {
		name, doc string
		want      []string
	}{
		{name: "placeholders, empties and nulls", doc: "ki: ${ORBIT_KI}\nopc: \"${ORBIT_OPC}\"\ntoken:\npassword: ~\nsecret: \"\"\n"},
		{name: "a placeholder with more after it is a secret", doc: "ki: ${ORBIT_KI}00\n", want: []string{"ki"}},
		{name: "a fleet's echo target is no secret", doc: "behaviors:\n  latency: { target: 192.0.2.30 }\n"},
		{name: "unparsable fails closed on any secret-like key", doc: "credentials: { ki: abc\n  \"token\": x: y: [\n", want: []string{"(unparsed) ki", "(unparsed) token"}},
		{name: "unparsable without one", doc: "name: x: y: [\n"},
	}, shapes...)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := YAMLSecrets(tc.doc)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("YAMLSecrets = %q, want %q", got, tc.want)
			}
		})
	}
}

// scalars returns every scalar value in a document.
func scalars(t *testing.T, doc string) []string {
	t.Helper()
	docs, err := parseYAML(doc)
	if err != nil {
		t.Fatalf("redacted output does not parse: %v\n%s", err, doc)
	}
	var out []string
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			out = append(out, n.Value)
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	for _, d := range docs {
		visit(d)
	}
	return out
}

// RedactYAML masks the secret in every shape, at its anchor when aliased,
// and the result still parses.
func TestRedactYAML(t *testing.T) {
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactYAML(tc.doc)
			if strings.Contains(got, "KEYVAL") {
				t.Errorf("the key survived redaction:\n%s", got)
			}
			for _, v := range scalars(t, got) {
				if strings.Contains(v, "KEYVAL") {
					t.Errorf("a scalar still holds the key: %q", v)
				}
			}
			if YAMLSecrets(got) != nil && !strings.Contains(got, Masked) {
				t.Errorf("nothing was masked:\n%s", got)
			}
		})
	}
}

func TestRedactYAMLKeepsTheRest(t *testing.T) {
	doc := "# nightly fleet\nkind: fleet\ncredentials:\n  ki: aaaa # the lab key\n  supi: imsi-001010000000001\nshared: &k bbbb\nloom: {token: *k, agent: 192.0.2.30:9551}\nplaceholder: ${ORBIT_OPC}\nopc: ${ORBIT_OPC}\n"
	got := RedactYAML(doc)
	for _, keep := range []string{"# nightly fleet", "kind: fleet", "imsi-001010000000001", "192.0.2.30:9551", "# the lab key", "opc: ${ORBIT_OPC}"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q:\n%s", keep, got)
		}
	}
	for _, secret := range []string{"aaaa", "bbbb"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived:\n%s", secret, got)
		}
	}
	// The alias to the masked anchor stays an alias, so the structure holds.
	if !strings.Contains(got, "*k") {
		t.Errorf("the alias was lost:\n%s", got)
	}
}

func TestRedactYAMLUnparsable(t *testing.T) {
	for _, doc := range []string{"credentials: { ki: abc\n", "ki: \"unterminated\n", "\t- : :\n  ki: x"} {
		if got := RedactYAML(doc); got != Unparsed {
			t.Errorf("RedactYAML(%q) = %q, want %q", doc, got, Unparsed)
		}
	}
	if got := RedactYAML(""); got != "" {
		t.Errorf("RedactYAML(\"\") = %q", got)
	}
}

// FuzzRedactYAML: a key value placed in any of the shapes never survives
// RedactYAML, in the text or in any scalar of the re-parsed result.
func FuzzRedactYAML(f *testing.F) {
	for _, seed := range []string{"00112233445566778899aabbccddeeff", "a,b}c]d", "line1\nline2", "'quoted'\"x\"", "  padded  ", "#hash", "&anchor *alias"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, key string) {
		if len(strings.TrimSpace(key)) < 4 || !utf8.ValidString(key) {
			t.Skip()
		}
		// The value is embedded as a JSON string, which is valid
		// double-quoted YAML for any input; the other shapes take it as is
		// and are kept only when they parse back to exactly the key.
		q, err := json.Marshal(key)
		if err != nil {
			t.Skip()
		}
		docs := []string{
			"credentials:\n  ki: " + string(q) + "\n",
			"credentials: {ki: " + string(q) + ", supi: imsi-001}\n",
			"shared: &k " + string(q) + "\ncredentials: {ki: *k}\n",
			"token: " + key + "\n",
			"credentials:\n  opc: |\n    " + key + "\n",
			"loom: {token: '" + key + "'}\n",
		}
		for _, doc := range docs {
			var probe yaml.Node
			if yaml.Unmarshal([]byte(doc), &probe) != nil || !strings.Contains(fmt.Sprint(scalarsOf(&probe)), key) {
				continue
			}
			if strings.Contains(strings.Join(scalarsOf(&probe), "\x00"), Masked) {
				continue
			}
			got := RedactYAML(doc)
			if got == Unparsed {
				continue
			}
			for _, v := range scalars(t, got) {
				if strings.Contains(v, strings.TrimSpace(key)) && strings.TrimSpace(key) != "" && !strings.Contains(Masked, strings.TrimSpace(key)) && !inTemplate(strings.TrimSpace(key)) {
					t.Fatalf("key %q survived in scalar %q of\n%s", key, v, got)
				}
			}
		}
	})
}

func scalarsOf(n *yaml.Node) []string {
	var out []string
	if n.Kind == yaml.ScalarNode {
		out = append(out, n.Value)
	}
	for _, c := range n.Content {
		out = append(out, scalarsOf(c)...)
	}
	return out
}

// inTemplate reports whether s occurs in the fuzz documents' own text, so
// finding it after redaction is no leak.
func inTemplate(s string) bool {
	for _, t := range []string{"credentials", "imsi-001", "shared", "token", "loom", "supi", "opc", "ki"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// Scrub replaces each value wherever it appears; a hex value in any letter
// case, any other value exactly.
func TestScrub(t *testing.T) {
	const ki = "00112233445566778899aabbccddeeff"
	tests := []struct {
		name, in, want string
	}{
		{name: "exact", in: "key " + ki + " here", want: "key " + Masked + " here"},
		{name: "hex upper-cased", in: "KEY " + strings.ToUpper(ki), want: "KEY " + Masked},
		{name: "hex mixed case, twice", in: "00112233445566778899AABBccddeeff/" + ki, want: Masked + "/" + Masked},
		{name: "a token in another case is not the token", in: "LOOMTOK-SECRET-1", want: "LOOMTOK-SECRET-1"},
		{name: "a token exactly", in: "t=loomtok-secret-1", want: "t=" + Masked},
		{name: "a reformatted key is not matched", in: "00:11:22:33", want: "00:11:22:33"},
		{name: "too short to scrub", in: "abc12 and abc12", want: "abc12 and abc12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &orbitv1.Run{Error: tc.in}
			Scrub(m, []string{ki, "loomtok-secret-1", "abc12"})
			if m.GetError() != tc.want {
				t.Errorf("Scrub(%q) = %q, want %q", tc.in, m.GetError(), tc.want)
			}
		})
	}
}

// A map<string, string> entry keyed by a secret name is a secret (app
// params), and a provenance entry whose path names a secret field is one
// too: detected, masked and scrubbed like a marked field.
func TestMapAndProvenanceSecrets(t *testing.T) {
	const tok = "params-token-0123456789"
	const ki = "00112233445566778899aabbccddeeff"
	plan := &orbitv1.RunPlan{
		Workloads: []*orbitv1.Workload{{Name: "web", Kind: &orbitv1.Workload_Population{Population: &orbitv1.PopulationWorkload{
			Behaviors: &orbitv1.Behaviors{Traffic: &orbitv1.Traffic{Mix: []*orbitv1.TrafficShare{{Name: "web",
				Kind: &orbitv1.TrafficShare_App{App: &orbitv1.AppCohort{App: "http", Params: map[string]string{
					"token": tok, "object_size": "256KB",
					"apiKey": "apikey-value-1", "api-key": "apikey-value-2", "X-Api-Key": "apikey-value-3", "bearer": "bearer-value-4",
				}}}}}}},
		}}}},
		Provenance: []*orbitv1.FieldProvenance{
			{Path: "subscribers[main].ki", Value: ki, Overridden: []*orbitv1.ProvenanceOverride{{Value: "ffeeddccbbaa99887766554433221100"}}},
			{Path: "core.plmn.mcc", Value: "001"},
		},
	}
	got := Find(plan)
	want := []string{
		"provenance.value (subscribers[main].ki)",
		"workloads.population.behaviors.traffic.mix.app.params[X-Api-Key]",
		"workloads.population.behaviors.traffic.mix.app.params[api-key]",
		"workloads.population.behaviors.traffic.mix.app.params[apiKey]",
		"workloads.population.behaviors.traffic.mix.app.params[bearer]",
		"workloads.population.behaviors.traffic.mix.app.params[token]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Find = %q, want %q", got, want)
	}
	values := Mask(plan)
	for _, v := range []string{tok, ki, "ffeeddccbbaa99887766554433221100", "apikey-value-1", "apikey-value-2", "apikey-value-3", "bearer-value-4"} {
		if !slices.Contains(values, v) {
			t.Errorf("Mask did not return %q among %q", v, values)
		}
	}
	params := plan.GetWorkloads()[0].GetPopulation().GetBehaviors().GetTraffic().GetMix()[0].GetApp().GetParams()
	if params["token"] != Masked || params["apiKey"] != Masked || params["api-key"] != Masked || params["X-Api-Key"] != Masked ||
		params["bearer"] != Masked || params["object_size"] != "256KB" {
		t.Errorf("params after Mask = %v", params)
	}
	p := plan.GetProvenance()
	if p[0].GetValue() != Masked || p[0].GetOverridden()[0].GetValue() != Masked || p[1].GetValue() != "001" {
		t.Errorf("provenance after Mask = %v", p)
	}
	if got := Find(plan); len(got) != 0 {
		t.Errorf("Find after Mask = %q", got)
	}
}

// Secret names match on words, case and separators aside: a name, or a
// word-aligned suffix, that is one of Names.
func TestIsSecretName(t *testing.T) {
	for name, want := range NameCases {
		if got := IsSecretName(name); got != want {
			t.Errorf("IsSecretName(%q) = %t, want %t", name, got, want)
		}
	}
}

// A provenance entry for a whole-value subtree (a workload's behaviors)
// that holds a secret-named key is masked whole, value and overridden
// values, and its secret joins the scrub list; a plain value is left be.
func TestMaskSubtreeProvenance(t *testing.T) {
	behaviors := `{"traffic":{"mix":[{"app":{"app":"http","token":"cohort-tok"}}]}}`
	plan := &orbitv1.RunPlan{Provenance: []*orbitv1.FieldProvenance{
		{Path: "workloads[pop].behaviors", Value: behaviors,
			Overridden: []*orbitv1.ProvenanceOverride{{Value: `{"traffic":{"bulk":{"token":"old-tok"}}}`}}},
		{Path: "workloads[pop].attach.count", Value: "5"},
		{Path: "workloads[pop].behaviors", Value: `{"latency":{"target":"192.0.2.1"}}`},
	}}
	found := Mask(plan)
	got := plan.GetProvenance()
	if got[0].GetValue() != Masked || got[0].GetOverridden()[0].GetValue() != Masked {
		t.Errorf("subtree with a token = %v, want masked whole", got[0])
	}
	if got[1].GetValue() != "5" || got[2].GetValue() == Masked {
		t.Errorf("plain values masked: %v / %v", got[1], got[2])
	}
	for _, want := range []string{"cohort-tok", "old-tok"} {
		var has bool
		for _, f := range found {
			has = has || f == want
		}
		if !has {
			t.Errorf("Mask did not report %q for the scrub (found %v)", want, found)
		}
	}
}
