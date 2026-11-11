package mockamf

import (
	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
)

// WithStoredCapabilitiesInPathSwitchAck makes the mock answer a Path Switch
// Request whose UE security capabilities differ from the ones it gave the
// RAN as TS 33.501 6.7.3.1 has an AMF do: the Acknowledge carries its
// stored capabilities. Without it the mismatch is only counted
// (PathSwitchCapabilityMismatches).
func WithStoredCapabilitiesInPathSwitchAck() Option { return func(a *AMF) { a.capsInAck = true } }

// WithEchoedCapabilitiesInPathSwitchAck makes the mock answer such a Path
// Switch Request with an Acknowledge that carries the capabilities the
// request did, not the stored ones.
func WithEchoedCapabilitiesInPathSwitchAck() Option { return func(a *AMF) { a.capsEchoed = true } }

// PathSwitchCapabilityMismatches reports how many acknowledged Path Switch
// Requests carried UE security capabilities other than those the mock gave
// the RAN for the UE (a request without the IE included).
func (a *AMF) PathSwitchCapabilityMismatches() int64 { return a.capsMismatches.Load() }

// ranSecurityCapabilities converts a NAS UE security capability (TS 24.501
// 9.11.3.54: 5G-EA0..7, 5G-IA0..7, then EEA0..7 and EIA0..7 when present,
// each from bit 8 down) to the NGAP UE Security Capabilities IE, whose
// 16-bit bitmaps start at algorithm 1: the null algorithms have no bit.
func ranSecurityCapabilities(caps nasType.UESecurityCapability) *ngapType.UESecurityCapabilities {
	bitmap := func(i int) uint16 {
		if i >= len(caps.Buffer) || i >= int(caps.GetLen()) {
			return 0
		}
		return uint16(caps.Buffer[i]<<1) << 8
	}
	return gnb.NewUESecurityCapabilities(bitmap(0), bitmap(1), bitmap(2), bitmap(3))
}

// initialContextSetup builds st's Initial Context Setup Request around the
// protected NAS PDU, with the UE security capabilities of its Registration
// Request, and records them as given to the RAN.
func (a *AMF) initialContextSetup(st *ueState, nasPDU []byte) ngapType.NGAPPDU {
	caps := ranSecurityCapabilities(st.ueCaps)
	a.mu.Lock()
	st.ranCaps = caps
	a.mu.Unlock()
	return buildInitialContextSetupRequest(st.amfID, a.ranIDOf(st), nasPDU, a.plmn, a.cfg.SST, a.sdBytes, gnb.CloneUESecurityCapabilities(caps))
}

// checkPathSwitchCaps compares a Path Switch Request's UE security
// capabilities with the ones st's RAN was given, counting a mismatch. It
// returns the capabilities to put in the Acknowledge: non-nil only on a
// mismatch, the stored ones with WithStoredCapabilitiesInPathSwitchAck and
// the request's own with WithEchoedCapabilitiesInPathSwitchAck. The caller
// holds a.mu.
func (a *AMF) checkPathSwitchCaps(st *ueState, got *ngapType.UESecurityCapabilities) *ngapType.UESecurityCapabilities {
	if st.ranCaps == nil || gnb.SameUESecurityCapabilities(st.ranCaps, got) {
		return nil
	}
	a.capsMismatches.Add(1)
	switch {
	case a.capsEchoed:
		return gnb.CloneUESecurityCapabilities(got)
	case a.capsInAck:
		return gnb.CloneUESecurityCapabilities(st.ranCaps)
	}
	return nil
}

// addStoredCaps adds the UE Security Capabilities IE to a Path Switch
// Request Acknowledge when caps is non-nil.
func addStoredCaps(ack *ngapType.NGAPPDU, caps *ngapType.UESecurityCapabilities) {
	if caps == nil {
		return
	}
	ie := ngapType.PathSwitchRequestAcknowledgeIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDUESecurityCapabilities
	ie.Criticality.Value = ngapType.CriticalityPresentReject
	ie.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentUESecurityCapabilities
	ie.Value.UESecurityCapabilities = caps
	// After the two UE NGAP IDs, where the message definition has it.
	list := &ack.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List
	const at = 2
	*list = append((*list)[:at], append([]ngapType.PathSwitchRequestAcknowledgeIEs{ie}, (*list)[at:]...)...)
}
