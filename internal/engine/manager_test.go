package engine

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
)

func TestDataStatsUnregistered(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if _, err := m.DataStats("001010000000001"); err == nil {
		t.Fatal("expected an error for an unregistered SUPI")
	}
}

// A registered UE whose tunnel has not been opened yet reports no flows —
// not an error (the data path is created lazily on first use).
func TestDataStatsNoDataPath(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	const supi = "001010000000001"
	m.sessions[supi] = &Session{SUPI: supi, Result: &AttachResult{}}

	stats, err := m.DataStats(supi)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("expected no flows before the tunnel opens, got %v", stats)
	}
}

// DataStats mirrors the tunnel's per-QFI counters once traffic has flowed.
func TestDataStatsCountsUplink(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()

	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: "127.0.0.1:0", UPFN3: upf.LocalAddr().String(),
		ULTEID: 0x111, DLTEID: 0x222, QFI: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	m := NewManager(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	const supi = "001010000000001"
	m.sessions[supi] = &Session{SUPI: supi, Result: &AttachResult{}, dataPath: tun}

	payload := []byte{0x45, 0, 0, 4} // stand-in inner IP packet
	if err := tun.SendUplink(payload); err != nil {
		t.Fatal(err)
	}

	stats, err := m.DataStats(supi)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := stats[1]
	if !ok {
		t.Fatalf("expected QFI 1 in %v", stats)
	}
	if s.UplinkPackets != 1 || s.UplinkBytes != uint64(len(payload)) {
		t.Errorf("uplink = %d pkts / %d bytes, want 1 / %d", s.UplinkPackets, s.UplinkBytes, len(payload))
	}
	if s.DownlinkPackets != 0 || s.DownlinkBytes != 0 {
		t.Errorf("downlink = %d pkts / %d bytes, want 0 / 0", s.DownlinkPackets, s.DownlinkBytes)
	}
}

// publishMobility must make each handover phase visible both to StateStream
// subscribers (stamped) and in the serve log at info level with the UE and
// gNB identifiers.
func TestPublishMobilityLogsAndStreams(t *testing.T) {
	var buf bytes.Buffer
	m := NewManager(slog.New(slog.NewTextHandler(&buf, nil)))
	events, cancel := m.Subscribe()
	defer cancel()

	const supi = "001010000000001"
	m.publishMobility(StateEvent{SUPI: supi, State: StatePathSwitchComplete,
		Detail: "PathSwitchRequestAcknowledge; downlink → 172.17.50.13 (TEID 0x100)"},
		"type", "xn", "target_gnb", "orbit-gnb-tgt", "gnb_id", uint32(0x43))

	select {
	case ev := <-events:
		if ev.State != StatePathSwitchComplete || ev.SUPI != supi {
			t.Errorf("streamed %s/%s, want %s/%s", ev.SUPI, ev.State, supi, StatePathSwitchComplete)
		}
		if ev.Time.IsZero() {
			t.Error("streamed event is not timestamped")
		}
	case <-time.After(time.Second):
		t.Fatal("no event reached the StateStream subscriber")
	}

	log := buf.String()
	if !strings.Contains(log, "level=INFO") {
		t.Errorf("mobility phase not logged at info: %q", log)
	}
	for _, want := range []string{supi, StatePathSwitchComplete, "orbit-gnb-tgt", "type=xn"} {
		if !strings.Contains(log, want) {
			t.Errorf("log line missing %q: %q", want, log)
		}
	}
}
