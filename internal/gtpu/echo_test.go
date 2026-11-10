package gtpu

import (
	"bytes"
	"testing"
)

// An Echo Request is a 12-byte header: version 1, PT and S set, type 1,
// length 4, TEID 0, then the sequence number (TS 29.281 5.1, 7.2.1).
func TestEncodeEchoRequest(t *testing.T) {
	want := []byte{0x32, 0x01, 0x00, 0x04, 0, 0, 0, 0, 0x12, 0x34, 0, 0}
	if got := EncodeEchoRequest(0x1234); !bytes.Equal(got, want) {
		t.Errorf("EncodeEchoRequest = % x, want % x", got, want)
	}
}

// EchoResponseSeq reads the sequence number of an Echo Response only.
func TestEchoResponseSeq(t *testing.T) {
	resp := []byte{0x32, 0x02, 0x00, 0x06, 0, 0, 0, 0, 0x12, 0x34, 0, 0, 14, 0} // with a Recovery IE
	tests := []struct {
		name    string
		pkt     []byte
		wantSeq uint16
		wantOK  bool
	}{
		{name: "echo response", pkt: resp, wantSeq: 0x1234, wantOK: true},
		{name: "echo request", pkt: EncodeEchoRequest(0x1234)},
		{name: "no S flag", pkt: append([]byte{0x30}, resp[1:]...)},
		{name: "short", pkt: resp[:9]},
		{name: "error indication", pkt: []byte{0x32, MsgTypeErrorInd, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seq, ok := EchoResponseSeq(tc.pkt)
			if seq != tc.wantSeq || ok != tc.wantOK {
				t.Errorf("= %#x %v, want %#x %v", seq, ok, tc.wantSeq, tc.wantOK)
			}
		})
	}
}

// An Echo Response is the 12-byte header with type 2 and the request's
// sequence number, then the Recovery IE (type 14) with a zero Restart
// Counter (TS 29.281 7.2.2); EchoResponseSeq reads it back.
func TestEncodeEchoResponse(t *testing.T) {
	want := []byte{0x32, 0x02, 0x00, 0x06, 0, 0, 0, 0, 0x12, 0x34, 0, 0, 14, 0}
	got := EncodeEchoResponse(0x1234)
	if !bytes.Equal(got, want) {
		t.Errorf("EncodeEchoResponse = % x, want % x", got, want)
	}
	if seq, ok := EchoResponseSeq(got); !ok || seq != 0x1234 {
		t.Errorf("EchoResponseSeq = %#x %v, want 0x1234 true", seq, ok)
	}
}

// EchoRequestSeq reads the sequence number of an Echo Request only.
func TestEchoRequestSeq(t *testing.T) {
	req := EncodeEchoRequest(0x1234)
	tests := []struct {
		name    string
		pkt     []byte
		wantSeq uint16
		wantOK  bool
	}{
		{name: "echo request", pkt: req, wantSeq: 0x1234, wantOK: true},
		{name: "echo response", pkt: EncodeEchoResponse(0x1234)},
		{name: "no S flag", pkt: append([]byte{0x30}, req[1:]...)},
		{name: "short", pkt: req[:9]},
		{name: "g-pdu", pkt: EncodeGPDU(1, 1, []byte{0x45})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seq, ok := EchoRequestSeq(tc.pkt)
			if seq != tc.wantSeq || ok != tc.wantOK {
				t.Errorf("= %#x %v, want %#x %v", seq, ok, tc.wantSeq, tc.wantOK)
			}
		})
	}
}
