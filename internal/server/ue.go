package server

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"connectrpc.com/connect"

	"github.com/bgrewell/loom/core/metrics"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ueService adapts the engine.Manager to the UEService RPCs.
type ueService struct {
	log  *slog.Logger
	mgr  *engine.Manager
	apps appSessions
}

// appSessions is the app-session slice of the engine.Manager surface, split
// out as an interface so the handler tests can drive the RPCs against a stub
// engine (the rest of ueService keeps the concrete Manager).
type appSessions interface {
	StartAppSession(ctx context.Context, supi string, cfg engine.AppSessionConfig) (string, error)
	AppSessionEvents(id string) (<-chan engine.AppSample, func())
	StopAppSession(ctx context.Context, id string) (engine.AppSessionReport, error)
}

func (s *ueService) Register(
	ctx context.Context,
	req *connect.Request[orbitv1.RegisterRequest],
) (*connect.Response[orbitv1.RegisterResponse], error) {
	m := req.Msg
	if m.GetAmfAddress() == "" || m.GetSupi() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("amf_address and supi are required"))
	}
	gnbCfg, err := gnbConfigFromProto(m.GetGnb())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ki, err := auth.ParseHexKey("Ki", m.GetCredentials().GetKi())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	opc, err := auth.ParseHexKey("OPc", m.GetCredentials().GetOpc())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	id, err := ue.ParseIdentity(m.GetSupi(), gnbCfg.MCC, gnbCfg.MNC, m.GetRoutingIndicator())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ueCfg := engine.UEConfig{
		Identity:  id,
		Sub:       auth.Subscription{SUPI: m.GetSupi(), Ki: ki, OPc: opc},
		GNBN3Addr: m.GetGnbN3Addr(),
	}
	if p := m.GetPduSession(); p != nil {
		if p.GetSst() > 0xFF {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("sst %d exceeds one octet", p.GetSst()))
		}
		ueCfg.PDUSession = &ue.PDUSessionParams{
			PDUSessionID: uint8(p.GetPduSessionId()),
			SST:          uint8(p.GetSst()),
			SD:           p.GetSd(),
			DNN:          p.GetDnn(),
		}
	}

	ctx, cancel := withTimeout(ctx, m.GetTimeoutMs(), 20000)
	defer cancel()

	res, err := s.mgr.Register(ctx, m.GetAmfAddress(), gnbCfg, ueCfg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&orbitv1.RegisterResponse{
		Supi:          res.SUPI,
		Registered:    res.Registered,
		AmfUeNgapId:   res.AMFUENGAPID,
		SessionActive: res.SessionActive,
		PduAddress:    res.PDUAddress,
		UpfAddress:    res.UPFAddress,
		UpfTeid:       res.UPFTEID,
	}), nil
}

func (s *ueService) Deregister(
	ctx context.Context,
	req *connect.Request[orbitv1.DeregisterRequest],
) (*connect.Response[orbitv1.DeregisterResponse], error) {
	if err := s.mgr.Deregister(ctx, req.Msg.GetSupi()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&orbitv1.DeregisterResponse{}), nil
}

func (s *ueService) Status(
	ctx context.Context,
	req *connect.Request[orbitv1.StatusRequest],
) (*connect.Response[orbitv1.StatusResponse], error) {
	sess, err := s.mgr.Status(req.Msg.GetSupi())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&orbitv1.StatusResponse{Status: ueStatusProto(sess)}), nil
}

func (s *ueService) List(
	ctx context.Context,
	req *connect.Request[orbitv1.ListRequest],
) (*connect.Response[orbitv1.ListResponse], error) {
	var out []*orbitv1.UEStatus
	for _, sess := range s.mgr.List() {
		out = append(out, ueStatusProto(sess))
	}
	return connect.NewResponse(&orbitv1.ListResponse{Ues: out}), nil
}

func (s *ueService) StateStream(
	ctx context.Context,
	req *connect.Request[orbitv1.StateStreamRequest],
	stream *connect.ServerStream[orbitv1.StateEvent],
) error {
	filter := req.Msg.GetSupi()
	events, cancel := s.mgr.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if filter != "" && ev.SUPI != filter {
				continue
			}
			if err := stream.Send(&orbitv1.StateEvent{
				Supi:     ev.SUPI,
				State:    ev.State,
				Detail:   ev.Detail,
				UnixNano: ev.Time.UnixNano(),
			}); err != nil {
				return err
			}
		}
	}
}

func (s *ueService) Ping(
	ctx context.Context,
	req *connect.Request[orbitv1.PingRequest],
) (*connect.Response[orbitv1.PingResponse], error) {
	res, err := s.mgr.Ping(req.Msg.GetSupi(), req.Msg.GetDestination(), int(req.Msg.GetCount()))
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&orbitv1.PingResponse{
		Sent:      uint32(res.Sent),
		Received:  uint32(res.Received),
		RttMs:     float64(res.LastRTT.Microseconds()) / 1000.0,
		ReplyFrom: res.ReplyFrom,
	}), nil
}

func (s *ueService) Traffic(
	ctx context.Context,
	req *connect.Request[orbitv1.TrafficRequest],
) (*connect.Response[orbitv1.TrafficResponse], error) {
	m := req.Msg
	if m.GetSupi() == "" || m.GetTarget() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("supi and target are required"))
	}
	dur := time.Duration(m.GetDurationMs()) * time.Millisecond
	if dur <= 0 {
		dur = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, dur+15*time.Second)
	defer cancel()

	res, err := s.mgr.Traffic(ctx, m.GetSupi(), m.GetTarget(), m.GetRate(), int(m.GetPacketSize()), dur)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&orbitv1.TrafficResponse{
		Bytes: res.Bytes, Packets: res.Packets, Mbps: res.Mbps,
		DurationMs: uint32(res.Duration.Milliseconds()),
	}), nil
}

func (s *ueService) Latency(
	ctx context.Context,
	req *connect.Request[orbitv1.LatencyRequest],
) (*connect.Response[orbitv1.LatencyResponse], error) {
	m := req.Msg
	if m.GetSupi() == "" || m.GetTarget() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("supi and target are required"))
	}
	spacing := time.Duration(m.GetSpacingMs()) * time.Millisecond
	timeout := time.Duration(m.GetTimeoutMs()) * time.Millisecond
	res, err := s.mgr.Latency(ctx, m.GetSupi(), m.GetTarget(), int(m.GetProbes()), spacing, timeout)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	return connect.NewResponse(&orbitv1.LatencyResponse{
		Sent: res.Sent, Received: res.Received, Lost: res.Lost, LossPct: res.LossPct,
		MinMs: ms(res.Min), MeanMs: ms(res.Mean), MaxMs: ms(res.Max), JitterMs: ms(res.Jitter),
	}), nil
}

func (s *ueService) DataStats(
	ctx context.Context,
	req *connect.Request[orbitv1.DataStatsRequest],
) (*connect.Response[orbitv1.DataStatsResponse], error) {
	if req.Msg.GetSupi() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("supi is required"))
	}
	stats, err := s.mgr.DataStats(req.Msg.GetSupi())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	qfis := make([]uint8, 0, len(stats))
	for qfi := range stats {
		qfis = append(qfis, qfi)
	}
	sort.Slice(qfis, func(i, j int) bool { return qfis[i] < qfis[j] })
	res := &orbitv1.DataStatsResponse{}
	for _, qfi := range qfis {
		st := stats[qfi]
		res.Flows = append(res.Flows, &orbitv1.QFIStats{
			Qfi:             uint32(qfi),
			UplinkPackets:   st.UplinkPackets,
			UplinkBytes:     st.UplinkBytes,
			DownlinkPackets: st.DownlinkPackets,
			DownlinkBytes:   st.DownlinkBytes,
		})
	}
	return connect.NewResponse(res), nil
}

func (s *ueService) Handover(
	ctx context.Context,
	req *connect.Request[orbitv1.HandoverRequest],
) (*connect.Response[orbitv1.HandoverResponse], error) {
	m := req.Msg
	if m.GetSupi() == "" || m.GetAmfAddress() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("supi and amf_address are required"))
	}
	target, err := gnbConfigFromProto(m.GetTargetGnb())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ctx, cancel := withTimeout(ctx, m.GetTimeoutMs(), 30000)
	defer cancel()

	err = s.mgr.Handover(ctx, m.GetSupi(), engine.GNBEndpoint{
		Config:   target,
		AMFAddr:  m.GetAmfAddress(),
		BindAddr: m.GetBindAddress(),
		N3Addr:   m.GetGnbN3Addr(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&orbitv1.HandoverResponse{
		Supi: m.GetSupi(), GnbId: target.ID, State: engine.StateHandoverComplete,
	}), nil
}

func (s *ueService) XnHandover(
	ctx context.Context,
	req *connect.Request[orbitv1.HandoverRequest],
) (*connect.Response[orbitv1.HandoverResponse], error) {
	m := req.Msg
	if m.GetSupi() == "" || m.GetAmfAddress() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("supi and amf_address are required"))
	}
	target, err := gnbConfigFromProto(m.GetTargetGnb())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ctx, cancel := withTimeout(ctx, m.GetTimeoutMs(), 30000)
	defer cancel()

	if err := s.mgr.XnHandover(ctx, m.GetSupi(), engine.GNBEndpoint{
		Config: target, AMFAddr: m.GetAmfAddress(), BindAddr: m.GetBindAddress(), N3Addr: m.GetGnbN3Addr(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&orbitv1.HandoverResponse{
		Supi: m.GetSupi(), GnbId: target.ID, State: engine.StateHandoverComplete,
	}), nil
}

func (s *ueService) StartApp(
	ctx context.Context,
	req *connect.Request[orbitv1.StartAppRequest],
) (*connect.Response[orbitv1.StartAppResponse], error) {
	m := req.Msg
	if m.GetSupi() == "" || m.GetApp() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("supi and app are required"))
	}
	dur := time.Duration(m.GetDurationMs()) * time.Millisecond
	if dur <= 0 {
		dur = 60 * time.Second
	}
	id, err := s.apps.StartAppSession(ctx, m.GetSupi(), engine.AppSessionConfig{
		App:        m.GetApp(),
		PeerAgent:  m.GetPeer(),
		Token:      m.GetToken(),
		PeerDataIP: m.GetPeerDataIp(),
		Params:     m.GetParams(),
		Duration:   dur,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&orbitv1.StartAppResponse{SessionId: id}), nil
}

func (s *ueService) AppStream(
	ctx context.Context,
	req *connect.Request[orbitv1.AppStreamRequest],
	stream *connect.ServerStream[orbitv1.AppSample],
) error {
	if req.Msg.GetSessionId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	events, cancel := s.apps.AppSessionEvents(req.Msg.GetSessionId())
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case a, ok := <-events:
			if !ok {
				return nil
			}
			if err := stream.Send(appSampleProto(a)); err != nil {
				return err
			}
		}
	}
}

func (s *ueService) StopApp(
	ctx context.Context,
	req *connect.Request[orbitv1.StopAppRequest],
) (*connect.Response[orbitv1.AppReport], error) {
	if req.Msg.GetSessionId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	rep, err := s.apps.StopAppSession(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(appReportProto(rep)), nil
}

// appSampleProto maps one engine sample onto the wire: a quality sample sets
// local or remote by end; a correlation event rides in events.
func appSampleProto(a engine.AppSample) *orbitv1.AppSample {
	p := &orbitv1.AppSample{
		UnixNano:    a.Time.UnixNano(),
		TimeErrNano: int64(a.TimeErr),
		TimeSource:  a.TimeSource,
		Final:       a.Final,
	}
	if a.Event != "" {
		p.Events = []*orbitv1.CorrelationEvent{{
			UnixNano: a.Time.UnixNano(), Kind: a.Event, Detail: a.Detail,
		}}
		return p
	}
	if a.End == engine.AppEndN6 {
		p.Remote = voipMetricsProto(a.VoIP)
	} else {
		p.Local = voipMetricsProto(a.VoIP)
	}
	return p
}

func appReportProto(rep engine.AppSessionReport) *orbitv1.AppReport {
	p := &orbitv1.AppReport{
		SessionId:       rep.ID,
		Supi:            rep.SUPI,
		App:             rep.App,
		Peer:            rep.PeerAgent,
		DataPort:        rep.DataPort,
		StartedUnixNano: rep.Started.UnixNano(),
		EndedUnixNano:   rep.Ended.UnixNano(),
		Local:           voipMetricsProto(&rep.Local),
		Remote:          voipMetricsProto(rep.Remote),
		Error:           rep.Err,
		MediaGaps:       appMediaGapSummaries(rep),
	}
	for _, ev := range rep.Events {
		var line string
		if ev.Event == engine.AppEventAnnotation {
			// The correlator's composed join ("XnHandover @t → DL media gap
			// 240ms → …") reads as a sentence on its own.
			line = fmt.Sprintf("%s  %s", ev.Time.Format("15:04:05.000"), ev.Detail)
		} else {
			line = fmt.Sprintf("%s  %s", ev.Time.Format("15:04:05.000"), ev.Event)
			if ev.Detail != "" {
				line += " — " + ev.Detail
			}
		}
		p.Annotations = append(p.Annotations, line)
	}
	return p
}

// appMediaGapSummaries maps the engine's report gaps onto the wire. The
// engine already put both ends on one timeline where possible (remote gaps
// re-stamped via the TimeSync offset) and labeled the clock of each — the
// wire carries that label so a "remote-clock" gap is never silently rendered
// alongside local timestamps as if aligned (design §5/§7).
func appMediaGapSummaries(rep engine.AppSessionReport) []*orbitv1.MediaGapSummary {
	var out []*orbitv1.MediaGapSummary
	for _, g := range rep.MediaGaps {
		out = append(out, &orbitv1.MediaGapSummary{
			End: g.End, StartUnixNano: g.Start.UnixNano(),
			EndUnixNano: g.Stop.UnixNano(), PacketsLost: g.PacketsLost,
			Clock: g.Clock, TimeErrNano: int64(g.TimeErr),
		})
	}
	return out
}

// voipMetricsProto maps loom's VoIP snapshot onto the wire (nil in → nil out,
// so an absent remote report stays absent).
func voipMetricsProto(v *metrics.VoIP) *orbitv1.VoipMetrics {
	if v == nil {
		return nil
	}
	p := &orbitv1.VoipMetrics{
		Codec:         v.Codec,
		TxPackets:     v.TxPackets,
		RxPackets:     v.RxPackets,
		Lost:          v.Lost,
		Duplicates:    v.Duplicates,
		Reordered:     v.Reordered,
		LossPct:       v.LossPct,
		DiscardPct:    v.DiscardPct,
		JitterMs:      v.JitterMs,
		RttMs:         v.RTTMs,
		OwdMs:         v.OWDMs,
		OwdErrMs:      v.OWDErrMs,
		OwdMethod:     v.OWDMethod,
		BurstR:        v.BurstR,
		RFactor:       v.RFactor,
		MosCq:         v.MOSCQ,
		RemoteRFactor: v.RemoteRFactor,
		RemoteMosCq:   v.RemoteMOSCQ,
		Emodel: &orbitv1.EModelBreakdown{
			Ro: v.EModel.Ro, Is: v.EModel.Is, Idte: v.EModel.Idte,
			Idle: v.EModel.Idle, Idd: v.EModel.Idd, Id: v.EModel.Id,
			Ie: v.EModel.Ie, IeEff: v.EModel.IeEff, A: v.EModel.A, R: v.EModel.R,
		},
	}
	for _, g := range v.MediaGaps {
		p.MediaGaps = append(p.MediaGaps, &orbitv1.MediaGap{
			StartUnixNano: g.Start.UnixNano(),
			EndUnixNano:   g.End.UnixNano(),
			PacketsLost:   g.PacketsLost,
		})
	}
	return p
}

func ueStatusProto(sess *engine.Session) *orbitv1.UEStatus {
	return &orbitv1.UEStatus{
		Supi:        sess.SUPI,
		State:       sess.State(),
		PduAddress:  sess.Result.PDUAddress,
		AmfUeNgapId: sess.Result.AMFUENGAPID,
	}
}
