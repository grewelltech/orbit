package server

import (
	"context"
	"fmt"
	"log/slog"

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
		Identity: id,
		Sub:      auth.Subscription{SUPI: m.GetSupi(), Ki: ki, OPc: opc},
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

func ueStatusProto(sess *engine.Session) *orbitv1.UEStatus {
	return &orbitv1.UEStatus{
		Supi:        sess.SUPI,
		State:       sess.State(),
		PduAddress:  sess.Result.PDUAddress,
		AmfUeNgapId: sess.Result.AMFUENGAPID,
	}
}
