package mockamf

import (
	"github.com/free5gc/ngap/ngapType"
)

// RANConfigUpdateAnswer is how the mock answers a RAN CONFIGURATION UPDATE
// (WithRANConfigUpdate).
type RANConfigUpdateAnswer int

const (
	// AcknowledgeRANConfigUpdate answers with RAN CONFIGURATION UPDATE
	// ACKNOWLEDGE (the default; TS 38.413 8.7.2.2).
	AcknowledgeRANConfigUpdate RANConfigUpdateAnswer = iota
	// FailRANConfigUpdate answers with RAN CONFIGURATION UPDATE FAILURE,
	// cause Misc unspecified (8.7.2.3).
	FailRANConfigUpdate
	// IgnoreRANConfigUpdate sends nothing.
	IgnoreRANConfigUpdate
)

// WithRANConfigUpdate sets how the mock answers a RAN CONFIGURATION UPDATE.
func WithRANConfigUpdate(answer RANConfigUpdateAnswer) Option {
	return func(a *AMF) { a.ranConfigAnswer = answer }
}

// RANConfigUpdate is one RAN CONFIGURATION UPDATE the mock received.
type RANConfigUpdate struct {
	// IEs are the protocol IE IDs the message carried, in order.
	IEs []int64
	// RANNodeName is the RAN Node Name IE's value, "" without one.
	RANNodeName string
}

// HasIE reports whether the update carried the protocol IE id.
func (u RANConfigUpdate) HasIE(id int64) bool {
	for _, got := range u.IEs {
		if got == id {
			return true
		}
	}
	return false
}

// RANConfigUpdates reports the RAN CONFIGURATION UPDATEs the mock has
// received, in order.
func (a *AMF) RANConfigUpdates() []RANConfigUpdate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]RANConfigUpdate(nil), a.ranConfigUpdates...)
}

// onRANConfigUpdate records a RAN CONFIGURATION UPDATE and answers it as
// configured.
func (a *AMF) onRANConfigUpdate(p *pipe, req *ngapType.RANConfigurationUpdate) error {
	var got RANConfigUpdate
	for _, ie := range req.ProtocolIEs.List {
		got.IEs = append(got.IEs, ie.Id.Value)
		if ie.Id.Value == ngapType.ProtocolIEIDRANNodeName && ie.Value.RANNodeName != nil {
			got.RANNodeName = ie.Value.RANNodeName.Value
		}
	}
	a.mu.Lock()
	a.ranConfigUpdates = append(a.ranConfigUpdates, got)
	a.mu.Unlock()
	switch a.ranConfigAnswer {
	case FailRANConfigUpdate:
		return a.send(p, buildRANConfigUpdateFailure())
	case IgnoreRANConfigUpdate:
		return nil
	default:
		return a.send(p, buildRANConfigUpdateAcknowledge())
	}
}

// buildRANConfigUpdateAcknowledge builds a RAN CONFIGURATION UPDATE
// ACKNOWLEDGE (TS 38.413 9.2.6.5) with no optional IE.
func buildRANConfigUpdateAcknowledge() ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	pdu.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	pdu.SuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeRANConfigurationUpdate
	pdu.SuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentRANConfigurationUpdateAcknowledge
	pdu.SuccessfulOutcome.Value.RANConfigurationUpdateAcknowledge = new(ngapType.RANConfigurationUpdateAcknowledge)
	return pdu
}

// buildRANConfigUpdateFailure builds a RAN CONFIGURATION UPDATE FAILURE (TS
// 38.413 9.2.6.6) with cause Misc unspecified.
func buildRANConfigUpdateFailure() ngapType.NGAPPDU {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentUnsuccessfulOutcome
	pdu.UnsuccessfulOutcome = new(ngapType.UnsuccessfulOutcome)
	pdu.UnsuccessfulOutcome.ProcedureCode.Value = ngapType.ProcedureCodeRANConfigurationUpdate
	pdu.UnsuccessfulOutcome.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.UnsuccessfulOutcome.Value.Present = ngapType.UnsuccessfulOutcomePresentRANConfigurationUpdateFailure
	pdu.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure = new(ngapType.RANConfigurationUpdateFailure)
	ie := ngapType.RANConfigurationUpdateFailureIEs{}
	ie.Id.Value = ngapType.ProtocolIEIDCause
	ie.Criticality.Value = ngapType.CriticalityPresentIgnore
	ie.Value.Present = ngapType.RANConfigurationUpdateFailureIEsPresentCause
	ie.Value.Cause = &ngapType.Cause{Present: ngapType.CausePresentMisc,
		Misc: &ngapType.CauseMisc{Value: ngapType.CauseMiscPresentUnspecified}}
	pdu.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure.ProtocolIEs.List = append(
		pdu.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure.ProtocolIEs.List, ie)
	return pdu
}
