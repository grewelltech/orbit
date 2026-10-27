// Package secrets finds the secrets in API messages: the fields the proto
// marks [(orbit.v1.secret) = true] (subscriber keys, bearer tokens) and the
// secrets inside YAML documents carried in fields marked
// [(orbit.v1.secret_yaml) = true] (a scenario's credentials). The server
// refuses such a request over plaintext from a non-loopback peer, the CLI
// refuses to send one over plaintext to a non-loopback server, and the
// archive redacts them with the same key detector.
package secrets

import (
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// Names are the field and YAML key names that hold secrets, normalised
// (lower-case, no '-' or '_'). A proto field with one of these names must
// carry the secret option (TestEverySecretNamedFieldIsMarked), and a YAML
// or map key with one holds a secret.
var Names = map[string]bool{
	"ki": true, "opc": true, "k": true, "key": true, "token": true, "password": true, "secret": true,
	"apikey": true, "accesskey": true, "bearer": true, "auth": true, "authorization": true,
	"credential": true, "passwd": true, "passphrase": true, "privatekey": true,
}

// IsSecretName reports whether a field or key name names a secret. The
// name is split into words at '-', '_' and lower-to-upper case changes
// ("X-Api-Key" and "x_api_key" are x, api, key; "apiKey" is api, key), and
// it names a secret when its last words, joined and lower-cased, are one of
// Names: the whole name ("bearer", "API_KEY") or a word-aligned suffix
// ("loom_token", "apiKey", "my-bearer"). A suffix inside a word does not
// count ("monkey" is not a key).
func IsSecretName(name string) bool {
	words := splitWords(name)
	for i := range words {
		if Names[strings.Join(words[i:], "")] {
			return true
		}
	}
	return false
}

// splitWords splits a name into lower-case words at '-', '_', and a
// lower-case letter or digit followed by an upper-case one.
func splitWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '-' || r == '_' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1])):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// Marked reports whether fd carries the secret option.
func Marked(fd protoreflect.FieldDescriptor) bool {
	return optionSet(fd, orbitv1.E_Secret)
}

// secretMapEntry reports whether a map<string, string> entry holds a
// secret: its key names one (IsSecretName) and its value is set. Free-form
// maps (app params) cannot carry the secret option on their entries, so
// the key is the mark.
func secretMapEntry(fd protoreflect.FieldDescriptor, k protoreflect.MapKey, v protoreflect.Value) bool {
	if fd.MapKey().Kind() != protoreflect.StringKind || fd.MapValue().Kind() != protoreflect.StringKind {
		return false
	}
	return IsSecretName(k.String()) && v.String() != "" && v.String() != Masked
}

// provenanceMessage is the provenance entry whose values are guarded by
// path: a value carries no static mark, so an entry whose path names a
// secret field is a secret.
const provenanceMessage protoreflect.FullName = "orbit.v1.FieldProvenance"

// provenanceSecret reports whether m is a FieldProvenance entry, and, when
// its path names a secret field (its last segment, list keys removed,
// IsSecretName) with a value or overridden value still unmasked, that path.
func provenanceSecret(m protoreflect.Message) (path string, isProvenance bool) {
	d := m.Descriptor()
	if d.FullName() != provenanceMessage {
		return "", false
	}
	path = m.Get(d.Fields().ByName("path")).String()
	if !secretPath(path) && !subtreeSecret(m) {
		return "", true
	}
	if v := m.Get(d.Fields().ByName("value")).String(); v != "" && v != Masked {
		return path, true
	}
	over := m.Get(d.Fields().ByName("overridden")).List()
	for i := 0; i < over.Len(); i++ {
		o := over.Get(i).Message()
		if v := o.Get(o.Descriptor().Fields().ByName("value")).String(); v != "" && v != Masked {
			return path, true
		}
	}
	return "", true
}

// subtreeSecret reports whether a provenance entry's value, or a value it
// overrode, is a whole-value subtree (a workload's behaviors, as YAML or
// JSON text) holding a secret-named key: the entry is masked whole
// (ADR-0008 §3), whatever its path says.
func subtreeSecret(m protoreflect.Message) bool {
	for _, v := range provenanceValues(m) {
		if v != Masked && len(YAMLSecretValues(v)) > 0 {
			return true
		}
	}
	return false
}

// provenanceValues are a provenance entry's value and overridden values.
func provenanceValues(m protoreflect.Message) []string {
	d := m.Descriptor()
	out := []string{m.Get(d.Fields().ByName("value")).String()}
	over := m.Get(d.Fields().ByName("overridden")).List()
	for i := 0; i < over.Len(); i++ {
		o := over.Get(i).Message()
		out = append(out, o.Get(o.Descriptor().Fields().ByName("value")).String())
	}
	return out
}

// secretPath reports whether a canonical field path (e.g.
// "subscribers[main].ki") names a secret field.
func secretPath(path string) bool {
	last := path
	if i := strings.LastIndex(path, "."); i >= 0 {
		last = path[i+1:]
	}
	if i := strings.Index(last, "["); i >= 0 {
		last = last[:i]
	}
	return IsSecretName(last)
}

// MarkedYAML reports whether fd carries the secret_yaml option.
func MarkedYAML(fd protoreflect.FieldDescriptor) bool {
	return optionSet(fd, orbitv1.E_SecretYaml)
}

func optionSet(fd protoreflect.FieldDescriptor, ext protoreflect.ExtensionType) bool {
	opts := fd.Options()
	if opts == nil {
		return false
	}
	v, ok := proto.GetExtension(opts, ext).(bool)
	return ok && v
}

// Find returns the paths of the secrets m carries, sorted: each non-empty
// field marked secret, at any depth, and each secret_yaml field whose
// document holds a secret key with a value. Empty means m carries none.
func Find(m proto.Message) []string {
	if m == nil {
		return nil
	}
	var out []string
	walk(m.ProtoReflect(), "", &out)
	sort.Strings(out)
	return out
}

func walk(m protoreflect.Message, prefix string, out *[]string) {
	if p, ok := provenanceSecret(m); ok {
		if p != "" {
			*out = append(*out, prefix+"value ("+p+")")
		}
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := prefix + string(fd.Name())
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				item(fd, l.Get(i), path, out)
			}
		case fd.IsMap():
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if secretMapEntry(fd, k, mv) {
					*out = append(*out, path+"["+k.String()+"]")
					return true
				}
				item(fd.MapValue(), mv, path, out)
				return true
			})
		default:
			item(fd, v, path, out)
		}
		return true
	})
}

func item(fd protoreflect.FieldDescriptor, v protoreflect.Value, path string, out *[]string) {
	switch {
	case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
		walk(v.Message(), path+".", out)
	case Marked(fd) && nonEmpty(fd, v):
		*out = append(*out, path)
	case MarkedYAML(fd) && fd.Kind() == protoreflect.StringKind:
		for _, k := range YAMLSecrets(v.String()) {
			*out = append(*out, path+": "+k)
		}
	}
}

// nonEmpty reports whether a marked field holds a secret: a value that is
// neither empty nor already Masked.
func nonEmpty(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return v.String() != "" && v.String() != Masked
	case protoreflect.BytesKind:
		return len(v.Bytes()) > 0 && string(v.Bytes()) != Masked
	}
	return true
}

// YAMLSecrets returns the paths of a YAML document's secrets (eachSecret),
// in document order. Text that does not parse fails closed: every
// secret-like key name in it is reported, so a request carrying it is
// refused over plaintext rather than guessed to hold none.
func YAMLSecrets(doc string) []string {
	if strings.TrimSpace(doc) == "" {
		return nil
	}
	docs, err := parseYAML(doc)
	if err != nil {
		var out []string
		for _, m := range keyName.FindAllStringSubmatch(doc, -1) {
			if IsSecretName(m[1]) {
				out = append(out, "(unparsed) "+m[1])
			}
		}
		return out
	}
	var out []string
	for _, d := range docs {
		eachSecret(d, func(path string, _ *yaml.Node) { out = append(out, path) })
	}
	return out
}

// Masked is what a redacted value shows as.
const Masked = "<redacted>"

// Unparsed is what RedactYAML stores for text that does not parse: nothing
// of it is kept, since a secret in it cannot be located.
const Unparsed = "<redacted: scenario YAML did not parse>"

// keyName finds candidate key names in text that does not parse.
var keyName = regexp.MustCompile(`["']?([A-Za-z0-9_-]+)["']?[ \t]*:`)

// placeholder is an unexpanded ${VAR} reference, which is no secret.
var placeholder = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// RedactYAML masks every secret in a YAML document (eachSecret) and
// re-encodes it, comments kept. An anchored secret is masked at its
// anchor, so every alias to it reads as masked. Text that does not parse
// is replaced whole by Unparsed.
func RedactYAML(doc string) string {
	if strings.TrimSpace(doc) == "" {
		return doc
	}
	docs, err := parseYAML(doc)
	if err != nil {
		return Unparsed
	}
	for _, d := range docs {
		eachSecret(d, func(_ string, v *yaml.Node) {
			*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: Masked, Anchor: v.Anchor,
				HeadComment: v.HeadComment, LineComment: v.LineComment, FootComment: v.FootComment}
		})
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return Unparsed
		}
	}
	if err := enc.Close(); err != nil {
		return Unparsed
	}
	return b.String()
}

// parseYAML parses every document in doc.
func parseYAML(doc string) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(strings.NewReader(doc))
	var out []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, &n)
	}
}

// maxAliasDepth bounds alias resolution.
const maxAliasDepth = 16

// resolve follows an alias to the node its anchor names.
func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && i < maxAliasDepth; i++ {
		n = n.Alias
	}
	return n
}

// eachSecret calls fn with the path and value node of every secret in a
// parsed document: the value of a secret-named key (IsSecretName), in any
// style (plain, quoted, block | and >, flow), with an alias resolved to its
// anchor's node, so fn gets the node that holds the secret. A scalar that
// is empty, null or a lone ${VAR} placeholder is no secret; a mapping or
// list under a secret-named key is a secret as a whole. Aliases are not
// descended into elsewhere: their anchors are visited where they are
// defined.
func eachSecret(root *yaml.Node, fn func(path string, value *yaml.Node)) {
	var visit func(n *yaml.Node, path string)
	visit = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				visit(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := resolve(n.Content[i]), n.Content[i+1]
				if k == nil {
					continue
				}
				p := k.Value
				if path != "" {
					p = path + "." + k.Value
				}
				if IsSecretName(k.Value) {
					if rv := resolve(v); rv != nil && secretValue(rv) {
						fn(p, rv)
						continue
					}
				}
				visit(v, p)
			}
		}
	}
	visit(root, "")
}

func secretValue(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value != "" && n.Value != Masked && n.Tag != "!!null" && !placeholder.MatchString(n.Value)
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) > 0
	}
	return false
}

// minScrubLen is the shortest secret value Scrub replaces by value: a
// shorter one would mangle unrelated text wherever it happens to occur,
// and is still masked structurally by Mask.
const minScrubLen = 6

// Mask masks, in place, every secret m carries at any depth: each non-empty
// field marked secret becomes Masked, and each secret_yaml document goes
// through RedactYAML. It returns the secret values it masked (the marked
// fields' values and the YAML secrets' scalars), deduplicated, for Scrub
// to remove wherever else they appear.
func Mask(m proto.Message) []string {
	if m == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	mask(m.ProtoReflect(), add)
	sort.Strings(out)
	return out
}

func mask(m protoreflect.Message, add func(string)) {
	if _, ok := provenanceSecret(m); ok {
		maskProvenance(m, add)
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if nv, ok := maskValue(fd, l.Get(i), add); ok {
					l.Set(i, nv)
				}
			}
		case fd.IsMap():
			mp := v.Map()
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if secretMapEntry(fd, k, mv) {
					add(mv.String())
					mp.Set(k, protoreflect.ValueOfString(Masked))
					return true
				}
				if nv, ok := maskValue(fd.MapValue(), mv, add); ok {
					mp.Set(k, nv)
				}
				return true
			})
		default:
			if nv, ok := maskValue(fd, v, add); ok {
				m.Set(fd, nv)
			}
		}
		return true
	})
}

// maskProvenance masks a provenance entry's value and overridden values
// when its path names a secret field.
func maskProvenance(m protoreflect.Message, add func(string)) {
	d := m.Descriptor()
	if !secretPath(m.Get(d.Fields().ByName("path")).String()) && !subtreeSecret(m) {
		return
	}
	// A whole-value subtree (a workload's behaviors) masks whole: the
	// secrets inside it are scrubbed everywhere else, too.
	for _, v := range provenanceValues(m) {
		for _, sec := range YAMLSecretValues(v) {
			add(sec)
		}
	}
	vf := d.Fields().ByName("value")
	if v := m.Get(vf).String(); v != "" && v != Masked {
		add(v)
		m.Set(vf, protoreflect.ValueOfString(Masked))
	}
	over := m.Get(d.Fields().ByName("overridden")).List()
	for i := 0; i < over.Len(); i++ {
		o := over.Get(i).Message()
		ovf := o.Descriptor().Fields().ByName("value")
		if v := o.Get(ovf).String(); v != "" && v != Masked {
			add(v)
			o.Set(ovf, protoreflect.ValueOfString(Masked))
		}
	}
}

// maskValue masks one value of field fd, reporting whether it changed.
func maskValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, add func(string)) (protoreflect.Value, bool) {
	switch {
	case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
		mask(v.Message(), add)
		return v, false
	case Marked(fd) && fd.Kind() == protoreflect.StringKind && v.String() != "":
		add(v.String())
		return protoreflect.ValueOfString(Masked), true
	case Marked(fd) && fd.Kind() == protoreflect.BytesKind && len(v.Bytes()) > 0:
		add(string(v.Bytes()))
		return protoreflect.ValueOfBytes([]byte(Masked)), true
	case MarkedYAML(fd) && fd.Kind() == protoreflect.StringKind && v.String() != "":
		for _, s := range YAMLSecretValues(v.String()) {
			add(s)
		}
		return protoreflect.ValueOfString(RedactYAML(v.String())), true
	}
	return v, false
}

// YAMLSecretValues returns the scalar values of a YAML document's secrets
// (eachSecret), each multi-line one also line by line, so Scrub finds a
// key wherever it was pasted. Text that does not parse yields none: it is
// never stored (RedactYAML replaces it whole).
func YAMLSecretValues(doc string) []string {
	docs, err := parseYAML(doc)
	if err != nil {
		return nil
	}
	var out []string
	var leaves func(n *yaml.Node)
	leaves = func(n *yaml.Node) {
		n = resolve(n)
		if n == nil {
			return
		}
		if n.Kind == yaml.ScalarNode {
			out = append(out, n.Value)
			for _, line := range strings.Split(n.Value, "\n") {
				if t := strings.TrimSpace(line); t != "" && t != n.Value {
					out = append(out, t)
				}
			}
			return
		}
		for _, c := range n.Content {
			leaves(c)
		}
	}
	for _, d := range docs {
		eachSecret(d, func(_ string, v *yaml.Node) { leaves(v) })
	}
	return out
}

// Scrub replaces, in place, every occurrence of each value (at least
// minScrubLen long) in every string and bytes field of m at any depth, map
// values included, with Masked. It is the by-value pass after Mask: a key
// pasted into a YAML comment, an error message or an event is found
// wherever it landed. A hex value (Ki, OPc) is matched in any letter case,
// since a key copied from elsewhere often differs only in that; other
// values, and a key reformatted with spaces or separators, are matched
// exactly.
func Scrub(m proto.Message, values []string) {
	var vs []string
	for _, v := range values {
		if len(v) >= minScrubLen {
			vs = append(vs, v)
		}
	}
	if m == nil || len(vs) == 0 {
		return
	}
	// Longest first, so a value containing another is replaced whole.
	sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	ms := make([]matcher, len(vs))
	for i, v := range vs {
		ms[i] = matcher{exact: v}
		if hexValue.MatchString(v) {
			ms[i].fold = regexp.MustCompile("(?i)" + v) // hex digits only: nothing to quote
		}
	}
	scrub(m.ProtoReflect(), ms)
}

// hexValue is a value made of hex digits only.
var hexValue = regexp.MustCompile(`^[0-9A-Fa-f]+$`)

// matcher finds one secret value: exactly, or for a hex value in any case.
type matcher struct {
	exact string
	fold  *regexp.Regexp
}

func scrubString(s string, ms []matcher) string {
	for _, m := range ms {
		if m.fold != nil {
			s = m.fold.ReplaceAllLiteralString(s, Masked)
		} else {
			s = strings.ReplaceAll(s, m.exact, Masked)
		}
	}
	return s
}

func scrub(m protoreflect.Message, vs []matcher) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if nv, ok := scrubValue(fd, l.Get(i), vs); ok {
					l.Set(i, nv)
				}
			}
		case fd.IsMap():
			mp := v.Map()
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if nv, ok := scrubValue(fd.MapValue(), mv, vs); ok {
					mp.Set(k, nv)
				}
				return true
			})
		default:
			if nv, ok := scrubValue(fd, v, vs); ok {
				m.Set(fd, nv)
			}
		}
		return true
	})
}

func scrubValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, vs []matcher) (protoreflect.Value, bool) {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		scrub(v.Message(), vs)
	case protoreflect.StringKind:
		if s := scrubString(v.String(), vs); s != v.String() {
			return protoreflect.ValueOfString(s), true
		}
	case protoreflect.BytesKind:
		if s := scrubString(string(v.Bytes()), vs); s != string(v.Bytes()) {
			return protoreflect.ValueOfBytes([]byte(s)), true
		}
	}
	return v, false
}
