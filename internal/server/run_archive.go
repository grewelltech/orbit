package server

import (
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
)

// archiveRun captures a terminal run and writes it to the store.
//
// Called from the sampler at the moment the run goes terminal, which is the one
// place that holds everything at once: the final info, the completed frame
// series, and a registry that has not yet evicted the record.
//
// What is stored is what the handlers ALREADY return — the same proto messages,
// built by the same converters. A restored run is therefore served by handing
// back what was stored, not by rebuilding engine state and re-deriving it, so
// it cannot drift into a degraded copy of a live one.
func (s *runService) archiveRun(id string, info engine.RunInfo, frames []*orbitv1.TelemetryFrame) {
	if !s.archive.enabled() || !info.State.Terminal() {
		return
	}
	a := &orbitv1.RunArchive{
		Version: archiveVersion,
		Run:     runProto(info),
		Frames:  frames,
	}

	// The final progress snapshot, exactly as GetRun would have returned it.
	switch info.Kind {
	case engine.RunKindLoad:
		if snap, ok := s.reg.Snapshot(id); ok {
			a.LoadProgress = loadProgressProto(snap)
		}
	case engine.RunKindFleet:
		if snap, ok := s.reg.FleetSnapshot(id); ok {
			// nil rate tracker, matching GetRun: rates come from consecutive
			// samples, and a final snapshot has no successor to differ from.
			a.FleetProgress = fleetProgressProto(snap, time.Now(), nil)
		}
	}

	// The report, which exists only for a run that completed cleanly.
	if info.State == engine.RunComplete {
		switch info.Kind {
		case engine.RunKindLoad:
			if rep, ok := s.reg.Report(id); ok {
				a.Report = &orbitv1.RunArchive_LoadReport{LoadReport: loadReportProto(rep)}
			}
		case engine.RunKindFleet:
			if rep, ok := s.reg.FleetResult(id); ok {
				a.Report = &orbitv1.RunArchive_FleetReport{FleetReport: fleetReportProto(rep)}
			}
		}
	}

	// The whole retained event log. fromSeq 0 asks for everything the ring
	// still holds; what it evicted is unrecoverable and reported as a gap.
	sub, err := s.reg.SubscribeEvents(id, 0)
	if err == nil {
		defer sub.Close()
		a.EventsDroppedBefore = sub.DroppedBefore
		for _, ev := range sub.Backlog {
			a.Events = append(a.Events, runEventProto(ev))
		}
	}

	if err := s.archive.save(a); err != nil {
		// The run itself succeeded; failing to archive it does not change that,
		// so this is reported and not propagated.
		s.log.Error("could not archive run", "run_id", id, "err", err)
		return
	}
	s.log.Info("archived run", "run_id", id, "frames", len(a.Frames), "events", len(a.Events))
}

// restoreFrames seeds a frame log per archived run, pre-populated and closed.
//
// This is what makes RunTelemetry work for a restored run without a special
// case: subscribeFrom on a closed log already returns the backlog plus a closed
// channel, which is precisely "here is the history, there is no more". Marking
// the log sampling also stops frameLogFor from starting a sampler for a run the
// registry has never heard of — that sampler would find nothing, close the log,
// and discard the restored series.
func (s *runService) restoreFrames() {
	if !s.archive.enabled() {
		return
	}
	s.framesMu.Lock()
	defer s.framesMu.Unlock()
	if s.frames == nil {
		s.frames = make(map[string]*frameLog)
	}
	for _, id := range s.archive.ids() {
		a, ok := s.archive.get(id)
		if !ok {
			continue
		}
		l := newFrameLog(maxInt(len(a.GetFrames()), 1))
		for _, f := range a.GetFrames() {
			l.append(f)
		}
		l.close()
		l.sampling = true
		s.frames[id] = l
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// archivedReportResponse serves GetRunReport for a run that exists only on
// disk. The state check mirrors the live path: a report exists only for a run
// that completed, and saying so beats returning an empty one.
func archivedReportResponse(id string, a *orbitv1.RunArchive) (*connect.Response[orbitv1.GetRunReportResponse], error) {
	run := a.GetRun()
	if run.GetState() != orbitv1.RunState_RUN_STATE_COMPLETE {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("run %s is %s; a report is available only once complete",
				id, runStateLabel(run.GetState())))
	}
	resp := &orbitv1.GetRunReportResponse{Run: run}
	switch r := a.GetReport().(type) {
	case *orbitv1.RunArchive_LoadReport:
		resp.Report = &orbitv1.GetRunReportResponse_Load{Load: r.LoadReport}
	case *orbitv1.RunArchive_FleetReport:
		resp.Report = &orbitv1.GetRunReportResponse_Fleet{Fleet: r.FleetReport}
	}
	return connect.NewResponse(resp), nil
}

// sendArchivedEvents replays a stored event log and returns, which ends the
// stream. A restored run is finished by definition, so there is nothing to
// follow — the stream closing IS the signal that the log is complete.
func sendArchivedEvents(stream *connect.ServerStream[orbitv1.RunEvent], a *orbitv1.RunArchive, fromSeq uint64) error {
	first := true
	for _, ev := range a.GetEvents() {
		if ev.GetSeq() < fromSeq {
			continue
		}
		if first {
			// Reported once, on the first event, exactly as the live path does.
			// Cloned so replaying to a second subscriber does not mutate the
			// stored archive.
			ev = proto.Clone(ev).(*orbitv1.RunEvent)
			ev.DroppedBefore = archivedDroppedBefore(a, fromSeq)
			first = false
		}
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

// archivedDroppedBefore is how many events before the caller's resume point are
// unavailable: those the ring evicted while the run was live, plus any the
// archive itself does not reach back to.
func archivedDroppedBefore(a *orbitv1.RunArchive, fromSeq uint64) uint64 {
	evs := a.GetEvents()
	if len(evs) == 0 {
		return a.GetEventsDroppedBefore()
	}
	if earliest := evs[0].GetSeq(); earliest > fromSeq {
		return earliest - fromSeq
	}
	return 0
}
