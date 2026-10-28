package server

import (
	"context"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/plan"
)

// GetRunConfig serves what a run was asked to run, as resolved: its plan
// and provenance, masked as the archive holds them, and the notices drawn
// from the provenance (an env value that beat a file's). A run the
// registry holds is read from its held spec (peek: the copy is scrubbed,
// the spec left for archiving), or from its archive once archived; a run
// the registry no longer holds, from its archive alone. A run the server
// knows neither way is NotFound, as GetRun answers it.
func (s *runService) GetRunConfig(_ context.Context, req *connect.Request[orbitv1.GetRunConfigRequest]) (*connect.Response[orbitv1.GetRunConfigResponse], error) {
	id := req.Msg.GetRunId()
	var held *orbitv1.RunArchive
	if _, err := s.reg.Get(id); err == nil {
		if a, ok := s.specs.peek(id); ok {
			held = a
		} else if a, ok := s.archive.get(id); ok {
			held = a
		} else {
			held = &orbitv1.RunArchive{} // known, nothing held: started from no spec the store keeps
		}
	} else if a, ok := s.archive.get(id); ok {
		held = a
	} else {
		return nil, runLookupError(err)
	}
	prov := held.GetProvenance()
	resp := &orbitv1.GetRunConfigResponse{
		Plan:       held.GetPlan(),
		Provenance: prov,
		Notices:    plan.OverrideNotices(prov),
		Resolved:   resolvedProvenance(prov),
	}
	return connect.NewResponse(resp), nil
}

// resolvedProvenance reports whether provenance came from the layers being
// resolved: a client's entries, not only the server's own (leases). A plan
// built without the resolver (orbit runs start-load, start-fleet) carries
// none of the client's.
func resolvedProvenance(prov []*orbitv1.FieldProvenance) bool {
	for _, p := range prov {
		if p.GetSource().GetLayer() != orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE {
			return true
		}
	}
	return false
}
