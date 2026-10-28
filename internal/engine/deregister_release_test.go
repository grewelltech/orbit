package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
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
		name          string
		opts          []mockamf.Option
		linger        time.Duration
		ctxTimeout    time.Duration // the Deregister call's context
		wantComplete  int64
		wantDetail    string
		wantDetailEnd string // the detail's end, when it holds a measured time
		minWait       time.Duration
		maxWait       time.Duration
	}{
		{name: "released: completed before the close", linger: 10 * time.Second, ctxTimeout: 10 * time.Second,
			wantComplete: 1, wantDetail: "UE deregistered (switch-off)", maxWait: 2 * time.Second},
		// A Downlink NAS Transport and a release's successful outcome come
		// first: neither is the command, and the wait goes on to it.
		// The release comes 200ms late, so a Complete sent for either of
		// the others arrives before it and is a stray.
		{name: "other PDUs before the release: skipped", opts: []mockamf.Option{mockamf.WithNoiseBeforeRelease(), mockamf.WithReleaseDelay(200 * time.Millisecond)},
			linger: 10 * time.Second, ctxTimeout: 10 * time.Second,
			wantComplete: 1, wantDetail: "UE deregistered (switch-off)", maxWait: 2 * time.Second},
		{name: "never released: bounded by the linger", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			linger: 300 * time.Millisecond, ctxTimeout: 10 * time.Second,
			wantDetail: "UE deregistered (switch-off); the core sent no UE Context Release within 300ms",
			minWait:    300 * time.Millisecond, maxWait: 2 * time.Second},
		{name: "never released: bounded by the caller's context", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			linger: time.Minute, ctxTimeout: 500 * time.Millisecond,
			wantDetail: "the core sent no UE Context Release: waited ", wantDetailEnd: "ms: context deadline exceeded", maxWait: 2 * time.Second},
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
			if n := amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d UE Context Release Completes came before the release they answer", n)
			}
			// The Complete names the UE the command released.
			if tc.wantComplete > 0 {
				sent, got := amf.DeregistrationReleaseIDs(), amf.ReleaseCompleteIDs()
				if len(sent) != 1 || len(got) != 1 || got[0] != sent[0] {
					t.Errorf("Release Complete for (AMF, RAN) UE NGAP IDs %v, want %v", got, sent)
				}
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
			if !strings.Contains(detail, tc.wantDetail) || !strings.HasSuffix(detail, tc.wantDetailEnd) || (tc.wantComplete == 1 && strings.Contains(detail, "no UE Context Release")) {
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
	if n := awaitSwitchOffReleases(ctx, sessions, linger, testLogger()); n != 4 {
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
		wantNote     string // in the teardown milestone
		hold         time.Duration
	}{
		{name: "released", wantComplete: 4, maxWait: switchOffReleaseLinger, wantNote: "deregistered 4/4 UEs"},
		{name: "never released", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			minWait: switchOffReleaseLinger, maxWait: 2 * switchOffReleaseLinger,
			wantNote: "deregistered 4/4 UEs; the core sent no UE Context Release for 4 of them within 3s"},
		// Each UE is released mid-run, 20ms after it registers, and the
		// release answering its switch-off comes 300ms late: the old one
		// waiting in the inbox must not be taken for it.
		{name: "a release queued before the deregistration is not taken",
			opts:         []mockamf.Option{mockamf.WithStaleRelease(20 * time.Millisecond), mockamf.WithReleaseDelay(300 * time.Millisecond)},
			hold:         300 * time.Millisecond,
			wantComplete: 4, minWait: 600 * time.Millisecond, maxWait: switchOffReleaseLinger, wantNote: "deregistered 4/4 UEs"},
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
			var mu sync.Mutex
			var milestones []string
			events := NewRunEvents(func(_, _, _, message string) {
				mu.Lock()
				defer mu.Unlock()
				milestones = append(milestones, message)
			}, EventsNormal)
			start := time.Now()
			rep, err := RunFleet(ctx, testLogger(), FleetRunSpec{
				AMFAddr:  addr,
				GNBs:     []FleetGNB{{Config: cfg}, {Config: cfgB}},
				BaseIMSI: "208930100007500", Count: 4, MCC: "208", MNC: "93", Ki: ki, OPc: opc,
			}, FleetBehaviors{Duration: tc.hold, Hold: tc.hold > 0}, FleetDeps{Stats: NewFleetLiveStats(), Events: events})
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
			if n := amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d UE Context Release Completes came before the release they answer", n)
			}
			// Each Complete names a UE a command released, each UE once.
			sent := map[[2]int64]bool{}
			for _, ids := range amf.DeregistrationReleaseIDs() {
				sent[ids] = true
			}
			for _, ids := range amf.ReleaseCompleteIDs() {
				if !sent[ids] {
					t.Errorf("a Release Complete for %v, which no command released (sent %v)", ids, amf.DeregistrationReleaseIDs())
				}
				delete(sent, ids)
			}
			mu.Lock()
			var teardown string
			for _, m := range milestones {
				if strings.HasPrefix(m, "deregistered ") {
					teardown = m
				}
			}
			mu.Unlock()
			if teardown != tc.wantNote {
				t.Errorf("teardown milestone %q, want %q", teardown, tc.wantNote)
			}
			// The whole run: four attaches take milliseconds, the teardown
			// up to one linger.
			if took < tc.minWait || took > tc.maxWait {
				t.Errorf("the run took %s, want %s..%s", took, tc.minWait, tc.maxWait)
			}
		})
	}
}
