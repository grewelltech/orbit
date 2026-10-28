package target

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetText is v, the resolved value of the run-file field at path, as the
// text of --set path=<text> that converts back to it. ok is false when
// --set cannot say it: a list of more than one string (--set gives a
// one-element list), a one-element list holding "" (an empty --set clears
// the list), or a target field.
func SetText(path string, v any) (text string, ok bool, err error) {
	_, f, err := lookup(path)
	if err != nil {
		return "", false, err
	}
	if f.target {
		return "", false, nil
	}
	switch f.typ {
	case tString:
		s, isStr := v.(string)
		return s, isStr, nil
	case tUint:
		n, isNum := v.(uint64)
		return strconv.FormatUint(n, 10), isNum, nil
	case tFloat:
		x, isNum := v.(float64)
		return strconv.FormatFloat(x, 'g', -1, 64), isNum, nil
	case tBool:
		b, isBool := v.(bool)
		return strconv.FormatBool(b), isBool, nil
	case tStrings:
		l, isList := v.([]string)
		switch {
		case !isList || len(l) > 1:
			return "", false, nil
		case len(l) == 0:
			return "", true, nil
		case l[0] == "":
			return "", false, nil
		}
		return l[0], true, nil
	case tAny:
		s, err := flowYAML(v)
		if err != nil {
			return "", false, err
		}
		return s, true, nil
	}
	return "", false, nil
}

// flowYAML is v as one line of flow-style YAML, which --set reads back
// (convert) to the same value.
func flowYAML(v any) (string, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return "", fmt.Errorf("%s: %w", Masked, err)
	}
	var flow func(*yaml.Node)
	flow = func(x *yaml.Node) {
		if x.Kind == yaml.MappingNode || x.Kind == yaml.SequenceNode {
			x.Style = yaml.FlowStyle
		}
		for _, c := range x.Content {
			flow(c)
		}
	}
	flow(&n)
	b, err := yaml.Marshal(&n)
	if err != nil {
		return "", fmt.Errorf("%s: %w", Masked, err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// IsRunBool and IsRunString report a run-file field's type, for a
// renderer that must say "unset" with --set: false and "" are how a flag
// unsets these.
func IsRunBool(path string) bool { return runType(path) == tBool }

// IsRunString reports whether the run-file field at path is a string.
func IsRunString(path string) bool { return runType(path) == tString }

func runType(path string) valueType {
	_, f, err := lookup(path)
	if err != nil || f.target {
		return -1
	}
	return f.typ
}

// RunFieldSpec is one run-file field as a client builds the document: its
// path pattern (workloads[].attach.count), value type and whether it is
// 64-bit.
type RunFieldSpec struct {
	Path string `json:"path"`
	Type string `json:"type"` // string, uint, float, bool, strings or any
	Wide bool   `json:"wide,omitempty"`
}

// RunFieldSpecs are the run-file fields, sorted by path: the field table
// a client's run-file editor follows.
func RunFieldSpecs() []RunFieldSpec {
	names := map[valueType]string{tString: "string", tUint: "uint", tFloat: "float", tBool: "bool", tStrings: "strings", tAny: "any"}
	var out []RunFieldSpec
	for p, f := range fields {
		if f.target {
			continue
		}
		out = append(out, RunFieldSpec{Path: p, Type: names[f.typ], Wide: f.wide})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// RunNamedLists are the run file's named lists (workloads, gates), sorted.
func RunNamedLists() []string {
	var out []string
	for p := range namedLists {
		if f, ok := fields[p+"[].name"]; ok && !f.target {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// HoldsSecret reports whether a whole value (a decoded mapping or list)
// has a key that names a secret, whatever its value: a --set must not
// carry one.
func HoldsSecret(v any) bool { return holdsSecret(v) }

// IsWholeValue reports whether the field a path names is a whole-value
// subtree (behaviors, start, conformance.expected), set as YAML text.
func IsWholeValue(path string) bool { return runType(path) == tAny }
