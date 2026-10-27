package target

import (
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/secrets"
)

// Every canonical path T1 resolves has a field in the wire schema
// (ADR-0008 §2): a target path in ResolvedTarget, run.* in StartRunRequest,
// and the rest in RunPlan. A workload path may leave out its kind
// ("workloads[].attach.count" is workloads[i].population.attach.count)
// when the field exists in only one kind.
func TestEveryPathHasAProtoField(t *testing.T) {
	paths := make([]string, 0, len(fields))
	for p := range fields {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if fileOnly[p] {
			continue
		}
		root, rest := rootFor(p)
		if _, err := protoPath(root, rest); err != "" {
			t.Errorf("%s: %s", p, err)
		}
	}
}

// fileOnly are a target file's format markers, which say what the file is
// and a resolved target on the wire has no use for, and a run file's own
// spellings that Build turns into a field (until.duration, "2h", into
// until.duration_ms).
var fileOnly = map[string]bool{"version": true, "kind": true, "until.duration": true}

// protoPath walks a dotted path ("[...]" list keys dropped) from md,
// returning the field the last segment names, or why it does not resolve.
func protoPath(md protoreflect.MessageDescriptor, path string) (protoreflect.FieldDescriptor, string) {
	segs := strings.Split(path, ".")
	var fd protoreflect.FieldDescriptor
	for i, seg := range segs {
		if j := strings.Index(seg, "["); j >= 0 {
			seg = seg[:j]
		}
		if md == nil {
			return nil, segs[i-1] + " is not a message"
		}
		fd = md.Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			fd = viaKind(md, seg)
		}
		if fd == nil {
			return nil, "no field " + seg + " in " + string(md.FullName())
		}
		md = nil
		if fd.Kind() == protoreflect.MessageKind {
			md = fd.Message()
		}
	}
	return fd, ""
}

// rootFor is where a canonical path starts in the wire schema, and the
// rest of the path below it.
func rootFor(p string) (protoreflect.MessageDescriptor, string) {
	switch {
	case fields[p].target:
		return (&orbitv1.ResolvedTarget{}).ProtoReflect().Descriptor(), p
	case strings.HasPrefix(p, "run."):
		return (&orbitv1.StartRunRequest{}).ProtoReflect().Descriptor(), strings.TrimPrefix(p, "run.")
	}
	return (&orbitv1.RunPlan{}).ProtoReflect().Descriptor(), p
}

// T1's secret-tagged paths are exactly the paths whose proto field is
// marked (orbit.v1.secret): provenance masking keys on the name, so this
// pins that the two never diverge.
func TestSecretPathsMatchProtoMarks(t *testing.T) {
	for p, f := range fields {
		if fileOnly[p] {
			continue
		}
		root, rest := rootFor(p)
		fd, why := protoPath(root, rest)
		if why != "" {
			continue // TestEveryPathHasAProtoField reports it
		}
		if marked := secrets.Marked(fd); marked != f.secret {
			t.Errorf("%s: T1 secret %t, proto field %s marked secret %t", p, f.secret, fd.FullName(), marked)
		}
		if f.secret && !secrets.IsSecretName(lastSeg(p)) {
			t.Errorf("%s is secret but its name is not a secret name: provenance masking (by name) would miss it", p)
		}
	}
}

// viaKind finds seg in a Workload's kind messages: a kind-less path is the
// population's (ADR-0008 §2), else the one kind that has it.
func viaKind(md protoreflect.MessageDescriptor, seg string) protoreflect.FieldDescriptor {
	if md.FullName() != "orbit.v1.Workload" {
		return nil
	}
	if fd := md.Fields().ByName("population").Message().Fields().ByName(protoreflect.Name(seg)); fd != nil {
		return fd
	}
	var found protoreflect.FieldDescriptor
	kind := md.Oneofs().ByName("kind")
	for i := 0; i < kind.Fields().Len(); i++ {
		if fd := kind.Fields().Get(i).Message().Fields().ByName(protoreflect.Name(seg)); fd != nil {
			if found != nil {
				return nil // ambiguous: the path must name the kind
			}
			found = fd
		}
	}
	return found
}

func lastSeg(path string) string {
	s := path[strings.LastIndex(path, ".")+1:]
	if i := strings.Index(s, "["); i >= 0 {
		s = s[:i]
	}
	return s
}
