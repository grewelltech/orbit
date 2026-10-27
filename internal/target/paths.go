package target

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A canonical path names one field (ADR-0008 §3): dot-separated keys, with
// a named-list element selected by name in brackets (subscribers[main].ki)
// or, as flags and env vars do because they know no names, by empty
// brackets for the list's default element: the first one, as the lower
// layers have it (subscribers[].ki).
//
// A path's pattern replaces every selector with [] (subscribers[].ki), and
// the field table below declares each pattern's shape and value type.

// valueType is a field's value type.
type valueType int

const (
	tString valueType = iota
	tUint
	tFloat
	tBool
	// The list types are one field each: a higher layer replaces the whole
	// list, so "which layer set this" always has one answer.
	tStrings // a list of strings
	tSlices  // a list of {sst, sd}
)

// field is one entry of the field table.
type field struct {
	typ    valueType
	secret bool
	// target marks a field of the target document; the others are run-file
	// fields (workloads, until, gates, the run's name).
	target bool
	// wide marks a tUint field that is 64-bit; the others are 32-bit.
	wide bool
}

// max is the largest value a tUint field holds.
func (f field) max() uint64 {
	if f.wide {
		return math.MaxUint64
	}
	return math.MaxUint32
}

// firstOnly are the plain lists whose first element's fields have paths of
// their own (core.slices[0].sst): --sst and --sd set those, on top of the
// merged list, so a lone --sd keeps the target's SST and other slices.
var firstOnly = map[string]bool{"core.slices": true}

// namedLists are the lists that merge element by element, by name.
var namedLists = map[string]bool{
	"subscribers":    true,
	"app.responders": true,
	"workloads":      true,
	"gates":          true,
}

// fields is the field table: every canonical path pattern the resolver
// accepts.
var fields = map[string]field{
	// The target document (ADR-0008 §1).
	"version":                         {typ: tUint, target: true},
	"kind":                            {target: true},
	"name":                            {target: true},
	"core.amf":                        {target: true},
	"core.plmn.mcc":                   {target: true},
	"core.plmn.mnc":                   {target: true},
	"core.tac":                        {typ: tUint, target: true},
	"core.slices":                     {typ: tSlices, target: true},
	"core.slices[].sst":               {typ: tUint, target: true}, // the first slice's, core.slices[0].sst
	"core.slices[].sd":                {target: true},             // core.slices[0].sd
	"core.dnn":                        {target: true},
	"core.profile":                    {target: true},
	"core.upf_n3":                     {target: true},
	"subscribers[].name":              {target: true},
	"subscribers[].first_supi":        {target: true},
	"subscribers[].count":             {typ: tUint, target: true},
	"subscribers[].ki":                {secret: true, target: true},
	"subscribers[].opc":               {secret: true, target: true},
	"subscribers[].routing_indicator": {target: true},
	"ran.node":                        {target: true},
	"ran.gnb_name":                    {target: true},
	"ran.n2_source_ips":               {typ: tStrings, target: true},
	"ran.n3_ips":                      {typ: tStrings, target: true},
	"ran.gnb_ids.first":               {typ: tUint, target: true},
	"ran.gnb_ids.count":               {typ: tUint, target: true},
	"ran.gnb_ids.bits":                {typ: tUint, target: true},
	"app.responders[].name":           {target: true},
	"app.responders[].control":        {target: true},
	"app.responders[].token":          {secret: true, target: true},
	"app.responders[].data_ip":        {target: true},

	// The run file (ADR-0008 §2), as far as today's flags reach it. A run
	// file names its target with "target"; the run's own name is
	// "run.name" so it cannot collide with the target's.
	"target":                                      {},
	"run.name":                                    {},
	"run.event_verbosity":                         {},
	"until.duration_ms":                           {typ: tUint},
	"workloads[].name":                            {},
	"workloads[].attach.count":                    {typ: tUint},
	"workloads[].attach.rate":                     {typ: tFloat},
	"workloads[].attach.ramp":                     {},
	"workloads[].attach.arrival":                  {},
	"workloads[].attach.concurrency":              {typ: tUint},
	"workloads[].attach.seed":                     {typ: tUint, wide: true},
	"workloads[].population.pdu_session":          {typ: tBool},
	"workloads[].population.topology.gnb_count":   {typ: tUint},
	"workloads[].population.sample_interval_ms":   {typ: tUint},
	"workloads[].conformance.categories":          {typ: tStrings},
	"workloads[].conformance.target_release":      {},
	"workloads[].conformance.expected":            {},
	"workloads[].conformance.per_test_timeout_ms": {typ: tUint},
	"workloads[].conformance.traffic":             {},
	"workloads[].conformance.nas.integrity":       {typ: tStrings},
	"workloads[].conformance.nas.ciphering":       {typ: tStrings},
	"gates[].name":                                {},
	"gates[].slo.threshold":                       {},
}

// segment is one step of a path: a key, or a named-list element.
type segment struct {
	key  string
	elem bool   // a list element
	name string // the element's name; "" with elem is the default element
}

// parsePath splits a canonical path into segments.
func parsePath(p string) ([]segment, error) {
	if p == "" {
		return nil, fmt.Errorf("empty path")
	}
	var out []segment
	for _, part := range strings.Split(p, ".") {
		key, rest, hasSel := strings.Cut(part, "[")
		if key == "" {
			return nil, fmt.Errorf("path %q: empty key", p)
		}
		out = append(out, segment{key: key})
		if hasSel {
			name, ok := strings.CutSuffix(rest, "]")
			if !ok || strings.ContainsAny(name, "[]") {
				return nil, fmt.Errorf("path %q: bad selector in %q", p, part)
			}
			out = append(out, segment{elem: true, name: name})
		}
	}
	return out, nil
}

// pattern is a path with every selector emptied: the field table's key.
func pattern(segs []segment) string {
	var b strings.Builder
	for i, s := range segs {
		switch {
		case s.elem:
			b.WriteString("[]")
		case i > 0:
			b.WriteString(".")
			b.WriteString(s.key)
		default:
			b.WriteString(s.key)
		}
	}
	return b.String()
}

// format joins segments back into a path.
func format(segs []segment) string {
	var b strings.Builder
	for i, s := range segs {
		switch {
		case s.elem:
			b.WriteString("[" + s.name + "]")
		case i > 0:
			b.WriteString(".")
			b.WriteString(s.key)
		default:
			b.WriteString(s.key)
		}
	}
	return b.String()
}

// listOf is the path of the named list an element segment belongs to: the
// segments before it.
func listOf(segs []segment, i int) string { return format(segs[:i]) }

// lookup returns the field a path names.
func lookup(p string) ([]segment, field, error) {
	segs, err := parsePath(p)
	if err != nil {
		return nil, field{}, err
	}
	pat := pattern(segs)
	f, ok := fields[pat]
	if !ok {
		return nil, field{}, fmt.Errorf("unknown field %q", p)
	}
	for i, s := range segs {
		if !s.elem {
			continue
		}
		lp := pattern(segs[:i])
		switch {
		case namedLists[lp]:
		case firstOnly[lp]:
			if s.name != "" && s.name != "0" {
				return nil, field{}, fmt.Errorf("path %q: only the first element of %s, [0], has paths of its own", p, lp)
			}
			segs[i].name = "0"
		default:
			return nil, field{}, fmt.Errorf("path %q: %s is not a named list", p, lp)
		}
	}
	return segs, f, nil
}

// IsSecret reports whether the field a path names is secret (ki, opc and
// responder tokens): its value is masked everywhere it is shown.
func IsSecret(p string) bool {
	_, f, err := lookup(p)
	return err == nil && f.secret
}

// Paths lists every canonical path pattern the resolver accepts, sorted.
func Paths() []string {
	out := make([]string, 0, len(fields))
	for p := range fields {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// convert turns a value from a file, env var or flag into the field's type.
// Strings convert; values already of the type pass through.
//
// Every value an error shows goes through errValue, so a secret field's
// value never prints, whatever went wrong. A tUint field is 32-bit unless
// marked wide (the load seed, 64-bit): a value above its width or below
// zero is a range error, never truncated.
func convert(f field, v any) (any, error) {
	switch f.typ {
	case tString:
		switch x := v.(type) {
		case string:
			return x, nil
		case int, int64, uint64, float64, bool:
			return fmt.Sprint(x), nil
		}
	case tUint:
		switch x := v.(type) {
		case string:
			t := strings.TrimSpace(x)
			if err := leadingZeroText(t); err != nil {
				return nil, err
			}
			bits := 32
			if f.wide {
				bits = 64
			}
			n, err := strconv.ParseUint(t, 0, bits)
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf("%s is out of range (0 to %d)", errValue(f, x), f.max())
			}
			if err != nil {
				return nil, fmt.Errorf("%s is not a non-negative integer", errValue(f, x))
			}
			return n, nil
		case int:
			if x >= 0 && uint64(x) <= f.max() {
				return uint64(x), nil
			}
			return nil, fmt.Errorf("%s is out of range (0 to %d)", errValue(f, x), f.max())
		case uint64:
			if x <= f.max() {
				return x, nil
			}
			return nil, fmt.Errorf("%s is out of range (0 to %d)", errValue(f, x), f.max())
		}
	case tFloat:
		switch x := v.(type) {
		case string:
			t := strings.TrimSpace(x)
			if err := leadingZeroText(t); err != nil {
				return nil, err
			}
			n, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return nil, fmt.Errorf("%s is not a number", errValue(f, x))
			}
			return n, nil
		case int:
			return float64(x), nil
		case float64:
			return x, nil
		}
	case tBool:
		switch x := v.(type) {
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(x))
			if err != nil {
				return nil, fmt.Errorf("%s is not true or false", errValue(f, x))
			}
			return b, nil
		case bool:
			return x, nil
		}
	case tStrings:
		switch x := v.(type) {
		case string:
			if x == "" {
				return []string{}, nil // an empty flag clears the list
			}
			return []string{x}, nil
		case []string:
			return append([]string(nil), x...), nil
		case []any:
			out := make([]string, 0, len(x))
			for _, e := range x {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("list entry %s is not a string", errValue(f, e))
				}
				out = append(out, s)
			}
			return out, nil
		}
	case tSlices:
		switch x := v.(type) {
		case []Slice:
			return append([]Slice(nil), x...), nil
		case []any:
			out := make([]Slice, 0, len(x))
			for _, e := range x {
				m, ok := e.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("slice entry %s is not a mapping", errValue(f, e))
				}
				var s Slice
				for k, val := range m {
					switch k {
					case "sst":
						n, err := convert(field{typ: tUint}, val)
						if err != nil {
							return nil, fmt.Errorf("slice sst: %w", err)
						}
						s.SST = uint32(n.(uint64))
					case "sd":
						sd, ok := val.(string)
						if !ok {
							return nil, fmt.Errorf("slice sd %s is not a string (quote it)", errValue(f, val))
						}
						s.SD = sd
					default:
						return nil, fmt.Errorf("unknown slice key %q", k)
					}
				}
				out = append(out, s)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("%s (%T) is the wrong type", errValue(f, v), v)
}

// errValue is how an error shows a field's value: masked when the field is
// secret, quoted when it is a string.
func errValue(f field, v any) string {
	if f.secret {
		return Masked
	}
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return show(f, v)
}
