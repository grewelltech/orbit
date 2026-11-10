package gnb

import (
	"context"
	"errors"
	"fmt"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// RANConfigUpdateOption adds one optional IE to a RAN CONFIGURATION UPDATE
// (BuildRANConfigurationUpdate).
type RANConfigUpdateOption func(*ngapType.ProtocolIEContainerRANConfigurationUpdateIEs) error

// WithRANNodeName includes the RAN Node Name IE (id 82, criticality ignore).
func WithRANNodeName(name string) RANConfigUpdateOption {
	return func(ies *ngapType.ProtocolIEContainerRANConfigurationUpdateIEs) error {
		if name == "" {
			return errors.New("RAN node name is empty")
		}
		ie := ngapType.RANConfigurationUpdateIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANNodeName
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.RANConfigurationUpdateIEsPresentRANNodeName
		ie.Value.RANNodeName = &ngapType.RANNodeName{Value: name}
		ies.List = append(ies.List, ie)
		return nil
	}
}

// WithSupportedTAList includes the Supported TA List IE (id 102, criticality
// reject) that cfg presents at NG Setup: its TAC, PLMN and slices.
func WithSupportedTAList(cfg Config) RANConfigUpdateOption {
	return func(ies *ngapType.ProtocolIEContainerRANConfigurationUpdateIEs) error {
		if err := cfg.validate(); err != nil {
			return err
		}
		plmn, err := ngap.EncodePLMN(cfg.MCC, cfg.MNC)
		if err != nil {
			return err
		}
		ta, err := supportedTAList(cfg, plmn)
		if err != nil {
			return err
		}
		ie := ngapType.RANConfigurationUpdateIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDSupportedTAList
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.RANConfigurationUpdateIEsPresentSupportedTAList
		ie.Value.SupportedTAList = ta
		ies.List = append(ies.List, ie)
		return nil
	}
}

// BuildRANConfigurationUpdate constructs a RAN CONFIGURATION UPDATE (TS
// 38.413 8.7.2, 9.2.6.4): a non-UE-associated InitiatingMessage,
// procedureCode id-RANConfigurationUpdate (35), criticality reject. Every IE
// of the message is optional, so it carries exactly the IEs the options
// add, in the order given; with none it is an update that changes nothing.
func BuildRANConfigurationUpdate(opts ...RANConfigUpdateOption) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeRANConfigurationUpdate
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentRANConfigurationUpdate
	pdu.InitiatingMessage.Value.RANConfigurationUpdate = new(ngapType.RANConfigurationUpdate)
	for _, o := range opts {
		if err := o(&pdu.InitiatingMessage.Value.RANConfigurationUpdate.ProtocolIEs); err != nil {
			return ngapType.NGAPPDU{}, fmt.Errorf("RAN configuration update: %w", err)
		}
	}
	return pdu, nil
}

// RANConfigUpdateResult is the decoded outcome of a RAN Configuration Update
// exchange.
type RANConfigUpdateResult struct {
	// Acknowledged is true on RAN CONFIGURATION UPDATE ACKNOWLEDGE, false on
	// RAN CONFIGURATION UPDATE FAILURE.
	Acknowledged bool
	// Cause is the FAILURE's Cause IE ("absent" without one; empty on an
	// ACKNOWLEDGE).
	Cause string
	// Skipped counts the PDUs read before the outcome that were not it.
	Skipped int
	// PDU is the decoded outcome.
	PDU *ngapType.NGAPPDU
}

// RANConfigurationUpdate runs one RAN Configuration Update exchange over an
// association that has completed NG Setup: it sends pdu on stream 0 and
// reads until the procedure's outcome arrives or ctx ends. PDUs of other
// procedures read meanwhile are dropped and counted. An error that wraps
// ctx's means no outcome came in time; the read it abandons takes the next
// PDU on the association, so the caller should close it.
func RANConfigurationUpdate(ctx context.Context, conn Transport, pdu ngapType.NGAPPDU) (*RANConfigUpdateResult, error) {
	if err := SendPDU(conn, sctp.StreamNonUEAssociated, pdu); err != nil {
		return nil, err
	}
	res := &RANConfigUpdateResult{}
	for {
		reply, err := ReadPDU(ctx, conn)
		if err != nil {
			return nil, fmt.Errorf("RAN configuration update: %w", err)
		}
		switch {
		case IsRANConfigurationUpdateAcknowledge(reply):
			res.Acknowledged, res.PDU = true, reply
			return res, nil
		case IsRANConfigurationUpdateFailure(reply):
			res.Cause, res.PDU = "absent", reply
			for _, ie := range reply.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure.ProtocolIEs.List {
				if ie.Id.Value == ngapType.ProtocolIEIDCause && ie.Value.Cause != nil {
					res.Cause = causeString(ie.Value.Cause)
				}
			}
			return res, nil
		}
		res.Skipped++
	}
}

// IsRANConfigurationUpdateAcknowledge reports whether pdu is a RAN
// CONFIGURATION UPDATE ACKNOWLEDGE.
func IsRANConfigurationUpdateAcknowledge(pdu *ngapType.NGAPPDU) bool {
	return pdu != nil && pdu.Present == ngapType.NGAPPDUPresentSuccessfulOutcome &&
		pdu.SuccessfulOutcome != nil &&
		pdu.SuccessfulOutcome.Value.Present == ngapType.SuccessfulOutcomePresentRANConfigurationUpdateAcknowledge &&
		pdu.SuccessfulOutcome.Value.RANConfigurationUpdateAcknowledge != nil
}

// IsRANConfigurationUpdateFailure reports whether pdu is a RAN CONFIGURATION
// UPDATE FAILURE.
func IsRANConfigurationUpdateFailure(pdu *ngapType.NGAPPDU) bool {
	return pdu != nil && pdu.Present == ngapType.NGAPPDUPresentUnsuccessfulOutcome &&
		pdu.UnsuccessfulOutcome != nil &&
		pdu.UnsuccessfulOutcome.Value.Present == ngapType.UnsuccessfulOutcomePresentRANConfigurationUpdateFailure &&
		pdu.UnsuccessfulOutcome.Value.RANConfigurationUpdateFailure != nil
}
