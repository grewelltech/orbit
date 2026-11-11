package engine

import (
	"slices"
	"testing"
)

// An AN release lists the PDU sessions whose user plane is up, and none
// once the user plane is down or for a UE without a session.
func TestActiveSessionIDs(t *testing.T) {
	two := &AttachResult{Sessions: []SessionResult{{PDUSessionID: 1}, {PDUSessionID: 5}}}
	for _, tc := range []struct {
		name   string
		result *AttachResult
		upDown bool
		want   []int64
	}{
		{name: "two sessions up", result: two, want: []int64{1, 5}},
		{name: "user plane down", result: two, upDown: true},
		{name: "registered without a session", result: &AttachResult{}},
		{name: "no attach result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Result: tc.result}
			s.setConnection(false, tc.upDown)
			if got := s.activeSessionIDs(); !slices.Equal(got, tc.want) {
				t.Errorf("activeSessionIDs = %v, want %v", got, tc.want)
			}
		})
	}
}
