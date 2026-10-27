package target

import (
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
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
		var root protoreflect.MessageDescriptor
		rest := p
		switch {
		case fields[p].target:
			root = (&orbitv1.ResolvedTarget{}).ProtoReflect().Descriptor()
		case strings.HasPrefix(p, "run."):
			root = (&orbitv1.StartRunRequest{}).ProtoReflect().Descriptor()
			rest = strings.TrimPrefix(p, "run.")
		default:
			root = (&orbitv1.RunPlan{}).ProtoReflect().Descriptor()
		}
		if err := protoPath(root, rest); err != "" {
			t.Errorf("%s: %s", p, err)
		}
	}
}

// fileOnly are a target file's format markers: they say what the file is,
// and a resolved target on the wire has no use for them.
var fileOnly = map[string]bool{"version": true, "kind": true}

// protoPath walks a dotted path ("[...]" list keys dropped) from md,
// returning "" when every segment is a field.
func protoPath(md protoreflect.MessageDescriptor, path string) string {
	for _, seg := range strings.Split(path, ".") {
		if i := strings.Index(seg, "["); i >= 0 {
			seg = seg[:i]
		}
		fd := md.Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			fd = viaKind(md, seg)
		}
		if fd == nil {
			return "no field " + seg + " in " + string(md.FullName())
		}
		if fd.Kind() == protoreflect.MessageKind {
			md = fd.Message()
		} else {
			md = nil
		}
		if md == nil && seg != lastSeg(path) {
			return seg + " is not a message"
		}
	}
	return ""
}

// viaKind finds seg in exactly one of a Workload's kind messages.
func viaKind(md protoreflect.MessageDescriptor, seg string) protoreflect.FieldDescriptor {
	if md.FullName() != "orbit.v1.Workload" {
		return nil
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
