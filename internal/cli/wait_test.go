package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// waitStub is a run that stays RUNNING until polled `runningFor` times or
// stopped, then ends in `end` (or, once stopped, per the stop: finished →
// COMPLETE stopped early, cancelled → CANCELLED).
type waitStub struct {
	orbitv1connect.UnimplementedRunServiceHandler
	mu         sync.Mutex
	polls      int
	runningFor int
	end        orbitv1.RunState
	stops      []bool // each StopRun's finish
	report     *orbitv1.GetRunReportResponse
}

func (s *waitStub) GetRun(_ context.Context, _ *connect.Request[orbitv1.GetRunRequest]) (*connect.Response[orbitv1.GetRunResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	run := &orbitv1.Run{RunId: "run-w", State: orbitv1.RunState_RUN_STATE_RUNNING}
	switch {
	case len(s.stops) > 0 && s.stops[len(s.stops)-1]:
		run.State, run.StoppedEarly = orbitv1.RunState_RUN_STATE_COMPLETE, true
	case len(s.stops) > 0:
		run.State = orbitv1.RunState_RUN_STATE_CANCELLED
	case s.runningFor >= 0 && s.polls > s.runningFor:
		run.State = s.end
	}
	return connect.NewResponse(&orbitv1.GetRunResponse{Run: run}), nil
}

func (s *waitStub) StopRun(_ context.Context, req *connect.Request[orbitv1.StopRunRequest]) (*connect.Response[orbitv1.StopRunResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, req.Msg.GetFinish())
	return connect.NewResponse(&orbitv1.StopRunResponse{}), nil
}

func (s *waitStub) GetRunReport(_ context.Context, _ *connect.Request[orbitv1.GetRunReportRequest]) (*connect.Response[orbitv1.GetRunReportResponse], error) {
	if s.report == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no report"))
	}
	return connect.NewResponse(s.report), nil
}

func gateResult(name string, passed bool) *orbitv1.GateResult {
	return &orbitv1.GateResult{Gate: &orbitv1.Gate{Name: name}, Passed: passed, Detail: "d"}
}

// --wait exits with the run's result: 0 a completed run whose gates all
// passed (or that had none), 1 a gate failed, 2 a run that did not
// complete; a first SIGINT finishes the run, a second or a SIGTERM
// cancels it.
func TestWaitRun(t *testing.T) {
	old := waitPoll
	waitPoll = 5 * time.Millisecond
	defer func() { waitPoll = old }()
	soak := &orbitv1.LoadReport{Attempted: 43, DistinctSupis: 30}
	tests := []struct {
		name       string
		stub       *waitStub
		sigs       []os.Signal
		wantCode   int
		wantStops  []bool
		wantOut    []string
		wantStderr []string
	}{
		{name: "passed", stub: &waitStub{runningFor: 2, end: orbitv1.RunState_RUN_STATE_COMPLETE,
			report: &orbitv1.GetRunReportResponse{Gates: []*orbitv1.GateResult{gateResult("ok", true)}, GatesPassed: true}},
			wantCode: ExitPass, wantOut: []string{"gate ok: PASS"}, wantStderr: []string{"exit 0: the run completed and every gate passed"}},
		{name: "no gates", stub: &waitStub{runningFor: 0, end: orbitv1.RunState_RUN_STATE_COMPLETE,
			report: &orbitv1.GetRunReportResponse{Report: &orbitv1.GetRunReportResponse_Load{Load: soak}}},
			wantCode: ExitPass, wantOut: []string{"soak: 43 attempts across 30 SUPIs"}},
		{name: "a gate failed", stub: &waitStub{runningFor: 0, end: orbitv1.RunState_RUN_STATE_COMPLETE,
			report: &orbitv1.GetRunReportResponse{Gates: []*orbitv1.GateResult{gateResult("ok", true), gateResult("reg", false)}}},
			wantCode: ExitGateFailed, wantOut: []string{"gate reg: FAIL"}},
		{name: "failed", stub: &waitStub{runningFor: 0, end: orbitv1.RunState_RUN_STATE_FAILED}, wantCode: ExitDidNotComplete},
		{name: "SIGINT finishes", stub: &waitStub{runningFor: -1,
			report: &orbitv1.GetRunReportResponse{Gates: []*orbitv1.GateResult{gateResult("ok", true)}, GatesPassed: true}},
			sigs: []os.Signal{os.Interrupt}, wantCode: ExitPass, wantStops: []bool{true},
			wantOut:    []string{"finished early: judged on the results so far"},
			wantStderr: []string{"finishing: judging gates on results so far; press Ctrl-C again to cancel"}},
		{name: "SIGTERM cancels", stub: &waitStub{runningFor: -1}, sigs: []os.Signal{syscall.SIGTERM},
			wantCode: ExitDidNotComplete, wantStops: []bool{false}, wantStderr: []string{"cancelling"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle(orbitv1connect.NewRunServiceHandler(tc.stub))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL)
			sigs := make(chan os.Signal, len(tc.sigs))
			for _, s := range tc.sigs {
				sigs <- s
			}
			var out, errOut bytes.Buffer
			err := waitRun(context.Background(), &out, &errOut, c, "run-w", sigs)
			if code := ExitCode(err); code != tc.wantCode {
				t.Fatalf("exit %d (%v), want %d\nout:\n%s\nerr:\n%s", code, err, tc.wantCode, out.String(), errOut.String())
			}
			if tc.wantStops != nil && !equalBools(tc.stub.stops, tc.wantStops) {
				t.Errorf("stops = %v, want %v", tc.stub.stops, tc.wantStops)
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("out lacks %q:\n%s", w, out.String())
				}
			}
			for _, w := range tc.wantStderr {
				if !strings.Contains(errOut.String(), w) {
					t.Errorf("stderr lacks %q:\n%s", w, errOut.String())
				}
			}
		})
	}
}

// A second SIGINT after a finish cancels.
func TestWaitRunSecondInterruptCancels(t *testing.T) {
	old := waitPoll
	waitPoll = 5 * time.Millisecond
	defer func() { waitPoll = old }()
	// A run that stays running through the first stop, so the second
	// signal is read: the stub ends only when cancelled.
	stub := &holdingStub{}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(stub))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sigs := make(chan os.Signal, 2)
	sigs <- os.Interrupt
	sigs <- os.Interrupt
	var out, errOut bytes.Buffer
	err := waitRun(context.Background(), &out, &errOut, orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL), "run-h", sigs)
	if ExitCode(err) != ExitDidNotComplete || !equalBools(stub.stops, []bool{true, false}) {
		t.Errorf("exit %d, stops %v; want 2 after finish then cancel", ExitCode(err), stub.stops)
	}
}

// holdingStub runs until a plain (cancelling) stop.
type holdingStub struct {
	orbitv1connect.UnimplementedRunServiceHandler
	mu    sync.Mutex
	stops []bool
}

func (s *holdingStub) GetRun(_ context.Context, _ *connect.Request[orbitv1.GetRunRequest]) (*connect.Response[orbitv1.GetRunResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := orbitv1.RunState_RUN_STATE_DRAINING
	for _, f := range s.stops {
		if !f {
			state = orbitv1.RunState_RUN_STATE_CANCELLED
		}
	}
	return connect.NewResponse(&orbitv1.GetRunResponse{Run: &orbitv1.Run{RunId: "run-h", State: state}}), nil
}

func (s *holdingStub) StopRun(_ context.Context, req *connect.Request[orbitv1.StopRunRequest]) (*connect.Response[orbitv1.StopRunResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = append(s.stops, req.Msg.GetFinish())
	return connect.NewResponse(&orbitv1.StopRunResponse{}), nil
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Each error ends the command with its code: a typed one's, else "could
// not start".
func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "none", want: ExitPass},
		{name: "a gate", err: gateFailed("SLO breached"), want: ExitGateFailed},
		{name: "did not complete", err: exitWith(ExitDidNotComplete, errors.New("x")), want: ExitDidNotComplete},
		{name: "wrapped", err: errors.Join(errors.New("ctx"), gateFailed("g")), want: ExitGateFailed},
		{name: "untyped", err: errors.New("no server"), want: ExitCouldNotStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
	if exitWith(ExitGateFailed, nil) != nil {
		t.Error("exitWith(nil) is not nil")
	}
	for code := ExitPass; code <= ExitCouldNotStart; code++ {
		if ExitMeaning(code) == "unknown" {
			t.Errorf("code %d has no meaning", code)
		}
	}
}

// soakLine speaks only when attempts outnumber the SUPIs they used.
func TestSoakLine(t *testing.T) {
	for _, tc := range []struct {
		attempted, distinct uint32
		want                string
	}{{43, 30, "soak: 43 attempts across 30 SUPIs"}, {30, 30, ""}, {5, 0, ""}} {
		if got := soakLine(tc.attempted, tc.distinct); got != tc.want {
			t.Errorf("soakLine(%d, %d) = %q, want %q", tc.attempted, tc.distinct, got, tc.want)
		}
	}
}

// Once a run has an ID, an error leaves its outcome unknown: it did not
// complete (2). Before that, a server that cannot be reached means the run
// could not start (3).
func TestStartAndWaitExitCodes(t *testing.T) {
	old := waitPoll
	waitPoll = 5 * time.Millisecond
	defer func() { waitPoll = old }()

	// A server that starts the run, then fails every poll: the stream to
	// the run is cut mid-wait.
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(&cutStub{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // nothing listens there any more

	tests := []struct {
		name string
		url  string
		want int
	}{
		{name: "cut mid-wait", url: srv.URL, want: ExitDidNotComplete},
		{name: "server unreachable", url: gone.URL, want: ExitCouldNotStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRunsCmd(&tc.url, options{})
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"start-load", "--wait", "--amf", "10.0.0.1:38412", "--base-imsi", "208930100007500",
				"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f", "--mcc", "208", "--mnc", "93"})
			err := cmd.Execute()
			if got := ExitCode(err); got != tc.want {
				t.Errorf("exit %d (%v), want %d", got, err, tc.want)
			}
		})
	}
}

// cutStub starts a run and then cannot report on it.
type cutStub struct {
	orbitv1connect.UnimplementedRunServiceHandler
}

func (cutStub) StartRun(context.Context, *connect.Request[orbitv1.StartRunRequest]) (*connect.Response[orbitv1.StartRunResponse], error) {
	return connect.NewResponse(&orbitv1.StartRunResponse{Run: &orbitv1.Run{RunId: "run-cut"}}), nil
}

func (cutStub) GetRun(context.Context, *connect.Request[orbitv1.GetRunRequest]) (*connect.Response[orbitv1.GetRunResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection reset"))
}
