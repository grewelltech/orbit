package server

import (
	"context"
	"fmt"

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
// knows neither way is NotFound, as GetRun answers it, and so is one the
// registry still holds, ended, whose archive was evicted; one in the moment
// between the registry having it and its spec being held, Unavailable.
func (s *runService) GetRunConfig(_ context.Context, req *connect.Request[orbitv1.GetRunConfigRequest]) (*connect.Response[orbitv1.GetRunConfigResponse], error) {
	id := req.Msg.GetRunId()
	var held *orbitv1.RunArchive
	if info, err := s.reg.Get(id); err == nil {
		if a, ok := s.specs.peek(id); ok {
			held = a
		} else if a, ok := s.archive.get(id); ok {
			held = a
		} else if info.State.Terminal() {
			// Ended, archived and let go, and the archive (which evicts in
			// the order runs end, not start) has since evicted it: there is
			// nothing left to serve.
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("run %s has ended and its archive was evicted", id))
		} else {
			// Started, its spec not yet held (StartRun puts it just after
			// the registry has the run): ask again in a moment.
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("run %s is starting; try again", id))
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
