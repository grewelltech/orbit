package ue

import (
	"encoding/hex"
	"errors"
	"testing"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
)

// The release builders produce the bare 5GSM header, and the codec reads
// them back as the messages they are.
func TestPDUSessionReleaseBuilders(t *testing.T) {
	tests := []struct {
		name    string
		build   func(id, pti uint8) ([]byte, error)
		wantHex string
		isMsg   func(*f5nas.Message) bool
	}{
		{name: "request", build: BuildPDUSessionReleaseRequest, wantHex: "2e0507d1",
			isMsg: func(m *f5nas.Message) bool { return m.PDUSessionReleaseRequest != nil }},
		{name: "complete", build: BuildPDUSessionReleaseComplete, wantHex: "2e0507d4",
			isMsg: func(m *f5nas.Message) bool { return m.PDUSessionReleaseComplete != nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.build(5, 7)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(b); got != tc.wantHex {
				t.Errorf("encoded %s, want %s", got, tc.wantHex)
			}
			m := f5nas.NewMessage()
			buf := append([]byte(nil), b...)
			if err := m.PlainNasDecode(&buf); err != nil || m.GsmMessage == nil || !tc.isMsg(m) {
				t.Errorf("decode: %v (message %+v)", err, m.GsmMessage)
			}
		})
	}
}

// The UL NAS Transport for an established session carries the payload and
// the PDU session ID, and none of the IEs of a request for a new session.
func TestBuildULNASTransportForSM(t *testing.T) {
	sm, err := BuildPDUSessionReleaseRequest(5, 7)
	if err != nil {
		t.Fatal(err)
	}
	m, err := BuildULNASTransportForSM(sm, 5)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	got := f5nas.NewMessage()
	if err := got.PlainNasDecode(&b); err != nil {
		t.Fatal(err)
	}
	tr := got.ULNASTransport
	if tr == nil {
		t.Fatal("not a UL NAS Transport")
	}
	if tr.GetPayloadContainerType() != nasMessage.PayloadContainerTypeN1SMInfo || hex.EncodeToString(tr.GetPayloadContainerContents()) != "2e0507d1" {
		t.Errorf("payload container type %d, contents %x", tr.GetPayloadContainerType(), tr.GetPayloadContainerContents())
	}
	if tr.PduSessionID2Value == nil || tr.GetPduSessionID2Value() != 5 {
		t.Errorf("PDU session ID IE %+v, want 5", tr.PduSessionID2Value)
	}
	if tr.RequestType != nil || tr.DNN != nil || tr.SNSSAI != nil {
		t.Errorf("carries a request type, DNN or S-NSSAI: %+v %+v %+v", tr.RequestType, tr.DNN, tr.SNSSAI)
	}
	if _, err := BuildULNASTransportForSM(nil, 5); err == nil {
		t.Error("an empty payload was accepted")
	}
}

// ParseSMHeader reads the header of any 5GSM message and the cause of the
// release messages, and refuses what is not 5GSM or is cut short.
func TestParseSMHeader(t *testing.T) {
	tests := []struct {
		name    string
		sm      []byte
		want    SMHeader
		wantErr bool
		not5GSM bool
	}{
		{name: "release command", sm: []byte{0x2e, 5, 7, 0xd3, 36}, want: SMHeader{PDUSessionID: 5, PTI: 7, MessageType: f5nas.MsgTypePDUSessionReleaseCommand, Cause: 36}},
		{name: "release command with more IEs", sm: []byte{0x2e, 5, 7, 0xd3, 26, 0x37, 1, 0}, want: SMHeader{PDUSessionID: 5, PTI: 7, MessageType: f5nas.MsgTypePDUSessionReleaseCommand, Cause: 26}},
		{name: "release reject", sm: []byte{0x2e, 5, 7, 0xd2, 43}, want: SMHeader{PDUSessionID: 5, PTI: 7, MessageType: f5nas.MsgTypePDUSessionReleaseReject, Cause: 43}},
		{name: "another message", sm: []byte{0x2e, 1, 0, 0xc2, 0x11}, want: SMHeader{PDUSessionID: 1, MessageType: f5nas.MsgTypePDUSessionEstablishmentAccept}},
		{name: "release command without its cause", sm: []byte{0x2e, 5, 7, 0xd3}, wantErr: true},
		{name: "5GMM", sm: []byte{0x7e, 0, 0x68, 1}, wantErr: true, not5GSM: true},
		{name: "too short", sm: []byte{0x2e, 5, 7}, wantErr: true, not5GSM: true},
		{name: "empty", wantErr: true, not5GSM: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSMHeader(tc.sm)
			if (err != nil) != tc.wantErr || errors.Is(err, ErrNot5GSM) != tc.not5GSM {
				t.Fatalf("err = %v, want error %v (not 5GSM %v)", err, tc.wantErr, tc.not5GSM)
			}
			if got != tc.want {
				t.Errorf("header %+v, want %+v", got, tc.want)
			}
		})
	}
}
