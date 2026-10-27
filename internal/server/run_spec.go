package server

import (
	"sync"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/secrets"
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
	specs map[string]heldSpec
}

// heldSpec is a run's spec, masked, and the secret values masked out of it.
// The values stay in memory for the life of the run only, so the archive can
// be scrubbed of them wherever else they surface (secrets.Scrub).
type heldSpec struct {
	archive *orbitv1.RunArchive
	secrets []string
}

func newSpecStore() *specStore {
	return &specStore{specs: make(map[string]heldSpec)}
}

// put records a run's spec with every secret masked (secrets.Mask: every
// marked field, so credentials and any future marked field, and the keys in
// a scenario document). The value is held as a RunArchive so the oneof is
// built once, here, rather than at archive time.
//
// A report is meant to be saved, attached to a ticket and emailed. Ki and OPc
// are the subscriber's long-term secrets and have no business in one — and an
// archive on disk is exactly the artefact someone would copy around without
// thinking about what is inside it.
//
// A plan is kept whole, with its provenance (the client's, plus extra: the
// server's own entries, such as the gNB IDs it assigned), and masked by the
// same walk: the resolved target's keys and tokens, and any provenance
// entry whose path names a secret.
func (s *specStore) put(id string, m *orbitv1.StartRunRequest, extra ...*orbitv1.FieldProvenance) {
	if s == nil || id == "" {
		return
	}
	holder := &orbitv1.RunArchive{}
	switch spec := m.GetSpec().(type) {
	case *orbitv1.StartRunRequest_Load:
		holder.Spec = &orbitv1.RunArchive_LoadSpec{LoadSpec: proto.Clone(spec.Load).(*orbitv1.LoadRunSpec)}
	case *orbitv1.StartRunRequest_Fleet:
		holder.Spec = &orbitv1.RunArchive_FleetSpec{FleetSpec: proto.Clone(spec.Fleet).(*orbitv1.FleetRunSpec)}
	case *orbitv1.StartRunRequest_Plan:
		plan := proto.Clone(spec.Plan).(*orbitv1.RunPlan)
		holder.Plan = plan
		for _, p := range plan.GetProvenance() {
			holder.Provenance = append(holder.Provenance, proto.Clone(p).(*orbitv1.FieldProvenance))
		}
		holder.Provenance = append(holder.Provenance, extra...)
	default:
		return
	}
	held := heldSpec{archive: holder, secrets: secrets.Mask(holder)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[id] = held
}

// take returns and forgets a run's masked spec and its secret values.
// Forgetting matters: the map would otherwise grow for the life of the
// process, holding a scenario document and keys per run that was started.
func (s *specStore) take(id string) (heldSpec, bool) {
	if s == nil {
		return heldSpec{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.specs[id]
	delete(s.specs, id)
	return h, ok
}

const redacted = secrets.Masked

// redactArchive is the last step before an archive is stored: every marked
// field anywhere in it is masked (secrets.Mask), then every secret value
// the run's request carried is replaced wherever it appears (secrets.Scrub)
// — a key pasted into a scenario comment, an event, an error.
func redactArchive(a *orbitv1.RunArchive, requestSecrets []string) {
	values := append(secrets.Mask(a), requestSecrets...)
	secrets.Scrub(a, values)
}
