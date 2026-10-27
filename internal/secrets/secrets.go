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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// Names are the field and YAML key names that hold secrets. A proto field
// with one of these names must carry the secret option (TestEverySecret
// NamedFieldIsMarked), and a YAML key with one holds a secret.
var Names = map[string]bool{"ki": true, "opc": true, "k": true, "key": true, "token": true, "password": true, "secret": true}

// IsSecretName reports whether a field or key name names a secret: one of
// Names, or ending in _token, _key, _password or _secret.
func IsSecretName(name string) bool {
	n := strings.ToLower(name)
	if Names[n] {
		return true
	}
	for _, suffix := range []string{"_token", "_key", "_password", "_secret"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// Marked reports whether fd carries the secret option.
func Marked(fd protoreflect.FieldDescriptor) bool {
	return optionSet(fd, orbitv1.E_Secret)
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
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := prefix + string(fd.Name())
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				item(fd, l.Get(i), path, out)
			}
		case fd.IsMap():
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
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

func nonEmpty(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return v.String() != ""
	case protoreflect.BytesKind:
		return len(v.Bytes()) > 0
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
		return n.Value != "" && n.Tag != "!!null" && !placeholder.MatchString(n.Value)
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) > 0
	}
	return false
}
