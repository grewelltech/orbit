package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/plan"
)

// PlanRun resolves the target and run file a client sends and builds the
// plan (plan.FromDocuments), as orbit run builds it from the same files.
// It keeps nothing and expands nothing from the server's environment; the
// plan's keys are the request's own. A document or field it refuses is
// InvalidArgument "invalid run: <where>: <reason>".
func (s *runService) PlanRun(_ context.Context, req *connect.Request[orbitv1.ResolveRequest]) (*connect.Response[orbitv1.PlanRunResponse], error) {
	m := req.Msg
	if m.GetRunYaml() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid run: %w", plan.ErrNoRun))
	}
	vars := make(map[string]string, len(m.GetVars()))
	for _, v := range m.GetVars() {
		vars[v.GetName()] = v.GetValue()
	}
	got, err := plan.FromDocuments(plan.Documents{
		TargetYAML: []byte(m.GetTargetYaml()),
		RunYAML:    []byte(m.GetRunYaml()),
		Template:   m.GetTemplate(),
		Sets:       m.GetSets(),
		Gates:      m.GetGates(),
		Vars:       vars,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid run: %w", err))
	}
	p := got.Request.GetPlan()
	return connect.NewResponse(&orbitv1.PlanRunResponse{
		Plan:           p,
		Provenance:     p.GetProvenance(),
		RunName:        got.Request.GetName(),
		EventVerbosity: got.Request.GetEventVerbosity(),
	}), nil
}

// RenderRun gives a run file as the orbit command that runs it and the
// run file to save (plan.Render). Keyless: a secret written out in the run
// file is InvalidArgument naming its path, never its value.
func (s *runService) RenderRun(_ context.Context, req *connect.Request[orbitv1.RenderRunRequest]) (*connect.Response[orbitv1.RenderRunResponse], error) {
	m := req.Msg
	r, err := plan.Render(plan.RenderInput{
		RunYAML:    []byte(m.GetRunYaml()),
		Template:   m.GetTemplate(),
		Target:     m.GetTargetName(),
		TargetYAML: []byte(m.GetTargetYaml()),
		Sets:       m.GetSets(),
		Gates:      m.GetGates(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid run: %w", err))
	}
	return connect.NewResponse(&orbitv1.RenderRunResponse{Command: r.Command, RunYaml: r.RunYAML, FileName: r.FileName}), nil
}
