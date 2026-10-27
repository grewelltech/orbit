package target

import (
	"reflect"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

// Every binding names a field the resolver accepts, so a mapped flag or
// env var can never fail to resolve.
func TestBindingPathsAreFields(t *testing.T) {
	for _, b := range Bindings {
		if _, _, err := lookup(b.Path); err != nil {
			t.Errorf("binding %+v: %v", b, err)
		}
		if (b.Env == "") == (b.Flag == "") {
			t.Errorf("binding %+v: names exactly one of an env var and a flag", b)
		}
	}
	for _, e := range Exclusions {
		if (e.Env == "") == (e.Flag == "") || e.Reason == "" {
			t.Errorf("exclusion %+v: one of an env var and a flag, and a reason", e)
		}
	}
}

// FlagInput takes only the flags the user set, maps each by the command's
// binding, and makes --sst and --sd one slice.
func TestFlagInput(t *testing.T) {
	newFS := func() *pflag.FlagSet {
		fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
		fs.String("amf", "", "")
		fs.Uint32("tac", 1, "")
		fs.Uint32("sst", 1, "")
		fs.String("sd", "", "")
		fs.String("ki", "", "")
		fs.String("supi", "", "")
		fs.StringSlice("category", nil, "")
		fs.Bool("json", false, "")
		fs.Duration("per-test-timeout", 15*time.Second, "")
		return fs
	}
	tests := []struct {
		name string
		cmd  string
		args []string
		want map[string]any
	}{
		{name: "nothing set", cmd: "orbit conformance", want: map[string]any{}},
		{name: "defaults never set a field", cmd: "orbit conformance", args: []string{"--json"}, want: map[string]any{}},
		{name: "scalars", cmd: "orbit conformance", args: []string{"--amf", "192.0.2.1:38412", "--tac", "7", "--ki", "11111111111111111111111111111111"},
			want: map[string]any{"core.amf": "192.0.2.1:38412", "core.tac": "7", "subscribers[].ki": "11111111111111111111111111111111"}},
		{name: "slice from both", cmd: "orbit load", args: []string{"--sst", "2", "--sd", "abcdef"},
			want: map[string]any{"core.slices[0].sst": "2", "core.slices[0].sd": "abcdef"}},
		{name: "sd alone sets only the first slice's SD", cmd: "orbit load", args: []string{"--sd", "abcdef"},
			want: map[string]any{"core.slices[0].sd": "abcdef"}},
		{name: "command scoping: supi on conformance", cmd: "orbit conformance", args: []string{"--supi", "001010000000001"},
			want: map[string]any{"subscribers[].first_supi": "001010000000001"}},
		{name: "command scoping: supi on ue ping is an operation argument", cmd: "orbit ue ping", args: []string{"--supi", "001010000000001"},
			want: map[string]any{}},
		{name: "duration flag to a millisecond field", cmd: "orbit conformance", args: []string{"--per-test-timeout", "2.5s"},
			want: map[string]any{"workloads[].conformance.per_test_timeout_ms": uint64(2500)}},
		{name: "slice flag", cmd: "orbit conformance", args: []string{"--category", "nas,security"},
			want: map[string]any{"workloads[].conformance.categories": []string{"nas", "security"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFS()
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			in, err := FlagInput(tc.cmd, fs)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			for _, s := range in.Settings {
				got[s.Path] = s.Value
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("settings = %v, want %v", got, tc.want)
			}
			if _, err := Resolve(in); err != nil {
				t.Errorf("the flag layer does not resolve: %v", err)
			}
		})
	}
}

// EnvInput reads the bound ORBIT_* variables that are set, and nothing else.
func TestEnvInput(t *testing.T) {
	in := EnvInput(envOf(map[string]string{"ORBIT_MCC": "001", "ORBIT_KI": "11111111111111111111111111111111", "ORBIT_SERVER": "http://x", "HOME": "/h"}))
	got := map[string]string{}
	for _, s := range in.Settings {
		got[s.Path] = s.Source.Name + "=" + s.Value.(string)
	}
	want := map[string]string{"core.plmn.mcc": "$ORBIT_MCC=001", "subscribers[].ki": "$ORBIT_KI=11111111111111111111111111111111"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env settings = %v, want %v", got, want)
	}
	r, err := Resolve(DefaultInput(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p := prov(t, r, "subscribers[default].ki"); p.Source.String() != "env $ORBIT_KI" || p.Value != Masked {
		t.Errorf("ki provenance = %s %q", p.Source, p.Value)
	}
}

// An env var holds a number by the same rule as a file: a leading zero is
// an error naming the variable, while 0x, 0o and plain 0 resolve.
func TestEnvNumberLeadingZero(t *testing.T) {
	tests := []struct {
		value   string
		want    uint64
		wantErr string
	}{
		{value: "0010", wantErr: "workloads[default].attach.seed: 0010 has a leading zero; write 10, or 0x… for hex (env $ORBIT_LOAD_SEED)"},
		{value: "10", want: 10},
		{value: "0x0a", want: 10},
		{value: "0o12", want: 10},
		{value: "0", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			r, err := Resolve(EnvInput(envOf(map[string]string{"ORBIT_LOAD_SEED": tc.value})))
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Run["workloads[default].attach.seed"]; got != tc.want {
				t.Errorf("seed = %v, want %d", got, tc.want)
			}
		})
	}
}

// On ue handover and ue xn-handover the destination gNB's flags are
// operation arguments: a target never supplies or overrides them. The same
// flags keep their paths on other commands, and --amf stays bound.
func TestHandoverTargetFlagsExcluded(t *testing.T) {
	flags := []string{"bind", "gnb-id", "gnb-bits", "gnb-n3", "gnb-name", "mcc", "mnc", "tac", "sst", "sd"}
	for _, cmd := range []string{"orbit ue handover", "orbit ue xn-handover"} {
		for _, f := range flags {
			if b, ok := FlagBinding(cmd, f); ok {
				t.Errorf("%s --%s is bound to %s", cmd, f, b.Path)
			}
			if e, ok := FlagExclusion(cmd, f); !ok || e.Reason != ReasonOperation {
				t.Errorf("%s --%s: exclusion %+v, %v; want an operation argument", cmd, f, e, ok)
			}
		}
		if b, ok := FlagBinding(cmd, "amf"); !ok || b.Path != "core.amf" {
			t.Errorf("%s --amf: %+v, %v; want core.amf", cmd, b, ok)
		}
	}
	for _, c := range []struct{ cmd, flag, path string }{
		{"orbit ue register", "bind", "ran.n2_source_ips"},
		{"orbit conformance", "tac", "core.tac"},
		{"orbit load", "gnb-id", "ran.gnb_ids.first"},
		{"orbit cell ngsetup", "mcc", "core.plmn.mcc"},
	} {
		if b, ok := FlagBinding(c.cmd, c.flag); !ok || b.Path != c.path {
			t.Errorf("%s --%s: %+v, %v; want %s", c.cmd, c.flag, b, ok, c.path)
		}
	}
}

// DefaultsInput is the command's flag defaults that say something, at the
// default layer: not the user's flags, not empty or zero defaults, not
// unbound or deprecated flags; a duration default on a _ms path in ms.
func TestDefaultsInput(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("amf", "", "")
	fs.Int("count", 100, "")
	fs.Int("concurrency", 64, "")
	fs.Float64("rate", 0, "")
	fs.Bool("pdu-session", false, "")
	fs.String("gnb-name", "orbit-gnb", "")
	fs.Duration("duration", 0, "")
	fs.Duration("sample-interval", 2*time.Second, "")
	fs.String("arrival", "constant", "")
	fs.Int("progress-every", 5, "") // unbound
	fs.Uint32("gnb-bits", 22, "")
	_ = fs.MarkDeprecated("gnb-bits", "use --gnb-id-bits")
	if err := fs.Parse([]string{"--count", "7"}); err != nil {
		t.Fatal(err)
	}
	in, err := DefaultsInput("orbit load", fs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, s := range in.Settings {
		got[s.Path] = s.Value
		if s.Source.String() != "default (orbit load)" {
			t.Errorf("%s source = %q", s.Path, s.Source.Name)
		}
	}
	want := map[string]any{
		"workloads[].attach.concurrency": "64",
		"ran.gnb_name":                   "orbit-gnb",
		"workloads[].sample_interval_ms": uint64(2000),
		"workloads[].attach.arrival":     "constant",
	}
	if in.Layer != LayerDefault || !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultsInput = %v (layer %v), want %v", got, in.Layer, want)
	}
}
