package mockamf

import (
	"errors"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/ngap/ngapType"

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
	if errors.Is(err, nas.ErrIntegrity) {
		a.badMACs.Add(1)
		return a.refuse(p, st, nasPDU, a.badMAC)
	}
	if errors.Is(err, nas.ErrReplay) {
		a.replays.Add(1)
		return a.refuse(p, st, nasPDU, a.replayed)
	}
	return msg, err
}

// refuse applies act to an uplink NAS message the mock must not accept. It
// returns the message for ProcessUplink, read without the integrity and
// replay checks, and nil otherwise.
func (a *AMF) refuse(p *pipe, st *ueState, nasPDU []byte, act UplinkAction) (*f5nas.Message, error) {
	switch act {
	case ProcessUplink:
		return st.decodeUnverified(nasPDU)
	case ReleaseOnUplink:
		return nil, a.send(p, buildUEContextReleaseCommandCause(st.amfID, a.ranIDOf(st), ngapType.CauseNasPresentUnspecified))
	case StatusOnUplink:
		return nil, a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), []byte{0x7e, 0x00, f5nas.MsgTypeStatus5GMM, 0x6f}))
	case MuteOnUplink:
		st.muted = true
	}
	return nil, nil
}

func (st *ueState) decodeUnverified(data []byte) (*f5nas.Message, error) {
	st.secMu.Lock()
	defer st.secMu.Unlock()
	return st.sec.DecodeUnverified(data)
}
