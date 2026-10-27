// Package secrets finds the secrets in API messages: the fields the proto
// marks [(orbit.v1.secret) = true] (subscriber keys, bearer tokens) and the
// secrets inside YAML documents carried in fields marked
// [(orbit.v1.secret_yaml) = true] (a scenario's credentials). The server
// refuses such a request over plaintext from a non-loopback peer, the CLI
// refuses to send one over plaintext to a non-loopback server, and the
// archive redacts them with the same key detector.
package secrets

import (
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

// YAMLSecrets returns the keys of a YAML document that hold a secret: a
// secret-named key (IsSecretName) with a non-empty scalar value that is not
// an unexpanded ${VAR} reference, in block or flow style. A document that
// does not parse is scanned with the redaction pattern instead, so an odd
// document is never taken to hold none.
func YAMLSecrets(doc string) []string {
	if strings.TrimSpace(doc) == "" {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		var out []string
		for _, m := range secretKeyLine.FindAllStringSubmatch(doc, -1) {
			out = append(out, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[2]), ":")))
		}
		return out
	}
	var out []string
	var visit func(n *yaml.Node, path string)
	visit = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				visit(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				p := k.Value
				if path != "" {
					p = path + "." + k.Value
				}
				if IsSecretName(k.Value) && v.Kind == yaml.ScalarNode && v.Value != "" && !strings.HasPrefix(v.Value, "${") {
					out = append(out, p)
				}
				visit(v, p)
			}
		}
	}
	visit(&root, "")
	return out
}

// Masked is what a redacted value shows as.
const Masked = "<redacted>"

// secretKeyLine matches a secret key's value in YAML text, in block style
// (`ki: …`, `- token: …`) or flow style (`{ ki: …, opc: … }`), capturing the
// key so only the value is replaced. Deliberately broad: over-masking a
// value that merely looks like a key costs nothing, while under-masking
// persists a secret.
var secretKeyLine = regexp.MustCompile(`(?mi)((?:^|[\s{,\-])((?:ki|opc|k|key|secret|token|password|[a-z_]*_token|[a-z_]*_key|[a-z_]*_password|[a-z_]*_secret)\s*:)[ \t]*)([^\s,}\]#][^,}\]\n]*)`)

// RedactYAML masks every secret key's value in a YAML document's text.
func RedactYAML(doc string) string {
	if doc == "" {
		return ""
	}
	return secretKeyLine.ReplaceAllString(doc, "${1}"+Masked)
}
