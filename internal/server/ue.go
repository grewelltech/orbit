package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ueService adapts the engine.Manager to the UEService RPCs.
type ueService struct {
	log *slog.Logger
	mgr *engine.Manager
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

func ueStatusProto(sess *engine.Session) *orbitv1.UEStatus {
	return &orbitv1.UEStatus{
		Supi:        sess.SUPI,
		State:       sess.State(),
		PduAddress:  sess.Result.PDUAddress,
		AmfUeNgapId: sess.Result.AMFUENGAPID,
	}
}
