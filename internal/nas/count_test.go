package nas_test

import (
	"errors"
	"testing"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/nas"
)

// DecodeUnverified reads a PDU whose MAC is wrong, under either cipher, and
// leaves the stored COUNT alone: the next genuine PDU still verifies. A PDU
// too short to hold a security header is refused.
func TestDecodeUnverified(t *testing.T) {
	tests := []struct {
		name    string
		ea      uint8
		corrupt bool
		short   bool
		wantErr bool
	}{
		{name: "bad MAC, NEA0", ea: nas.NEA0, corrupt: true},
		{name: "bad MAC, NEA2", ea: nas.NEA2, corrupt: true},
		{name: "good MAC", ea: nas.NEA2},
		{name: "too short", ea: nas.NEA0, short: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ue, amf := pairCtx(tc.ea)
			first, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(first); err != nil {
				t.Fatal(err)
			}
			pdu, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if tc.corrupt {
				pdu[3] ^= 0xff
				if _, err := amf.DecodeSecure(pdu); !errors.Is(err, nas.ErrIntegrity) {
					t.Fatalf("DecodeSecure of the corrupted PDU = %v, want ErrIntegrity", err)
				}
			}
			if tc.short {
				pdu = pdu[:5]
			}
			m, err := amf.DecodeUnverified(pdu)
			if (err != nil) != tc.wantErr {
				t.Fatalf("DecodeUnverified = %v, want error %v", err, tc.wantErr)
			}
			if err == nil && m.GmmHeader.GetMessageType() != f5nas.MsgTypeRegistrationComplete {
				t.Errorf("decoded message type %#x, want Registration Complete", m.GmmHeader.GetMessageType())
			}
			next, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(next); err != nil {
				t.Errorf("the next genuine PDU no longer verifies: %v", err)
			}
		})
	}
}

// Under RejectReplay a PDU decoded twice is ErrReplay the second time, the
// stored COUNT stays where it was, and the next PDU still verifies. A fresh
// context accepts its first PDU at COUNT 0. A new-context header and 5G-IA0
// are exempt, and a context without RejectReplay accepts the repeat.
func TestDecodeSecureReplay(t *testing.T) {
	tests := []struct {
		name       string
		reject     bool
		ia         uint8
		hdr        uint8
		wantReplay bool
	}{
		{name: "repeat refused", reject: true, ia: nas.NIA2, hdr: nas.SecHdrIntegrityCiphered, wantReplay: true},
		{name: "integrity only, repeat refused", reject: true, ia: nas.NIA1, hdr: nas.SecHdrIntegrity, wantReplay: true},
		{name: "new-context header exempt", reject: true, ia: nas.NIA2, hdr: nas.SecHdrIntegrityCipheredNewCtx},
		{name: "5G-IA0 exempt", reject: true, ia: nas.NIA0, hdr: nas.SecHdrIntegrityCiphered},
		{name: "not rejecting", reject: false, ia: nas.NIA2, hdr: nas.SecHdrIntegrityCiphered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ue, amf := pairCtx(nas.NEA0)
			ue.IntegrityAlg, amf.IntegrityAlg = tc.ia, tc.ia
			amf.RejectReplay = tc.reject
			// The first PDU of a fresh context is at COUNT 0.
			first, err := ue.EncodeSecure(plainMsg(), tc.hdr)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(first); err != nil {
				t.Fatalf("first PDU at COUNT 0: %v", err)
			}
			_, err = amf.DecodeSecure(first)
			if got := errors.Is(err, nas.ErrReplay); got != tc.wantReplay || (!got && err != nil) {
				t.Fatalf("second decode = %v, want ErrReplay %v", err, tc.wantReplay)
			}
			second, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(second); err != nil {
				t.Fatalf("the next PDU after the repeat: %v", err)
			}
			// The second PDU has an ordinary header, so only 5G-IA0 and
			// a context that does not reject let its repeat through.
			wantRepeat := tc.reject && tc.ia != nas.NIA0
			if _, err := amf.DecodeSecure(second); errors.Is(err, nas.ErrReplay) != wantRepeat || (!wantRepeat && err != nil) {
				t.Errorf("repeat of the second PDU = %v, want ErrReplay %v", err, wantRepeat)
			}
		})
	}
}

// ResetCounts makes the next COUNT 0 a first COUNT again.
func TestResetCountsClearsReplayState(t *testing.T) {
	ue, amf := pairCtx(nas.NEA0)
	amf.RejectReplay = true
	pdu, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := amf.DecodeSecure(pdu); err != nil {
		t.Fatal(err)
	}
	amf.ResetCounts()
	if _, err := amf.DecodeSecure(pdu); err != nil {
		t.Errorf("COUNT 0 after ResetCounts = %v, want accepted", err)
	}
}

// EstimateRxCount gives the COUNT a PDU is checked at: the same overflow
// for a sequence number at or above the stored one, the next overflow for a
// lower one, and 0 under a new-context header. It stores nothing.
func TestEstimateRxCount(t *testing.T) {
	tests := []struct {
		name   string
		stored uint32
		seq    byte
		hdr    uint8
		want   uint32
	}{
		{name: "next sequence number", stored: 5, seq: 6, hdr: nas.SecHdrIntegrityCiphered, want: 6},
		{name: "same sequence number", stored: 5, seq: 5, hdr: nas.SecHdrIntegrityCiphered, want: 5},
		{name: "lower sequence number", stored: 5, seq: 4, hdr: nas.SecHdrIntegrityCiphered, want: 0x104},
		{name: "wrap", stored: 0xff, seq: 0, hdr: nas.SecHdrIntegrity, want: 0x100},
		{name: "second overflow", stored: 0x1fe, seq: 0xff, hdr: nas.SecHdrIntegrityCiphered, want: 0x1ff},
		{name: "new context", stored: 0x1fe, seq: 0, hdr: nas.SecHdrIntegrityNewContext, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, amf := pairCtx(nas.NEA0)
			amf.ForceRxCount(tc.stored)
			got, err := amf.EstimateRxCount([]byte{0x7e, tc.hdr, 0, 0, 0, 0, tc.seq, 0x7e, 0x00, 0x43})
			if err != nil || got != tc.want {
				t.Errorf("EstimateRxCount = %#x, %v, want %#x", got, err, tc.want)
			}
			if amf.RxCount() != tc.stored {
				t.Errorf("stored COUNT moved to %#x", amf.RxCount())
			}
		})
	}
	_, amf := pairCtx(nas.NEA0)
	if _, err := amf.EstimateRxCount([]byte{0x7e, 0x02, 0}); err == nil {
		t.Error("a PDU without a full security header was estimated")
	}
}

// DecodeSecureAt verifies at the COUNT it is given and stores nothing: an
// old PDU verifies at its own COUNT and fails at any other, under either
// cipher. ForceRxCount moves the stored COUNT, and with it the estimate.
func TestDecodeSecureAt(t *testing.T) {
	for _, ea := range []uint8{nas.NEA0, nas.NEA2} {
		ue, amf := pairCtx(ea)
		var pdus [][]byte
		for i := 0; i < 3; i++ {
			p, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := amf.DecodeSecure(p); err != nil {
				t.Fatal(err)
			}
			pdus = append(pdus, p)
		}
		tests := []struct {
			name    string
			pdu     int
			count   uint32
			wantErr bool
		}{
			{name: "old PDU at its own COUNT", pdu: 0, count: 0},
			{name: "old PDU at the estimate", pdu: 0, count: 0x100, wantErr: true},
			{name: "last PDU at its COUNT", pdu: 2, count: 2},
			{name: "last PDU at another COUNT", pdu: 2, count: 1, wantErr: true},
		}
		for _, tc := range tests {
			m, err := amf.DecodeSecureAt(pdus[tc.pdu], tc.count)
			if errors.Is(err, nas.ErrIntegrity) != tc.wantErr || (!tc.wantErr && (err != nil || m == nil)) {
				t.Errorf("NEA%d %s: DecodeSecureAt = %v, want integrity error %v", ea, tc.name, err, tc.wantErr)
			}
			if amf.RxCount() != 2 {
				t.Fatalf("NEA%d %s: stored COUNT moved to %d", ea, tc.name, amf.RxCount())
			}
		}
		if _, err := amf.DecodeSecureAt(pdus[0][:4], 0); err == nil {
			t.Errorf("NEA%d: a truncated PDU was decoded", ea)
		}
		// A COUNT stored without a check makes the next genuine PDU fail.
		amf.ForceRxCount(0x100)
		next, err := ue.EncodeSecure(plainMsg(), nas.SecHdrIntegrityCiphered)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := amf.DecodeSecure(next); !errors.Is(err, nas.ErrIntegrity) {
			t.Errorf("NEA%d: after ForceRxCount the next PDU = %v, want ErrIntegrity", ea, err)
		}
	}
}
