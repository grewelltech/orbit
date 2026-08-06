package server

import (
	"testing"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

func frame(seq uint64, flows int) *orbitv1.TelemetryFrame {
	rows := make([]*orbitv1.FlowRow, flows)
	for i := range rows {
		rows[i] = &orbitv1.FlowRow{Supi: "s"}
	}
	return &orbitv1.TelemetryFrame{
		FrameSeq: seq,
		Progress: &orbitv1.TelemetryFrame_Fleet{Fleet: &orbitv1.FleetProgress{
			Attached: 1, Flows: rows, FlowsTotal: uint32(flows),
		}},
	}
}

// The point of the log: a subscriber arriving late — a reload, or attaching to
// a run already under way — gets the history rather than an empty chart.
func TestFrameLogReplaysHistory(t *testing.T) {
	l := newFrameLog(100)
	for i := uint64(0); i < 5; i++ {
		l.append(frame(i, 0))
	}
	sub := l.subscribeFrom(0)
	defer sub.Close()
	if len(sub.Backlog) != 5 {
		t.Fatalf("backlog = %d frames, want 5", len(sub.Backlog))
	}
	if sub.DroppedBefore != 0 {
		t.Errorf("DroppedBefore = %d, want 0 — nothing was evicted", sub.DroppedBefore)
	}

	// Resuming mid-series returns only what the caller has not seen.
	sub2 := l.subscribeFrom(3)
	defer sub2.Close()
	if len(sub2.Backlog) != 2 || sub2.Backlog[0].GetFrameSeq() != 3 {
		t.Errorf("resume from 3 gave %d frames starting at %d, want 2 starting at 3",
			len(sub2.Backlog), sub2.Backlog[0].GetFrameSeq())
	}
}

// Eviction must be stated, not silently drawn as a continuous series.
func TestFrameLogReportsEviction(t *testing.T) {
	l := newFrameLog(4)
	for i := uint64(0); i < 10; i++ {
		l.append(frame(i, 0))
	}
	sub := l.subscribeFrom(0)
	defer sub.Close()
	if len(sub.Backlog) != 4 {
		t.Fatalf("retained %d frames, want the cap 4", len(sub.Backlog))
	}
	if sub.Backlog[0].GetFrameSeq() != 6 {
		t.Errorf("oldest retained seq = %d, want 6", sub.Backlog[0].GetFrameSeq())
	}
	if sub.DroppedBefore != 6 {
		t.Errorf("DroppedBefore = %d, want 6", sub.DroppedBefore)
	}
}

// Flow rows dominate a frame's size and nothing reads them from history, so
// they are dropped on retention — but the live copy keeps them, because the
// flow table shows the latest frame.
func TestFrameLogTrimsFlowsButKeepsLiveCopy(t *testing.T) {
	l := newFrameLog(10)
	sub := l.subscribeFrom(0)
	defer sub.Close()

	live := frame(0, 100)
	l.append(live)

	select {
	case got := <-sub.Ch:
		if n := len(got.GetFleet().GetFlows()); n != 100 {
			t.Errorf("live frame carried %d flow rows, want 100", n)
		}
	default:
		t.Fatal("live subscriber received nothing")
	}

	retained := l.subscribeFrom(0)
	defer retained.Close()
	if n := len(retained.Backlog[0].GetFleet().GetFlows()); n != 0 {
		t.Errorf("retained frame kept %d flow rows, want 0", n)
	}
	// The count survives: how many flows there were is part of the history.
	if got := retained.Backlog[0].GetFleet().GetFlowsTotal(); got != 100 {
		t.Errorf("retained flows_total = %d, want 100", got)
	}
	// Trimming must not have mutated the frame the live subscriber holds.
	if n := len(live.GetFleet().GetFlows()); n != 100 {
		t.Errorf("trimming mutated the original frame (%d rows left)", n)
	}
}

// A finished run still replays: the log closes live delivery but keeps the
// series, which is what makes a completed run reviewable at all.
func TestFrameLogClosedStillReplays(t *testing.T) {
	l := newFrameLog(10)
	for i := uint64(0); i < 3; i++ {
		l.append(frame(i, 0))
	}
	l.close()

	sub := l.subscribeFrom(0)
	defer sub.Close()
	if len(sub.Backlog) != 3 {
		t.Errorf("a finished run replayed %d frames, want 3", len(sub.Backlog))
	}
	if _, open := <-sub.Ch; open {
		t.Error("channel is open on a finished run; a subscriber would wait forever")
	}
}
