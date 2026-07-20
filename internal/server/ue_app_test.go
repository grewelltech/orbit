package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/quality/emodel"
	"github.com/bgrewell/loom/core/rtp"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
)

// stubAppEngine implements the appSessions seam so the RPC handlers can be
// exercised without a Manager, a loomd, or a data path.
type stubAppEngine struct {
	mu        sync.Mutex
	startSUPI string
	startCfg  engine.AppSessionConfig
	startID   string
	startErr  error
	events    []engine.AppSample
	report    engine.AppSessionReport
	stopErr   error
	stoppedID string
}

func (s *stubAppEngine) StartAppSession(ctx context.Context, supi string, cfg engine.AppSessionConfig) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startSUPI, s.startCfg = supi, cfg
	return s.startID, s.startErr
}

func (s *stubAppEngine) AppSessionEvents(id string) (<-chan engine.AppSample, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan engine.AppSample, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, func() {}
}

func (s *stubAppEngine) StopAppSession(ctx context.Context, id string) (engine.AppSessionReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stoppedID = id
	return s.report, s.stopErr
}

// newAppRPCClient serves a ueService whose app-session seam is the stub and
// returns a Connect client against it (streaming works over HTTP/1.1).
func newAppRPCClient(t *testing.T, stub *stubAppEngine) orbitv1connect.UEServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		apps: stub,
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orbitv1connect.NewUEServiceClient(srv.Client(), srv.URL)
}

func testVoIP() metrics.VoIP {
	return metrics.VoIP{
		Codec:      "pcmu",
		TxPackets:  3000,
		RxPackets:  2990,
		Lost:       10,
		Duplicates: 1,
		Reordered:  2,
		LossPct:    0.33,
		DiscardPct: 0.10,
		JitterMs:   1.5,
		RTTMs:      24.0,
		OWDMs:      12.0,
		OWDErrMs:   0.4,
		OWDMethod:  "timesync",
		BurstR:     1.2,
		RFactor:    88.4,
		MOSCQ:      4.31,
		EModel: emodel.Components{
			Ro: 94.77, Is: 1.41, Idte: 0.1, Idle: 0.2, Idd: 0,
			Id: 0.3, Ie: 0, IeEff: 4.6, A: 0, R: 88.4,
		},
		RemoteRFactor: 90.1,
		RemoteMOSCQ:   4.4,
		MediaGaps: []rtp.Gap{{
			Start:       time.Unix(100, 0),
			End:         time.Unix(100, 240e6),
			PacketsLost: 12,
		}},
	}
}

func TestStartAppRPC(t *testing.T) {
	stub := &stubAppEngine{startID: "app-1"}
	client := newAppRPCClient(t, stub)
	ctx := context.Background()

	res, err := client.StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{
		Supi:       "001010000000001",
		App:        "voip",
		Peer:       "n6:9551",
		Token:      "secret",
		PeerDataIp: "10.0.6.2",
		Params:     map[string]string{"codec": "pcmu", "jb_ms": "40"},
		DurationMs: 30000,
	}))
	if err != nil {
		t.Fatalf("StartApp: %v", err)
	}
	if got := res.Msg.GetSessionId(); got != "app-1" {
		t.Errorf("session id = %q, want app-1", got)
	}
	if stub.startSUPI != "001010000000001" {
		t.Errorf("supi = %q", stub.startSUPI)
	}
	cfg := stub.startCfg
	if cfg.App != "voip" || cfg.PeerAgent != "n6:9551" || cfg.Token != "secret" ||
		cfg.PeerDataIP != "10.0.6.2" || cfg.Duration != 30*time.Second {
		t.Errorf("cfg mapped wrong: %+v", cfg)
	}
	if cfg.Params["codec"] != "pcmu" || cfg.Params["jb_ms"] != "40" {
		t.Errorf("params mapped wrong: %v", cfg.Params)
	}

	// Zero duration falls back to 60s.
	if _, err := client.StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{
		Supi: "001010000000001", App: "voip", Peer: "n6:9551",
	})); err != nil {
		t.Fatalf("StartApp default duration: %v", err)
	}
	if stub.startCfg.Duration != 60*time.Second {
		t.Errorf("default duration = %v, want 60s", stub.startCfg.Duration)
	}

	// Missing supi/app is refused before the engine sees it.
	_, err = client.StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{App: "voip"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing supi: code %v, want invalid_argument", connect.CodeOf(err))
	}
	_, err = client.StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{Supi: "x"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing app: code %v, want invalid_argument", connect.CodeOf(err))
	}

	// Engine refusals surface as failed_precondition (the Traffic pattern).
	stub.startErr = errors.New("UE x has no active PDU session")
	_, err = client.StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{
		Supi: "x", App: "voip", Peer: "n6:9551",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("engine error: code %v, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "no active PDU session") {
		t.Errorf("engine error text lost: %v", err)
	}
}

func TestAppStreamRPC(t *testing.T) {
	v := testVoIP()
	t0 := time.Unix(1000, 0)
	stub := &stubAppEngine{
		startID: "app-1",
		events: []engine.AppSample{
			{Time: t0, TimeSource: "local", End: engine.AppEndUE, VoIP: &v},
			{Time: t0.Add(time.Second), TimeErr: 300 * time.Microsecond,
				TimeSource: "timesync", End: engine.AppEndN6, Final: true, VoIP: &v},
			{Time: t0.Add(2 * time.Second), TimeSource: "local",
				Event: "HANDOVER_STARTED", Detail: "to gNB 2"},
		},
	}
	client := newAppRPCClient(t, stub)

	stream, err := client.AppStream(context.Background(),
		connect.NewRequest(&orbitv1.AppStreamRequest{SessionId: "app-1"}))
	if err != nil {
		t.Fatalf("AppStream: %v", err)
	}
	var got []*orbitv1.AppSample
	for stream.Receive() {
		got = append(got, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("received %d samples, want 3", len(got))
	}

	local := got[0]
	if local.GetLocal() == nil || local.GetRemote() != nil || len(local.GetEvents()) != 0 {
		t.Errorf("sample 0 not a pure local sample: %v", local)
	}
	if local.GetUnixNano() != t0.UnixNano() || local.GetTimeSource() != "local" {
		t.Errorf("sample 0 stamp: %d %q", local.GetUnixNano(), local.GetTimeSource())
	}
	lv := local.GetLocal()
	if lv.GetMosCq() != v.MOSCQ || lv.GetRFactor() != v.RFactor || lv.GetJitterMs() != v.JitterMs ||
		lv.GetLossPct() != v.LossPct || lv.GetDiscardPct() != v.DiscardPct ||
		lv.GetRttMs() != v.RTTMs || lv.GetOwdMs() != v.OWDMs || lv.GetOwdErrMs() != v.OWDErrMs ||
		lv.GetOwdMethod() != v.OWDMethod || lv.GetTxPackets() != v.TxPackets ||
		lv.GetRxPackets() != v.RxPackets || lv.GetLost() != v.Lost || lv.GetBurstR() != v.BurstR {
		t.Errorf("voip metrics mapped wrong: %v", lv)
	}
	if em := lv.GetEmodel(); em.GetRo() != v.EModel.Ro || em.GetIeEff() != v.EModel.IeEff || em.GetR() != v.EModel.R {
		t.Errorf("emodel breakdown mapped wrong: %v", lv.GetEmodel())
	}
	if len(lv.GetMediaGaps()) != 1 || lv.GetMediaGaps()[0].GetPacketsLost() != 12 ||
		lv.GetMediaGaps()[0].GetStartUnixNano() != time.Unix(100, 0).UnixNano() {
		t.Errorf("media gaps mapped wrong: %v", lv.GetMediaGaps())
	}

	remote := got[1]
	if remote.GetRemote() == nil || remote.GetLocal() != nil {
		t.Errorf("sample 1 not a pure remote sample: %v", remote)
	}
	if !remote.GetFinal() || remote.GetTimeSource() != "timesync" ||
		remote.GetTimeErrNano() != (300*time.Microsecond).Nanoseconds() {
		t.Errorf("sample 1 restamp fields: final=%t source=%q err=%d",
			remote.GetFinal(), remote.GetTimeSource(), remote.GetTimeErrNano())
	}

	event := got[2]
	if event.GetLocal() != nil || event.GetRemote() != nil || len(event.GetEvents()) != 1 {
		t.Fatalf("sample 2 not a pure event: %v", event)
	}
	ev := event.GetEvents()[0]
	if ev.GetKind() != "HANDOVER_STARTED" || ev.GetDetail() != "to gNB 2" ||
		ev.GetUnixNano() != t0.Add(2*time.Second).UnixNano() {
		t.Errorf("event mapped wrong: %v", ev)
	}

	// A missing session id is refused.
	stream2, err := client.AppStream(context.Background(),
		connect.NewRequest(&orbitv1.AppStreamRequest{}))
	if err == nil {
		for stream2.Receive() {
		}
		err = stream2.Err()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty session_id: code %v, want invalid_argument", connect.CodeOf(err))
	}
}

func TestStopAppRPC(t *testing.T) {
	v := testVoIP()
	remote := testVoIP()
	remote.MediaGaps = []rtp.Gap{{Start: time.Unix(200, 0), End: time.Unix(201, 0), PacketsLost: 50}}
	t0 := time.Unix(1000, 0)
	stub := &stubAppEngine{
		report: engine.AppSessionReport{
			ID: "app-1", SUPI: "001010000000001", App: "voip", PeerAgent: "n6:9551",
			DataPort: 40000, Started: t0, Ended: t0.Add(time.Minute),
			Local: v, Remote: &remote,
			Events: []engine.AppSample{
				{Time: t0.Add(10 * time.Second), Event: "HANDOVER_STARTED", Detail: "to gNB 2"},
				{Time: t0.Add(11 * time.Second), Event: engine.AppEventEndMarker},
			},
			// The engine's report puts both ends' gaps on one timeline with
			// clock provenance (remote gaps re-stamped via TimeSync).
			MediaGaps: []engine.MediaGapReport{
				{End: engine.AppEndUE, Start: time.Unix(100, 0), Stop: time.Unix(100, 240e6),
					PacketsLost: 12, Clock: "local"},
				{End: engine.AppEndN6, Start: time.Unix(197, 0), Stop: time.Unix(198, 0),
					PacketsLost: 50, Clock: "timesync", TimeErr: 1500 * time.Microsecond},
			},
			Err: "voip: no return media within handshake timeout",
		},
	}
	client := newAppRPCClient(t, stub)

	res, err := client.StopApp(context.Background(),
		connect.NewRequest(&orbitv1.StopAppRequest{SessionId: "app-1"}))
	if err != nil {
		t.Fatalf("StopApp: %v", err)
	}
	r := res.Msg
	if stub.stoppedID != "app-1" {
		t.Errorf("engine stopped %q", stub.stoppedID)
	}
	if r.GetSessionId() != "app-1" || r.GetSupi() != "001010000000001" || r.GetApp() != "voip" ||
		r.GetPeer() != "n6:9551" || r.GetDataPort() != 40000 ||
		r.GetStartedUnixNano() != t0.UnixNano() || r.GetEndedUnixNano() != t0.Add(time.Minute).UnixNano() {
		t.Errorf("report header mapped wrong: %v", r)
	}
	if r.GetLocal().GetMosCq() != v.MOSCQ || r.GetRemote().GetMosCq() != remote.MOSCQ {
		t.Errorf("cumulative snapshots mapped wrong")
	}
	if len(r.GetAnnotations()) != 2 ||
		!strings.Contains(r.GetAnnotations()[0], "HANDOVER_STARTED") ||
		!strings.Contains(r.GetAnnotations()[0], "to gNB 2") ||
		!strings.Contains(r.GetAnnotations()[1], engine.AppEventEndMarker) {
		t.Errorf("annotations: %v", r.GetAnnotations())
	}
	gaps := r.GetMediaGaps()
	if len(gaps) != 2 || gaps[0].GetEnd() != engine.AppEndUE || gaps[1].GetEnd() != engine.AppEndN6 ||
		gaps[1].GetPacketsLost() != 50 {
		t.Errorf("media gap summaries: %v", gaps)
	}
	// Clock provenance rides the wire: the ue gap is local, the n6 gap
	// re-stamped with its error bound (never silently presented as aligned).
	if gaps[0].GetClock() != "local" || gaps[1].GetClock() != "timesync" ||
		gaps[1].GetTimeErrNano() != (1500*time.Microsecond).Nanoseconds() ||
		gaps[1].GetStartUnixNano() != time.Unix(197, 0).UnixNano() {
		t.Errorf("media gap clock provenance: %v", gaps)
	}
	if r.GetError() != "voip: no return media within handshake timeout" {
		t.Errorf("error passthrough: %q", r.GetError())
	}

	// An absent remote snapshot stays absent on the wire.
	stub.report.Remote = nil
	res, err = client.StopApp(context.Background(),
		connect.NewRequest(&orbitv1.StopAppRequest{SessionId: "app-1"}))
	if err != nil {
		t.Fatalf("StopApp (no remote): %v", err)
	}
	if res.Msg.GetRemote() != nil {
		t.Error("absent remote snapshot serialized as a zero message")
	}

	// Unknown ids map to not_found.
	stub.stopErr = errors.New("app session app-404 not found")
	_, err = client.StopApp(context.Background(),
		connect.NewRequest(&orbitv1.StopAppRequest{SessionId: "app-404"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown id: code %v, want not_found", connect.CodeOf(err))
	}
}
