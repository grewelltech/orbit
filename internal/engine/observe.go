package engine

import (
	"fmt"

	"github.com/bgrewell/orbit/internal/datapath"
)

// ObserveDownlink opens the UE's N3 data path if needed and installs
// observers on its downlink lane: onEndMarker for each GTP-U End Marker and
// onGPDU for each downlink G-PDU, on the lane's live TEID or on a TEID a
// handover just vacated (within the End-Marker grace window). The lane
// follows the UE across handovers, so observers installed before a path
// switch see what the UPF sends on both the old and the new path. Either
// callback may be nil. Callbacks run on demux reader goroutines (the source
// and target sockets may fire concurrently) and must not block. The returned
// func removes both observers.
//
// The End Marker slot is shared with an app session's correlator: a caller
// that observes a UE running an app session replaces its End Marker events.
func (m *Manager) ObserveDownlink(supi string, onEndMarker func(datapath.EndMarker), onGPDU func(datapath.GPDUArrival)) (func(), error) {
	m.mu.Lock()
	sess, ok := m.sessions[supi]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("UE %s is not registered", supi)
	}
	if !sess.Result.SessionActive {
		return nil, fmt.Errorf("UE %s has no active PDU session", supi)
	}
	if sess.gnbN3 == "" {
		return nil, fmt.Errorf("UE %s registered without a gNB N3 address; data path disabled", supi)
	}
	_, rx, err := sess.dataplane()
	if err != nil {
		return nil, err
	}
	rx.SetEndMarkerFunc(onEndMarker)
	rx.SetGPDUFunc(onGPDU)
	return func() {
		rx.SetEndMarkerFunc(nil)
		rx.SetGPDUFunc(nil)
	}, nil
}
