package server

import (
	"regexp"
	"sync"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// specStore remembers what each run was asked to do, from StartRun until the
// run is archived.
//
// The registry deliberately does not hold specs — it owns lifecycle, and a
// launcher closure is all it needs to execute. But an archive without the
// configuration says what happened and not what was asked for, which makes two
// runs incomparable: the difference between them is precisely what is absent.
type specStore struct {
	mu    sync.Mutex
	specs map[string]*orbitv1.RunArchive
}

func newSpecStore() *specStore {
	return &specStore{specs: make(map[string]*orbitv1.RunArchive)}
}

// put records a run's spec, redacted. The value is held as a RunArchive so the
// oneof is built once, here, rather than at archive time.
func (s *specStore) put(id string, m *orbitv1.StartRunRequest) {
	if s == nil || id == "" {
		return
	}
	holder := &orbitv1.RunArchive{}
	switch spec := m.GetSpec().(type) {
	case *orbitv1.StartRunRequest_Load:
		holder.Spec = &orbitv1.RunArchive_LoadSpec{LoadSpec: redactLoadSpec(spec.Load)}
	case *orbitv1.StartRunRequest_Fleet:
		holder.Spec = &orbitv1.RunArchive_FleetSpec{FleetSpec: redactFleetSpec(spec.Fleet)}
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[id] = holder
}

// take returns and forgets a run's spec. Forgetting matters: the map would
// otherwise grow for the life of the process, holding a scenario document per
// run that was started.
func (s *specStore) take(id string) *orbitv1.RunArchive {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.specs[id]
	delete(s.specs, id)
	return h
}

// redactLoadSpec copies a load spec without its subscriber credentials.
//
// A report is meant to be saved, attached to a ticket and emailed. Ki and OPc
// are the subscriber's long-term secrets and have no business in one — and an
// archive on disk is exactly the artefact someone would copy around without
// thinking about what is inside it.
func redactLoadSpec(in *orbitv1.LoadRunSpec) *orbitv1.LoadRunSpec {
	if in == nil {
		return nil
	}
	out, _ := proto.Clone(in).(*orbitv1.LoadRunSpec)
	out.Credentials = nil
	return out
}

// redactFleetSpec copies a fleet spec with credential values masked in the
// scenario document.
//
// The server ignores the scenario's own credentials block (it takes keys from
// the request instead), but nothing stops a document from carrying a literal
// key, and storing it verbatim would persist it. Masking is textual so the
// document survives as written — comments, ordering and formatting intact,
// which is the point of keeping it verbatim at all.
func redactFleetSpec(in *orbitv1.FleetRunSpec) *orbitv1.FleetRunSpec {
	if in == nil {
		return nil
	}
	out, _ := proto.Clone(in).(*orbitv1.FleetRunSpec)
	out.ScenarioYaml = redactSecretsInYAML(out.GetScenarioYaml())
	return out
}

// secretKeyLine matches a `ki:`/`opc:` mapping entry and captures everything up
// to and including the colon, so only the VALUE is replaced. Deliberately broad
// — over-masking a field that merely looks like a key costs nothing, while
// under-masking persists a secret.
var secretKeyLine = regexp.MustCompile(`(?mi)^(\s*(?:ki|opc|key|secret|token|password)\s*:\s*)\S.*$`)

const redacted = "<redacted>"

func redactSecretsInYAML(doc string) string {
	if doc == "" {
		return ""
	}
	return secretKeyLine.ReplaceAllString(doc, "${1}"+redacted)
}
