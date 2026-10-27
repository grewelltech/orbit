package mockamf

import (
	"testing"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
)

// The Reject decodes back to its session, transaction and 5GSM cause.
func TestBuildPDUSessionEstablishmentReject(t *testing.T) {
	tests := []struct {
		name         string
		id, pti, cau uint8
	}{
		{name: "#27", id: 1, pti: 0, cau: nasMessage.Cause5GSMMissingOrUnknownDNN},
		{name: "#91 as 5GSM", id: 5, pti: 7, cau: nasMessage.Cause5GMMDNNNotSupportedOrNotSubscribedInTheSlice},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := buildPDUSessionEstablishmentReject(tc.id, tc.pti, tc.cau)
			if err != nil {
				t.Fatal(err)
			}
			m := f5nas.NewMessage()
			if err := m.PlainNasDecode(&b); err != nil {
				t.Fatal(err)
			}
			rej := m.PDUSessionEstablishmentReject
			if m.GsmMessage == nil || rej == nil {
				t.Fatal("not a PDU Session Establishment Reject")
			}
			if rej.GetPDUSessionID() != tc.id || rej.GetPTI() != tc.pti || rej.GetCauseValue() != tc.cau {
				t.Errorf("= id %d pti %d cause %d, want %d %d %d", rej.GetPDUSessionID(), rej.GetPTI(), rej.GetCauseValue(), tc.id, tc.pti, tc.cau)
			}
		})
	}
}

func TestCauseOr(t *testing.T) {
	tests := []struct{ c, def, want uint8 }{
		{0, 27, 27},
		{31, 27, 31},
	}
	for _, tc := range tests {
		if got := causeOr(tc.c, tc.def); got != tc.want {
			t.Errorf("causeOr(%d, %d) = %d, want %d", tc.c, tc.def, got, tc.want)
		}
	}
}

// The 5GSM STATUS decodes back to its session, transaction and cause.
func TestBuildStatus5GSM(t *testing.T) {
	tests := []struct{ id, pti, cause uint8 }{
		{1, 0, nasMessage.Cause5GSMInvalidMandatoryInformation},
		{3, 9, nasMessage.Cause5GSMProtocolErrorUnspecified},
	}
	for _, tc := range tests {
		b, err := buildStatus5GSM(tc.id, tc.pti, tc.cause)
		if err != nil {
			t.Fatal(err)
		}
		m := f5nas.NewMessage()
		if err := m.PlainNasDecode(&b); err != nil {
			t.Fatal(err)
		}
		st := m.Status5GSM
		if m.GsmMessage == nil || st == nil || st.GetPDUSessionID() != tc.id || st.GetPTI() != tc.pti || st.GetCauseValue() != tc.cause {
			t.Errorf("Status5GSM %+v, want id %d pti %d cause %d", st, tc.id, tc.pti, tc.cause)
		}
	}
}
