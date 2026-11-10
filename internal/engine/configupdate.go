package engine

import (
	"context"
	"fmt"
	"log/slog"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

// acceptConfigurationUpdate is the UE's side of the generic UE configuration
// update procedure (TS 24.501 5.4.4.3) for one Configuration Update Command.
// A new 5G-GUTI replaces *guti, the only parameter of the command the UE
// keeps: a new TAI list or allowed NSSAI is noted in the log, and so is a
// registration request, which the UE does not act on. With acknowledgement
// requested, the Configuration Update Complete goes out through send,
// protected by sec. A command that is not integrity protected is not
// processed (4.4.4.2).
func acceptConfigurationUpdate(ctx context.Context, log *slog.Logger, msg *f5nas.Message, protected bool,
	sec *nas.SecurityContext, guti *[]byte, send func([]byte) error) error {
	if !protected || sec == nil {
		log.WarnContext(ctx, "not processing a Configuration Update Command that is not integrity protected")
		return nil
	}
	cu, err := ue.ParseConfigurationUpdateCommand(msg)
	if err != nil {
		return err
	}
	if cu.GUTI != nil {
		*guti = cu.GUTI.Raw
	}
	if cu.RegistrationRequested {
		log.WarnContext(ctx, "Configuration Update Command requests a registration; the UE starts none")
	}
	if cu.AckRequested {
		complete, err := ue.BuildConfigurationUpdateComplete()
		if err != nil {
			return err
		}
		wrapped, err := sec.EncodeSecure(complete, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return fmt.Errorf("wrap Configuration Update Complete: %w", err)
		}
		if err := send(wrapped); err != nil {
			return fmt.Errorf("send Configuration Update Complete: %w", err)
		}
	}
	log.InfoContext(ctx, "accepted Configuration Update Command",
		"ack_requested", cu.AckRequested, "new_guti", cu.GUTI != nil,
		"new_tai_list", cu.HasTAIList, "new_allowed_nssai", cu.HasAllowedNSSAI)
	return nil
}

// onConfigurationUpdateCommand accepts a Configuration Update Command that
// arrives during the attach.
func (s *attachState) onConfigurationUpdateCommand(msg *f5nas.Message, protected bool) error {
	return acceptConfigurationUpdate(s.ctx, s.log, msg, protected, s.sec, &s.guti, s.sendUL)
}

// absorbDownlinkNAS handles a Downlink NAS Transport that reaches a
// registered UE while one of its procedures waits for another PDU, and
// reports whether pdu was one, so the wait reads on. A Configuration Update
// Command is accepted (TS 24.501 5.4.4.3: the network may send one at any
// time in 5GMM-CONNECTED mode); any other NAS message is logged and
// dropped.
func (s *Session) absorbDownlinkNAS(ctx context.Context, log *slog.Logger, pdu *ngapType.NGAPPDU) bool {
	if pdu == nil || pdu.Present != ngapType.NGAPPDUPresentInitiatingMessage ||
		pdu.InitiatingMessage.Value.Present != ngapType.InitiatingMessagePresentDownlinkNASTransport {
		return false
	}
	dl, err := gnb.ParseDownlink(pdu)
	if err != nil || len(dl.NASPDU) < 2 || s.sec == nil ||
		f5nas.GetSecurityHeaderType(dl.NASPDU) == f5nas.SecurityHeaderTypePlainNas {
		log.WarnContext(ctx, "dropping a downlink NAS PDU that is not protected or does not parse", "supi", s.SUPI, "err", err)
		return true
	}
	msg, err := s.sec.DecodeSecure(dl.NASPDU)
	if err != nil {
		log.WarnContext(ctx, "dropping a downlink NAS PDU that does not verify", "supi", s.SUPI, "err", err)
		return true
	}
	if msg.GmmMessage == nil || msg.GmmHeader.GetMessageType() != f5nas.MsgTypeConfigurationUpdateCommand {
		log.WarnContext(ctx, "dropping a downlink NAS message no procedure waits for", "supi", s.SUPI, "type", msg.GmmHeader.GetMessageType())
		return true
	}
	send := func(nasPDU []byte) error {
		ul, err := gnb.BuildUplinkNASTransport(s.ServingGNB(), s.amfID, s.ranID, nasPDU)
		if err != nil {
			return err
		}
		return gnb.SendPDU(s.conn, ueStream, ul)
	}
	if err := acceptConfigurationUpdate(ctx, log, msg, true, s.sec, &s.guti, send); err != nil {
		log.WarnContext(ctx, "Configuration Update Command not accepted", "supi", s.SUPI, "err", err)
	}
	return true
}
