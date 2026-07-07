package gnb

import "github.com/free5gc/ngap/ngapType"

// BuildNGReset constructs an NG RESET that resets the whole NG interface
// (TS 38.413 §8.7.4 Reset; §9.2.6.4 NG RESET). It is a non-UE-associated
// InitiatingMessage, procedureCode id-NGReset (20), criticality reject. On
// receipt the AMF releases all UE contexts for this association and replies
// with NG RESET ACKNOWLEDGE. IEs (id / criticality per §9.2.6.4):
//
//	id-Cause     (15, ignore)  reason for the reset (Misc / O&M intervention)
//	id-ResetType (88, reject)  scope: NG Interface (Reset All), not a UE subset
func BuildNGReset() ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeNGReset
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentNGReset
	pdu.InitiatingMessage.Value.NGReset = new(ngapType.NGReset)
	ies := &pdu.InitiatingMessage.Value.NGReset.ProtocolIEs

	// Cause (mandatory, ignore) — Misc / O&M intervention.
	{
		ie := ngapType.NGResetIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDCause
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.NGResetIEsPresentCause
		ie.Value.Cause = new(ngapType.Cause)
		ie.Value.Cause.Present = ngapType.CausePresentMisc
		ie.Value.Cause.Misc = new(ngapType.CauseMisc)
		ie.Value.Cause.Misc.Value = ngapType.CauseMiscPresentOmIntervention
		ies.List = append(ies.List, ie)
	}
	// ResetType = whole NG interface / Reset All (mandatory, reject).
	{
		ie := ngapType.NGResetIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDResetType
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.NGResetIEsPresentResetType
		ie.Value.ResetType = new(ngapType.ResetType)
		ie.Value.ResetType.Present = ngapType.ResetTypePresentNGInterface
		ie.Value.ResetType.NGInterface = new(ngapType.ResetAll)
		ie.Value.ResetType.NGInterface.Value = ngapType.ResetAllPresentResetAll
		ies.List = append(ies.List, ie)
	}
	return pdu
}
