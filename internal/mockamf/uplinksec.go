package mockamf

import (
	"crypto/rand"
	"errors"
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/ngap/ngapType"
	"github.com/free5gc/util/milenage"

	"github.com/bgrewell/orbit/internal/nas"
)

// UplinkAction is what the mock does with a protected uplink NAS message
// it must not accept: one whose MAC does not verify (WithBadMACUplink), or
// one that repeats an accepted NAS COUNT (WithReplayedUplink).
type UplinkAction int

const (
	// DiscardUplink drops the message and keeps the UE as it was (the
	// default; TS 24.501 4.4.4.3, 4.4.3.2).
	DiscardUplink UplinkAction = iota
	// ProcessUplink handles the message as if it were acceptable: an AMF
	// that does not enforce the check.
	ProcessUplink
	// ReleaseOnUplink drops the message and releases the UE with a UE
	// CONTEXT RELEASE COMMAND.
	ReleaseOnUplink
	// StatusOnUplink answers the message with a plain 5GMM STATUS, cause
	// #111 protocol error, unspecified.
	StatusOnUplink
	// MuteOnUplink drops the message and every later NAS message of the
	// UE: an AMF whose state the message changed.
	MuteOnUplink
)

// WithBadMACUplink sets what the mock does with a protected uplink NAS
// message whose MAC does not verify.
func WithBadMACUplink(act UplinkAction) Option { return func(a *AMF) { a.badMAC = act } }

// WithReplayedUplink sets what the mock does with a protected uplink NAS
// message whose MAC verifies at a NAS COUNT it has already accepted.
// ProcessUplink is an AMF without replay protection.
func WithReplayedUplink(act UplinkAction) Option { return func(a *AMF) { a.replayed = act } }

// ReplayedUplinks reports how many protected uplink NAS messages repeated
// an accepted NAS COUNT.
func (a *AMF) ReplayedUplinks() int64 { return a.replays.Load() }

// WithUplinkCountBeforeMAC makes the mock store a protected uplink's
// estimated NAS COUNT before checking its MAC, so a message that fails the
// check still advances the stored COUNT (against TS 24.501 4.4.3.3). By
// default the stored COUNT changes only when the MAC verifies.
func WithUplinkCountBeforeMAC() Option { return func(a *AMF) { a.countBeforeMAC = true } }

// WithLaxUplinkCountEstimate makes the mock, for an uplink whose sequence
// number is lower than the last accepted one, also try the NAS COUNT with
// the stored overflow counter, which is the sender's original COUNT of an
// old message sent again. TS 24.501 4.4.3.1 has required the higher
// estimate since V16.5.1; earlier versions give no rule. A message that
// verifies at the old COUNT repeats an accepted COUNT, so WithReplayedUplink
// says what happens to it. By default only the higher estimate is tried,
// and the old message fails the integrity check.
func WithLaxUplinkCountEstimate() Option { return func(a *AMF) { a.laxEstimate = true } }

// WithPDUSessionAnswers makes the mock answer only each UE's first n PDU
// Session Establishment Requests and ignore the rest. The default answers
// every one.
func WithPDUSessionAnswers(n int) Option { return func(a *AMF) { a.smAnswers, a.smLimited = n, true } }

// BadMACRegistration is what the mock does with a REGISTRATION REQUEST that
// arrives protected on a UE's secured connection with a MAC that does not
// verify (WithBadMACRegistration).
type BadMACRegistration int

const (
	// PlainBadMACRegistration treats it as any other bad-MAC uplink
	// (WithBadMACUplink; discarded by default).
	PlainBadMACRegistration BadMACRegistration = iota
	// AcceptBadMACRegistration answers with a REGISTRATION ACCEPT, with no
	// authentication first.
	AcceptBadMACRegistration
	// ReauthenticateBadMACRegistration answers with an AUTHENTICATION
	// REQUEST.
	ReauthenticateBadMACRegistration
	// RejectBadMACRegistration answers with a REGISTRATION REJECT, 5GMM
	// cause #9 UE identity cannot be derived by the network.
	RejectBadMACRegistration
	// SMCBadMACRegistration answers with a SECURITY MODE COMMAND, with no
	// authentication first.
	SMCBadMACRegistration
)

// WithBadMACRegistration sets what the mock does with a protected
// REGISTRATION REQUEST whose MAC does not verify. Each answer is protected
// under the UE's security context.
func WithBadMACRegistration(b BadMACRegistration) Option { return func(a *AMF) { a.badMACReg = b } }

// answerBadMACRegistration answers nasPDU as WithBadMACRegistration says
// and reports true, when it is a REGISTRATION REQUEST and the option is
// set.
func (a *AMF) answerBadMACRegistration(p *pipe, st *ueState, nasPDU []byte) (bool, error) {
	if a.badMACReg == PlainBadMACRegistration {
		return false, nil
	}
	msg, err := st.decodeUnverified(nasPDU)
	if err != nil || msg.GmmMessage == nil || msg.GmmHeader.GetMessageType() != f5nas.MsgTypeRegistrationRequest {
		return false, nil
	}
	var wrapped []byte
	switch a.badMACReg {
	case AcceptBadMACRegistration:
		acc, err := a.registrationAccept(st)
		if err != nil {
			return true, err
		}
		wrapped, err = st.encodeSecure(acc, nas.SecHdrIntegrityCiphered)
		if err != nil {
			return true, err
		}
	case ReauthenticateBadMACRegistration:
		randv, autn, err := a.challenge()
		if err != nil {
			return true, err
		}
		plain, err := buildAuthenticationRequest(randv, autn, st.ngksi)
		if err != nil {
			return true, err
		}
		if wrapped, err = st.encodeSecureRaw(plain, nas.SecHdrIntegrityCiphered); err != nil {
			return true, err
		}
	case RejectBadMACRegistration:
		plain, err := buildRegistrationReject(nasMessage.Cause5GMMUEIdentityCannotBeDerivedByTheNetwork)
		if err != nil {
			return true, err
		}
		if wrapped, err = st.encodeSecureRaw(plain, nas.SecHdrIntegrityCiphered); err != nil {
			return true, err
		}
	case SMCBadMACRegistration:
		st.secMu.Lock()
		ia, ea := st.sec.IntegrityAlg, st.sec.CipheringAlg
		st.secMu.Unlock()
		smc, err := buildSecurityModeCommand(ia, ea, st.ngksi, st.ueCaps)
		if err != nil {
			return true, err
		}
		if wrapped, err = st.encodeSecure(smc, nas.SecHdrIntegrity); err != nil {
			return true, err
		}
	}
	return true, a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), wrapped))
}

// challenge is a fresh 5G-AKA challenge over the shared credential.
func (a *AMF) challenge() (randv, autn [16]byte, err error) {
	if _, err = rand.Read(randv[:]); err != nil {
		return randv, autn, err
	}
	_, _, _, autnSlice, err := milenage.GenerateAKAParameters(a.cfg.OPc, a.cfg.Ki, randv[:], a.cfg.SQN, a.cfg.AMF)
	if err != nil {
		return randv, autn, fmt.Errorf("mock amf AKA: %w", err)
	}
	copy(autn[:], autnSlice)
	return randv, autn, nil
}

// BadMACUplinks reports how many protected uplink NAS messages failed the
// integrity check.
func (a *AMF) BadMACUplinks() int64 { return a.badMACs.Load() }

// PDUSessionReleaseRequests reports how many PDU Session Release Requests
// a mock with a UPF has received. It does not answer them.
func (a *AMF) PDUSessionReleaseRequests() int64 { return a.smReleases.Load() }

// decodeUplink unwraps a protected uplink NAS message under st's security
// context. A nil message with a nil error means the message was dropped
// (refuse says what else happened).
func (a *AMF) decodeUplink(p *pipe, st *ueState, nasPDU []byte) (*f5nas.Message, error) {
	msg, err := st.decodeSecure(nasPDU)
	switch {
	case errors.Is(err, nas.ErrReplay):
		a.replays.Add(1)
		return a.refuse(p, st, a.replayed, func() (*f5nas.Message, error) { return st.decodeUnverified(nasPDU) })
	case errors.Is(err, nas.ErrIntegrity):
		if old, ok := a.oldCount(st, nasPDU); ok {
			a.replays.Add(1)
			return a.refuse(p, st, a.replayed, func() (*f5nas.Message, error) { return st.decodeSecureAt(nasPDU, old) })
		}
		a.badMACs.Add(1)
		if answered, err := a.answerBadMACRegistration(p, st, nasPDU); answered || err != nil {
			return nil, err
		}
		if a.countBeforeMAC {
			st.forceEstimatedCount(nasPDU)
		}
		return a.refuse(p, st, a.badMAC, func() (*f5nas.Message, error) { return st.decodeUnverified(nasPDU) })
	}
	return msg, err
}

// oldCount reports, under WithLaxUplinkCountEstimate, the NAS COUNT below
// the stored one at which an uplink that failed the integrity check at its
// estimate verifies: its sequence number with the stored overflow counter.
func (a *AMF) oldCount(st *ueState, nasPDU []byte) (uint32, bool) {
	if !a.laxEstimate || len(nasPDU) < 7 {
		return 0, false
	}
	st.secMu.Lock()
	defer st.secMu.Unlock()
	stored := st.sec.RxCount()
	old := stored&^0xff | uint32(nasPDU[6])
	if old >= stored {
		return 0, false
	}
	_, err := st.sec.DecodeSecureAt(nasPDU, old)
	return old, !errors.Is(err, nas.ErrIntegrity)
}

// refuse applies act to an uplink NAS message the mock must not accept. It
// returns what process reads from the message for ProcessUplink, and nil
// otherwise.
func (a *AMF) refuse(p *pipe, st *ueState, act UplinkAction, process func() (*f5nas.Message, error)) (*f5nas.Message, error) {
	switch act {
	case ProcessUplink:
		return process()
	case ReleaseOnUplink:
		return nil, a.send(p, buildUEContextReleaseCommandCause(st.amfID, a.ranIDOf(st), ngapType.CauseNasPresentUnspecified))
	case StatusOnUplink:
		return nil, a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), []byte{0x7e, 0x00, f5nas.MsgTypeStatus5GMM, 0x6f}))
	case MuteOnUplink:
		st.muted = true
	}
	return nil, nil
}

func (st *ueState) decodeSecureAt(data []byte, count uint32) (*f5nas.Message, error) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	return st.sec.DecodeSecureAt(data, count)
}

// forceEstimatedCount stores the NAS COUNT data would be checked at as the
// last accepted one (WithUplinkCountBeforeMAC).
func (st *ueState) forceEstimatedCount(data []byte) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	if count, err := st.sec.EstimateRxCount(data); err == nil {
		st.sec.ForceRxCount(count)
	}
}

func (st *ueState) decodeUnverified(data []byte) (*f5nas.Message, error) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	return st.sec.DecodeUnverified(data)
}
