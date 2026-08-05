package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	loomv1 "github.com/bgrewell/loom/api/loomv1"
)

// FleetFarEnd is what the N6 agent reports about a cohort's traffic — a second
// observer of the same flows, measured on its own clock beyond the UPF.
//
// It is deliberately NOT a delivery ratio against the N3 counters. Those count
// encapsulated inner-IP bytes while this counts application payload, so their
// quotient sits near 1.04 even when nothing is lost; and for the TCP apps
// (http, video) the transport retransmits until delivered, so an application
// byte-delta is ~0 whatever the core does. Where UDP loss is real — voip —
// RTP sequence numbers already measure it properly and feed MOS.
//
// What a second observer is genuinely good for is disagreement: traffic that
// never reaches the intended far end, a peer that is not the one configured,
// and ORBIT's own accounting being wrong (a session once reported 0.00 Mbps
// while 3.47 MB crossed the tunnel — an independent witness catches that at
// once).
type FleetFarEnd struct {
	// Available is false when no far-end view could be collected; Reason then
	// says why, so an absent witness is never read as a silent zero.
	Available bool
	Reason    string

	Bytes      uint64
	Packets    uint64
	BitsPerSec float64 // the far end's own interval rate, from its own clock

	// Requests and Errors are the http origin's application view; zero for
	// apps that do not serve requests.
	Requests uint64
	Errors   uint64
}

// farEndWatch accumulates one cohort's far-end telemetry. A cohort with a
// shared origin has exactly one of these; the fields are read by the sampler
// while the stream writes them.
type farEndWatch struct {
	mu  sync.Mutex
	cur FleetFarEnd
}

func newFarEndWatch() *farEndWatch { return &farEndWatch{} }

// unavailable records why no far-end view exists, so the reason travels with
// the cohort instead of the panel showing an unexplained blank.
func (w *farEndWatch) unavailable(reason string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.cur = FleetFarEnd{Reason: reason}
	w.mu.Unlock()
}

// observe folds in one telemetry sample. The rate comes from the sample's own
// interval accounting rather than being derived here: the agent measured that
// interval on its own clock, which is the whole point of a second observer.
func (w *farEndWatch) observe(s *loomv1.TelemetrySample) {
	if w == nil || s == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cur.Available = true
	w.cur.Reason = ""
	w.cur.Bytes = s.GetBytes()
	w.cur.Packets = s.GetPackets()
	if ns := s.GetIntervalNanos(); ns > 0 {
		w.cur.BitsPerSec = float64(s.GetIntervalBytes()) * 8 / (float64(ns) / 1e9)
	}
	if h := s.GetApp().GetHttp(); h != nil {
		// Cumulative across the run: the origin reports per-interval counts,
		// so a running total is what a cohort-level view wants.
		w.cur.Requests += h.GetRequests()
		w.cur.Errors += h.GetErrors()
	}
}

func (w *farEndWatch) snapshot() FleetFarEnd {
	if w == nil {
		return FleetFarEnd{Reason: "far-end telemetry not collected for this cohort"}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

// watchFarEnd streams a far-end flow's telemetry into w until ctx ends. A
// dropped stream is resubscribed rather than abandoned: the witness is only
// useful if it keeps watching, and the lazy grpc connection reconnects
// transparently underneath.
func watchFarEnd(ctx context.Context, log *slog.Logger, a *fleetAgent, flowID, cohort string, w *farEndWatch) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		stream, err := a.client.StreamTelemetry(ctx, &loomv1.TelemetryRequest{FlowId: flowID})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.unavailable("far-end telemetry subscribe failed: " + err.Error())
			if attempt == 0 {
				log.Warn("fleet far-end telemetry subscribe; retrying",
					"cohort", cohort, "flow", flowID, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for {
			s, rerr := stream.Recv()
			if rerr != nil {
				if rerr == io.EOF || ctx.Err() != nil {
					return
				}
				break // resubscribe
			}
			w.observe(s)
		}
	}
}
