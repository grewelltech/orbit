package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// A switch-off Deregister keeps the association open until the core's UE
// Context Release Command has come and been completed, so the AMF never
// sees the SHUTDOWN ahead of the deregistration. A core that sends no
// release costs the linger, or less when the caller's context ends first,
// and is reported on the DEREGISTERED event.
func TestDeregisterAwaitsRelease(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		linger       time.Duration
		ctxTimeout   time.Duration // the Deregister call's context
		wantComplete int64
		wantDetail   string
		minWait      time.Duration
		maxWait      time.Duration
	}{
		{name: "released: completed before the close", linger: 10 * time.Second, ctxTimeout: 10 * time.Second,
			wantComplete: 1, wantDetail: "UE deregistered (switch-off)", maxWait: 2 * time.Second},
		{name: "never released: bounded by the linger", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			linger: 300 * time.Millisecond, ctxTimeout: 10 * time.Second,
			wantDetail: "UE deregistered (switch-off); the core sent no UE Context Release within 300ms",
			minWait:    300 * time.Millisecond, maxWait: 2 * time.Second},
		{name: "never released: bounded by the caller's context", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			linger: time.Minute, ctxTimeout: 500 * time.Millisecond,
			wantDetail: "the core sent no UE Context Release", maxWait: 2 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithGUTI()}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			_, cfg, _, _ := newBindTestAMF(t)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.dial = (&fakeN2Dialer{amf: amf}).dial
			m.releaseLinger = tc.linger
			events, stop := m.Subscribe()
			defer stop()

			const supi = "208930100000001"
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if res, err := m.Register(rctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supi, ki, opc)); err != nil || !res.Registered {
				t.Fatalf("Register: %v", err)
			}
			dctx, dcancel := context.WithTimeout(context.Background(), tc.ctxTimeout)
			defer dcancel()
			start := time.Now()
			if err := m.Deregister(dctx, supi); err != nil {
				t.Fatalf("Deregister: %v", err)
			}
			took := time.Since(start)
			if took < tc.minWait || took > tc.maxWait {
				t.Errorf("Deregister took %s, want %s..%s", took, tc.minWait, tc.maxWait)
			}
			// Deregister has returned, so the association is closed: a
			// Release Complete counted now was sent before the close.
			deadline := time.Now().Add(time.Second)
			for amf.ReleaseCompletes() < tc.wantComplete && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := amf.ReleaseCompletes(); got != tc.wantComplete {
				t.Errorf("the mock got %d UE Context Release Completes, want %d", got, tc.wantComplete)
			}
			if amf.Deregistrations() != 1 {
				t.Errorf("the mock got %d Deregistration Requests, want 1", amf.Deregistrations())
			}
			var detail string
			for detail == "" {
				select {
				case ev := <-events:
					if ev.SUPI == supi && ev.State == StateDeregistered {
						detail = ev.Detail
					}
				case <-time.After(time.Second):
					t.Fatal("no DEREGISTERED event")
				}
			}
			if !strings.Contains(detail, tc.wantDetail) || (tc.wantComplete == 1 && strings.Contains(detail, "no UE Context Release")) {
				t.Errorf("DEREGISTERED detail %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

// A fleet's UEs wait for their releases at once: the teardown of several
// UEs a core never releases costs one linger, not one per UE.
func TestAwaitSwitchOffReleasesParallel(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, mockamf.WithGUTI(), mockamf.WithNoReleaseOnDeregistration())
	if err != nil {
		t.Fatal(err)
	}
	_, cfg, _, _ := newBindTestAMF(t)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = (&fakeN2Dialer{amf: amf}).dial
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sessions []*Session
	for _, supi := range []string{"208930100000001", "208930100000002", "208930100000003", "208930100000004"} {
		if res, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supi, ki, opc)); err != nil || !res.Registered {
			t.Fatalf("Register %s: %v", supi, err)
		}
		s, err := m.Status(supi)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.conn.Close() }()
		if err := s.deregister(ctx); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	const linger = 400 * time.Millisecond
	start := time.Now()
	if n := awaitSwitchOffReleases(ctx, sessions, linger); n != 4 {
		t.Errorf("%d unreleased, want 4", n)
	}
	if took := time.Since(start); took > 2*linger {
		t.Errorf("4 waits took %s: serialised (linger %s)", took, linger)
	}
}

// A fleet run's UEs, muxed on their gNBs' shared associations, are each
// released and completed before the associations close; with a core that
// releases none, the teardown waits one linger for all of them, not one
// per UE.
func TestRunFleetAwaitsReleases(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		wantComplete int64
		minWait      time.Duration
		maxWait      time.Duration
	}{
		{name: "released", wantComplete: 4, maxWait: switchOffReleaseLinger},
		{name: "never released", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			minWait: switchOffReleaseLinger, maxWait: 2 * switchOffReleaseLinger},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithGUTI()}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			_, cfg, _, _ := newBindTestAMF(t)
			addr := sctpFront(t, amf)
			cfgB := cfg
			cfgB.ID, cfgB.Name = 2, ""
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := time.Now()
			rep, err := RunFleet(ctx, testLogger(), FleetRunSpec{
				AMFAddr:  addr,
				GNBs:     []FleetGNB{{Config: cfg}, {Config: cfgB}},
				BaseIMSI: "208930100007500", Count: 4, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
			}, FleetBehaviors{}, FleetDeps{Stats: NewFleetLiveStats()})
			took := time.Since(start)
			if err != nil {
				t.Fatalf("RunFleet: %v", err)
			}
			if rep.Attached != 4 || rep.Deregistered != 4 {
				t.Fatalf("report = %+v, want 4 attached and deregistered", rep)
			}
			// RunFleet has returned, so the associations are closed.
			deadline := time.Now().Add(time.Second)
			for amf.ReleaseCompletes() < tc.wantComplete && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := amf.ReleaseCompletes(); got != tc.wantComplete {
				t.Errorf("the mock got %d UE Context Release Completes, want %d", got, tc.wantComplete)
			}
			// The whole run: four attaches take milliseconds, the teardown
			// up to one linger.
			if took < tc.minWait || took > tc.maxWait {
				t.Errorf("the run took %s, want %s..%s", took, tc.minWait, tc.maxWait)
			}
		})
	}
}
