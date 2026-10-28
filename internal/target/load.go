package target

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is one target as loaded from a file: the document and its
// settings, ready to be the target layer.
type File struct {
	Target Target
	// Input is the target layer: every field the file sets, with the file
	// path and line, and the ${ENV} variables a value came from.
	Input Input
	// Err is set for a document in a list that does not load (its Target
	// holds only its name): selecting it fails with Err, while the other
	// documents in the file still load.
	Err error
	// Path and Line are where the document starts.
	Path string
	Line int
}

// DefaultGNBID is the first ID of the default gNB range, which init and
// probe use until leasing (S4) hands IDs out.
const DefaultGNBID = 1

// Entry is one target List found: its file, and whether a target of the
// same name in ./orbit.yaml hides it.
type Entry struct {
	*File
	Shadowed bool
}

// List loads every target in both locations, ./orbit.yaml first: one bad
// document is an entry with its Err, not a failure. A file that does not
// parse at all (a YAML syntax error) is one Err entry for that file.
func List(loc Locations, env Getenv) ([]Entry, error) {
	var out []Entry
	local := map[string]bool{}
	add := func(path string, fromDir bool) {
		files, err := LoadFile(path, env)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			out = append(out, Entry{File: &File{Path: path, Err: err}})
			return
		}
		for _, f := range files {
			if stem := strings.TrimSuffix(filepath.Base(path), ".yaml"); fromDir && f.Err == nil && f.Target.Name != stem {
				// Find looks a config-dir target up by its file name, so this
				// one could not be selected by either name.
				f = &File{Target: f.Target, Path: f.Path, Line: f.Line,
					Err: fmt.Errorf("name %q does not match file %s; rename one of them", f.Target.Name, filepath.Base(path))}
			}
			out = append(out, Entry{File: f, Shadowed: fromDir && f.Target.Name != "" && local[f.Target.Name]})
			if !fromDir {
				local[f.Target.Name] = true
			}
		}
	}
	add(filepath.Join(loc.Dir, "orbit.yaml"), false)
	paths, err := filepath.Glob(filepath.Join(loc.ConfigDir, "targets", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		add(p, true)
	}
	return out, nil
}

// Getenv looks up an environment variable; os.LookupEnv in production.
type Getenv func(string) (string, bool)

// ErrNotFound is returned by Find when no file defines the named target.
var ErrNotFound = errors.New("target not found")

// Locations are where Find looks for a target.
type Locations struct {
	// Dir holds ./orbit.yaml, which may define one target or a list.
	Dir string
	// ConfigDir holds targets/<name>.yaml (~/.config/orbit).
	ConfigDir string
}

// DefaultLocations are the working directory and the user's config
// directory.
func DefaultLocations() (Locations, error) {
	wd, err := os.Getwd()
	if err != nil {
		return Locations{}, err
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return Locations{}, err
	}
	return Locations{Dir: wd, ConfigDir: filepath.Join(cfg, "orbit")}, nil
}

// Find loads the target named name: from ./orbit.yaml when it defines that
// target, else from <ConfigDir>/targets/<name>.yaml.
func Find(name string, loc Locations, env Getenv) (*File, error) {
	if name == "" {
		return nil, errors.New("no target named")
	}
	local := filepath.Join(loc.Dir, "orbit.yaml")
	if _, err := os.Stat(local); err == nil {
		files, err := LoadFile(local, env)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.Target.Name == name {
				if f.Err != nil {
					return nil, f.Err
				}
				return f, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	p := filepath.Join(loc.ConfigDir, "targets", name+".yaml")
	files, err := LoadFile(p, env)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q is in neither %s nor %s", ErrNotFound, name, local, p)
	}
	if err != nil {
		return nil, err
	}
	if len(files) != 1 || files[0].Target.Name != name {
		return nil, fmt.Errorf("%s: want the one target %q", p, name)
	}
	if files[0].Err != nil {
		return nil, files[0].Err
	}
	return files[0], nil
}

// LoadFile reads a target file: one target document, or a list of them.
// Unknown keys are errors, as are a version other than 1, a kind other
// than target, a ran.node other than "", and a ${VAR} that is not set. A
// YAML syntax error fails the whole file; in a list, a document with any
// other error is returned with File.Err set, so the rest still load.
func LoadFile(path string, env Getenv) ([]*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, b, env)
}

// Parse is LoadFile for bytes already read; name labels errors and sources.
func Parse(name string, b []byte, env Getenv) ([]*File, error) {
	files, err := parse(name, b, env)
	for _, f := range files {
		f.Err = maskErr(f.Err)
	}
	return files, maskErr(err)
}

func parse(name string, b []byte, env Getenv) ([]*File, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if line, ok := secondDocument(b); ok {
		return nil, secondDocumentError(name, "a target file", line)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, fmt.Errorf("%s: empty", name)
	}
	doc := root.Content[0]
	var docs []*yaml.Node
	switch doc.Kind {
	case yaml.MappingNode:
		docs = []*yaml.Node{doc}
	case yaml.SequenceNode:
		docs = doc.Content
	default:
		return nil, fmt.Errorf("%s:%d: want a target or a list of targets", name, doc.Line)
	}
	var out []*File
	seen := map[string]bool{}
	for _, d := range docs {
		f, err := parseDoc(name, d, env)
		if err != nil {
			if doc.Kind == yaml.MappingNode {
				return nil, err
			}
			// In a list, a bad document fails only when it is selected.
			f = &File{Target: Target{Name: mapValue(d, "name")}, Err: err}
		}
		f.Path, f.Line = name, d.Line
		if f.Target.Name != "" && seen[f.Target.Name] {
			return nil, fmt.Errorf("%s:%d: target %q is defined twice", name, d.Line, f.Target.Name)
		}
		seen[f.Target.Name] = true
		out = append(out, f)
	}
	return out, nil
}

func parseDoc(name string, d *yaml.Node, env Getenv) (*File, error) {
	if d.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s:%d: a target is a mapping", name, d.Line)
	}
	// Flatten first, so a field's own errors (a quoted number, a leading
	// zero) come with their path and line; then a strict decode catches
	// unknown keys and wrong types it lets through.
	f := &File{Input: Input{Layer: LayerTarget}}
	if err := flatten(name, nil, d, env, targetField, LayerTarget, &f.Input.Settings); err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	enc := yaml.NewEncoder(&raw)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	dec := yaml.NewDecoder(&raw)
	dec.KnownFields(true)
	var t Target
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", name, withoutValues(err, pathsByLine(d)))
	}
	// The expanded values are what the document holds.
	res, err := Resolve(f.Input)
	if err != nil {
		return nil, err
	}
	f.Target = res.Target
	if err := f.Target.Validate(); err != nil {
		return nil, fmt.Errorf("%s:%d: %w", name, d.Line, err)
	}
	return f, nil
}

// targetField and runField pick a target document's fields and a run
// file's.
func targetField(f field) bool { return f.target }
func runField(f field) bool    { return !f.target }

// flatten turns a document node into settings on canonical paths, expanding
// ${ENV} in string values: the fields allow accepts, set in layer. A
// whole-value field (tAny) takes its node whole, mapping or list.
func flatten(file string, prefix []segment, n *yaml.Node, env Getenv, allow func(field) bool, layer Layer, out *[]Setting) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		segs := append(append([]segment(nil), prefix...), segment{key: k.Value})
		pat := pattern(segs)
		switch {
		case namedLists[pat]:
			if v.Kind != yaml.SequenceNode {
				return fmt.Errorf("%s:%d: %s is a list", file, v.Line, pat)
			}
			names := map[string]bool{}
			for _, el := range v.Content {
				elName := mapValue(el, "name")
				if elName == "" {
					return fmt.Errorf("%s:%d: %s entries need a name", file, el.Line, pat)
				}
				if strings.ContainsAny(elName, ".[]") {
					return fmt.Errorf("%s:%d: %s name %q may not contain '.', '[' or ']'", file, el.Line, pat, elName)
				}
				if names[elName] {
					return fmt.Errorf("%s:%d: %s entries need distinct names (%q twice)", file, el.Line, pat, elName)
				}
				names[elName] = true
				es := append(append([]segment(nil), segs...), segment{elem: true, name: elName})
				if err := flatten(file, es, el, env, allow, layer, out); err != nil {
					return err
				}
			}
		case v.Kind == yaml.MappingNode && fields[pat].typ != tAny:
			if _, ok := fields[pat]; ok {
				return fmt.Errorf("%s:%d: %s is not a mapping", file, v.Line, pat)
			}
			if err := flatten(file, segs, v, env, allow, layer, out); err != nil {
				return err
			}
		default:
			f, ok := fields[pat]
			if !ok || !allow(f) {
				return fmt.Errorf("%s:%d: unknown key %s", file, k.Line, pat)
			}
			val, err := nodeValue(f, v)
			if err != nil {
				return fmt.Errorf("%s:%d: %s: %w", file, v.Line, pat, err)
			}
			src := Source{Layer: layer, Name: file, Line: v.Line}
			expanded, via, err := expand(val, env)
			if err != nil {
				return fmt.Errorf("%s:%d: %s: %w", file, v.Line, pat, err)
			}
			src.Via = via
			*out = append(*out, Setting{Path: format(segs), Value: expanded, Source: src})
		}
	}
	return nil
}

// nodeValue decodes a field's node. A string field takes a plain scalar's
// text as written, whatever tag YAML would resolve it to, so 001 stays
// "001" rather than becoming the number 1; the same holds for the entries
// of a string list and for a slice's sd. Other fields decode normally.
func nodeValue(f field, v *yaml.Node) (any, error) {
	switch f.typ {
	case tString:
		if text, ok := scalarText(v); ok {
			return text, nil
		}
	case tStrings:
		if text, ok := scalarText(v); ok {
			return text, nil
		}
		if v.Kind == yaml.SequenceNode {
			out := make([]any, 0, len(v.Content))
			for _, e := range v.Content {
				text, ok := scalarText(e)
				if !ok {
					return nil, fmt.Errorf("list entry on line %d is not a string", e.Line)
				}
				out = append(out, text)
			}
			return out, nil
		}
	case tUint, tFloat:
		if v.Kind == yaml.ScalarNode && envRef.MatchString(v.Value) {
			return nil, fmt.Errorf("%s is not expanded in numeric fields; set it with a flag or an env-layer binding", v.Value)
		}
		if v.Kind == yaml.ScalarNode && v.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			msg := fmt.Sprintf("%s is quoted; write the number unquoted", errValue(f, v.Value))
			if err := leadingZeroText(v.Value); err != nil {
				msg += "; " + err.Error()
			}
			return nil, errors.New(msg)
		}
		if err := leadingZero(v); err != nil {
			return nil, err
		}
	case tSlices:
		if v.Kind == yaml.SequenceNode {
			out := make([]any, 0, len(v.Content))
			for _, e := range v.Content {
				if sst := mapNode(e, "sst"); sst != nil {
					if _, err := nodeValue(field{typ: tUint}, sst); err != nil {
						return nil, fmt.Errorf("slice sst: %w", err)
					}
				}
				var m map[string]any
				if err := e.Decode(&m); err != nil {
					return nil, withoutValues(err, nil)
				}
				if sd := mapNode(e, "sd"); sd != nil {
					if text, ok := scalarText(sd); ok {
						m["sd"] = text
					}
				}
				out = append(out, m)
			}
			return out, nil
		}
	}
	return decodeWhole(v)
}

// decodeWhole decodes a whole-value node as written. A plain scalar that
// YAML 1.1 would read as a timestamp (2026-10-12) stays its text: a run
// file's dates (an inline expected-failures list's added) are strings, and
// a time would come back re-encoded in another form.
func decodeWhole(v *yaml.Node) (any, error) {
	var plain func(*yaml.Node)
	plain = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && n.Style == 0 && n.ShortTag() == "!!timestamp" {
			n.Tag = "!!str"
		}
		for _, c := range n.Content {
			plain(c)
		}
	}
	plain(v)
	var val any
	if err := v.Decode(&val); err != nil {
		return nil, err
	}
	return val, nil
}

var leadingZeroRe = regexp.MustCompile(`^0[0-9]+$`)

// leadingZero rejects a number written with a leading zero, such as 0010:
// YAML reads it as octal, and a reader would take it as decimal, so
// neither guess is safe. Plain 0 and explicit 0x hex are unambiguous and
// pass.
func leadingZero(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Style != 0 {
		return nil
	}
	return leadingZeroText(n.Value)
}

// LeadingZero rejects a number's text written with a leading zero, such
// as 0010, the rule a target file and the env layer apply; a CLI flag
// applies it too. Plain 0, decimal, and prefixed 0x, 0o and 0b pass.
func LeadingZero(s string) error { return leadingZeroText(s) }

// leadingZeroText is leadingZero for a number's text, from a file, an env
// var or a flag.
func leadingZeroText(s string) error {
	if !leadingZeroRe.MatchString(s) {
		return nil
	}
	if keyShaped(s) {
		return fmt.Errorf("%s has a leading zero", Masked)
	}
	dec := strings.TrimLeft(s, "0")
	if dec == "" {
		dec = "0"
	}
	return fmt.Errorf("%s has a leading zero; write %s, or 0x… for hex", s, dec)
}

// scalarText is a non-null scalar's text as written.
func scalarText(n *yaml.Node) (string, bool) {
	if n.Kind != yaml.ScalarNode || n.ShortTag() == "!!null" {
		return "", false
	}
	return n.Value, true
}

// mapNode is the value node of key in a mapping node, nil when absent.
func mapNode(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// mapValue is the scalar value of key in a mapping node, "" when absent.
func mapValue(n *yaml.Node, key string) string {
	if v := mapNode(n, key); v != nil {
		return v.Value
	}
	return ""
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand substitutes ${VAR} in every string of v, returning the variables
// used. A variable that is not set is an error. Only string fields expand:
// a numeric field takes no ${VAR} in v1. A number is overridden by the env
// layer (an ORBIT_* variable bound to its path, such as ORBIT_LOAD_SEED)
// or a flag, both with provenance.
func expand(v any, env Getenv) (any, []string, error) {
	var via []string
	var err error
	sub := func(s string) string {
		return envRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := envRef.FindStringSubmatch(ref)[1]
			val, ok := env(name)
			if !ok {
				if err == nil {
					err = fmt.Errorf("${%s} is not set", name)
				}
				return ""
			}
			if !contains(via, name) {
				via = append(via, name)
			}
			return val
		})
	}
	var walk func(any) any
	walk = func(x any) any {
		switch y := x.(type) {
		case string:
			return sub(y)
		case []any:
			out := make([]any, len(y))
			for i, e := range y {
				out[i] = walk(e)
			}
			return out
		case map[string]any:
			out := make(map[string]any, len(y))
			for k, e := range y {
				out[k] = walk(e)
			}
			return out
		}
		return x
	}
	out := walk(v)
	if err != nil {
		return nil, nil, err
	}
	return out, via, nil
}

// SelectName is the target a run uses: the highest layer that names one
// (ADR-0008 §1: -t/--target, then $ORBIT_TARGET, then the run file's
// target key). "" means none is named.
func SelectName(inputs ...Input) string {
	name, layer := "", Layer(-1)
	for _, in := range inputs {
		for _, s := range in.Settings {
			if s.Path == "target" && in.Layer >= layer {
				if v, ok := s.Value.(string); ok && strings.TrimSpace(v) != "" {
					name, layer = v, in.Layer
				}
			}
		}
	}
	return name
}

// RunFile is a run file (ADR-0008 §2, kind: run): the target it names and
// its settings, ready to be the run-file layer.
type RunFile struct {
	// Target is the file's target: key, "" when it names none.
	Target string
	Input  Input
}

// ParseRunFile reads a run file: a mapping with kind: run (and version 1
// when given), every other key a run-file field (workloads, until, gates,
// run, target). Unknown keys and target fields are errors: a run file
// names its target, it does not restate it. ${ENV} expands in string
// values, as in a target file.
func ParseRunFile(name string, b []byte, env Getenv) (*RunFile, error) {
	rf, err := parseRunFile(name, b, env)
	return rf, maskErr(err)
}

func parseRunFile(name string, b []byte, env Getenv) (*RunFile, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if line, ok := secondDocument(b); ok {
		return nil, secondDocumentError(name, "a run file", line)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, fmt.Errorf("%s: empty", name)
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s:%d: a run file is a mapping", name, doc.Line)
	}
	if k := mapValue(doc, "kind"); k != "run" {
		return nil, fmt.Errorf("%s: kind is %q, want run", name, k)
	}
	if v := mapNode(doc, "version"); v != nil && v.Value != "1" {
		return nil, fmt.Errorf("%s:%d: version %s: want 1", name, v.Line, v.Value)
	}
	// The format markers say what the file is; the rest are fields.
	body := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		switch doc.Content[i].Value {
		case "kind", "version":
		default:
			body.Content = append(body.Content, doc.Content[i], doc.Content[i+1])
		}
	}
	rf := &RunFile{Input: Input{Layer: LayerRunFile}}
	if err := flatten(name, nil, body, env, runField, LayerRunFile, &rf.Input.Settings); err != nil {
		return nil, err
	}
	for _, st := range rf.Input.Settings {
		if st.Path == "target" {
			rf.Target, _ = st.Value.(string)
		}
	}
	return rf, nil
}

// yamlLineRe is the line a yaml error names.
var yamlLineRe = regexp.MustCompile(`line ([0-9]+)`)

// secondDocument reports whether b holds a second YAML document
// (yaml.Unmarshal reads the first and ignores the rest), and the line it
// starts on, or where it fails to parse (0 when yaml names none). A
// document that is empty, such as a trailing "---", is not one; the
// first document's own errors are yaml.Unmarshal's to report.
func secondDocument(b []byte) (int, bool) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for n := 0; ; n++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		switch {
		case errors.Is(err, io.EOF):
			return 0, false
		case err != nil && n == 0:
			return 0, false
		case err != nil:
			// Past the first document: a second one that does not parse.
			line := 0
			if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
				line, _ = strconv.Atoi(m[1])
			}
			return line, true
		case n > 0 && len(doc.Content) > 0 && !emptyNode(doc.Content[0]):
			return doc.Content[0].Line, true
		}
	}
}

// secondDocumentError is the refusal of a file holding a second YAML
// document; what is a file of kind what.
func secondDocumentError(name, what string, line int) error {
	at := name
	if line > 0 {
		at = fmt.Sprintf("%s:%d", name, line)
	}
	return fmt.Errorf("%s: a second YAML document (---); %s is one document, and the rest would be ignored", at, what)
}

// emptyNode reports whether n is a document's nothing: a null scalar with
// no text, as a trailing --- gives.
func emptyNode(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Value == "" && n.ShortTag() == "!!null"
}

// typeErrorRe is one of a yaml.TypeError's entries: its line, the tag
// found, the value quoted (a prefix of it), and the Go type wanted.
var typeErrorRe = regexp.MustCompile("^line ([0-9]+): cannot unmarshal (!![a-z]+)(?: `[^`]*`)? into (.+)$")

// quotedRe is a value a yaml error quotes.
var quotedRe = regexp.MustCompile("`[^`]*`")

// withoutValues is err with no value from the document in it: each entry
// of a yaml.TypeError (which quotes a prefix of the value it could not
// decode: a key pasted into the wrong field) becomes the field's path,
// when at knows the line's, and the type wanted and found. Any other
// error is returned as it is.
func withoutValues(err error, at func(line int) string) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, m := range te.Errors {
		sub := typeErrorRe.FindStringSubmatch(m)
		if sub == nil {
			msgs = append(msgs, quotedRe.ReplaceAllString(m, "a value"))
			continue
		}
		where := ""
		if line, err := strconv.Atoi(sub[1]); err == nil && at != nil {
			if p := at(line); p != "" {
				where = p + ": "
			}
		}
		want, found := goTypeWord(sub[3]), tagWord(sub[2])
		if want == found {
			// The type is right and the value is not: a number too big
			// for its field.
			msgs = append(msgs, fmt.Sprintf("line %s: %sout of range%s", sub[1], where, rangeOf(sub[3])))
			continue
		}
		msgs = append(msgs, fmt.Sprintf("line %s: %swant %s, not %s", sub[1], where, want, found))
	}
	return errors.New(strings.Join(msgs, "; "))
}

// goTypeWord is a Go type a yaml error names, as a document's reader has it.
func goTypeWord(t string) string {
	switch {
	case strings.HasPrefix(t, "uint"), strings.HasPrefix(t, "int"), strings.HasPrefix(t, "float"):
		return "a number"
	case t == "bool":
		return "true or false"
	case t == "string":
		return "a string"
	case strings.HasPrefix(t, "[]"):
		return "a list"
	default:
		return "a mapping"
	}
}

// rangeOf is " (at most N)" for a Go integer type a yaml error names, ""
// for another.
func rangeOf(t string) string {
	bits := map[string]uint{"uint8": 8, "uint16": 16, "uint32": 32, "uint64": 64, "uint": 64, "int8": 7, "int16": 15, "int32": 31, "int64": 63, "int": 63}
	n, ok := bits[t]
	if !ok {
		return ""
	}
	return fmt.Sprintf(" (at most %d)", uint64(1)<<n-1)
}

// tagWord is a YAML tag, as a document's reader has it.
func tagWord(tag string) string {
	switch tag {
	case "!!str":
		return "a string"
	case "!!int", "!!float":
		return "a number"
	case "!!bool":
		return "true or false"
	case "!!seq":
		return "a list"
	case "!!map":
		return "a mapping"
	case "!!null":
		return "null"
	default:
		return tag
	}
}

// pathsByLine is, for document d, the path of the one field whose value
// is on a line ("" when none is, or more than one: a flow mapping's line).
func pathsByLine(d *yaml.Node) func(line int) string {
	byLine := map[int][]string{}
	var walk func(segs []segment, n *yaml.Node)
	walk = func(segs []segment, n *yaml.Node) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(append(append([]segment(nil), segs...), segment{key: n.Content[i].Value}), n.Content[i+1])
			}
		case yaml.SequenceNode:
			for _, e := range n.Content {
				walk(append(append([]segment(nil), segs...), segment{elem: true, name: mapValue(e, "name")}), e)
			}
		default:
			byLine[n.Line] = append(byLine[n.Line], format(segs))
		}
	}
	walk(nil, d)
	return func(line int) string {
		if ps := byLine[line]; len(ps) == 1 {
			return ps[0]
		}
		return ""
	}
}
