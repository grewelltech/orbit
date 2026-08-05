package engine

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// HandoverLoadSpec drives a mobile UE through a series of N2 handovers while an
// optional attach storm loads the core, to measure whether handover latency
// and success degrade under load.
type HandoverLoadSpec struct {
	MobileSUPI string
	Ki, OPc    []byte
	MCC, MNC   string
	AMFAddr    string
	SourceGNB  gnb.Config // register the mobile UE here
	SourceN3   string     // its gNB N3 (data path)
	Slice      ue.PDUSessionParams
	Hops       []GNBEndpoint // hand the UE through these in order (fresh IDs, alternating binds)
	Background *LoadSpec     // optional concurrent attach storm (nil = handovers alone)
	Profile    string        // core-compatibility profile (e.g. "sdcore")
}

// HandoverLoadReport summarises the handover measurements and the concurrent
// background storm.
type HandoverLoadReport struct {
	Handovers        int
	HandoverFailures int
	P50, P99, Max    time.Duration
	Background       *load.Report // nil if no storm was configured
	FirstError       error
}

// RunHandoverUnderLoad registers the mobile UE, optionally starts a background
// attach storm, and times each handover through the hop list, returning the
// handover latency distribution alongside the storm's result. Handovers are
// sequential (the UE has one session that moves); the load is the concurrent
// attach storm. Run from the RAN node with distinct routed source IPs per gNB.
func RunHandoverUnderLoad(ctx context.Context, log *slog.Logger, spec HandoverLoadSpec) (*HandoverLoadReport, error) {
	mgr := NewManager(log)
	if spec.Profile != "" {
		if err := mgr.UseProfile(spec.Profile); err != nil {
			return nil, err
		}
	}

	id, err := ue.ParseIdentity(spec.MobileSUPI, spec.MCC, spec.MNC, "0")
	if err != nil {
		return nil, err
	}
	pdu := spec.Slice
	if _, err := mgr.Register(ctx, spec.AMFAddr, spec.SourceGNB, UEConfig{
		Identity:   id,
		Sub:        auth.Subscription{SUPI: spec.MobileSUPI, Ki: spec.Ki, OPc: spec.OPc},
		PDUSession: &pdu,
		GNBN3Addr:  spec.SourceN3,
	}); err != nil {
		return nil, err
	}

	// Background attach storm, concurrent with the handovers.
	//
	// The storm's results land in goroutine-local variables and are merged
	// after wg.Wait(), which supplies the happens-before edge. Writing them
	// straight into rep would race the handover loop below, which writes the
	// same struct from this goroutine.
	rep := &HandoverLoadReport{}
	var bgReport *load.Report
	var bgErr error
	var wg sync.WaitGroup
	if spec.Background != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bg, err := RunLoad(ctx, log, *spec.Background)
			bgReport, bgErr = &bg, err
		}()
	}

	var lats []time.Duration
	for _, ep := range spec.Hops {
		hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
		// The Manager times the procedure itself now, so this uses its number
		// rather than measuring the same span a second time.
		d, err := mgr.Handover(hctx, spec.MobileSUPI, ep)
		hcancel()
		if err != nil {
			rep.HandoverFailures++
			if rep.FirstError == nil {
				rep.FirstError = err
			}
			continue
		}
		lats = append(lats, d)
	}
	wg.Wait()

	// Merge the storm's outcome now that it has finished. A handover failure
	// takes precedence over a background failure, which makes FirstError
	// deterministic — previously whichever goroutine won the race decided it.
	if bgReport != nil {
		rep.Background = bgReport
	}
	if bgErr != nil && rep.FirstError == nil {
		rep.FirstError = bgErr
	}

	rep.Handovers = len(lats)
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	rep.P50 = pctl(lats, 0.50)
	rep.P99 = pctl(lats, 0.99)
	if n := len(lats); n > 0 {
		rep.Max = lats[n-1]
	}
	return rep, nil
}

// pctl returns the p-quantile of a sorted duration slice.
func pctl(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}
