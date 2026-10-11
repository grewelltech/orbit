package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

func TestExpandBindAddrs(t *testing.T) {
	tests := []struct {
		name    string
		binds   []string
		n       int
		want    []string
		wantErr string
	}{
		{name: "none: route-selected source for every gNB", binds: nil, n: 3, want: []string{"", "", ""}},
		{name: "none for one gNB", binds: nil, n: 1, want: []string{""}},
		{name: "one shared by every gNB, normalised", binds: []string{"10.0.0.1"}, n: 3, want: []string{"10.0.0.1:0", "10.0.0.1:0", "10.0.0.1:0"}},
		{name: "one shared ip:0 by every gNB", binds: []string{"10.0.0.1:0"}, n: 2, want: []string{"10.0.0.1:0", "10.0.0.1:0"}},
		{name: "one for one gNB", binds: []string{"10.0.0.1:0"}, n: 1, want: []string{"10.0.0.1:0"}},
		{name: "one fixed port for one gNB is legal", binds: []string{"10.0.0.1:38412"}, n: 1, want: []string{"10.0.0.1:38412"}},
		{name: "one per gNB in order", binds: []string{"10.0.0.1", "10.0.0.2:0", "::3"}, n: 3, want: []string{"10.0.0.1:0", "10.0.0.2:0", "[::3]:0"}},
		{name: "one per gNB with distinct fixed ports", binds: []string{"10.0.0.1:38412", "10.0.0.1:38413"}, n: 2, want: []string{"10.0.0.1:38412", "10.0.0.1:38413"}},
		{name: "one per gNB repeating ip:0 is sharing", binds: []string{"10.0.0.1", "10.0.0.1:0"}, n: 2, want: []string{"10.0.0.1:0", "10.0.0.1:0"}},
		{name: "hostnames pass through to the dial", binds: []string{"ran-21"}, n: 1, want: []string{"ran-21:0"}},
		{name: "one fewer than gNBs", binds: []string{"10.0.0.1", "10.0.0.2"}, n: 3, wantErr: "2 bind addresses for 3 gNBs"},
		{name: "one more than gNBs", binds: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}, n: 3, wantErr: "4 bind addresses for 3 gNBs"},
		{name: "two for one gNB", binds: []string{"10.0.0.1", "10.0.0.2"}, n: 1, wantErr: "2 bind addresses for 1 gNBs"},
		{name: "shared fixed port for several gNBs", binds: []string{"10.0.0.1:38412"}, n: 3, wantErr: `shared bind address "10.0.0.1:38412" has a fixed port`},
		{name: "duplicate fixed ip:port in the list", binds: []string{"10.0.0.1:38412", "10.0.0.2", "10.0.0.1:38412"}, n: 3, wantErr: `bind addresses 1 and 3 are both "10.0.0.1:38412"`},
		{name: "malformed entry", binds: []string{"10.0.0.1", "10.0.0.2:0:0"}, n: 2, wantErr: `invalid local address "10.0.0.2:0:0"`},
		{name: "empty entry", binds: []string{"10.0.0.1", ""}, n: 2, wantErr: "bind address 2 is empty"},
		{name: "no gNBs", binds: nil, n: 0, wantErr: "need at least one gNB"},
		{name: "negative gNBs", binds: []string{"10.0.0.1"}, n: -1, wantErr: "need at least one gNB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandBindAddrs(tc.binds, tc.n)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ExpandBindAddrs(%v, %d) err = %v, want containing %q", tc.binds, tc.n, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandBindAddrs(%v, %d): %v", tc.binds, tc.n, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ExpandBindAddrs hands back its own slice, never the caller's, so a caller
// mutating the result cannot corrupt the flag value it came from.
func TestExpandBindAddrsCopies(t *testing.T) {
	in := []string{"10.0.0.1", "10.0.0.2"}
	out, err := ExpandBindAddrs(in, 2)
	if err != nil {
		t.Fatal(err)
	}
	out[0] = "changed"
	if in[0] != "10.0.0.1" {
		t.Error("ExpandBindAddrs aliased the input slice")
	}
}

// pipeConn adapts the mock AMF's in-process transport to n2Conn, reporting a
// fabricated local address derived from the bind the dialer was given — the
// shape sctp.Conn.LocalAddr has once the kernel fills in the port.
type pipeConn struct {
	gnb.Transport
	local net.Addr
}

func (p pipeConn) LocalAddr() net.Addr { return p.local }

// fakeN2Dialer records every (laddr, raddr) it is asked for and connects each
// to the mock AMF, standing in for sctp.Dial where kernel SCTP is unavailable.
type fakeN2Dialer struct {
	amf *mockamf.AMF
	mu  sync.Mutex
	// laddrs is the bind address requested on each dial, in order.
	laddrs []string
	raddrs []string
	port   int
}

func (d *fakeN2Dialer) dial(laddr, raddr string) (n2Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.laddrs = append(d.laddrs, laddr)
	d.raddrs = append(d.raddrs, raddr)
	// The kernel would assign an ephemeral port to a :0 bind; mimic that so
	// the reported source carries a real port, as the sctp.Conn would.
	d.port++
	host := laddr
	if h, _, err := net.SplitHostPort(laddr); err == nil {
		host = h
	}
	var local net.Addr
	if host != "" {
		local = &net.TCPAddr{IP: net.ParseIP(host), Port: 40000 + d.port}
	} else {
		local = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 40000 + d.port} // the "primary" address
	}
	return pipeConn{Transport: d.amf.ConnectShared(context.Background()), local: local}, nil
}

func (d *fakeN2Dialer) dialed() ([]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.laddrs...), append([]string(nil), d.raddrs...)
}

func newBindTestAMF(t *testing.T) (*mockamf.AMF, gnb.Config, []byte, []byte) {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	return amf, cfg, ki, opc
}

func bindTestUE(t *testing.T, supi string, ki, opc []byte) UEConfig {
	t.Helper()
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	return UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}}
}

// Manager.Register dials from the GNBSpec's bind address and reports the
// source actually bound, both on the result and on the retained session.
func TestManagerRegisterBindsAndReportsN2Source(t *testing.T) {
	tests := []struct {
		name     string
		bind     string
		wantDial string // laddr handed to the dialer
		wantHost string // host of the reported N2 source
	}{
		{name: "unset lets the kernel choose", bind: "", wantDial: "", wantHost: "192.0.2.1"},
		{name: "bare ip", bind: "127.0.0.1", wantDial: "127.0.0.1", wantHost: "127.0.0.1"},
		{name: "ip with ephemeral port", bind: "127.0.0.2:0", wantDial: "127.0.0.2:0", wantHost: "127.0.0.2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, cfg, ki, opc := newBindTestAMF(t)
			d := &fakeN2Dialer{amf: amf}
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.dial = d.dial

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const supi = "208930100000001"
			res, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412", BindAddr: tc.bind}, bindTestUE(t, supi, ki, opc))
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if !res.Registered {
				t.Fatal("UE did not register against the mock AMF")
			}

			laddrs, raddrs := d.dialed()
			if len(laddrs) != 1 || laddrs[0] != tc.wantDial {
				t.Errorf("dialer saw laddrs %q, want [%q]", laddrs, tc.wantDial)
			}
			if raddrs[0] != "amf.test:38412" {
				t.Errorf("dialer saw raddr %q, want the spec's AMFAddr", raddrs[0])
			}
			host, port, err := net.SplitHostPort(res.N2Source)
			if err != nil {
				t.Fatalf("N2Source %q is not host:port: %v", res.N2Source, err)
			}
			if host != tc.wantHost || port == "0" || port == "" {
				t.Errorf("N2Source = %q, want host %s with a real port", res.N2Source, tc.wantHost)
			}
			sess, err := m.Status(supi)
			if err != nil {
				t.Fatal(err)
			}
			if sess.N2Source() != res.N2Source {
				t.Errorf("Session.N2Source() = %q, result says %q", sess.N2Source(), res.N2Source)
			}
		})
	}
}

// A Manager built outside NewManager (zero dial) still dials rather than
// panicking on a nil func.
func TestManagerRegisterNilDialerFallsBack(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}, hub: newHub(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, ki, opc := newBindTestAMF(t)
	// A malformed bind fails in sctp.Dial's normalisation before any socket
	// is opened, so this proves the fallback path without kernel SCTP.
	_, err := m.Register(context.Background(), GNBSpec{AMFAddr: "127.0.0.1:38412", BindAddr: "10.0.0.1:0:0"}, bindTestUE(t, "208930100000002", ki, opc))
	if err == nil || !strings.Contains(err.Error(), `invalid local address "10.0.0.1:0:0"`) {
		t.Fatalf("err = %v, want the sctp normalisation error", err)
	}
}

// NewFleet dials each gNB from its own BindAddr and reports the sources in
// gNB order: one shared address, one per gNB, or none at all.
func TestNewFleetBindsPerGNB(t *testing.T) {
	tests := []struct {
		name      string
		binds     []string // one per gNB spec
		wantHosts []string
	}{
		{name: "no binds", binds: []string{"", "", ""}, wantHosts: []string{"192.0.2.1", "192.0.2.1", "192.0.2.1"}},
		{name: "one address shared", binds: []string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}, wantHosts: []string{"127.0.0.1", "127.0.0.1", "127.0.0.1"}},
		{name: "one address each", binds: []string{"10.0.0.1", "10.0.0.2:0", "10.0.0.3"}, wantHosts: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, cfg, _, _ := newBindTestAMF(t)
			d := &fakeN2Dialer{amf: amf}
			var specs []GNBSpec
			for i, b := range tc.binds {
				c := cfg
				c.ID = uint32(i + 1)
				c.Name = fmt.Sprintf("gnb-%d", i)
				specs = append(specs, GNBSpec{Config: c, AMFAddr: "amf.test:38412", BindAddr: b})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f, err := newFleet(ctx, specs, slog.New(slog.NewTextHandler(io.Discard, nil)), d.dial)
			if err != nil {
				t.Fatalf("newFleet: %v", err)
			}
			defer f.Close()

			laddrs, _ := d.dialed()
			if strings.Join(laddrs, ",") != strings.Join(tc.binds, ",") {
				t.Errorf("dialer saw %q, want %q", laddrs, tc.binds)
			}
			srcs := f.N2Sources()
			if len(srcs) != len(tc.wantHosts) {
				t.Fatalf("N2Sources = %q, want %d entries", srcs, len(tc.wantHosts))
			}
			for i, s := range srcs {
				host, port, err := net.SplitHostPort(s)
				if err != nil || host != tc.wantHosts[i] || port == "0" {
					t.Errorf("N2Sources[%d] = %q, want host %s with a real port", i, s, tc.wantHosts[i])
				}
			}
			// The slice is a copy: callers cannot reach into the fleet.
			srcs[0] = "x"
			if f.N2Sources()[0] == "x" {
				t.Error("N2Sources returned the fleet's own slice")
			}
		})
	}
}

// N2Source follows the association: the handover paths swap the connection
// and record the target's bound source in the same locked section as the
// serving cell, so a status read after a move shows the target's address.
func TestSessionN2SourceFollowsHandover(t *testing.T) {
	sess := &Session{SUPI: "208930100000001", Result: &AttachResult{}}
	sess.setN2Source("10.0.0.21:40001")
	sess.setServingGNB(gnb.Config{ID: 1, Name: "src"})
	if got := sess.N2Source(); got != "10.0.0.21:40001" {
		t.Fatalf("N2Source after register = %q", got)
	}
	// What handover.go / xn.go do under m.mu when the target is accepted.
	tgt := pipeConn{local: &net.TCPAddr{IP: net.ParseIP("10.0.0.22"), Port: 40002}}
	sess.setServingGNB(gnb.Config{ID: 2, Name: "tgt"})
	sess.setN2Source(localAddrString(tgt))
	if got := sess.N2Source(); got != "10.0.0.22:40002" {
		t.Errorf("N2Source after handover = %q, want the target's bound source", got)
	}
	if sess.ServingGNB().ID != 2 {
		t.Errorf("serving gNB did not move with the source")
	}
	// A transport that cannot report a local address leaves it empty rather
	// than keeping the stale source.
	sess.setN2Source(localAddrString(pipeConn{}))
	if got := sess.N2Source(); got != "" {
		t.Errorf("N2Source with no local address = %q, want empty", got)
	}
}

// A fleet records each UE's N2 source from its gNB's association, and a
// fleet handover moves it to the target gNB's source.
func TestFleetN2SourceFor(t *testing.T) {
	f := &Fleet{sources: []string{"10.0.0.21:40001", "10.0.0.22:40002"}}
	for i, want := range []string{"10.0.0.21:40001", "10.0.0.22:40002", "10.0.0.21:40001"} {
		if got := f.n2SourceFor(i); got != want {
			t.Errorf("n2SourceFor(%d) = %q, want %q", i, got, want)
		}
	}
}
