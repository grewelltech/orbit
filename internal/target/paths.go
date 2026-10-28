package target

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
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
	// tAny is a whole-value subtree (a workload's behaviors, a gate's
	// check): one field, its value a mapping, list or scalar as written,
	// replaced whole by a higher layer, with provenance at the subtree.
	tAny
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
	"core.ue_pools":                   {typ: tStrings, target: true},
	"core.upf_n6":                     {target: true},
	"core.n6_subnet":                  {target: true},
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

	// The run file (ADR-0008 §2): every RunPlan field below the target.
	// A run file names its target with "target"; the run's own name is
	// "run.name" so it cannot collide with the target's. A population's
	// fields leave out the kind (workloads[].attach.count is
	// workloads[i].population.attach.count); a conformance workload's name
	// it. behaviors, start and conformance.expected are whole-value
	// subtrees (tAny), as are a gate's checks below slo; conformance.nas is
	// two lists, integrity and ciphering, as --nas-integrity and
	// --nas-ciphering set them.
	"target":                                      {},
	"run.name":                                    {},
	"run.event_verbosity":                         {},
	"until.duration_ms":                           {typ: tUint},
	"until.duration":                              {}, // "2h": a run file's spelling of duration_ms
	"until.complete":                              {typ: tBool},
	"until.forever":                               {typ: tBool},
	"workloads[].name":                            {},
	"workloads[].start":                           {typ: tAny},
	"workloads[].attach.count":                    {typ: tUint},
	"workloads[].attach.rate":                     {typ: tFloat},
	"workloads[].attach.ramp":                     {},
	"workloads[].attach.arrival":                  {},
	"workloads[].attach.concurrency":              {typ: tUint},
	"workloads[].attach.distribution":             {},
	"workloads[].attach.distribution_seed":        {typ: tUint, wide: true},
	"workloads[].attach.seed":                     {typ: tUint, wide: true},
	"workloads[].pdu_session":                     {typ: tBool},
	"workloads[].topology.gnb_count":              {typ: tUint},
	"workloads[].topology.layout":                 {},
	"workloads[].topology.spacing_m":              {typ: tFloat},
	"workloads[].topology.gnb_ids.first":          {typ: tUint},
	"workloads[].topology.gnb_ids.assign":         {typ: tBool},
	"workloads[].behaviors":                       {typ: tAny},
	"workloads[].subscriber_pool":                 {},
	"workloads[].sample_interval_ms":              {typ: tUint},
	"workloads[].conformance.categories":          {typ: tStrings},
	"workloads[].conformance.target_release":      {},
	"workloads[].conformance.expected":            {typ: tAny}, // a file path, or the list inline
	"workloads[].conformance.per_test_timeout_ms": {typ: tUint},
	"workloads[].conformance.responder":           {},
	"workloads[].conformance.traffic":             {},
	"workloads[].conformance.gnb_ids.first":       {typ: tUint},
	"workloads[].conformance.gnb_ids.assign":      {typ: tBool},
	"workloads[].conformance.subscriber_pool":     {},
	"workloads[].conformance.n3_bind":             {},
	"workloads[].conformance.nas.integrity":       {typ: tStrings},
	"workloads[].conformance.nas.ciphering":       {typ: tStrings},
	"gates[].name":                                {},
	"gates[].workload":                            {},
	"gates[].slo.metric":                          {},
	"gates[].slo.op":                              {},
	"gates[].slo.threshold":                       {},
	"gates[].conformance":                         {typ: tBool},
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

// RunPaths lists the run file's path patterns (every field below the
// target: workloads, until, gates, the run's own), sorted.
func RunPaths() []string {
	var out []string
	for p, f := range fields {
		if !f.target {
			out = append(out, p)
		}
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
	if f.typ == tAny {
		// A flag or --set gives text: read it as YAML, so a mapping or a
		// list can be written inline; a plain word stays a string.
		if x, ok := v.(string); ok {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(x), &doc); err != nil {
				return nil, fmt.Errorf("%s is not valid YAML: %w", errValue(f, x), err)
			}
			if doc.Kind == 0 || len(doc.Content) == 0 {
				return x, nil
			}
			parsed, err := decodeWhole(doc.Content[0])
			if err != nil {
				return nil, fmt.Errorf("%s is not valid YAML: %w", errValue(f, x), err)
			}
			if parsed == nil {
				return x, nil
			}
			return parsed, nil
		}
		return v, nil
	}
	return nil, fmt.Errorf("%s (%T) is the wrong type", errValue(f, v), v)
}

// errValue is how an error shows a field's value: masked when the field is
// secret, or when the value is shaped like a key or token (keyShaped), a
// secret pasted into the wrong field; quoted when it is a string.
func errValue(f field, v any) string {
	if f.secret || f.typ == tAny {
		return Masked // a subtree's text may hold a token
	}
	if s, ok := v.(string); ok {
		if keyShaped(s) {
			return Masked
		}
		return strconv.Quote(s)
	}
	return maskKeyShaped(show(f, v)) // a list or mapping: a key in an entry
}

// keyShapePattern is a key or token's shape, unanchored: hex digits with
// an optional 0x, as a Ki or OPc (32) or a 64-digit key is written, give
// or take a digit (a key mistyped), whole or in pairs or quads separated
// by ':', '-' or ' '; or one of orbit's tokens (a join token, a responder
// credential, a loom token) by its prefix. A base64 or bearer secret has
// no shape to know it by.
const keyShapePattern = `(?:0[xX])?(?:` +
	`[0-9A-Fa-f]{63,65}|[0-9A-Fa-f]{31,33}|` +
	`[0-9A-Fa-f]{2}(?:[:\- ][0-9A-Fa-f]{2}){14,31}(?:[:\- ][0-9A-Fa-f])?|` +
	`[0-9A-Fa-f]{4}(?:[:\- ][0-9A-Fa-f]{4}){6,15}(?:[:\- ][0-9A-Fa-f]{1,3})?)` +
	`|(?:oj1|orc1|orl1)_[A-Za-z0-9_-]+`

// keyShapedRe is a whole value of keyShapePattern's shape.
var keyShapedRe = regexp.MustCompile(`^(?:` + keyShapePattern + `)$`)

// keyShaped reports whether s, trimmed, is shaped like a key or token. A
// ${VAR} is not: it names where a secret comes from, and shows as written.
func keyShaped(s string) bool { return keyShapedRe.MatchString(strings.TrimSpace(s)) }

// keyShapedInRe is keyShapePattern within a longer text, bounded by what
// is not a hex digit (so '_' splits a token's tail, and a longer run of hex
// digits is not taken apart).
var keyShapedInRe = regexp.MustCompile(`(^|[^0-9A-Fa-f])(` + keyShapePattern + `)([^0-9A-Fa-f]|$)`)

// maskedErr is an error whose text has every key- or token-shaped word
// masked (maskKeyShaped): a key used as a YAML key, a pool's name, or a
// value an error quotes. errors.Is and errors.As still see the error it
// wraps, whose own text is NOT masked: a caller must never show an
// Unwrap()ped target error, or the text of one errors.As found inside it
// (TestNoUnmaskedTargetErrorShown keeps internal/cli and internal/server to
// that).
type maskedErr struct{ err error }

func (e maskedErr) Error() string { return maskKeyShaped(e.err.Error()) }
func (e maskedErr) Unwrap() error { return e.err }

// maskErr is err as its callers see it from the target package: masked
// (maskedErr), nil for nil.
func maskErr(err error) error {
	if err == nil {
		return nil
	}
	return maskedErr{err}
}

// maskKeyShaped is text with every key- or token-shaped word in it masked:
// a value rendered whole (a slice list) or quoted in a reason. A match
// takes the character after it as its boundary, so a key right after
// another is masked on the next pass.
func maskKeyShaped(text string) string {
	for {
		out := keyShapedInRe.ReplaceAllString(text, "${1}"+Masked+"${3}")
		if out == text {
			return out
		}
		text = out
	}
}
