package target

import (
	"fmt"
	"sort"
	"strings"
)

// Layer is a configuration layer, lowest first (ADR-0008 §3): a built-in
// default < the target < the run file < the environment < flags.
type Layer int

const (
	LayerDefault Layer = iota
	LayerTarget
	LayerRunFile
	LayerEnv
	LayerFlag
)

func (l Layer) String() string {
	switch l {
	case LayerDefault:
		return "default"
	case LayerTarget:
		return "target"
	case LayerRunFile:
		return "run file"
	case LayerEnv:
		return "env"
	case LayerFlag:
		return "flag"
	}
	return fmt.Sprintf("layer %d", int(l))
}

// Source says where a value came from within its layer.
type Source struct {
	Layer Layer
	// Name is the file path, the env var, the flag (with its dashes) or
	// "built-in".
	Name string
	// Line is the value's line in Name when it is a file, else 0.
	Line int
	// Via names the ${ENV} variables a file value was expanded from: the
	// value still belongs to the file's layer.
	Via []string
}

func (s Source) String() string {
	out := s.Layer.String()
	if s.Name != "" {
		out += " " + s.Name
		if s.Line > 0 {
			out += fmt.Sprintf(":%d", s.Line)
		}
	}
	for _, v := range s.Via {
		out += " via ${" + v + "}"
	}
	return out
}

// Setting is one field a layer sets.
type Setting struct {
	Path   string
	Value  any
	Source Source
}

// Input is one layer's settings. Settings carry their own Source; Resolve
// sets each one's Layer from the Input's.
type Input struct {
	Layer    Layer
	Settings []Setting
}

// Masked is what a secret field's value shows as.
const Masked = "<redacted>"

// Override is a value a higher layer replaced.
type Override struct {
	Source Source
	Value  string
}

// FieldProvenance records, for one field that ends up set, the layer and
// source that set it, its value (masked when secret), and the values it
// overrode, lowest layer first (ADR-0008 §3).
type FieldProvenance struct {
	Path       string
	Source     Source
	Value      string
	Overridden []Override
}

// Resolved is the result of layering: the target, the run-file fields the
// layers set (by canonical path), and every field's provenance, sorted by
// path.
type Resolved struct {
	Target     Target
	Run        map[string]any
	Provenance []FieldProvenance

	state map[string]*entry   // every set field by concrete path
	order map[string][]string // named list path → element names
}

// Field is one resolved field: its concrete path, its value in the field's
// type (string, uint64, float64, bool, []string or []Slice; secrets
// unmasked) and the source that set it.
type Field struct {
	Path   string
	Value  any
	Source Source
}

// Lookup is the resolved field at path. An empty selector (subscribers[].ki)
// names the list's default element, its first. ok is false when no layer
// set the field.
func (r *Resolved) Lookup(path string) (Field, bool) {
	segs, _, err := lookup(path)
	if err != nil {
		return Field{}, false
	}
	for i := range segs {
		if segs[i].elem && segs[i].name == "" {
			names := r.order[listOf(segs, i)]
			if len(names) == 0 {
				return Field{}, false
			}
			segs[i].name = names[0]
		}
	}
	p := format(segs)
	e, ok := r.state[p]
	if !ok {
		return Field{}, false
	}
	return Field{Path: p, Value: e.value, Source: e.src}, true
}

// entry is a field's state during the merge.
type entry struct {
	value any
	src   Source
	prov  *FieldProvenance
}

// Resolve merges the inputs by layer, lowest first; inputs of one layer
// apply in the order given. The merge rules: scalars replace; maps merge
// key by key (each field is its own path); a plain list is one field and
// is replaced whole; a named list merges element by element, by name, and
// an empty selector (subscribers[].ki) picks the list's default element,
// its first as the lower layers have it, creating one named "default" when
// there is none. The resolved target's ran.node must be "" (ErrNodeReserved);
// the document checks of Validate apply to target files, at load.
func Resolve(inputs ...Input) (*Resolved, error) {
	ordered := append([]Input(nil), inputs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Layer < ordered[j].Layer })

	state := map[string]*entry{}
	order := map[string][]string{} // named list path → element names, first seen first
	for _, in := range ordered {
		for _, s := range in.Settings {
			s.Source.Layer = in.Layer
			segs, f, err := lookup(s.Path)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Source, err)
			}
			// Resolve default selectors and record element order.
			for i := range segs {
				if !segs[i].elem {
					continue
				}
				lp := listOf(segs, i)
				if segs[i].name == "" {
					if names := order[lp]; len(names) > 0 {
						segs[i].name = names[0]
					} else {
						segs[i].name = "default"
					}
				}
				if !contains(order[lp], segs[i].name) {
					order[lp] = append(order[lp], segs[i].name)
				}
			}
			v, err := convert(f, s.Value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w (%s)", format(segs), err, s.Source)
			}
			path := format(segs)
			if e, ok := state[path]; ok {
				e.prov.Overridden = append(e.prov.Overridden, Override{Source: e.src, Value: show(f, e.value)})
				e.value, e.src = v, s.Source
				e.prov.Source, e.prov.Value = s.Source, show(f, v)
				continue
			}
			state[path] = &entry{value: v, src: s.Source,
				prov: &FieldProvenance{Path: path, Source: s.Source, Value: show(f, v)}}
		}
	}
	// A named-list element's name field is its selector.
	for lp, names := range order {
		for _, n := range names {
			p := lp + "[" + n + "].name"
			if _, ok := state[p]; !ok {
				state[p] = &entry{value: n, src: Source{Layer: LayerDefault, Name: "selector"},
					prov: &FieldProvenance{Path: p, Source: Source{Layer: LayerDefault, Name: "selector"}, Value: n}}
			} else if state[p].value != n {
				return nil, fmt.Errorf("%s: name %q does not match its selector %q", p, state[p].value, n)
			}
		}
	}

	paths := make([]string, 0, len(state))
	for p := range state {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		_, f, _ := lookup(p)
		if err := validateValue(p, f, state[p].value, state[p].src); err != nil {
			return nil, err
		}
	}

	res := &Resolved{Run: map[string]any{}, state: state, order: order}
	for p, e := range state {
		res.Provenance = append(res.Provenance, *e.prov)
		if !isTargetPath(p) {
			res.Run[p] = e.value
		}
	}
	sort.Slice(res.Provenance, func(i, j int) bool { return res.Provenance[i].Path < res.Provenance[j].Path })
	t, err := buildTarget(state, order)
	if err != nil {
		return nil, err
	}
	res.Target = t
	return res, nil
}

// isTargetPath reports whether p is a field of the target document.
func isTargetPath(p string) bool {
	_, f, err := lookup(p)
	return err == nil && f.target
}

// show renders a value for provenance, masking a secret.
func show(f field, v any) string {
	if f.secret {
		return Masked
	}
	switch x := v.(type) {
	case []string:
		return "[" + strings.Join(x, ", ") + "]"
	case []Slice:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = fmt.Sprint(s.SST)
			if s.SD != "" {
				parts[i] += "/" + s.SD
			}
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// buildTarget assembles the target document from the merged fields, named
// lists in their elements' order.
func buildTarget(state map[string]*entry, order map[string][]string) (Target, error) {
	var t Target
	get := func(p string) (any, bool) {
		e, ok := state[p]
		if !ok {
			return nil, false
		}
		return e.value, true
	}
	str := func(p string) string {
		if v, ok := get(p); ok {
			return v.(string)
		}
		return ""
	}
	num := func(p string) uint32 {
		if v, ok := get(p); ok {
			return uint32(v.(uint64))
		}
		return 0
	}
	strs := func(p string) []string {
		if v, ok := get(p); ok {
			return v.([]string)
		}
		return nil
	}
	t.Version = int(num("version"))
	t.Kind, t.Name = str("kind"), str("name")
	t.Core.AMF = str("core.amf")
	t.Core.PLMN = PLMN{MCC: str("core.plmn.mcc"), MNC: str("core.plmn.mnc")}
	t.Core.TAC = num("core.tac")
	if v, ok := get("core.slices"); ok {
		t.Core.Slices = v.([]Slice)
	}
	t.Core.DNN, t.Core.Profile, t.Core.UPFN3 = str("core.dnn"), str("core.profile"), str("core.upf_n3")
	for _, n := range order["subscribers"] {
		b := "subscribers[" + n + "]."
		t.Subscribers = append(t.Subscribers, Pool{Name: n, FirstSUPI: str(b + "first_supi"), Count: num(b + "count"),
			Ki: str(b + "ki"), OPc: str(b + "opc"), RoutingIndicator: str(b + "routing_indicator")})
	}
	t.RAN = RAN{Node: str("ran.node"), GNBName: str("ran.gnb_name"), N2SourceIPs: strs("ran.n2_source_ips"), N3IPs: strs("ran.n3_ips"),
		GNBIDs: GNBIDs{First: num("ran.gnb_ids.first"), Count: num("ran.gnb_ids.count"), Bits: num("ran.gnb_ids.bits")}}
	for _, n := range order["app.responders"] {
		b := "app.responders[" + n + "]."
		t.App.Responders = append(t.App.Responders, Responder{Name: n, Control: str(b + "control"), Token: str(b + "token"), DataIP: str(b + "data_ip")})
	}
	if t.RAN.Node != "" {
		return t, fmt.Errorf("%w (got %q)", ErrNodeReserved, t.RAN.Node)
	}
	return t, nil
}
