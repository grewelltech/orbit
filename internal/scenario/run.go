package scenario

import (
	"context"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// Runner executes a scenario's steps against a live ORBIT API server.
type Runner struct {
	sc     *Scenario
	client orbitv1connect.UEServiceClient
	out    io.Writer
	ues    []UE
	bySUPI map[string]UE
	gnbs   map[string]GNB
}

// NewRunner resolves the scenario's UEs/gNBs and binds an API client.
func NewRunner(sc *Scenario, client orbitv1connect.UEServiceClient, out io.Writer) (*Runner, error) {
	ues, err := sc.ResolveUEs()
	if err != nil {
		return nil, err
	}
	bySUPI := make(map[string]UE, len(ues))
	for _, u := range ues {
		bySUPI[u.SUPI] = u
	}
	gnbs := make(map[string]GNB, len(sc.GNBs))
	for _, g := range sc.GNBs {
		gnbs[g.Name] = g
	}
	return &Runner{sc: sc, client: client, out: out, ues: ues, bySUPI: bySUPI, gnbs: gnbs}, nil
}

// Run executes the steps in order, printing a line per step, and stops at the
// first failure.
func (r *Runner) Run(ctx context.Context) error {
	fmt.Fprintf(r.out, "▶ %s (%d UEs, %d steps)\n", r.name(), len(r.ues), len(r.sc.Steps))
	for i, step := range r.sc.Steps {
		detail, err := r.exec(ctx, step)
		if err != nil {
			fmt.Fprintf(r.out, "✗ [%d] %s — %v\n", i+1, step.Action, err)
			return fmt.Errorf("step %d (%s): %w", i+1, step.Action, err)
		}
		fmt.Fprintf(r.out, "✓ [%d] %s — %s\n", i+1, step.Action, detail)
	}
	fmt.Fprintf(r.out, "✓ %s complete\n", r.name())
	return nil
}

func (r *Runner) name() string {
	if r.sc.Name != "" {
		return r.sc.Name
	}
	return "scenario"
}

func (r *Runner) exec(ctx context.Context, step Step) (string, error) {
	switch step.Action {
	case "register":
		return r.register(ctx, step)
	case "deregister":
		return r.deregister(ctx, step)
	case "ping":
		return r.ping(ctx, step)
	case "traffic":
		return r.traffic(ctx, step)
	case "latency":
		return r.latency(ctx, step)
	case "handover":
		return r.handover(ctx, step)
	case "wait":
		return r.wait(ctx, step)
	default:
		return "", fmt.Errorf("unknown action %q", step.Action)
	}
}

func (r *Runner) gnbProto(g GNB) *orbitv1.GnbConfig {
	return &orbitv1.GnbConfig{
		Id: g.ID, IdBits: 24, Name: g.Name,
		Mcc: r.sc.Core.PLMN.MCC, Mnc: r.sc.Core.PLMN.MNC, Tac: r.sc.Core.TAC,
		Slices: []*orbitv1.Snssai{{Sst: r.sc.Core.Slice.SST, Sd: r.sc.Core.Slice.SD}},
	}
}

// selectUEs resolves a step target: "all" (or empty) → every UE; otherwise a SUPI.
func (r *Runner) selectUEs(target string) ([]UE, error) {
	if target == "" || target == "all" {
		if len(r.ues) == 0 {
			return nil, fmt.Errorf("no UEs defined")
		}
		return r.ues, nil
	}
	u, ok := r.bySUPI[target]
	if !ok {
		return nil, fmt.Errorf("no UE %q in scenario", target)
	}
	return []UE{u}, nil
}

func (r *Runner) register(ctx context.Context, step Step) (string, error) {
	ues, err := r.selectUEs(step.str())
	if err != nil {
		return "", err
	}
	for _, u := range ues {
		req := &orbitv1.RegisterRequest{
			AmfAddress:  r.sc.Core.AMF,
			Supi:        u.SUPI,
			Gnb:         r.gnbProto(u.GNB),
			Credentials: &orbitv1.Credentials{Ki: r.sc.Credentials.Ki, Opc: r.sc.Credentials.OPc},
		}
		if u.PDUSession {
			req.PduSession = &orbitv1.PDUSession{
				PduSessionId: 1, Sst: r.sc.Core.Slice.SST, Sd: r.sc.Core.Slice.SD, Dnn: r.sc.Core.DNN,
			}
			req.GnbN3Addr = u.GNB.N3
		}
		res, err := r.client.Register(ctx, connect.NewRequest(req))
		if err != nil {
			return "", fmt.Errorf("register %s: %w", u.SUPI, err)
		}
		if !res.Msg.GetRegistered() {
			return "", fmt.Errorf("register %s: not registered", u.SUPI)
		}
	}
	return fmt.Sprintf("%d UE(s) registered", len(ues)), nil
}

func (r *Runner) deregister(ctx context.Context, step Step) (string, error) {
	ues, err := r.selectUEs(step.str())
	if err != nil {
		return "", err
	}
	for _, u := range ues {
		if _, err := r.client.Deregister(ctx, connect.NewRequest(&orbitv1.DeregisterRequest{Supi: u.SUPI})); err != nil {
			return "", fmt.Errorf("deregister %s: %w", u.SUPI, err)
		}
	}
	return fmt.Sprintf("%d UE(s) deregistered", len(ues)), nil
}

type pingParams struct {
	UE    string `yaml:"ue"`
	Dst   string `yaml:"dst"`
	Count uint32 `yaml:"count"`
}

func (r *Runner) ping(ctx context.Context, step Step) (string, error) {
	var p pingParams
	if err := step.decode(&p); err != nil {
		return "", err
	}
	if p.Dst == "" {
		p.Dst = "8.8.8.8"
	}
	res, err := r.client.Ping(ctx, connect.NewRequest(&orbitv1.PingRequest{Supi: p.UE, Destination: p.Dst, Count: p.Count}))
	if err != nil {
		return "", err
	}
	m := res.Msg
	if m.GetReceived() == 0 {
		return "", fmt.Errorf("%s: no replies from %s", p.UE, p.Dst)
	}
	return fmt.Sprintf("%s: %d/%d replies from %s (%.1f ms)", p.UE, m.GetReceived(), m.GetSent(), m.GetReplyFrom(), m.GetRttMs()), nil
}

type trafficParams struct {
	UE         string `yaml:"ue"`
	Target     string `yaml:"target"`
	Rate       string `yaml:"rate"`
	PacketSize uint32 `yaml:"packet_size"`
	Duration   string `yaml:"duration"`
}

func (r *Runner) traffic(ctx context.Context, step Step) (string, error) {
	var p trafficParams
	if err := step.decode(&p); err != nil {
		return "", err
	}
	durMs, err := durMs(p.Duration, 5000)
	if err != nil {
		return "", err
	}
	res, err := r.client.Traffic(ctx, connect.NewRequest(&orbitv1.TrafficRequest{
		Supi: p.UE, Target: p.Target, Rate: p.Rate, PacketSize: p.PacketSize, DurationMs: durMs,
	}))
	if err != nil {
		return "", err
	}
	m := res.Msg
	return fmt.Sprintf("%s: %d packets, %.1f Mbps over %dms", p.UE, m.GetPackets(), m.GetMbps(), m.GetDurationMs()), nil
}

type latencyParams struct {
	UE     string `yaml:"ue"`
	Target string `yaml:"target"`
	Probes uint32 `yaml:"probes"`
}

func (r *Runner) latency(ctx context.Context, step Step) (string, error) {
	var p latencyParams
	if err := step.decode(&p); err != nil {
		return "", err
	}
	if p.Target == "" {
		p.Target = "8.8.8.8"
	}
	res, err := r.client.Latency(ctx, connect.NewRequest(&orbitv1.LatencyRequest{Supi: p.UE, Target: p.Target, Probes: p.Probes}))
	if err != nil {
		return "", err
	}
	m := res.Msg
	return fmt.Sprintf("%s: %d/%d replies, rtt %.2f ms, jitter %.2f ms", p.UE, m.GetReceived(), m.GetSent(), m.GetMeanMs(), m.GetJitterMs()), nil
}

type handoverParams struct {
	UE   string `yaml:"ue"`
	To   string `yaml:"to"`
	Type string `yaml:"type"`
}

func (r *Runner) handover(ctx context.Context, step Step) (string, error) {
	var p handoverParams
	if err := step.decode(&p); err != nil {
		return "", err
	}
	g, ok := r.gnbs[p.To]
	if !ok {
		return "", fmt.Errorf("handover target gNB %q not defined", p.To)
	}
	req := connect.NewRequest(&orbitv1.HandoverRequest{
		Supi: p.UE, AmfAddress: r.sc.Core.AMF,
		TargetGnb: r.gnbProto(g), BindAddress: g.Bind, GnbN3Addr: g.N3,
	})
	kind := "N2"
	call := r.client.Handover
	if p.Type == "xn" {
		kind, call = "Xn", r.client.XnHandover
	}
	res, err := call(ctx, req)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s → %s (%s): %s", p.UE, p.To, kind, res.Msg.GetState()), nil
}

func (r *Runner) wait(ctx context.Context, step Step) (string, error) {
	d, err := time.ParseDuration(step.str())
	if err != nil {
		return "", fmt.Errorf("wait: %w", err)
	}
	select {
	case <-time.After(d):
		return d.String(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// durMs parses a duration string ("5s") to milliseconds, or def if empty.
func durMs(s string, def uint32) (uint32, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	return uint32(d.Milliseconds()), nil
}
