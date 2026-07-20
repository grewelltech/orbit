package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// TestUEAppHTTPFlags pins the http command surface: flag set, defaults, and
// the required flags refusing to run without values.
func TestUEAppHTTPFlags(t *testing.T) {
	root := New("test")
	cmd := findCommand(t, root, "ue", "app", "http")

	for flag, def := range map[string]string{
		"supi":         "",
		"peer":         "",
		"token":        "",
		"peer-data-ip": "",
		"url-path":     "",
		"objects":      "0",
		"object-size":  "",
		"think":        "",
		"tls":          "false",
		"h2":           "false",
		"host":         "",
		"tls-ca":       "",
		"tls-insecure": "false",
		"port-min":     "0",
		"port-max":     "0",
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

	root.SetArgs([]string{"ue", "app", "http"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	err := root.Execute()
	if err == nil {
		t.Fatal("http without --supi/--peer should fail")
	}
	for _, want := range []string{"supi", "peer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("required-flag error %q does not mention %q", err, want)
		}
	}
}

// TestUEAppVideoFlags pins the video command surface.
func TestUEAppVideoFlags(t *testing.T) {
	root := New("test")
	cmd := findCommand(t, root, "ue", "app", "video")

	for flag, def := range map[string]string{
		"supi":            "",
		"peer":            "",
		"token":           "",
		"peer-data-ip":    "",
		"url-name":        "",
		"ladder":          "",
		"seg-duration":    "0s",
		"segments":        "0",
		"start-threshold": "0s",
		"buffer-target":   "0s",
		"rebuffer-target": "0s",
		"abr":             "",
		"tls":             "false",
		"h2":              "false",
		"host":            "",
		"tls-ca":          "",
		"tls-insecure":    "false",
		"port-min":        "0",
		"port-max":        "0",
		"duration":        "1m0s",
		"json":            "false",
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

	root.SetArgs([]string{"ue", "app", "video"})
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	if err := root.Execute(); err == nil {
		t.Fatal("video without --supi/--peer should fail")
	}
}

// TestUEAppWebFlagValidation pins the pre-RPC refusals: cross-flag TLS
// mistakes and a bad --abr fail before any server contact (no --server is
// even reachable here).
func TestUEAppWebFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"h2 without tls", []string{"ue", "app", "http", "--supi", "s", "--peer", "p:1", "--h2"}, "--h2 requires --tls"},
		{"tls-ca without tls", []string{"ue", "app", "http", "--supi", "s", "--peer", "p:1", "--tls-ca", "x.pem"}, "only meaningful with --tls"},
		{"tls-insecure without tls", []string{"ue", "app", "video", "--supi", "s", "--peer", "p:1", "--tls-insecure"}, "only meaningful with --tls"},
		{"bad abr", []string{"ue", "app", "video", "--supi", "s", "--peer", "p:1", "--abr", "psychic"}, "--abr must be"},
		{"negative objects", []string{"ue", "app", "http", "--supi", "s", "--peer", "p:1", "--objects", "-2"}, "--objects must be >= 0"},
		// --tls-ca is structurally unusable against a stock loomd origin
		// (per-flow self-signed cert, never exposed over the control plane):
		// it must refuse up front, for BOTH TCP apps, instead of letting the
		// session burn its whole duration on 100%-error samples.
		{"tls-ca refused (http)", []string{"ue", "app", "http", "--supi", "s", "--peer", "p:1", "--tls", "--tls-ca", "x.pem"}, "--tls-ca cannot verify a stock loomd origin"},
		{"tls-ca refused (video)", []string{"ue", "app", "video", "--supi", "s", "--peer", "p:1", "--tls", "--tls-ca", "x.pem"}, "--tls-ca cannot verify a stock loomd origin"},
		{"negative duration", []string{"ue", "app", "http", "--supi", "s", "--peer", "p:1", "--duration", "-5s"}, "--duration must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := New("test")
			root.SetOut(new(bytes.Buffer))
			root.SetErr(new(bytes.Buffer))
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func testWireHTTP(reqs uint64) *orbitv1.HttpMetrics {
	return &orbitv1.HttpMetrics{
		Requests: reqs, Errors: 1, TtfbMsP95: 34.2, GoodputMbps: 18.4,
		ConnectMs: 3.1, TlsHandshakeMs: 8.25,
	}
}

// TestUEAppHTTPSmoke runs `orbit ue app http` against the stub server: the
// request carries the flag-derived params, interval lines render both ends,
// and the final report follows. (tls_insecure stands in for the pin that
// cannot exist: --tls-ca is refused pre-RPC — see TestUEAppWebFlagValidation.)
func TestUEAppHTTPSmoke(t *testing.T) {
	now := time.Now()
	h := &stubUEHandler{
		samples: []*orbitv1.AppSample{
			{UnixNano: now.UnixNano(), TimeSource: "local", LocalHttp: testWireHTTP(12)},
			{UnixNano: now.UnixNano(), TimeSource: "timesync", RemoteHttp: &orbitv1.HttpMetrics{Requests: 11, GoodputMbps: 18.1}},
		},
		report: &orbitv1.AppReport{
			SessionId: "app-9", Supi: "001010000000001", App: "http", Peer: "127.0.0.1:9551",
			DataPort:        40080,
			StartedUnixNano: now.UnixNano(), EndedUnixNano: now.Add(3 * time.Second).UnixNano(),
			LocalHttp:  testWireHTTP(240),
			RemoteHttp: &orbitv1.HttpMetrics{Requests: 240, GoodputMbps: 17.9},
		},
	}
	url := startStubUEServer(t, h)

	root := New("test")
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", url, "ue", "app", "http",
		"--supi", "001010000000001", "--peer", "127.0.0.1:9551",
		"--objects", "240", "--object-size", "32KB", "--think", "50ms..200ms",
		"--tls", "--h2", "--tls-insecure", "--duration", "3s"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v\noutput:\n%s", err, out.String())
	}

	req := h.startReq
	if req == nil {
		t.Fatal("StartApp never called")
	}
	if req.GetApp() != "http" || req.GetDurationMs() != 3000 {
		t.Errorf("StartApp request: %v", req)
	}
	p := req.GetParams()
	for key, want := range map[string]string{
		"objects":      "240",
		"object_size":  "32KB",
		"think":        "50ms..200ms",
		"tls":          "true",
		"h2":           "true",
		"tls_insecure": "true",
	} {
		if p[key] != want {
			t.Errorf("params[%q] = %q, want %q", key, p[key], want)
		}
	}
	if _, ok := p["tls_ca"]; ok {
		t.Error("tls_ca must never travel (refused pre-RPC; it cannot verify a stock loomd origin)")
	}

	text := out.String()
	for _, want := range []string{
		"ue  reqs 12  err 1  ttfb-p95 34.20ms  goodput 18.40 Mbps  connect 3.10ms  tls 8.25ms",
		"n6  reqs 11  err 0",
		"call app-9: http 001010000000001 → 127.0.0.1:9551 (media port 40080), 3s",
		"ue  reqs 240",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, text)
		}
	}
}

// TestUEAppVideoSmoke runs `orbit ue app video`: ladder/buffer params travel,
// interval lines carry buffer/bitrate/stalls/startup, and the report renders
// the QoE summary with its timestamped stall list and the origin's (http)
// far-end line.
func TestUEAppVideoSmoke(t *testing.T) {
	now := time.Unix(1700000000, 0)
	h := &stubUEHandler{
		samples: []*orbitv1.AppSample{
			{UnixNano: now.UnixNano(), TimeSource: "local", LocalVideo: &orbitv1.VideoMetrics{
				BufferMs: 2400, AvgBitrateKbps: 1200, Stalls: 0,
			}},
			{UnixNano: now.UnixNano(), TimeSource: "local", LocalVideo: &orbitv1.VideoMetrics{
				BufferMs: 800, AvgBitrateKbps: 400, Stalls: 1, StallTimeMs: 1800, StartupMs: 340,
			}},
		},
		report: &orbitv1.AppReport{
			SessionId: "app-4", Supi: "001010000000001", App: "video", Peer: "127.0.0.1:9551",
			DataPort:        40080,
			StartedUnixNano: now.UnixNano(), EndedUnixNano: now.Add(10 * time.Second).UnixNano(),
			LocalVideo: &orbitv1.VideoMetrics{
				Stalls: 2, StallTimeMs: 3100, RebufferRatio: 0.041,
				AvgBitrateKbps: 1980, StartupMs: 340,
				StallEvents: []*orbitv1.MediaGap{{
					StartUnixNano: now.Add(2 * time.Second).UnixNano(),
					EndUnixNano:   now.Add(3800 * time.Millisecond).UnixNano(),
				}},
			},
			RemoteHttp: &orbitv1.HttpMetrics{Requests: 97, GoodputMbps: 4.2},
		},
	}
	url := startStubUEServer(t, h)

	root := New("test")
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"--server", url, "ue", "app", "video",
		"--supi", "001010000000001", "--peer", "127.0.0.1:9551",
		"--ladder", "240p:400k,480p:1200k", "--seg-duration", "2s", "--segments", "30",
		"--start-threshold", "1s", "--buffer-target", "6s", "--rebuffer-target", "2s",
		"--abr", "buffer", "--duration", "30s"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v\noutput:\n%s", err, out.String())
	}

	req := h.startReq
	if req == nil {
		t.Fatal("StartApp never called")
	}
	if req.GetApp() != "video" {
		t.Errorf("StartApp app = %q", req.GetApp())
	}
	for key, want := range map[string]string{
		"ladder":          "240p:400k,480p:1200k",
		"seg_duration":    "2s",
		"segments":        "30",
		"start_threshold": "1s",
		"buffer_target":   "6s",
		"rebuffer_target": "2s",
		"abr":             "buffer",
	} {
		if p := req.GetParams(); p[key] != want {
			t.Errorf("params[%q] = %q, want %q", key, p[key], want)
		}
	}

	text := out.String()
	for _, want := range []string{
		"ue  buffer 2.4s  bitrate 1200 kbps  stalls 0  stalled 0.0s  startup n/a",
		"ue  buffer 0.8s  bitrate 400 kbps  stalls 1  stalled 1.8s  startup 340ms",
		"call app-4: video 001010000000001 → 127.0.0.1:9551 (media port 40080), 10s",
		"ue  startup 340ms  stalls 2 (3.1s stalled, rebuffer 4.1%)  avg bitrate 1980 kbps",
		"n6  reqs 97  err 0",
		"stalls:",
		"1.8s",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, text)
		}
	}
}
