package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/join"
)

// The two procedures an app host calls without the API token (ADR-0009
// §3): the join token, then the responder credential, is the credential.
// security.protected exempts exactly these, by full name.
const (
	procRegisterResponder = "/orbit.v1.JoinService/RegisterResponder"
	procHeartbeat         = "/orbit.v1.JoinService/Heartbeat"
)

// Heartbeat refusals and the header that marks a removed responder, which
// is told to stop (rather than keep trying as for an unknown credential).
const (
	responderAuthScheme   = "Responder "
	msgResponderUnknown   = "the server does not know this responder; re-join with orbit join create, or restore the server's state dir"
	headerResponderState  = "Orbit-Responder-State"
	responderStateRemoved = "removed"
)

// RegisterResponder registers an app host's responder with a join token.
func (j *joinService) RegisterResponder(ctx context.Context, req *connect.Request[orbitv1.RegisterResponderRequest]) (*connect.Response[orbitv1.RegisterResponderResponse], error) {
	if cs, _ := ctx.Value(connStateKey{}).(connState); !j.tls || !cs.tls {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msgJoinNeedsTLS))
	}
	source := req.Peer().Addr
	peer := peerKey(source)
	if j.sec.lockedOut(peer) || !j.admit(peer) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New(msgJoinBusy))
	}
	m := req.Msg
	rs, err := j.reg.Register(j.store, join.RegisterRequest{JoinToken: m.GetJoinToken(), Credential: m.GetCredential(),
		ExistingCredential: m.GetExistingCredential(), Name: m.GetName(), Control: m.GetControlAddress(), DataIP: m.GetDataIp(),
		PortLow: m.GetDataPortLow(), PortHigh: m.GetDataPortHigh(), Version: m.GetVersion(), Source: source})
	var taken *join.NameTakenError
	switch {
	case errors.Is(err, join.ErrUnknownToken):
		j.sec.failWith(peer, "unknown join token at registration")
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(msgJoinNotFound))
	case errors.As(err, &taken):
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, join.ErrUnknownExisting):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil && strings.Contains(err.Error(), "did not persist"):
		j.log.Error("responder registry", "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	j.log.Info("responder registered", "id", rs.ID, "name", rs.Name, "peer", peer, "join_token", rs.JoinTokenID)
	return connect.NewResponse(&orbitv1.RegisterResponderResponse{ResponderId: rs.ID, Name: rs.Name, LoomToken: rs.LoomToken,
		HeartbeatSeconds: uint32(join.HeartbeatInterval.Seconds())}), nil
}

// Heartbeat records that a registered responder is alive. The credential
// is in the Authorization header; nothing logs it.
func (j *joinService) Heartbeat(ctx context.Context, req *connect.Request[orbitv1.HeartbeatRequest]) (*connect.Response[orbitv1.HeartbeatResponse], error) {
	if cs, _ := ctx.Value(connStateKey{}).(connState); !j.tls || !cs.tls {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msgJoinNeedsTLS))
	}
	source := req.Peer().Addr
	peer := peerKey(source)
	if j.sec.lockedOut(peer) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New(msgLockedOut))
	}
	h := req.Header().Get("Authorization")
	if len(h) < len(responderAuthScheme) || !strings.EqualFold(h[:len(responderAuthScheme)], responderAuthScheme) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("a responder credential is required (Authorization: Responder <credential>)"))
	}
	m := req.Msg
	id, prev, err := j.reg.Heartbeat(strings.TrimSpace(h[len(responderAuthScheme):]), source, m.GetVersion(), m.GetLoomListening(), m.GetProbe())
	switch {
	case errors.Is(err, join.ErrResponderRemoved):
		e := connect.NewError(connect.CodeUnauthenticated, join.ErrResponderRemoved)
		e.Meta().Set(headerResponderState, responderStateRemoved)
		return nil, e
	case err != nil:
		j.sec.failWith(peer, "unknown responder credential")
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(msgResponderUnknown))
	}
	out := &orbitv1.HeartbeatResponse{ResponderId: id, HeartbeatSeconds: uint32(join.HeartbeatInterval.Seconds())}
	if !prev.IsZero() {
		out.LastSeenUnixNano = prev.UnixNano()
	}
	return connect.NewResponse(out), nil
}

// responderService lists and removes registered responders.
type responderService struct {
	log *slog.Logger
	reg *join.Registry
}

func (s *responderService) ListResponders(context.Context, *connect.Request[orbitv1.ListRespondersRequest]) (*connect.Response[orbitv1.ListRespondersResponse], error) {
	out := &orbitv1.ListRespondersResponse{}
	for _, st := range s.reg.List() {
		r := &orbitv1.RegisteredResponder{Id: st.ID, Name: st.Name, State: st.State, Source: st.LastSource,
			ControlAddress: st.Control, DataIp: st.DataIP, DataPortLow: st.PortLow, DataPortHigh: st.PortHigh,
			Version: st.Version, RegisteredUnixNano: st.Registered.UnixNano(), RegisteredFrom: st.RegisteredFrom,
			JoinTokenId: st.JoinTokenID}
		if !st.LastSeen.IsZero() {
			r.LastSeenUnixNano = st.LastSeen.UnixNano()
		}
		out.Responders = append(out.Responders, r)
	}
	return connect.NewResponse(out), nil
}

func (s *responderService) RemoveResponder(_ context.Context, req *connect.Request[orbitv1.RemoveResponderRequest]) (*connect.Response[orbitv1.RemoveResponderResponse], error) {
	id, err := s.reg.Remove(req.Msg.GetName())
	switch {
	case errors.Is(err, join.ErrNoResponder):
		return nil, connect.NewError(connect.CodeNotFound, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	s.log.Info("responder removed", "id", id, "name", req.Msg.GetName())
	return connect.NewResponse(&orbitv1.RemoveResponderResponse{Id: id}), nil
}

// errResponderOffline marks a plan refused because a responder it uses is
// registered but not online: FailedPrecondition, not InvalidArgument.
var errResponderOffline = errors.New("responder offline")

// offlineError is the registry's offline error, which errors.Is matches as
// errResponderOffline.
type offlineError struct{ error }

func (offlineError) Is(target error) bool { return target == errResponderOffline }

// referencedResponders is the target responder names a plan's behaviours
// use.
func referencedResponders(p *orbitv1.RunPlan) map[string]bool {
	names := map[string]bool{}
	for _, w := range p.GetWorkloads() {
		tr := w.GetPopulation().GetBehaviors().GetTraffic()
		for _, share := range tr.GetMix() {
			if n := share.GetApp().GetResponder(); n != "" {
				names[n] = true
			}
		}
		if n := tr.GetBulk().GetResponder(); n != "" {
			names[n] = true
		}
	}
	return names
}

// withRegistered fills in the responders a plan uses that its target names
// only (`{name: app1}`, no control or token): each from the registry, as
// its control address, loom token and data IP. A population's latency probe
// with no target echoes the target's first responder, as a scenario's
// omitted latency.target does; when that responder is a registered one its
// data IP comes from the registry here, recorded in the provenance returned
// as "responder <name> (registry)". It works on a copy: the plan a client
// sent, and so GetRun and the archive, never hold the token. A name no
// responder registered is an error naming orbit responder list; a
// registered one that is not online wraps errResponderOffline, before
// anything else is built.
func (s *runService) withRegistered(p *orbitv1.RunPlan) (*orbitv1.RunPlan, []*orbitv1.FieldProvenance, error) {
	used := referencedResponders(p)
	responders := p.GetTarget().GetApp().GetResponders()
	// A latency probe with no target echoes the first responder.
	latencyFrom := ""
	for _, w := range p.GetWorkloads() {
		if l := w.GetPopulation().GetBehaviors().GetLatency(); l != nil && l.GetTarget() == "" && len(responders) > 0 {
			latencyFrom = responders[0].GetName()
			used[latencyFrom] = true
		}
	}
	var out *orbitv1.RunPlan
	resolved := map[string]join.Responder{}
	for i, r := range responders {
		if !used[r.GetName()] || r.GetControl() != "" || r.GetToken() != "" {
			continue
		}
		if s.responders == nil {
			return nil, nil, fmt.Errorf("no responder named %q is registered (orbit responder list)", r.GetName())
		}
		rs, err := s.responders.Resolve(r.GetName())
		var off *join.OfflineError
		switch {
		case errors.As(err, &off):
			return nil, nil, offlineError{err}
		case err != nil:
			return nil, nil, fmt.Errorf("no responder named %q is registered (orbit responder list)", r.GetName())
		}
		if out == nil {
			out = proto.Clone(p).(*orbitv1.RunPlan)
		}
		got := out.GetTarget().GetApp().GetResponders()[i]
		got.Control, got.Token = rs.Control, rs.LoomToken
		if got.GetDataIp() == "" {
			got.DataIp = rs.DataIP
		}
		resolved[r.GetName()] = rs
	}
	// The latency target: a registered responder's data IP from the
	// registry, or an explicit one's own data_ip from the target, as the
	// scenario path takes it.
	var prov []*orbitv1.FieldProvenance
	ip, source, layer := "", "", orbitv1.ProvenanceLayer_PROVENANCE_LAYER_UNSPECIFIED
	if rs, ok := resolved[latencyFrom]; ok && latencyFrom != "" {
		ip, source, layer = rs.DataIP, "responder "+latencyFrom+" (registry)", orbitv1.ProvenanceLayer_PROVENANCE_LAYER_LEASE
	} else if latencyFrom != "" && responders[0].GetDataIp() != "" {
		ip, source, layer = responders[0].GetDataIp(), "responder "+latencyFrom+" (target)", orbitv1.ProvenanceLayer_PROVENANCE_LAYER_TARGET
	}
	if ip != "" {
		if out == nil {
			out = proto.Clone(p).(*orbitv1.RunPlan)
		}
		for _, w := range out.GetWorkloads() {
			if l := w.GetPopulation().GetBehaviors().GetLatency(); l != nil && l.GetTarget() == "" {
				l.Target = ip
				prov = append(prov, &orbitv1.FieldProvenance{
					Path:   "workloads[" + w.GetName() + "].behaviors.latency.target",
					Source: &orbitv1.ProvenanceSource{Layer: layer, Detail: source},
					Value:  ip,
				})
			}
		}
	}
	if out == nil {
		return p, nil, nil
	}
	return out, prov, nil
}
