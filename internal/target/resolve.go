package target

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bgrewell/orbit/internal/secrets"
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
	// Problems are the bad values Report recorded; Resolve fails instead.
	Problems []Problem

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
func Resolve(inputs ...Input) (*Resolved, error) { return resolve(false, inputs) }

// Problem is a value Report found bad: the path, the source that set it
// and the error, with a secret's value masked as everywhere else.
type Problem struct {
	Path   string
	Source Source
	Err    string
}

// Report is Resolve for reporting only (--explain with no target): a value
// that does not convert or fails its format check is recorded in
// Resolved.Problems instead of failing the call. A value that does not
// convert is left out, so the field shows what the lower layers set. The
// structural errors (an unknown field, a reserved ran.node) still fail.
func Report(inputs ...Input) (*Resolved, error) { return resolve(true, inputs) }

func resolve(lenient bool, inputs []Input) (*Resolved, error) {
	var problems []Problem
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
				if firstOnly[lp] {
					continue // lookup has made it [0]
				}
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
				err = fmt.Errorf("%s: %w (%s)", format(segs), err, s.Source)
				if lenient {
					problems = append(problems, Problem{Path: format(segs), Source: s.Source, Err: err.Error()})
					continue
				}
				return nil, err
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
	firstSlice(state)
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
			if lenient {
				problems = append(problems, Problem{Path: p, Source: state[p].src, Err: err.Error()})
				continue
			}
			return nil, err
		}
	}

	// A pool's first SUPI must be in the target's own PLMN: a mismatch fails
	// every registration whatever the count.
	if pr := supiInPLMN(state, order); pr != nil {
		if !lenient {
			return nil, errors.New(pr.Err)
		}
		problems = append(problems, *pr)
	}
	res := &Resolved{Run: map[string]any{}, state: state, order: order, Problems: problems}
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

// firstSlice gives the first slice's fields (core.slices[0].sst and .sd)
// provenance of their own whenever the merged core.slices has a first
// slice or a layer set one of them. A field a layer set records the list's
// value it replaced; one no layer set comes from the list's source. A
// field set below the list's layer is dropped: the higher list replaced
// it, as a list replaces whole.
func firstSlice(state map[string]*entry) {
	list := state["core.slices"]
	var first *Slice
	if list != nil {
		if sl := list.value.([]Slice); len(sl) > 0 {
			first = &sl[0]
		}
	}
	for _, k := range []string{"sst", "sd"} {
		p := "core.slices[0]." + k
		var listVal any
		if first != nil {
			listVal = any(uint64(first.SST))
			if k == "sd" {
				listVal = first.SD
			}
		}
		e, set := state[p]
		_, f, _ := lookup(p)
		switch {
		case set && list != nil && e.src.Layer < list.src.Layer:
			delete(state, p)
			fallthrough
		case !set:
			if first != nil {
				state[p] = &entry{value: listVal, src: list.src,
					prov: &FieldProvenance{Path: p, Source: list.src, Value: show(f, listVal)}}
			}
		case first != nil:
			e.prov.Overridden = append([]Override{{Source: list.src, Value: show(f, listVal)}}, e.prov.Overridden...)
		}
	}
}

// supiInPLMN checks each pool's first_supi against core.plmn: its leading
// MCC+MNC digits must be the PLMN's. Unset fields are not checked.
func supiInPLMN(state map[string]*entry, order map[string][]string) *Problem {
	str := func(p string) string {
		if e, ok := state[p]; ok {
			if v, ok := e.value.(string); ok {
				return v
			}
		}
		return ""
	}
	mcc, mnc := str("core.plmn.mcc"), str("core.plmn.mnc")
	if mcc == "" || mnc == "" {
		return nil
	}
	for _, n := range order["subscribers"] {
		p := "subscribers[" + n + "].first_supi"
		supi := strings.TrimPrefix(str(p), "imsi-")
		if supi == "" || strings.HasPrefix(supi, mcc+mnc) {
			continue
		}
		return &Problem{Path: p, Source: state[p].src,
			Err: fmt.Sprintf("%s: %s is not in PLMN %s/%s (%s)", p, supi, mcc, mnc, state[p].src)}
	}
	return nil
}

// isTargetPath reports whether p is a field of the target document.
func isTargetPath(p string) bool {
	_, f, err := lookup(p)
	return err == nil && f.target
}

// unrenderable is what show prints for a whole value it does not render
// (one holding a map with a key that is not a string, which encoding/json
// renders on some Go versions and refuses on others): masked, whatever it
// holds.
const unrenderable = "<unrenderable: masked>"

// show renders a value for provenance, masking a secret. A whole-value
// subtree is masked whole when any key in it names a secret (a cohort's
// token, a params entry called password): its provenance is one entry,
// and a partly masked rendering would still be the subtree's text.
func show(f field, v any) string {
	if f.secret || (f.typ == tAny && holdsSecret(v)) {
		return Masked
	}
	if f.typ == tAny {
		if s, ok := v.(string); ok {
			return s
		}
		if !plainTree(v) {
			return unrenderable
		}
		b, err := json.Marshal(v)
		if err != nil {
			// Fails closed: a subtree that cannot be rendered is shown
			// masked, never as Go's own text of its values.
			return unrenderable
		}
		return string(b)
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

// plainTree reports whether a whole value is only string-keyed maps,
// lists and scalars, the shapes show renders.
func plainTree(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			if !plainTree(e) {
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !plainTree(e) {
				return false
			}
		}
	case nil, string, bool, int, int64, uint64, float64, time.Time:
	default:
		return false
	}
	return true
}

// holdsSecret reports whether a subtree has a key that names a secret.
func holdsSecret(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if secrets.IsSecretName(k) || holdsSecret(e) {
				return true
			}
		}
	case map[any]any:
		// A YAML map with a key that is not a string (1: x) decodes so.
		for k, e := range x {
			if secrets.IsSecretName(fmt.Sprint(k)) || holdsSecret(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if holdsSecret(e) {
				return true
			}
		}
	}
	return false
}

// Names is the element names of a named list (workloads, gates), in the
// order the layers gave them.
func (r *Resolved) Names(list string) []string {
	return append([]string(nil), r.order[list]...)
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
		t.Core.Slices = append([]Slice(nil), v.([]Slice)...)
	}
	sst, hasSST := get("core.slices[0].sst")
	sd, hasSD := get("core.slices[0].sd")
	if (hasSST || hasSD) && len(t.Core.Slices) == 0 {
		t.Core.Slices = []Slice{{}}
	}
	if hasSST {
		t.Core.Slices[0].SST = uint32(sst.(uint64))
	}
	if hasSD {
		t.Core.Slices[0].SD = sd.(string)
	}
	t.Core.DNN, t.Core.Profile, t.Core.UPFN3 = str("core.dnn"), str("core.profile"), str("core.upf_n3")
	t.Core.UEPools, t.Core.UPFN6, t.Core.N6Subnet = strs("core.ue_pools"), str("core.upf_n6"), str("core.n6_subnet")
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
