package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
)

// fakeResolve resolves amf.lab to 10.0.0.1, as DNS would, and leaves other
// addresses as written.
func fakeResolve(_ context.Context, amf string) string {
	if amf == "amf.lab:38412" {
		return "10.0.0.1:38412"
	}
	return amf
}

func TestLeaseConflicts(t *testing.T) {
	supis := func(plmn string, first, last uint64) Needs {
		return Needs{SUPIs: []SUPIBlock{{PLMN: plmn, First: first, Last: last}}}
	}
	gnbID := func(amf, plmn string, id uint32) Needs {
		return Needs{GNBs: []GNBKey{{AMF: amf, PLMN: plmn, ID: id}}}
	}
	n2 := func(binds ...string) Needs { return Needs{N2IPs: binds} }
	tests := []struct {
		name       string
		held, want Needs
		resource   string // "" = no conflict
		value      string
	}{
		{name: "SUPI blocks apart", held: supis("001/01", 1, 10), want: supis("001/01", 11, 20)},
		{name: "SUPI blocks touching", held: supis("001/01", 1, 10), want: supis("001/01", 10, 20), resource: "SUPIs", value: "000000000000010 (PLMN 001/01)"},
		{name: "SUPI block inside another", held: supis("001/01", 1, 100), want: supis("001/01", 40, 60), resource: "SUPIs", value: "000000000000040-000000000000060 (PLMN 001/01)"},
		{name: "same SUPIs, other PLMN", held: supis("001/01", 1, 10), want: supis("001/02", 1, 10)},
		{name: "same gNB ID, same AMF and PLMN", held: gnbID("10.0.0.1:38412", "001/01", 7), want: gnbID("10.0.0.1:38412", "001/01", 7), resource: "gNB ID", value: "7 on AMF 10.0.0.1:38412 (PLMN 001/01)"},
		{name: "same gNB ID, other AMF", held: gnbID("10.0.0.1:38412", "001/01", 7), want: gnbID("10.0.0.2:38412", "001/01", 7)},
		{name: "same gNB ID, other PLMN", held: gnbID("10.0.0.1:38412", "001/01", 7), want: gnbID("10.0.0.1:38412", "001/02", 7)},
		{name: "same gNB ID, AMF by name and by address", held: gnbID("10.0.0.1:38412", "001/01", 7), want: gnbID("amf.lab:38412", "001/01", 7), resource: "gNB ID", value: "7 on AMF 10.0.0.1:38412 (PLMN 001/01)"},
		{name: "other gNB ID", held: gnbID("10.0.0.1:38412", "001/01", 7), want: gnbID("10.0.0.1:38412", "001/01", 8)},
		{name: "same N2 IP", held: n2("10.0.1.5"), want: n2("10.0.1.5"), resource: "N2 source IP", value: "10.0.1.5"},
		{name: "same N2 IP with a port", held: n2("10.0.1.5"), want: n2("10.0.1.5:0"), resource: "N2 source IP", value: "10.0.1.5"},
		{name: "same N2 IP, v4-mapped", held: n2("10.0.1.5"), want: n2("[::ffff:10.0.1.5]:0"), resource: "N2 source IP", value: "10.0.1.5"},
		{name: "kernel-chosen N2 sources lease nothing", held: n2("", "0.0.0.0", "[::]:0"), want: n2("", "0.0.0.0")},
		{name: "other N2 IP", held: n2("10.0.1.5"), want: n2("10.0.1.6")},
		{name: "N3 is not leased", held: Needs{}, want: Needs{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lt := newLeaseTable()
			lt.resolveAMF = fakeResolve
			if err := lt.acquire("run-held", lt.canonical(tc.held)); err != nil {
				t.Fatal(err)
			}
			err := lt.acquire("run-new", lt.canonical(tc.want))
			if tc.resource == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var c *ErrLeaseConflict
			if !errors.As(err, &c) {
				t.Fatalf("err = %v, want an ErrLeaseConflict", err)
			}
			if c.Resource != tc.resource || c.Value != tc.value || c.Holder != "run-held" {
				t.Errorf("conflict = %+v, want %s %q held by run-held", c, tc.resource, tc.value)
			}
			if _, ok := lt.held["run-new"]; ok {
				t.Error("a refused run kept leases")
			}
		})
	}
}

// One run may use one N2 address for all its gNBs (a load run's shared
// --bind); that is not a conflict with itself.
func TestLeaseSharedBindWithinARun(t *testing.T) {
	lt := newLeaseTable()
	if err := lt.acquire("run-1", lt.canonical(Needs{N2IPs: []string{"10.0.1.5", "10.0.1.5", "10.0.1.5:0"}})); err != nil {
		t.Fatalf("a run sharing one bind across its gNBs was refused: %v", err)
	}
	if n := len(lt.held["run-1"].n2); n != 1 {
		t.Errorf("held %d N2 leases, want 1", n)
	}
	lt.release("run-1")
	lt.release("run-1") // idempotent
	if len(lt.held) != 0 {
		t.Error("release left leases")
	}
}

func TestResolveAMF(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:38412":        "127.0.0.1:38412",
		"[::ffff:127.0.0.1]:38":  "127.0.0.1:38",
		"localhost:38412":        "127.0.0.1:38412",
		"no-such-host.invalid:1": "no-such-host.invalid:1",
		"AMF.Example.INVALID:1":  "amf.example.invalid:1",
	} {
		got := resolveAMF(context.Background(), in)
		if in == "localhost:38412" && got == "[::1]:38412" {
			continue // a host resolving localhost to ::1 first
		}
		if got != want {
			t.Errorf("resolveAMF(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadSUPISpan(t *testing.T) {
	tests := []struct {
		name        string
		spec        LoadSpec
		first, last uint64
		ok          bool
	}{
		{name: "count", spec: LoadSpec{BaseIMSI: "208930000000001", Count: 10}, first: 208930000000001, last: 208930000000010, ok: true},
		{name: "imsi- spelling", spec: LoadSpec{BaseIMSI: "imsi-208930000000001", Count: 1}, first: 208930000000001, last: 208930000000001, ok: true},
		{name: "soak cycling a count", spec: LoadSpec{BaseIMSI: "208930000000001", Count: 5, Duration: time.Minute}, first: 208930000000001, last: 208930000000005, ok: true},
		{name: "soak without a count climbs to the end", spec: LoadSpec{BaseIMSI: "208930000000001", Duration: time.Minute}, first: 208930000000001, last: maxSUPI, ok: true},
		{name: "nothing generated", spec: LoadSpec{BaseIMSI: "208930000000001"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, last, ok, err := tc.spec.SUPISpan()
			if err != nil {
				t.Fatal(err)
			}
			if first != tc.first || last != tc.last || ok != tc.ok {
				t.Errorf("SUPISpan = %d..%d %t, want %d..%d %t", first, last, ok, tc.first, tc.last, tc.ok)
			}
		})
	}
	if _, _, _, err := (LoadSpec{BaseIMSI: "x1"}).SUPISpan(); err == nil {
		t.Error("a malformed base IMSI gave a span")
	}
}

func TestLoadAndFleetNeeds(t *testing.T) {
	cfg := func(id uint32) gnb.Config { return gnb.Config{ID: id, MCC: "208", MNC: "93"} }
	ln, err := LoadNeeds(LoadSpec{
		BaseIMSI: "208930000000001", Count: 4, MCC: "208", MNC: "93",
		GNBs: []GNBSpec{{Config: cfg(1), AMFAddr: "10.0.0.1:38412", BindAddr: "10.0.1.5"}, {Config: cfg(2), AMFAddr: "10.0.0.1:38412", BindAddr: "10.0.1.6"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ln.SUPIs) != 1 || ln.SUPIs[0] != (SUPIBlock{PLMN: "208/93", First: 208930000000001, Last: 208930000000004}) {
		t.Errorf("load SUPIs = %+v", ln.SUPIs)
	}
	if len(ln.GNBs) != 2 || ln.GNBs[1] != (GNBKey{AMF: "10.0.0.1:38412", PLMN: "208/93", ID: 2}) || len(ln.N2IPs) != 2 {
		t.Errorf("load gNBs %+v, N2 %v", ln.GNBs, ln.N2IPs)
	}
	fn, err := FleetNeeds(FleetRunSpec{
		AMFAddr: "10.0.0.1:38412", BaseIMSI: "208930000000100", Count: 3, MCC: "208", MNC: "93",
		GNBs: []FleetGNB{{Config: cfg(9), BindAddr: "10.0.1.9"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fn.SUPIs[0] != (SUPIBlock{PLMN: "208/93", First: 208930000000100, Last: 208930000000102}) ||
		fn.GNBs[0] != (GNBKey{AMF: "10.0.0.1:38412", PLMN: "208/93", ID: 9}) || fn.N2IPs[0] != "10.0.1.9" {
		t.Errorf("fleet needs = %+v", fn)
	}
}

// Leases are released however a run ends: complete, failed or cancelled.
func TestLeasesReleasedOnEveryEnd(t *testing.T) {
	needs := testNeeds("208930000000001", 1, 1, "10.0.0.11")
	for _, tc := range []struct {
		name  string
		end   func(r *RunRegistry, id string)
		fn    func(ctx context.Context, s *load.LiveStats, _ RunEventFunc) (load.Report, error)
		state RunState
	}{
		{name: "complete", state: RunComplete, fn: func(context.Context, *load.LiveStats, RunEventFunc) (load.Report, error) {
			return load.Report{}, nil
		}},
		{name: "failed", state: RunFailed, fn: func(context.Context, *load.LiveStats, RunEventFunc) (load.Report, error) {
			return load.Report{}, errors.New("no AMF")
		}},
		{name: "cancelled", state: RunCancelled, end: func(r *RunRegistry, id string) { _, _ = r.Stop(id) },
			fn: func(ctx context.Context, _ *load.LiveStats, _ RunEventFunc) (load.Report, error) {
				<-ctx.Done()
				return load.Report{}, ctx.Err()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := quietRegistry(0)
			info, err := r.StartLoad(RunOptions{Needs: needs}, tc.fn)
			if err != nil {
				t.Fatal(err)
			}
			if tc.end != nil {
				tc.end(r, info.ID)
			}
			waitState(t, r, info.ID, tc.state)
			r.mu.Lock()
			_, held := r.leases.held[info.ID]
			r.mu.Unlock()
			if held {
				t.Fatalf("a %s run still holds its leases", tc.state)
			}
			if _, err := r.StartLoad(RunOptions{Needs: needs}, func(context.Context, *load.LiveStats, RunEventFunc) (load.Report, error) {
				return load.Report{}, nil
			}); err != nil {
				t.Errorf("the same resources after a %s run: %v", tc.state, err)
			}
		})
	}
}

// Resolving an AMF name happens outside the registry lock and is bounded:
// while one StartRun waits on a hanging resolver, other calls on the
// registry return at once, and a resolver past its deadline falls back to
// the name as written.
func TestSlowAMFResolutionBlocksNobody(t *testing.T) {
	r := quietRegistry(0)
	entered := make(chan struct{})
	unblock := make(chan struct{})
	r.leases.resolveAMF = func(ctx context.Context, amf string) string {
		close(entered)
		select {
		case <-unblock:
		case <-ctx.Done():
		}
		return amf
	}
	started := make(chan error, 1)
	go func() {
		_, err := r.StartLoad(RunOptions{Needs: Needs{GNBs: []GNBKey{{AMF: "amf.slow:38412", PLMN: "001/01", ID: 1}}}},
			func(context.Context, *load.LiveStats, RunEventFunc) (load.Report, error) { return load.Report{}, nil })
		started <- err
	}()
	<-entered
	done := make(chan struct{})
	go func() {
		r.List()
		_, _ = r.Get("run-none")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("List/Get blocked behind a StartRun resolving its AMF")
	}
	close(unblock)
	if err := <-started; err != nil {
		t.Fatalf("StartLoad: %v", err)
	}
}

// A resolver that never answers is cut off at the timeout and the name is
// leased as written.
func TestResolveAMFTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if got := resolveAMF(ctx, "Some.Host.Invalid:38412"); got != "some.host.invalid:38412" {
		t.Errorf("resolveAMF after the deadline = %q, want the lower-cased name", got)
	}
}
