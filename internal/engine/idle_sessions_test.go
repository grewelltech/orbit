package engine

import (
	"io"
	"log/slog"
	"slices"
	"testing"
)

// An AN release lists the PDU sessions whose user plane is up, and none
// once the user plane is down or for a UE without a session; a session the
// UE released is not listed.
func TestActiveSessionIDs(t *testing.T) {
	two := &AttachResult{Sessions: []SessionResult{{PDUSessionID: 1}, {PDUSessionID: 5}}}
	for _, tc := range []struct {
		name     string
		result   *AttachResult
		upDown   bool
		released []uint8
		want     []int64
	}{
		{name: "two sessions up", result: two, want: []int64{1, 5}},
		{name: "the first released", result: two, released: []uint8{1}, want: []int64{5}},
		{name: "user plane down", result: two, upDown: true},
		{name: "registered without a session", result: &AttachResult{}},
		{name: "no attach result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Result: tc.result}
			s.setConnection(false, tc.upDown)
			for _, id := range tc.released {
				s.markPDUReleased(id)
			}
			if got := s.activeSessionIDs(); !slices.Equal(got, tc.want) {
				t.Errorf("activeSessionIDs = %v, want %v", got, tc.want)
			}
		})
	}
}

// A Service Accept that reactivates any session the UE holds brings the
// user plane up, the first reactivated one giving the data path; one that
// reactivates only a released session, or none, leaves it down.
func TestServiceAcceptUserPlane(t *testing.T) {
	for _, tc := range []struct {
		name        string
		released    []uint8
		reactivated []uint8
		wantUp      bool
		wantTEID    uint32
	}{
		{name: "the first session", reactivated: []uint8{1}, wantUp: true, wantTEID: 101},
		{name: "the second session only", reactivated: []uint8{5}, wantUp: true, wantTEID: 105},
		{name: "none", wantUp: false},
		{name: "the first released, the second reactivated", released: []uint8{1}, reactivated: []uint8{5}, wantUp: true, wantTEID: 105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{SUPI: "208930100007542", Result: &AttachResult{Sessions: []SessionResult{{PDUSessionID: 1}, {PDUSessionID: 5}}}}
			s.setConnection(true, true)
			for _, id := range tc.released {
				s.markPDUReleased(id)
			}
			res := &ServiceResult{Accepted: true}
			for _, id := range tc.reactivated {
				res.Reactivated = append(res.Reactivated, SessionResult{PDUSessionID: id, UPFTEID: 100 + uint32(id)})
			}
			r := &serviceRun{m: NewManager(slog.New(slog.NewTextHandler(io.Discard, nil))), sess: s, res: res}
			r.apply()
			if got := !s.userPlaneDown(); got != tc.wantUp {
				t.Errorf("user plane up = %v, want %v", got, tc.wantUp)
			}
			if tc.wantUp && s.Result.UPFTEID != tc.wantTEID {
				t.Errorf("data path UPF TEID %d, want %d", s.Result.UPFTEID, tc.wantTEID)
			}
		})
	}
}

// The sessions a Service Request reports as held leave out the ones the UE
// released.
func TestHeldSessions(t *testing.T) {
	s := &Session{Result: &AttachResult{Sessions: []SessionResult{{PDUSessionID: 1}, {PDUSessionID: 5}}}}
	s.markPDUReleased(1)
	var ids []uint8
	for _, sr := range s.heldSessions() {
		ids = append(ids, sr.PDUSessionID)
	}
	if !slices.Equal(ids, []uint8{5}) {
		t.Errorf("held sessions %v, want [5]", ids)
	}
}
