package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/bgrewell/loom/core/metrics"
	"github.com/bgrewell/loom/core/rtp"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
)

func testHTTP() metrics.HTTP {
	return metrics.HTTP{
		Requests: 240, Errors: 3,
		ConnectMs: 3.1, TLSHandshakeMs: 8.2,
		TTFBMsP50: 21.0, TTFBMsP95: 34.5, TTFBMsP99: 61.0,
		ObjectMsP50: 40.0, ObjectMsP95: 80.0, ObjectMsP99: 120.0,
		GoodputMbps: 18.4,
	}
}

func testVideo() metrics.Video {
	return metrics.Video{
		SegmentsFetched: 45, Stalls: 2, StartupMs: 340,
		StallTimeMs: 3100, RebufferRatio: 0.041, BufferMs: 2400,
		AvgBitrateKbps: 1980, RepSwitchesUp: 1, RepSwitchesDown: 2,
		StallEvents: []rtp.Gap{{
			Start: time.Unix(200, 0),
			End:   time.Unix(201, 800e6),
		}},
	}
}

// TestAppStreamRPCWebKinds pins the wire mapping of http/video interval
// samples: the populated field follows the metric kind and the measuring end
// — and a video session's remote (origin) samples travel as remote_http.
func TestAppStreamRPCWebKinds(t *testing.T) {
	now := time.Now()
	h := testHTTP()
	v := testVideo()
	stub := &stubAppEngine{
		events: []engine.AppSample{
			{Time: now, TimeSource: "local", End: engine.AppEndUE, HTTP: &h},
			{Time: now, TimeSource: "timesync", TimeErr: 400 * time.Microsecond,
				End: engine.AppEndN6, HTTP: &h},
			{Time: now, TimeSource: "local", End: engine.AppEndUE, Video: &v},
		},
	}
	client := newAppRPCClient(t, stub)

	stream, err := client.AppStream(context.Background(),
		connect.NewRequest(&orbitv1.AppStreamRequest{SessionId: "app-1"}))
	if err != nil {
		t.Fatal(err)
	}
	var got []*orbitv1.AppSample
	for stream.Receive() {
		got = append(got, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("received %d samples, want 3", len(got))
	}

	local := got[0]
	if local.GetLocalHttp().GetRequests() != 240 || local.GetLocalHttp().GetTtfbMsP95() != 34.5 {
		t.Errorf("local http sample: %v", local.GetLocalHttp())
	}
	if local.GetRemoteHttp() != nil || local.GetLocal() != nil || local.GetLocalVideo() != nil {
		t.Errorf("local http sample leaked other kinds: %v", local)
	}

	remote := got[1]
	if remote.GetRemoteHttp().GetGoodputMbps() != 18.4 {
		t.Errorf("remote http sample: %v", remote.GetRemoteHttp())
	}
	if remote.GetTimeSource() != "timesync" || remote.GetTimeErrNano() != 400_000 {
		t.Errorf("remote provenance: source %q err %d", remote.GetTimeSource(), remote.GetTimeErrNano())
	}
	if remote.GetLocalHttp() != nil || remote.GetRemote() != nil {
		t.Errorf("remote http sample leaked other kinds: %v", remote)
	}

	vid := got[2]
	lv := vid.GetLocalVideo()
	if lv.GetStalls() != 2 || lv.GetBufferMs() != 2400 || lv.GetStartupMs() != 340 {
		t.Errorf("local video sample: %v", lv)
	}
	if len(lv.GetStallEvents()) != 1 || lv.GetStallEvents()[0].GetEndUnixNano()-lv.GetStallEvents()[0].GetStartUnixNano() != int64(1800*time.Millisecond) {
		t.Errorf("stall events on the wire: %v", lv.GetStallEvents())
	}
}

// TestStopAppRPCWebReport pins the report mapping for the TCP apps: local
// whole-run snapshots by kind, the origin's http final on the remote side,
// and NO phantom all-zero VoipMetrics on a non-voip report.
func TestStopAppRPCWebReport(t *testing.T) {
	h := testHTTP()
	v := testVideo()
	origin := metrics.HTTP{Requests: 97, GoodputMbps: 4.2}
	stub := &stubAppEngine{
		report: engine.AppSessionReport{
			ID: "app-2", SUPI: "001010000000001", App: "video", PeerAgent: "n6:9551",
			DataPort: 40080,
			Started:  time.Unix(1000, 0), Ended: time.Unix(1010, 0),
			LocalVideo: &v,
			RemoteHTTP: &origin,
		},
	}
	client := newAppRPCClient(t, stub)

	rep, err := client.StopApp(context.Background(),
		connect.NewRequest(&orbitv1.StopAppRequest{SessionId: "app-2"}))
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Msg
	if r.GetLocal() != nil || r.GetRemote() != nil {
		t.Errorf("video report carries voip snapshots: %v / %v", r.GetLocal(), r.GetRemote())
	}
	if r.GetLocalVideo().GetAvgBitrateKbps() != 1980 || r.GetLocalVideo().GetRebufferRatio() != 0.041 {
		t.Errorf("local video report: %v", r.GetLocalVideo())
	}
	if len(r.GetLocalVideo().GetStallEvents()) != 1 {
		t.Errorf("stall events missing from the report: %v", r.GetLocalVideo())
	}
	if r.GetRemoteHttp().GetRequests() != 97 {
		t.Errorf("origin final: %v", r.GetRemoteHttp())
	}

	// An http report maps LocalHTTP/RemoteHTTP the same way.
	stub.mu.Lock()
	stub.report = engine.AppSessionReport{
		ID: "app-3", SUPI: "001010000000001", App: "http", PeerAgent: "n6:9551",
		LocalHTTP:  &h,
		RemoteHTTP: &origin,
	}
	stub.mu.Unlock()
	rep, err = client.StopApp(context.Background(),
		connect.NewRequest(&orbitv1.StopAppRequest{SessionId: "app-3"}))
	if err != nil {
		t.Fatal(err)
	}
	r = rep.Msg
	if r.GetLocalHttp().GetTtfbMsP95() != 34.5 || r.GetLocalHttp().GetErrors() != 3 {
		t.Errorf("local http report: %v", r.GetLocalHttp())
	}
	if r.GetLocal() != nil {
		t.Error("http report carries a phantom voip local snapshot")
	}
}
