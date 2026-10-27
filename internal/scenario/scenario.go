// Package scenario defines ORBIT's declarative YAML test scenarios and runs
// them against a live ORBIT API server. A scenario declares the core, the gNBs,
// and the UEs once, then an ordered list of steps references them — replacing
// long, repetitive command lines. The runner is an ordinary API client (the
// CLI never touches the engine directly), so scenarios drive exactly the same
// operations as the individual commands.
package scenario

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue"
)

// Scenario is a whole test description.
type Scenario struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Core        Core        `yaml:"core"`
	Credentials Credentials `yaml:"credentials"`
	GNBs        []GNB       `yaml:"gnbs"`
	UEs         []UESpec    `yaml:"ues"`
	Steps       []Step      `yaml:"steps"`

	// targetName and echo are set by ApplyTarget: the target in use, and
	// its first responder's data_ip, which an omitted ping.dst or
	// latency.target takes.
	targetName string
	echo       string
}

// Core is the target and the defaults inherited by every gNB and UE.
//
// plmn.mcc and plmn.mnc are required (${ORBIT_MCC}/${ORBIT_MNC} expand like
// the credential references); tac, slice.sst and dnn take the CLI defaults
// (1, 1, "internet") when zero or empty, and slice.sd defaults to no SD. An
// omitted slice.sst is 1 even when slice.sd is given, so "SD given, SST
// omitted" means 1/sd here exactly as on the CLI and the API, and SST 0 is
// not expressible in a file (nor is it over the API).
type Core struct {
	AMF   string `yaml:"amf"`
	PLMN  PLMN   `yaml:"plmn"`
	TAC   uint32 `yaml:"tac"`
	Slice Slice  `yaml:"slice"`
	DNN   string `yaml:"dnn"`
}

// Defaults applied to a scenario's core when a field is zero or empty — the
// same values the CLI flags default to, so a file and a command line that
// omit the same thing mean the same thing.
const (
	DefaultTAC = 1
	DefaultSST = 1
	DefaultDNN = "internet"
)

// PLMN is the home network. Both fields are strings so a leading zero in the
// MNC survives: yaml.v3 keeps the source text of a scalar when decoding into
// a string, so `mnc: 01` arrives as "01" even unquoted, but quoting it in
// files is still the convention that makes the intent obvious.
type PLMN struct {
	MCC string `yaml:"mcc"`
	MNC string `yaml:"mnc"`
}

// applyDefaults fills the defaulted core fields and validates the identity
// with the rule the CLI and the API use (gnb.ValidateIdentity), so a file
// cannot describe a network the commands would refuse.
func (c *Core) applyDefaults() error {
	if c.AMF == "" {
		return fmt.Errorf("core.amf is required")
	}
	for _, f := range []struct{ name, v string }{{"mcc", c.PLMN.MCC}, {"mnc", c.PLMN.MNC}} {
		name, v := f.name, f.v
		if strings.Contains(v, "${") {
			// Only `orbit run` expands ${ENV} (ParseFleet / Parse); a scenario
			// sent to the server arrives literal (ParseFleetNoEnv), so the
			// reference is still here and the digit check below would only say
			// "must be 3 digits" about a string that looks nothing like one.
			return fmt.Errorf("core.plmn.%s is an unexpanded %s reference: a scenario sent to the server is not env-expanded, so give literal values there (or run it locally with `orbit run`, which expands ${ENV})", name, v)
		}
	}
	if c.PLMN.MCC == "" || c.PLMN.MNC == "" {
		return fmt.Errorf("core.plmn.mcc and core.plmn.mnc are required (quote them: mcc: \"001\", mnc: \"01\"). A scenario sent to the server is not env-expanded; `orbit run` expands ${ORBIT_MCC}/${ORBIT_MNC} locally")
	}
	if c.TAC == 0 {
		c.TAC = DefaultTAC
	}
	if c.Slice.SST == 0 {
		// Whenever unset, SD given or not: the CLI gives 1/<sd> for a bare
		// --sd and the API maps sst 0 to 1, so a file must agree with both.
		c.Slice.SST = DefaultSST
	}
	if c.DNN == "" {
		c.DNN = DefaultDNN
	}
	cfg := gnb.Config{MCC: c.PLMN.MCC, MNC: c.PLMN.MNC, TAC: c.TAC,
		Slices: []gnb.SNSSAI{{SST: uint8(c.Slice.SST), SD: c.Slice.SD}}}
	if c.Slice.SST > 0xFF {
		return fmt.Errorf("core.slice.sst %d: must be 0-255", c.Slice.SST)
	}
	if err := cfg.ValidateIdentity(); err != nil {
		return fmt.Errorf("core: %w", err)
	}
	return nil
}

type Slice struct {
	SST uint32 `yaml:"sst"`
	SD  string `yaml:"sd"`
}

// Credentials are the subscriber secrets; use ${ENV} references rather than
// committing them (e.g. ki: ${ORBIT_KI}).
type Credentials struct {
	Ki  string `yaml:"ki"`
	OPc string `yaml:"opc"`
}

// GNB is one gNB the scenario presents. Bind is the SCTP source (needed for a
// second gNB in a handover); N3 is the user-plane address reported to the UPF.
type GNB struct {
	ID   uint32 `yaml:"id"`
	Name string `yaml:"name"`
	N3   string `yaml:"n3"`
	Bind string `yaml:"bind"`
}

// UESpec declares one UE (supi) or a contiguous range, attached through a gNB.
type UESpec struct {
	// Name lets a step refer to this UE (ue: <name>); a single-UE entry
	// only. With a target the SUPIs are allocated, so steps use names.
	Name       string `yaml:"name"`
	SUPI       string `yaml:"supi"`
	Range      *Range `yaml:"range"`
	GNB        string `yaml:"gnb"`
	PDUSession bool   `yaml:"pdu_session"`
}

type Range struct {
	Base  string `yaml:"base"`
	Count int    `yaml:"count"`
}

// UE is a resolved subscriber: one SUPI bound to a concrete gNB.
type UE struct {
	SUPI       string
	GNB        GNB
	PDUSession bool
}

// Step is one action in the ordered list, written as a single-key mapping
// (`register: all`, `traffic: {ue: ..., rate: 20Mbps}`). Action is the key; the
// value is decoded per action by the runner.
type Step struct {
	Action string
	value  yaml.Node
}

func (s *Step) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("each step must be a single-action mapping, e.g. `- register: all`")
	}
	s.Action = node.Content[0].Value
	s.value = *node.Content[1]
	return nil
}

// decode unmarshals the step's value into out (a per-action params struct).
func (s *Step) decode(out any) error {
	if s.value.Kind == 0 {
		return nil
	}
	return s.value.Decode(out)
}

// str returns the step value as a scalar string (for `register: all`, `wait: 5s`).
func (s *Step) str() string { return s.value.Value }

var envRef = regexp.MustCompile(`\$\{(\w+)\}`)

// expandEnv replaces ${VAR} with the environment value (empty if unset).
func expandEnv(data []byte) []byte {
	return envRef.ReplaceAllFunc(data, func(m []byte) []byte {
		return []byte(os.Getenv(string(envRef.FindSubmatch(m)[1])))
	})
}

// Parse decodes a scenario from YAML, expanding ${ENV} references first.
func Parse(data []byte) (*Scenario, error) {
	return ParseWithTarget(data, nil)
}

// ParseWithTarget is Parse with the scenario's core, credentials, gNB
// addresses and UE SUPIs filled from t (ApplyTarget) before validation.
// With t nil the file must be self-contained: one that names a target
// (target:) is an error, since resolving it is the caller's (the CLI's).
func ParseWithTarget(data []byte, t *target.Target) (*Scenario, error) {
	ref, err := ReadTargetRef(data)
	if err != nil {
		return nil, err
	}
	s, err := decodeSteps(expandEnv(data))
	if err != nil {
		return nil, err
	}
	switch {
	case t != nil:
		if err := s.ApplyTarget(*t); err != nil {
			return nil, err
		}
	case ref.Set():
		return nil, fmt.Errorf("the scenario names a target, which the caller must resolve (orbit run does)")
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// decodeSteps decodes a step scenario without validating it.
func decodeSteps(data []byte) (*Scenario, error) {
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse scenario: %w", err)
	}
	return &s, nil
}

// Load reads and parses a scenario file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func (s *Scenario) validate() error {
	if err := s.Core.applyDefaults(); err != nil {
		return err
	}
	names := map[string]bool{}
	for i, g := range s.GNBs {
		if g.Name == "" {
			return fmt.Errorf("every gNB needs a name")
		}
		if names[g.Name] {
			return fmt.Errorf("duplicate gNB name %q", g.Name)
		}
		names[g.Name] = true
		n3, err := gnb.CanonicalN3(g.N3)
		if err != nil {
			return fmt.Errorf("gnb %s: n3: %w", g.Name, err)
		}
		s.GNBs[i].N3 = n3
	}
	ues, err := s.ResolveUEs()
	if err != nil {
		return err
	}
	if err := s.resolveSteps(); err != nil {
		return err
	}
	// Every UE's SUPI must parse under core.plmn, so a supi or range from
	// another PLMN (or a range that runs past 15 digits) fails at parse
	// instead of at registration.
	for _, u := range ues {
		if _, err := ue.ParseIdentity(u.SUPI, s.Core.PLMN.MCC, s.Core.PLMN.MNC, "0"); err != nil {
			return fmt.Errorf("ue %s: %w", u.SUPI, err)
		}
	}
	return nil
}

// ResolveUEs expands ranges and binds each UE to its gNB.
func (s *Scenario) ResolveUEs() ([]UE, error) {
	byName := map[string]GNB{}
	for _, g := range s.GNBs {
		byName[g.Name] = g
	}
	var out []UE
	for _, spec := range s.UEs {
		g, ok := byName[spec.GNB]
		if !ok {
			return nil, fmt.Errorf("UE references unknown gNB %q", spec.GNB)
		}
		supis, err := spec.supis()
		if err != nil {
			return nil, err
		}
		for _, supi := range supis {
			out = append(out, UE{SUPI: supi, GNB: g, PDUSession: spec.PDUSession})
		}
	}
	return out, nil
}

// supis returns the SUPI(s) a spec expands to.
func (u UESpec) supis() ([]string, error) {
	if u.Range != nil {
		if u.SUPI != "" {
			return nil, fmt.Errorf("a UE sets either supi or range, not both")
		}
		digits, err := ue.NormalizeSUPI(u.Range.Base)
		if err != nil {
			return nil, fmt.Errorf("range.base: %w", err)
		}
		base, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("range.base %q is not numeric: %w", u.Range.Base, err)
		}
		if u.Range.Count < 1 {
			return nil, fmt.Errorf("range.count must be >= 1")
		}
		width := len(digits)
		out := make([]string, u.Range.Count)
		for i := range out {
			out[i] = fmt.Sprintf("%0*d", width, base+uint64(i))
		}
		return out, nil
	}
	if u.SUPI == "" {
		return nil, fmt.Errorf("a UE needs a supi or a range")
	}
	supi, err := ue.NormalizeSUPI(u.SUPI)
	if err != nil {
		return nil, fmt.Errorf("supi: %w", err)
	}
	return []string{supi}, nil
}
