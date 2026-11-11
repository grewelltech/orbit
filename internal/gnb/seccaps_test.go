package gnb

import (
	"testing"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/ngap"
)

func TestUESecurityCapabilitiesHelpers(t *testing.T) {
	a := NewUESecurityCapabilities(0xC000, 0x4000, 0x8000, 0x0001)
	if b := a.NRencryptionAlgorithms.Value; b.BitLength != 16 || b.Bytes[0] != 0xC0 || b.Bytes[1] != 0x00 {
		t.Errorf("NR encryption bitmap = %+v", b)
	}
	if b := a.EUTRAintegrityProtectionAlgorithms.Value; b.Bytes[0] != 0x00 || b.Bytes[1] != 0x01 {
		t.Errorf("E-UTRA integrity bitmap = %+v", b)
	}
	tests := []struct {
		name string
		x, y *ngapType.UESecurityCapabilities
		want bool
	}{
		{name: "both nil", want: true},
		{name: "one nil", x: a, want: false},
		{name: "clone", x: a, y: CloneUESecurityCapabilities(a), want: true},
		{name: "NR encryption differs", x: a, y: NewUESecurityCapabilities(0xE000, 0x4000, 0x8000, 0x0001), want: false},
		{name: "NR integrity differs", x: a, y: NewUESecurityCapabilities(0xC000, 0xE000, 0x8000, 0x0001), want: false},
		{name: "E-UTRA encryption differs", x: a, y: NewUESecurityCapabilities(0xC000, 0x4000, 0, 0x0001), want: false},
		{name: "E-UTRA integrity differs", x: a, y: NewUESecurityCapabilities(0xC000, 0x4000, 0x8000, 0), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameUESecurityCapabilities(tc.x, tc.y); got != tc.want {
				t.Errorf("= %t, want %t", got, tc.want)
			}
			if got := SameUESecurityCapabilities(tc.y, tc.x); got != tc.want {
				t.Errorf("swapped = %t, want %t", got, tc.want)
			}
		})
	}
	c := CloneUESecurityCapabilities(a)
	c.NRencryptionAlgorithms.Value.Bytes[0] = 0
	if a.NRencryptionAlgorithms.Value.Bytes[0] != 0xC0 {
		t.Error("the clone shares its bitmap with the original")
	}
	if CloneUESecurityCapabilities(nil) != nil {
		t.Error("clone of nil is not nil")
	}
}

// The Path Switch Request carries exactly the capabilities it is given and
// is refused without any; the capabilities are read back from an Initial
// Context Setup Request, a Handover Request and a Path Switch Request
// Acknowledge.
func TestUESecurityCapabilitiesOnTheWire(t *testing.T) {
	cfg := Config{ID: 2, Name: "gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []SNSSAI{{SST: 1}}}
	sessions := []AdmittedSession{{PDUSessionID: 1, GNBTunnel: GNBTunnel{Address: "127.0.0.2", TEID: 9}, QFIs: []int64{1}}}
	if _, err := BuildPathSwitchRequest(cfg, 1, 2, nil, sessions); err == nil {
		t.Error("built a Path Switch Request without UE security capabilities")
	}
	for _, caps := range []*ngapType.UESecurityCapabilities{
		NewUESecurityCapabilities(0, 0x4000, 0, 0),
		NewUESecurityCapabilities(0xE000, 0xE000, 0xE000, 0xE000),
	} {
		pdu, err := BuildPathSwitchRequest(cfg, 1, 2, caps, sessions)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ngap.Encode(pdu)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := ngap.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		var got *ngapType.UESecurityCapabilities
		for _, ie := range raw.InitiatingMessage.Value.PathSwitchRequest.ProtocolIEs.List {
			if ie.Id.Value == ngapType.ProtocolIEIDUESecurityCapabilities {
				got = ie.Value.UESecurityCapabilities
			}
		}
		if !SameUESecurityCapabilities(got, caps) {
			t.Errorf("Path Switch Request carries %+v, want %+v", got, caps)
		}
	}

	want := NewUESecurityCapabilities(0xC000, 0x6000, 0, 0)

	var ics ngapType.NGAPPDU
	ics.Present = ngapType.NGAPPDUPresentInitiatingMessage
	ics.InitiatingMessage = new(ngapType.InitiatingMessage)
	ics.InitiatingMessage.ProcedureCode.Value = ngapType.ProcedureCodeInitialContextSetup
	ics.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentInitialContextSetupRequest
	ics.InitiatingMessage.Value.InitialContextSetupRequest = new(ngapType.InitialContextSetupRequest)
	dl, err := ParseDownlink(&ics)
	if err != nil || dl.UESecurityCapabilities != nil {
		t.Errorf("Initial Context Setup Request without the IE: %+v, %v", dl, err)
	}
	icsIE := ngapType.InitialContextSetupRequestIEs{}
	icsIE.Id.Value = ngapType.ProtocolIEIDUESecurityCapabilities
	icsIE.Value.Present = ngapType.InitialContextSetupRequestIEsPresentUESecurityCapabilities
	icsIE.Value.UESecurityCapabilities = want
	ics.InitiatingMessage.Value.InitialContextSetupRequest.ProtocolIEs.List = []ngapType.InitialContextSetupRequestIEs{icsIE}
	if dl, err := ParseDownlink(&ics); err != nil || !SameUESecurityCapabilities(dl.UESecurityCapabilities, want) {
		t.Errorf("Initial Context Setup Request: %+v, %v", dl, err)
	}

	var hr ngapType.NGAPPDU
	hr.Present = ngapType.NGAPPDUPresentInitiatingMessage
	hr.InitiatingMessage = new(ngapType.InitiatingMessage)
	hr.InitiatingMessage.Value.Present = ngapType.InitiatingMessagePresentHandoverRequest
	hr.InitiatingMessage.Value.HandoverRequest = new(ngapType.HandoverRequest)
	hrIE := ngapType.HandoverRequestIEs{}
	hrIE.Id.Value = ngapType.ProtocolIEIDUESecurityCapabilities
	hrIE.Value.Present = ngapType.HandoverRequestIEsPresentUESecurityCapabilities
	hrIE.Value.UESecurityCapabilities = want
	hr.InitiatingMessage.Value.HandoverRequest.ProtocolIEs.List = []ngapType.HandoverRequestIEs{hrIE}
	if info, err := ParseHandoverRequest(&hr); err != nil || !SameUESecurityCapabilities(info.UESecurityCapabilities, want) {
		t.Errorf("Handover Request: %+v, %v", info, err)
	}

	var ack ngapType.NGAPPDU
	ack.Present = ngapType.NGAPPDUPresentSuccessfulOutcome
	ack.SuccessfulOutcome = new(ngapType.SuccessfulOutcome)
	ack.SuccessfulOutcome.Value.Present = ngapType.SuccessfulOutcomePresentPathSwitchRequestAcknowledge
	ack.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge = new(ngapType.PathSwitchRequestAcknowledge)
	if got := PathSwitchAcknowledgeUESecurityCapabilities(&ack); got != nil {
		t.Errorf("Acknowledge without the IE: %+v", got)
	}
	ackIE := ngapType.PathSwitchRequestAcknowledgeIEs{}
	ackIE.Id.Value = ngapType.ProtocolIEIDUESecurityCapabilities
	ackIE.Value.Present = ngapType.PathSwitchRequestAcknowledgeIEsPresentUESecurityCapabilities
	ackIE.Value.UESecurityCapabilities = want
	ack.SuccessfulOutcome.Value.PathSwitchRequestAcknowledge.ProtocolIEs.List = []ngapType.PathSwitchRequestAcknowledgeIEs{ackIE}
	if got := PathSwitchAcknowledgeUESecurityCapabilities(&ack); !SameUESecurityCapabilities(got, want) {
		t.Errorf("Acknowledge: %+v, want %+v", got, want)
	}
	if got := PathSwitchAcknowledgeUESecurityCapabilities(&ics); got != nil {
		t.Errorf("a message that is not an Acknowledge: %+v", got)
	}
}
