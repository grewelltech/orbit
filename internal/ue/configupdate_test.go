package ue

import (
	"bytes"
	"testing"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

func TestParseConfigurationUpdateCommand(t *testing.T) {
	guti := []byte{0xf2, 0x02, 0xf8, 0x39, 0xca, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x07}
	indication := func(octet uint8) *nasType.ConfigurationUpdateIndication {
		return &nasType.ConfigurationUpdateIndication{Octet: octet}
	}
	tests := []struct {
		name string
		fill func(*nasMessage.ConfigurationUpdateCommand)
		want ConfigurationUpdate
	}{
		{name: "no optional IE", fill: func(*nasMessage.ConfigurationUpdateCommand) {}},
		{name: "indication with neither bit", fill: func(c *nasMessage.ConfigurationUpdateCommand) { c.ConfigurationUpdateIndication = indication(0xd0) }},
		{name: "acknowledgement requested", fill: func(c *nasMessage.ConfigurationUpdateCommand) { c.ConfigurationUpdateIndication = indication(0xd1) },
			want: ConfigurationUpdate{AckRequested: true}},
		{name: "registration requested", fill: func(c *nasMessage.ConfigurationUpdateCommand) { c.ConfigurationUpdateIndication = indication(0xd2) },
			want: ConfigurationUpdate{RegistrationRequested: true}},
		{name: "both bits", fill: func(c *nasMessage.ConfigurationUpdateCommand) { c.ConfigurationUpdateIndication = indication(0xd3) },
			want: ConfigurationUpdate{AckRequested: true, RegistrationRequested: true}},
		{name: "new 5G-GUTI", fill: func(c *nasMessage.ConfigurationUpdateCommand) {
			c.GUTI5G = nasType.NewGUTI5G(nasMessage.ConfigurationUpdateCommandGUTI5GType)
			c.GUTI5G.SetLen(uint16(len(guti)))
			copy(c.GUTI5G.Octet[:], guti)
		}, want: ConfigurationUpdate{GUTI: &GUTI5G{Raw: guti}}},
		{name: "TAI list and allowed NSSAI", fill: func(c *nasMessage.ConfigurationUpdateCommand) {
			c.TAIList = nasType.NewTAIList(nasMessage.ConfigurationUpdateCommandTAIListType)
			c.AllowedNSSAI = nasType.NewAllowedNSSAI(nasMessage.ConfigurationUpdateCommandAllowedNSSAIType)
		}, want: ConfigurationUpdate{HasTAIList: true, HasAllowedNSSAI: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := f5nas.NewMessage()
			m.GmmMessage = f5nas.NewGmmMessage()
			m.ConfigurationUpdateCommand = nasMessage.NewConfigurationUpdateCommand(f5nas.MsgTypeConfigurationUpdateCommand)
			tc.fill(m.ConfigurationUpdateCommand)
			got, err := ParseConfigurationUpdateCommand(m)
			if err != nil {
				t.Fatal(err)
			}
			if got.AckRequested != tc.want.AckRequested || got.RegistrationRequested != tc.want.RegistrationRequested ||
				got.HasTAIList != tc.want.HasTAIList || got.HasAllowedNSSAI != tc.want.HasAllowedNSSAI {
				t.Errorf("= %+v, want %+v", got, tc.want)
			}
			if (got.GUTI == nil) != (tc.want.GUTI == nil) || (got.GUTI != nil && !bytes.Equal(got.GUTI.Raw, tc.want.GUTI.Raw)) {
				t.Errorf("5G-GUTI = %+v, want %+v", got.GUTI, tc.want.GUTI)
			}
		})
	}
	if _, err := ParseConfigurationUpdateCommand(f5nas.NewMessage()); err == nil {
		t.Error("parsed a message that is not a Configuration Update Command")
	}
}

func TestConfigurationUpdateCompleteRoundTrip(t *testing.T) {
	m, err := BuildConfigurationUpdateComplete()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := m.PlainNasEncode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := nas.DecodePlain(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.GmmHeader.GetMessageType() != f5nas.MsgTypeConfigurationUpdateComplete || got.ConfigurationUpdateComplete == nil {
		t.Errorf("decoded %x as message type %#x", raw, got.GmmHeader.GetMessageType())
	}
}
