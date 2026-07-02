package gnb

import (
	"context"
	"fmt"

	"github.com/free5gc/aper"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
)

// BuildNGSetupRequest constructs the NG Setup Request PDU (TS 38.413
// §8.7.1, §9.2.6.1). It is an NGAP InitiatingMessage with procedureCode
// id-NGSetup (21) and criticality reject. IE set and criticalities follow
// the spec and match gnbsim's field-used builder (omec-project/gnbsim
// util/ngapTestpacket/build.go). Protocol IE IDs (TS 38.413 §9.3.1):
//
//	id-GlobalRANNodeID   (27,  reject)  who the gNB is (PLMN + gNB ID)
//	id-RANNodeName       (82,  ignore)  optional human name
//	id-SupportedTAList   (102, reject)  tracking areas + slices served
//	id-DefaultPagingDRX  (21,  ignore)  paging cycle
//
// (Procedure codes and IE IDs are separate registries — NGSetup and
// DefaultPagingDRX sharing the number 21 is a coincidence, not an error.)
func BuildNGSetupRequest(cfg Config) (ngapType.NGAPPDU, error) {
	var pdu ngapType.NGAPPDU
	if err := cfg.validate(); err != nil {
		return pdu, err
	}
	plmn, err := ngap.EncodePLMN(cfg.MCC, cfg.MNC)
	if err != nil {
		return pdu, err
	}

	pdu.Present = ngapType.NGAPPDUPresentInitiatingMessage
	pdu.InitiatingMessage = new(ngapType.InitiatingMessage)
	pdu.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeNGSetup
	pdu.InitiatingMessage.Criticality.Value = ngapType.CriticalityPresentReject
	pdu.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentNGSetupRequest
	pdu.InitiatingMessage.Value.NGSetupRequest = new(ngapType.NGSetupRequest)
	ies := &pdu.InitiatingMessage.Value.NGSetupRequest.ProtocolIEs

	// GlobalRANNodeID (mandatory, reject)
	{
		ie := ngapType.NGSetupRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDGlobalRANNodeID
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.NGSetupRequestIEsPresentGlobalRANNodeID
		ie.Value.GlobalRANNodeID = new(ngapType.GlobalRANNodeID)
		ie.Value.GlobalRANNodeID.Present = ngapType.GlobalRANNodeIDPresentGlobalGNBID
		ie.Value.GlobalRANNodeID.GlobalGNBID = new(ngapType.GlobalGNBID)
		g := ie.Value.GlobalRANNodeID.GlobalGNBID
		g.PLMNIdentity.Value = aper.OctetString(plmn[:])
		g.GNBID.Present = ngapType.GNBIDPresentGNBID
		g.GNBID.GNBID = new(aper.BitString)
		*g.GNBID.GNBID = gnbIDBitString(cfg.ID, cfg.IDBits)
		ies.List = append(ies.List, ie)
	}

	// RANNodeName (optional, ignore)
	if cfg.Name != "" {
		ie := ngapType.NGSetupRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDRANNodeName
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.NGSetupRequestIEsPresentRANNodeName
		ie.Value.RANNodeName = new(ngapType.RANNodeName)
		ie.Value.RANNodeName.Value = cfg.Name
		ies.List = append(ies.List, ie)
	}

	// SupportedTAList (mandatory, reject)
	{
		ie := ngapType.NGSetupRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDSupportedTAList
		ie.Criticality.Value = ngapType.CriticalityPresentReject
		ie.Value.Present = ngapType.NGSetupRequestIEsPresentSupportedTAList
		ie.Value.SupportedTAList = new(ngapType.SupportedTAList)

		item := ngapType.SupportedTAItem{}
		item.TAC.Value = aper.OctetString{byte(cfg.TAC >> 16), byte(cfg.TAC >> 8), byte(cfg.TAC)}

		bp := ngapType.BroadcastPLMNItem{}
		bp.PLMNIdentity.Value = aper.OctetString(plmn[:])
		for _, s := range cfg.Slices {
			sd, err := s.sdBytes()
			if err != nil {
				return pdu, err
			}
			si := ngapType.SliceSupportItem{}
			si.SNSSAI.SST.Value = aper.OctetString{s.SST}
			if sd != nil {
				si.SNSSAI.SD = new(ngapType.SD)
				si.SNSSAI.SD.Value = aper.OctetString(sd)
			}
			bp.TAISliceSupportList.List = append(bp.TAISliceSupportList.List, si)
		}
		item.BroadcastPLMNList.List = append(item.BroadcastPLMNList.List, bp)
		ie.Value.SupportedTAList.List = append(ie.Value.SupportedTAList.List, item)
		ies.List = append(ies.List, ie)
	}

	// DefaultPagingDRX (mandatory, ignore)
	{
		ie := ngapType.NGSetupRequestIEs{}
		ie.Id.Value = ngapType.ProtocolIEIDDefaultPagingDRX
		ie.Criticality.Value = ngapType.CriticalityPresentIgnore
		ie.Value.Present = ngapType.NGSetupRequestIEsPresentDefaultPagingDRX
		ie.Value.DefaultPagingDRX = new(ngapType.PagingDRX)
		ie.Value.DefaultPagingDRX.Value = ngapType.PagingDRXPresentV128
		ies.List = append(ies.List, ie)
	}

	return pdu, nil
}

// gnbIDBitString left-aligns the gNB ID into an APER BIT STRING of bits
// length (APER bit strings are MSB-first). 24 bits → 3 bytes, matching the
// gnbsim reference encoding.
func gnbIDBitString(id uint32, bits int) aper.BitString {
	if bits == 0 {
		bits = 24
	}
	nbytes := (bits + 7) / 8
	shifted := uint64(id) << (uint(nbytes*8) - uint(bits))
	b := make([]byte, nbytes)
	for i := nbytes - 1; i >= 0; i-- {
		b[i] = byte(shifted)
		shifted >>= 8
	}
	return aper.BitString{Bytes: b, BitLength: uint64(bits)}
}

// NGSetupResult is the decoded outcome of an NG Setup exchange.
type NGSetupResult struct {
	// Accepted is true on NGSetupResponse, false on NGSetupFailure.
	Accepted bool
	// AMFName from the response (empty on failure).
	AMFName string
	// Cause describes the failure (empty on success).
	Cause string
	// ReplyPPID is the PPID carried by the AMF's reply. 60 is the
	// conventional NGAP encoding; the ATB-01 SD-Core AMF sends the
	// byte-reversed 0x3c000000 (see sctp.PPIDNGAPSwapped for why that is a
	// byte-order divergence rather than a protocol violation).
	ReplyPPID uint32
	// PDU is the raw decoded reply for callers needing more IEs.
	PDU *ngapType.NGAPPDU
}

// NGSetup runs one NG Setup exchange over an established association:
// encode, send on stream 0 (non-UE-associated signalling, TS 38.412 §7),
// then block for the AMF's reply until ctx expires.
func NGSetup(ctx context.Context, conn *sctp.Conn, cfg Config) (*NGSetupResult, error) {
	pdu, err := BuildNGSetupRequest(cfg)
	if err != nil {
		return nil, err
	}
	req, err := ngap.Encode(pdu)
	if err != nil {
		return nil, err
	}
	if err := conn.WriteNGAP(sctp.StreamNonUEAssociated, req); err != nil {
		return nil, err
	}

	type readResult struct {
		payload []byte
		ppid    uint32
		err     error
	}
	ch := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 65536)
		payload, _, ppid, err := conn.ReadMsg(buf)
		ch <- readResult{payload: payload, ppid: ppid, err: err}
	}()

	var rr readResult
	select {
	case rr = <-ch:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for NG Setup reply: %w", ctx.Err())
	}
	if rr.err != nil {
		return nil, rr.err
	}
	// Accept both the conventional PPID 60 and the byte-reversed encoding
	// the omec AMF emits (RFC 4960 §3.3.1 leaves PPID byte order to the
	// application; see sctp.PPIDNGAPSwapped). Anything else is unexpected.
	if rr.ppid != sctp.PPIDNGAP && rr.ppid != sctp.PPIDNGAPSwapped {
		return nil, fmt.Errorf("reply PPID = %d, want %d (NGAP)", rr.ppid, sctp.PPIDNGAP)
	}

	reply, err := ngap.Decode(rr.payload)
	if err != nil {
		return nil, err
	}
	res, err := classifyNGSetupReply(reply)
	if err != nil {
		return nil, err
	}
	res.ReplyPPID = rr.ppid
	return res, nil
}

func classifyNGSetupReply(pdu *ngapType.NGAPPDU) (*NGSetupResult, error) {
	switch pdu.Present {
	case ngapType.NGAPPDUPresentSuccessfulOutcome:
		so := pdu.SuccessfulOutcome
		if so.ProcedureCode.Value != ngapType.ProcedureCodeNGSetup ||
			so.Value.NGSetupResponse == nil {
			return nil, fmt.Errorf("unexpected successful outcome: procedure %d", so.ProcedureCode.Value)
		}
		res := &NGSetupResult{Accepted: true, PDU: pdu}
		for _, ie := range so.Value.NGSetupResponse.ProtocolIEs.List {
			if ie.Id.Value == ngapType.ProtocolIEIDAMFName && ie.Value.AMFName != nil {
				res.AMFName = ie.Value.AMFName.Value
			}
		}
		return res, nil
	case ngapType.NGAPPDUPresentUnsuccessfulOutcome:
		uo := pdu.UnsuccessfulOutcome
		if uo.ProcedureCode.Value != ngapType.ProcedureCodeNGSetup ||
			uo.Value.NGSetupFailure == nil {
			return nil, fmt.Errorf("unexpected unsuccessful outcome: procedure %d", uo.ProcedureCode.Value)
		}
		res := &NGSetupResult{Accepted: false, PDU: pdu}
		for _, ie := range uo.Value.NGSetupFailure.ProtocolIEs.List {
			if ie.Id.Value == ngapType.ProtocolIEIDCause && ie.Value.Cause != nil {
				res.Cause = causeString(ie.Value.Cause)
			}
		}
		return res, nil
	default:
		return nil, fmt.Errorf("reply is not an NG Setup outcome (present=%d)", pdu.Present)
	}
}

func causeString(c *ngapType.Cause) string {
	switch c.Present {
	case ngapType.CausePresentRadioNetwork:
		return fmt.Sprintf("radioNetwork(%d)", c.RadioNetwork.Value)
	case ngapType.CausePresentTransport:
		return fmt.Sprintf("transport(%d)", c.Transport.Value)
	case ngapType.CausePresentNas:
		return fmt.Sprintf("nas(%d)", c.Nas.Value)
	case ngapType.CausePresentProtocol:
		return fmt.Sprintf("protocol(%d)", c.Protocol.Value)
	case ngapType.CausePresentMisc:
		return fmt.Sprintf("misc(%d)", c.Misc.Value)
	default:
		return fmt.Sprintf("unknown(present=%d)", c.Present)
	}
}
