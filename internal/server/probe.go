package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
	"github.com/bgrewell/loom/control"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/target"
)

// The doctor's steps (ADR-0009 §5), in order.
var probeStepNames = []string{"NG Setup", "register", "PDU session", "responder", "control", "UE to N6 echo", "data ports"}

// Step outcomes.
const (
	probeOK   = "OK"
	probeFail = "FAIL"
	probeSkip = "SKIP"
)

// probeEnv is what the probe does outside itself; tests replace any of it.
type probeEnv struct {
	ngSetup    func(ctx context.Context, req *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error)
	register   func(ctx context.Context, req *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error)
	deregister func(ctx context.Context, supi string)
	resolve    func(name string) (join.Responder, error)
	// health asks the responder's loom agent at control, with token.
	health func(ctx context.Context, control, token string) error
	// echo sends ICMP echoes from the UE to dst and counts the replies.
	echo func(ctx context.Context, supi, dst string) (int, error)
	// udp sends a short UDP flow from the UE to a throwaway receiver on the
	// responder and returns the receiver's packet count and port; the
	// receiver is gone when it returns, whatever happened.
	udp func(ctx context.Context, supi, control, token, dataIP string) (uint64, uint32, error)
}

// targetService is TargetService.
type targetService struct {
	log  *slog.Logger
	reg  *engine.RunRegistry
	env  probeEnv
	hold func(holder string, needs engine.Needs) (func(), error)
}

func newTargetService(log *slog.Logger, reg *engine.RunRegistry, mgr *engine.Manager, cells *cellService, ues *ueService, responders *join.Registry) *targetService {
	return &targetService{log: log, reg: reg, hold: reg.Hold, env: probeEnv{
		ngSetup: func(ctx context.Context, req *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error) {
			res, err := cells.RunNGSetup(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return res.Msg, nil
		},
		register: func(ctx context.Context, req *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error) {
			res, err := ues.Register(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return res.Msg, nil
		},
		deregister: func(ctx context.Context, supi string) {
			if _, err := ues.Deregister(ctx, connect.NewRequest(&orbitv1.DeregisterRequest{Supi: supi})); err != nil {
				log.Warn("probe: deregistering after the probe", "supi", supi, "err", err)
			}
		},
		resolve: responders.Resolve,
		health:  loomHealth,
		echo: func(ctx context.Context, supi, dst string) (int, error) {
			res, err := mgr.Ping(supi, dst, 3)
			if err != nil {
				return 0, err
			}
			return res.Received, nil
		},
		udp: func(ctx context.Context, supi, ctl, token, dataIP string) (uint64, uint32, error) {
			send := func(ctx context.Context, target string) error {
				_, err := mgr.Traffic(ctx, supi, target, "1Mbps", 200, time.Second)
				return err
			}
			return loomUDPCheck(ctx, send, ctl, token, dataIP)
		},
	}}
}

// probeRun is one probe's state.
type probeRun struct {
	stream *connect.ServerStream[orbitv1.ProbeStep]
	next   int
	last   int // the steps this probe covers: 3 with --core-only, else 7
	failed bool
}

// send streams the step at p.next.
func (p *probeRun) send(status, detail, fix string) error {
	p.next++
	if status == probeFail {
		p.failed = true
	}
	return p.stream.Send(&orbitv1.ProbeStep{Index: uint32(p.next), Name: probeStepNames[p.next-1], Status: status, Detail: detail, Fix: fix})
}

// skipRest marks every remaining step SKIP, after the step that failed.
func (p *probeRun) skipRest(why string) error {
	for p.next < p.last {
		if err := p.send(probeSkip, why, ""); err != nil {
			return err
		}
	}
	return nil
}

// Probe runs the doctor (ADR-0009 §5) from this server, streaming each
// step. A refusal before step 1 (a bad target, a lease another run holds)
// is an error; a step that fails is a FAIL step, and the stream ends well.
func (s *targetService) Probe(ctx context.Context, req *connect.Request[orbitv1.ProbeRequest], stream *connect.ServerStream[orbitv1.ProbeStep]) error {
	m := req.Msg
	t := m.GetTarget()
	name := m.GetTargetName()
	if name == "" {
		name = "the target"
	}
	core := t.GetCore()
	if core.GetAmf() == "" || core.GetPlmn().GetMcc() == "" || core.GetPlmn().GetMnc() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the target needs core.amf and core.plmn"))
	}
	timeout := time.Duration(m.GetStepTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	plmn := core.GetPlmn().GetMcc() + "/" + core.GetPlmn().GetMnc()
	gnb, err := probeGNB(t)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	var bind, n3 string
	if n2s, err := target.ExpandPool(t.GetRan().GetN2SourceIps(), 1); err == nil && len(n2s) > 0 {
		bind = n2s[0]
	}
	if n3s, err := target.ExpandPool(t.GetRan().GetN3Ips(), 1); err == nil && len(n3s) > 0 {
		n3 = n3s[0]
	}
	var pool *orbitv1.SubscriberPool
	if ps := t.GetSubscribers(); len(ps) > 0 && ps[0].GetFirstSupi() != "" {
		pool = ps[0]
	}

	// Leases, as a run takes them: the three gNB IDs, the SUPI, the N2 source.
	needs := engine.Needs{}
	for i := uint32(0); i < 3; i++ {
		needs.GNBs = append(needs.GNBs, engine.GNBKey{AMF: core.GetAmf(), PLMN: plmn, ID: gnb.GetId() + i})
	}
	if pool != nil {
		if supi, err := strconv.ParseUint(pool.GetFirstSupi(), 10, 64); err == nil {
			needs.SUPIs = []engine.SUPIBlock{{PLMN: plmn, First: supi, Last: supi}}
		}
	}
	if bind != "" {
		needs.N2IPs = []string{bind}
	}
	holder := "probe-" + randomHex(4)
	release, err := s.hold(holder, needs)
	var lc *engine.ErrLeaseConflict
	switch {
	case errors.As(err, &lc):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return connect.NewError(connect.CodeInternal, err)
	}
	defer release()

	p := &probeRun{stream: stream, last: len(probeStepNames)}
	if m.GetCoreOnly() {
		p.last = 3
	}
	ms := uint32(timeout.Milliseconds())
	gnbFor := func(n uint32) *orbitv1.GnbConfig {
		g := proto.Clone(gnb).(*orbitv1.GnbConfig)
		g.Id += n
		return g
	}

	// 1. NG Setup.
	start := time.Now()
	ng, err := s.env.ngSetup(ctx, &orbitv1.RunNGSetupRequest{AmfAddress: core.GetAmf(), LocalAddress: bind, Gnb: gnbFor(0), TimeoutMs: ms})
	switch {
	case err != nil && connect.CodeOf(err) == connect.CodeUnavailable:
		return p.fail(fmt.Sprintf("AMF %s is not reachable over SCTP from the orbit server: %s", core.GetAmf(), connectMsg(err)),
			"check core.amf and the N2 route from the orbit server")
	case err != nil:
		return p.fail(fmt.Sprintf("NG Setup to AMF %s failed: %s", core.GetAmf(), connectMsg(err)), "check core.amf")
	case !ng.GetAccepted():
		return p.fail("the AMF rejected NG Setup: "+ng.GetCause()+target.PLMNHint(ng.GetCause(), core.GetPlmn().GetMcc(), core.GetPlmn().GetMnc(), core.GetTac()),
			"check core.plmn, core.tac and the slices against the core's configuration")
	}
	if err := p.send(probeOK, fmt.Sprintf("accepted by AMF %q from %s in %s", ng.GetAmfName(), ng.GetLocalAddress(), sinceMS(start)), ""); err != nil {
		return err
	}

	// 2. Register one UE.
	if pool == nil {
		return p.fail(name+" has no subscribers", "add subscribers[].first_supi, ki and opc to the target (orbit target init --first-supi does it)")
	}
	if pool.GetKi() == "" || pool.GetOpc() == "" {
		return p.fail(fmt.Sprintf("subscribers[%s] has no ki/opc", pool.GetName()), "set them, or export ${ORBIT_KI}/${ORBIT_OPC}")
	}
	supi := pool.GetFirstSupi()
	regReq := func(withPDU bool) *orbitv1.RegisterRequest {
		r := &orbitv1.RegisterRequest{AmfAddress: core.GetAmf(), BindAddress: bind, Supi: supi, Gnb: gnbFor(1),
			Credentials: &orbitv1.Credentials{Ki: pool.GetKi(), Opc: pool.GetOpc()}, RoutingIndicator: pool.GetRoutingIndicator(), TimeoutMs: ms}
		if r.RoutingIndicator == "" {
			r.RoutingIndicator = "0"
		}
		if withPDU {
			r.Gnb = gnbFor(2)
			var sst uint32
			var sd string
			if sl := core.GetSlices(); len(sl) > 0 {
				sst, sd = sl[0].GetSst(), sl[0].GetSd()
			}
			r.PduSession = &orbitv1.PDUSession{PduSessionId: 1, Sst: sst, Sd: sd, Dnn: core.GetDnn()}
			r.GnbN3Addr = n3
		}
		return r
	}
	// Whatever is registered goes on every path out, cancel included.
	registered := false
	defer func() {
		if registered {
			dctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			s.env.deregister(dctx, supi)
		}
	}()
	// TODO(T3 follow-up, "feat(engine): typed 5GMM/5GSM reject causes"):
	// name the 5GMM reject cause here once the engine reports it.
	start = time.Now()
	reg, err := s.env.register(ctx, regReq(false))
	if err != nil || !reg.GetRegistered() {
		why := "not registered"
		if err != nil {
			why = connectMsg(err)
		}
		return p.fail(fmt.Sprintf("registering %s failed: %s (reject cause not reported yet)", supi, why),
			fmt.Sprintf("check that subscribers[%s] (first_supi, ki, opc) is provisioned in the core", pool.GetName()))
	}
	registered = true
	if err := p.send(probeOK, fmt.Sprintf("%s registered in %s", supi, sinceMS(start)), ""); err != nil {
		return err
	}
	s.env.deregister(ctx, supi)
	registered = false

	// 3. A PDU session, kept for the user-plane steps.
	// TODO(T3 follow-up): name the 5GSM reject cause, without waiting out
	// the timeout, once the engine detects the reject.
	start = time.Now()
	pdu, err := s.env.register(ctx, regReq(true))
	if err == nil && pdu.GetRegistered() {
		registered = true
	}
	slice := "none"
	if sl := core.GetSlices(); len(sl) > 0 {
		slice = strconv.Itoa(int(sl[0].GetSst()))
		if sl[0].GetSd() != "" {
			slice += "/" + sl[0].GetSd()
		}
	}
	pduFix := "check core.dnn and core.slices[0] against the core's configuration"
	timedOut := err != nil && (connect.CodeOf(err) == connect.CodeDeadlineExceeded || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded"))
	switch {
	case timedOut || (err == nil && !pdu.GetSessionActive()):
		return p.fail(fmt.Sprintf("no PDU session accept within %s (DNN %s, slice %s)", timeout, core.GetDnn(), slice), pduFix)
	case err != nil:
		return p.fail(fmt.Sprintf("the PDU session for DNN %s on slice %s failed: %s (reject cause not reported yet)", core.GetDnn(), slice, connectMsg(err)), pduFix)
	}
	if err := p.send(probeOK, fmt.Sprintf("UE IP %s via UPF %s in %s", pdu.GetPduAddress(), pdu.GetUpfAddress(), sinceMS(start)), ""); err != nil {
		return err
	}
	if m.GetCoreOnly() {
		return nil
	}

	// 4. The responder: registered and online, or static.
	rs := t.GetApp().GetResponders()
	if len(rs) == 0 {
		return p.fail("no responder in "+name, fmt.Sprintf("join an app host: orbit join create -t %s (or orbit target add-responder --ssh <host> -t %s); or probe with --core-only", name, name))
	}
	r := rs[0]
	ctl, token, dataIP := r.GetControl(), r.GetToken(), r.GetDataIp()
	portRange := "its receive ports"
	if ctl == "" && token == "" {
		got, err := s.env.resolve(r.GetName())
		var off *join.OfflineError
		switch {
		case errors.As(err, &off):
			return p.fail(off.Error(), "on the app host: systemctl status orbit-responder")
		case err != nil:
			return p.fail(fmt.Sprintf("responder %s is not registered with this server", r.GetName()),
				fmt.Sprintf("orbit join create -t %s --name %s", name, r.GetName()))
		}
		ctl, token = got.Control, got.LoomToken
		if dataIP == "" {
			dataIP = got.DataIP
		}
		portRange = fmt.Sprintf("%d-%d", got.PortLow, got.PortHigh)
		if err := p.send(probeOK, fmt.Sprintf("responder %s registered, online, control %s, data %s", r.GetName(), ctl, dataIP), ""); err != nil {
			return err
		}
	} else {
		if dataIP == "" {
			return p.fail(fmt.Sprintf("static responder %s has no data_ip", r.GetName()), "set app.responders[0].data_ip")
		}
		if err := p.send(probeOK, fmt.Sprintf("static responder %s (not registered): control %s, data %s", r.GetName(), ctl, dataIP), ""); err != nil {
			return err
		}
	}

	// 5. Its control port, with its token.
	hctx, cancel := context.WithTimeout(ctx, timeout)
	err = s.env.health(hctx, ctl, token)
	cancel()
	if err != nil {
		host, port, _ := net.SplitHostPort(ctl)
		switch status.Code(err) {
		case codes.Unauthenticated, codes.PermissionDenied:
			return p.fail("the responder refused its loom token", "re-join it with a fresh token and --reregister")
		case codes.Unavailable, codes.DeadlineExceeded:
			return p.fail(fmt.Sprintf("control port %s on %s is unreachable from this server (from %s)", port, host, sourceToward(host)),
				fmt.Sprintf("on the app host, allow tcp %s from %s, or rerun the join one-liner", port, sourceToward(host)))
		default:
			return p.fail("the responder's control plane answered: "+status.Convert(err).Message(), "check the responder's version")
		}
	}
	if err := p.send(probeOK, "loom agent at "+ctl+" answered", ""); err != nil {
		return err
	}

	// 6 and 7. An echo, then a UDP flow, from the UE over its own tunnel.
	pools, upfN6, n6Subnet := core.GetUePools(), core.GetUpfN6(), core.GetN6Subnet()
	route := "ip route replace <ue pool> via <upf n6> dev <n6 interface>"
	if len(pools) > 0 && upfN6 != "" {
		route = fmt.Sprintf("ip route replace %s via %s dev <n6 interface>", pools[0], upfN6)
	}
	src := "the UE pools"
	if len(pools) > 0 {
		src = pools[0]
	}
	got, echoErr := s.env.echo(ctx, supi, dataIP)
	udpN, udpPort, udpErr := s.env.udp(ctx, supi, ctl, token, dataIP)
	udpOK := udpErr == nil && udpN > 0
	switch {
	case echoErr == nil && got > 0:
		if err := p.send(probeOK, fmt.Sprintf("%d of 3 echoes from %s", got, dataIP), ""); err != nil {
			return err
		}
	case udpOK:
		// UDP reaches the receiver, so the route is there: ICMP is blocked.
		if err := p.send(probeFail, fmt.Sprintf("no echo from %s, though UDP reaches it: ICMP is blocked on the app host", dataIP),
			fmt.Sprintf("allow icmp echo-request from %s on the app host (rerun the join one-liner)", src)); err != nil {
			return err
		}
	default:
		if err := p.send(probeFail, fmt.Sprintf("no echo from %s (3 sent)", dataIP),
			fmt.Sprintf("on the app host, the return route (%s), or ICMP blocked (allow icmp echo-request from %s); the next step tells them apart", route, src)); err != nil {
			return err
		}
	}
	if udpOK {
		return p.send(probeOK, fmt.Sprintf("%d UDP datagrams arrived at %s:%d", udpN, dataIP, udpPort), "")
	}
	why := fmt.Sprintf("no UDP datagram arrived at %s:%d", dataIP, udpPort)
	if udpErr != nil {
		why = "the UDP check could not run: " + udpErr.Error()
	}
	sources := src
	if n6Subnet != "" {
		sources += " and " + n6Subnet
	}
	return p.send(probeFail, why, fmt.Sprintf("on the app host, allow udp %s from %s, and check the return route (%s)", portRange, sources, route))
}

// fail streams the current step as FAIL and the rest as SKIP.
func (p *probeRun) fail(detail, fix string) error {
	failed := probeStepNames[p.next]
	if err := p.send(probeFail, detail, fix); err != nil {
		return err
	}
	return p.skipRest("after " + failed + " failed")
}

// probeGNB is the target's first gNB, as the probe uses it.
func probeGNB(t *orbitv1.ResolvedTarget) (*orbitv1.GnbConfig, error) {
	core, ran := t.GetCore(), t.GetRan()
	id, bits := ran.GetGnbIds().GetFirst(), ran.GetGnbIds().GetBits()
	if id == 0 {
		id = target.DefaultGNBID
	}
	if bits == 0 {
		bits = 24
	}
	name := ran.GetGnbName()
	if name == "" {
		name = "orbit-probe"
	}
	g := &orbitv1.GnbConfig{Id: id, IdBits: bits, Name: name, Mcc: core.GetPlmn().GetMcc(), Mnc: core.GetPlmn().GetMnc(), Tac: core.GetTac()}
	for _, s := range core.GetSlices() {
		g.Slices = append(g.Slices, &orbitv1.Snssai{Sst: s.GetSst(), Sd: s.GetSd()})
	}
	return g, nil
}

// loomHealth asks the loom agent at addr whether it is up, with token.
func loomHealth(ctx context.Context, addr, token string) error {
	client, conn, err := control.Dial(addr, control.WithToken(token))
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	defer func() { _ = conn.Close() }()
	_, err = client.Health(ctx, &loomv1.HealthRequest{})
	return err
}

// loomUDPCheck sets up a UDP receiver flow on the responder, has send
// send a short flow to it (from the UE), and reads whether it saw any. The
// receiver is stopped and destroyed on every path, a cancel or a server
// shutdown included, on a context of its own.
func loomUDPCheck(ctx context.Context, send func(ctx context.Context, target string) error, addr, token, dataIP string) (uint64, uint32, error) {
	client, conn, err := control.Dial(addr, control.WithToken(token))
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = conn.Close() }()
	cfg, err := client.Configure(ctx, &loomv1.ConfigureRequest{Flow: &loomv1.FlowSpec{
		Role: loomv1.FlowRole_FLOW_ROLE_RECEIVER, Datapath: "udp", PacketSize: 200}})
	if err != nil {
		return 0, 0, err
	}
	id, port := cfg.GetFlowId(), cfg.GetDataPort()
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.Stop(cctx, &loomv1.StopRequest{FlowId: id})
		_, _ = client.Destroy(cctx, &loomv1.DestroyRequest{FlowId: id})
	}()
	if _, err := client.Start(ctx, &loomv1.StartRequest{FlowId: id, ReportIntervalNanos: (200 * time.Millisecond).Nanoseconds()}); err != nil {
		return 0, port, err
	}
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.StreamTelemetry(tctx, &loomv1.TelemetryRequest{FlowId: id})
	if err != nil {
		return 0, port, err
	}
	seen := make(chan uint64, 64)
	go func() {
		for {
			s, err := stream.Recv()
			if err != nil {
				close(seen)
				return
			}
			seen <- s.GetPackets()
		}
	}()
	if err := send(ctx, net.JoinHostPort(dataIP, strconv.Itoa(int(port)))); err != nil {
		return 0, port, err
	}
	// The first sample that counts any packet answers; a sample comes every
	// 200ms, so a second and a half without one is none.
	deadline := time.After(1500 * time.Millisecond)
	for {
		select {
		case n, ok := <-seen:
			if !ok || n > 0 {
				return n, port, nil
			}
		case <-deadline:
			return 0, port, nil
		case <-ctx.Done():
			return 0, port, ctx.Err()
		}
	}
}

// sourceToward is this server's source address toward host, for a message.
func sourceToward(host string) string {
	c, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return "this server"
	}
	defer func() { _ = c.Close() }()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return "this server"
}

func connectMsg(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

func sinceMS(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
