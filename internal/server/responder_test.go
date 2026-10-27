package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/join"
)

// Exactly RegisterResponder and Heartbeat skip the API token, matched by
// full name: every other JoinService and ResponderService procedure, a
// near-miss name among them, is refused without it.
func TestResponderProceduresNeedTheToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := join.OpenRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	j := newJoinService(log, newSecurity(log, "sekrit"), Options{Listen: "192.0.2.10:8412"}, reg)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewJoinServiceHandler(j))
	mux.Handle(orbitv1connect.NewResponderServiceHandler(&responderService{log: log, reg: reg}))
	srv := httptest.NewServer(j.sec.middleware(mux))
	t.Cleanup(srv.Close)

	open := map[string]bool{procRegisterResponder: true, procHeartbeat: true}
	var procs []string
	for _, svc := range []string{"JoinService", "ResponderService"} {
		sd := orbitv1.File_orbit_v1_join_proto.Services().ByName(protoreflect.Name(svc))
		for i := 0; i < sd.Methods().Len(); i++ {
			procs = append(procs, "/orbit.v1."+svc+"/"+string(sd.Methods().Get(i).Name()))
		}
	}
	procs = append(procs, procHeartbeat+"x", "/orbit.v1.JoinService/heartbeat", procRegisterResponder+"/")
	for _, p := range procs {
		res, err := http.Post(srv.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		needsToken := res.StatusCode == http.StatusUnauthorized && res.Header.Get("Content-Type") == "application/json"
		b := res.StatusCode
		if open[p] {
			// The handler answers (no TLS here), not the token guard.
			if b == http.StatusUnauthorized {
				t.Errorf("%s: refused for want of the API token", p)
			}
		} else if !needsToken && b != http.StatusUnauthorized {
			t.Errorf("%s: answered %d without the API token", p, b)
		}
	}
	if len(procs) < 7 {
		t.Errorf("only %d procedures checked", len(procs))
	}
}

func registerReq(tok, cred, name string) *orbitv1.RegisterResponderRequest {
	return &orbitv1.RegisterResponderRequest{JoinToken: tok, Credential: cred, Name: name, ControlAddress: "198.51.100.20:9551",
		DataIp: "198.51.100.20", DataPortLow: 32768, DataPortHigh: 60999, Version: "test"}
}

func newCred(t *testing.T) string {
	t.Helper()
	c, err := join.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func beat(l *joinLab, cred string, probe bool) (*connect.Response[orbitv1.HeartbeatResponse], error) {
	req := connect.NewRequest(&orbitv1.HeartbeatRequest{Version: "test", Probe: probe})
	req.Header().Set("Authorization", "Responder "+cred)
	return l.join.Heartbeat(context.Background(), req)
}

// Registration and heartbeats over TLS: the answer carries the ID and loom
// token; a heartbeat counts, a probe does not; a removed responder is told
// so with the removed header; an unknown credential is a miss toward the
// lockout. No secret reaches the log.
func TestRegisterAndHeartbeat(t *testing.T) {
	l := newJoinLab(t, true, "192.0.2.10:8412")
	tok := l.create(t, "", "")
	cred := newCred(t)
	res, err := l.join.RegisterResponder(context.Background(), connect.NewRequest(registerReq(tok.GetSecret(), cred, "app1")))
	if err != nil {
		t.Fatal(err)
	}
	rs := res.Msg
	if !strings.HasPrefix(rs.GetResponderId(), "rs-") || !strings.HasPrefix(rs.GetLoomToken(), join.LoomTokenPrefix) || rs.GetHeartbeatSeconds() != 10 {
		t.Fatalf("registered: %v", rs)
	}
	// The same token, another credential: the uniform answer.
	if _, err := l.join.RegisterResponder(context.Background(), connect.NewRequest(registerReq(tok.GetSecret(), newCred(t), "app2"))); connect.CodeOf(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), msgJoinNotFound) {
		t.Errorf("a used token: %v", err)
	}
	// A probe changes nothing.
	if r, err := beat(l, cred, true); err != nil || r.Msg.GetLastSeenUnixNano() != 0 {
		t.Fatalf("probe: %v, %v", r, err)
	}
	if st := l.j.reg.List(); st[0].State != join.StateNeverSeen {
		t.Errorf("a probe made it %s", st[0].State)
	}
	if _, err := beat(l, cred, false); err != nil {
		t.Fatal(err)
	}
	if r, err := beat(l, cred, true); err != nil || r.Msg.GetLastSeenUnixNano() == 0 || r.Msg.GetResponderId() != rs.GetResponderId() {
		t.Errorf("probe after a heartbeat: %v, %v", r, err)
	}
	if st := l.j.reg.List(); st[0].State != join.StateOnline {
		t.Errorf("after a heartbeat: %s", st[0].State)
	}
	// Removed: told so, and the removed header set.
	if _, err := l.j.reg.Remove("app1"); err != nil {
		t.Fatal(err)
	}
	_, err = beat(l, cred, false)
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Meta().Get(headerResponderState) != responderStateRemoved {
		t.Errorf("removed: %v", err)
	}
	// A missing scheme is refused without counting; unknown credentials
	// are misses, and enough of them lock the peer out.
	req := connect.NewRequest(&orbitv1.HeartbeatRequest{})
	if _, err := l.join.Heartbeat(context.Background(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no credential: %v", err)
	}
	// The used-token registration above was the first miss.
	for i := 0; i < authFailLimit-1; i++ {
		if _, err := beat(l, newCred(t), false); connect.CodeOf(err) != connect.CodeUnauthenticated || !strings.Contains(err.Error(), "does not know this responder") {
			t.Fatalf("unknown %d: %v", i, err)
		}
	}
	if _, err := beat(l, newCred(t), false); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("after %d misses: %v", authFailLimit, err)
	}
	logs := l.log.String()
	for _, secret := range []string{cred, rs.GetLoomToken(), tok.GetSecret(), "Responder "} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

// Without TLS neither procedure answers: both carry secrets.
func TestRegisterAndHeartbeatNeedTLS(t *testing.T) {
	l := newJoinLab(t, false, "192.0.2.10:8412")
	if _, err := l.join.RegisterResponder(context.Background(), connect.NewRequest(registerReq(join.Prefix+"AAAAAAAAAAAAAAAAAAAAAA", newCred(t), "app1"))); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("register: %v", err)
	}
	if _, err := beat(l, newCred(t), false); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("heartbeat: %v", err)
	}
}

// A run's responders named only in the target resolve from the registry:
// an unknown name and an offline responder are refused by name, an online
// one fills a copy of the plan; the plan as sent keeps no token. A
// responder the plan does not use is not looked up.
func TestWithRegistered(t *testing.T) {
	clk := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	reg, err := join.OpenRegistry("", join.WithRegistryClock(func() time.Time { return clk }))
	if err != nil {
		t.Fatal(err)
	}
	store := join.NewStore(join.WithClock(func() time.Time { return clk }))
	_, secret, err := store.Create("", 0, join.Spec{UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "192.0.2.10:8412"})
	if err != nil {
		t.Fatal(err)
	}
	cred := newCred(t)
	rs, err := reg.Register(store, join.RegisterRequest{JoinToken: secret, Credential: cred, Name: "app1", Control: "198.51.100.20:9551",
		DataIP: "198.51.100.20", PortLow: 32768, PortHigh: 60999})
	if err != nil {
		t.Fatal(err)
	}
	s := &runService{responders: reg}
	plan := func(names ...string) *orbitv1.RunPlan {
		p := &orbitv1.RunPlan{Target: &orbitv1.ResolvedTarget{App: &orbitv1.TargetApp{}}}
		for _, n := range names {
			p.Target.App.Responders = append(p.Target.App.Responders, &orbitv1.Responder{Name: n})
		}
		p.Workloads = []*orbitv1.Workload{{Name: "w", Kind: &orbitv1.Workload_Population{Population: &orbitv1.PopulationWorkload{
			Behaviors: &orbitv1.Behaviors{Traffic: &orbitv1.Traffic{Bulk: &orbitv1.BulkTraffic{Responder: names[0]}}}}}}}
		return p
	}
	if _, err := s.withRegisteredPlan(plan("nope")); err == nil || !strings.Contains(err.Error(), "orbit responder list") {
		t.Errorf("unknown: %v", err)
	}
	if _, err := s.withRegisteredPlan(plan("app1")); !errors.Is(err, errResponderOffline) || !strings.Contains(err.Error(), "responder app1 is offline") {
		t.Errorf("never seen: %v", err)
	}
	if _, _, err := reg.Heartbeat(cred, "198.51.100.20:1", "", true, false); err != nil {
		t.Fatal(err)
	}
	sent := plan("app1", "unused")
	got, err := s.withRegisteredPlan(sent)
	if err != nil {
		t.Fatal(err)
	}
	r := got.GetTarget().GetApp().GetResponders()[0]
	if r.GetControl() != rs.Control || r.GetToken() != rs.LoomToken || r.GetDataIp() != rs.DataIP {
		t.Errorf("resolved: %v", r)
	}
	if sent.GetTarget().GetApp().GetResponders()[0].GetToken() != "" {
		t.Error("the plan as sent now holds the loom token")
	}
	clk = clk.Add(31 * time.Second)
	if _, err := s.withRegisteredPlan(plan("app1")); !errors.Is(err, errResponderOffline) || !strings.Contains(err.Error(), "last heartbeat 31s ago") {
		t.Errorf("offline: %v", err)
	}
}

// withRegisteredPlan is withRegistered without its provenance.
func (s *runService) withRegisteredPlan(p *orbitv1.RunPlan) (*orbitv1.RunPlan, error) {
	out, _, err := s.withRegistered(p)
	return out, err
}

// A population's latency probe with no target echoes the first responder:
// a registered one's data IP comes from the registry, recorded in the
// provenance; an explicit target is kept; an offline responder is refused
// before anything is filled.
func TestWithRegisteredLatency(t *testing.T) {
	clk := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	reg, err := join.OpenRegistry("", join.WithRegistryClock(func() time.Time { return clk }))
	if err != nil {
		t.Fatal(err)
	}
	store := join.NewStore(join.WithClock(func() time.Time { return clk }))
	_, secret, err := store.Create("", 0, join.Spec{UEPools: []string{"10.250.0.0/16"}, UPFN6: "198.51.100.1", N6Subnet: "198.51.100.0/24", Management: "192.0.2.10:8412"})
	if err != nil {
		t.Fatal(err)
	}
	cred := newCred(t)
	if _, err := reg.Register(store, join.RegisterRequest{JoinToken: secret, Credential: cred, Name: "app1", Control: "198.51.100.20:9551",
		DataIP: "198.51.100.21", PortLow: 32768, PortHigh: 60999}); err != nil {
		t.Fatal(err)
	}
	s := &runService{responders: reg}
	plan := func(latencyTarget string) *orbitv1.RunPlan {
		return &orbitv1.RunPlan{Target: &orbitv1.ResolvedTarget{App: &orbitv1.TargetApp{Responders: []*orbitv1.Responder{{Name: "app1"}}}},
			Workloads: []*orbitv1.Workload{{Name: "fleet", Kind: &orbitv1.Workload_Population{Population: &orbitv1.PopulationWorkload{
				Behaviors: &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{Target: latencyTarget}}}}}}}
	}
	if _, _, err := s.withRegistered(plan("")); !errors.Is(err, errResponderOffline) {
		t.Errorf("offline: %v", err)
	}
	if _, _, err := reg.Heartbeat(cred, "x", "", true, false); err != nil {
		t.Fatal(err)
	}
	sent := plan("")
	got, prov, err := s.withRegistered(sent)
	if err != nil {
		t.Fatal(err)
	}
	if tgt := got.GetWorkloads()[0].GetPopulation().GetBehaviors().GetLatency().GetTarget(); tgt != "198.51.100.21" {
		t.Errorf("latency target = %q", tgt)
	}
	if len(prov) != 1 || prov[0].GetPath() != "workloads[fleet].behaviors.latency.target" || prov[0].GetSource().GetDetail() != "responder app1 (registry)" || prov[0].GetValue() != "198.51.100.21" {
		t.Errorf("provenance: %v", prov)
	}
	if sent.GetWorkloads()[0].GetPopulation().GetBehaviors().GetLatency().GetTarget() != "" {
		t.Error("the plan as sent changed")
	}
	got, prov, err = s.withRegistered(plan("203.0.113.9"))
	if err != nil || len(prov) != 0 || got.GetWorkloads()[0].GetPopulation().GetBehaviors().GetLatency().GetTarget() != "203.0.113.9" {
		t.Errorf("explicit target: %v, %v, %v", got, prov, err)
	}
}
