package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// findCommand walks the command tree by use-names.
func findCommand(t *testing.T, root *cobra.Command, path ...string) *cobra.Command {
	t.Helper()
	cmd := root
	for _, name := range path {
		var next *cobra.Command
		for _, c := range cmd.Commands() {
			if c.Name() == name {
				next = c
				break
			}
		}
		if next == nil {
			t.Fatalf("command %q not found under %q", name, cmd.Name())
		}
		cmd = next
	}
	return cmd
}

// TestUEAppVoipFlags pins the command surface: flag set, defaults, and the
// required flags refusing to run without values.
func TestUEAppVoipFlags(t *testing.T) {
	root := New("test")
	cmd := findCommand(t, root, "ue", "app", "voip")

	for flag, def := range map[string]string{
		"supi":         "",
		"peer":         "",
		"token":        "",
		"peer-data-ip": "",
		"codec":        "g711",
		"ptime":        "20ms",
		"jb":           "40",
		"duration":     "1m0s",
		"json":         "false",
	} {
		f := cmd.Flags().Lookup(flag)
		if f == nil {
			t.Errorf("flag --%s missing", flag)
			continue
		}
		if f.DefValue != def {
			t.Errorf("flag --%s default = %q, want %q", flag, f.DefValue, def)
		}
	}

	// Required flags fail fast, before any RPC.
	root.SetArgs([]string{"ue", "app", "voip"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	err := root.Execute()
	if err == nil {
		t.Fatal("voip without --supi/--peer should fail")
	}
	for _, want := range []string{"supi", "peer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("required-flag error %q does not mention %q", err, want)
		}
	}
}

// stubUEHandler serves just the app RPCs over a real Connect server so the
// command can be executed end to end against --server.
type stubUEHandler struct {
	orbitv1connect.UnimplementedUEServiceHandler

	mu       sync.Mutex
	startReq *orbitv1.StartAppRequest
	stopID   string
	samples  []*orbitv1.AppSample
	report   *orbitv1.AppReport
}

func (h *stubUEHandler) StartApp(
	ctx context.Context, req *connect.Request[orbitv1.StartAppRequest],
) (*connect.Response[orbitv1.StartAppResponse], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.startReq = req.Msg
	return connect.NewResponse(&orbitv1.StartAppResponse{SessionId: "app-7"}), nil
}

func (h *stubUEHandler) AppStream(
	ctx context.Context, req *connect.Request[orbitv1.AppStreamRequest],
	stream *connect.ServerStream[orbitv1.AppSample],
) error {
	for _, s := range h.samples {
		if err := stream.Send(s); err != nil {
			return err
		}
	}
	return nil
}

func (h *stubUEHandler) StopApp(
	ctx context.Context, req *connect.Request[orbitv1.StopAppRequest],
) (*connect.Response[orbitv1.AppReport], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopID = req.Msg.GetSessionId()
	return connect.NewResponse(h.report), nil
}

func startStubUEServer(t *testing.T, h *stubUEHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func testWireVoIP(mos float64) *orbitv1.VoipMetrics {
	return &orbitv1.VoipMetrics{
		Codec: "pcmu", TxPackets: 500, RxPackets: 498, Lost: 2,
		LossPct: 0.4, DiscardPct: 0.1, JitterMs: 1.2, RttMs: 24.5,
		OwdMs: 12.1, OwdErrMs: 0.4, OwdMethod: "timesync",
		BurstR: 1.0, RFactor: 88.0, MosCq: mos,
	}
}

// TestUEAppVoipSmoke runs `orbit ue app voip` against a stub server: the
// request carries the flag-derived params, interval lines and correlation
// events print as they stream, and the final report renders.
func TestUEAppVoipSmoke(t *testing.T) {
	now := time.Now()
	h := &stubUEHandler{
		samples: []*orbitv1.AppSample{
			{UnixNano: now.UnixNano(), TimeSource: "local", Local: testWireVoIP(4.31)},
			{UnixNano: now.UnixNano(), TimeSource: "timesync", TimeErrNano: 400000, Remote: testWireVoIP(4.28)},
			{UnixNano: now.UnixNano(), TimeSource: "local", Events: []*orbitv1.CorrelationEvent{
				{UnixNano: now.UnixNano(), Kind: "HANDOVER_STARTED", Detail: "to gNB 2"},
			}},
		},
		report: &orbitv1.AppReport{
			SessionId: "app-7", Supi: "001010000000001", App: "voip", Peer: "127.0.0.1:9551",
			DataPort:        40000,
			StartedUnixNano: now.UnixNano(), EndedUnixNano: now.Add(2 * time.Second).UnixNano(),
			Local: testWireVoIP(4.30), Remote: testWireVoIP(4.27),
			Annotations: []string{"10:00:10.000  HANDOVER_STARTED — to gNB 2"},
			MediaGaps: []*orbitv1.MediaGapSummary{{
				End: "ue", StartUnixNano: now.UnixNano(),
				EndUnixNano: now.Add(240 * time.Millisecond).UnixNano(), PacketsLost: 12,
			}},
		},
	}
	url := startStubUEServer(t, h)

	root := New("test")
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", url, "ue", "app", "voip",
		"--supi", "001010000000001", "--peer", "127.0.0.1:9551", "--duration", "2s"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v\noutput:\n%s", err, out.String())
	}

	req := h.startReq
	if req == nil {
		t.Fatal("StartApp never called")
	}
	if req.GetSupi() != "001010000000001" || req.GetApp() != "voip" ||
		req.GetPeer() != "127.0.0.1:9551" || req.GetDurationMs() != 2000 {
		t.Errorf("StartApp request: %v", req)
	}
	if p := req.GetParams(); p["codec"] != "g711" || p["ptime"] != "20ms" || p["jb_ms"] != "40" {
		t.Errorf("params from flags: %v", p)
	}
	if h.stopID != "app-7" {
		t.Errorf("StopApp called with %q, want app-7", h.stopID)
	}

	text := out.String()
	for _, want := range []string{
		"ue  MOS 4.31",                   // local interval line
		"n6  MOS 4.28",                   // remote interval line
		"owd 12.10±0.40ms (timesync)",    // OWD with method and error bar
		"** HANDOVER_STARTED — to gNB 2", // correlation event inline
		"call app-7: voip 001010000000001 → 127.0.0.1:9551 (media port 40000), 2s",
		"MOS 4.30", // final local
		"MOS 4.27", // final remote
		"media gaps:",
		"240ms (12 pkts lost)",
		"HANDOVER_STARTED — to gNB 2", // annotation replay
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, text)
		}
	}
}

// TestUEAppVoipFailureExit pins the nonzero-exit contract: a session that
// ended with a terminal error (e.g. media handshake timeout) fails the
// command even though the RPCs themselves succeeded.
func TestUEAppVoipFailureExit(t *testing.T) {
	h := &stubUEHandler{
		report: &orbitv1.AppReport{
			SessionId: "app-7", Supi: "s", App: "voip",
			Error: "voip: no return media within handshake timeout",
		},
	}
	url := startStubUEServer(t, h)

	root := New("test")
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", url, "ue", "app", "voip",
		"--supi", "s", "--peer", "p:1", "--duration", "1s"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("expected failure exit, output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "handshake timeout") {
		t.Errorf("error %q does not carry the session error", err)
	}
}

// TestUEAppVoipJSON checks --json emits machine-readable lines.
func TestUEAppVoipJSON(t *testing.T) {
	h := &stubUEHandler{
		samples: []*orbitv1.AppSample{
			{UnixNano: 1, TimeSource: "local", Local: testWireVoIP(4.31)},
		},
		report: &orbitv1.AppReport{SessionId: "app-7", Supi: "s", App: "voip"},
	}
	url := startStubUEServer(t, h)

	root := New("test")
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", url, "ue", "app", "voip",
		"--supi", "s", "--peer", "p:1", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, `"time_source"`) || !strings.Contains(text, `"mos_cq"`) {
		t.Errorf("sample JSON missing proto-name fields:\n%s", text)
	}
	if !strings.Contains(text, `"session_id"`) {
		t.Errorf("report JSON missing:\n%s", text)
	}
}
