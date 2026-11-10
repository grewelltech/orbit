package ue

import (
	"fmt"

	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
)

// ConfigurationUpdate is what the UE reads from a Configuration Update
// Command (TS 24.501 §8.2.19) of the generic UE configuration update
// procedure (§5.4.4).
type ConfigurationUpdate struct {
	// AckRequested is the Acknowledgement bit of the Configuration update
	// indication IE: the UE answers with a Configuration Update Complete.
	AckRequested bool
	// RegistrationRequested is that IE's Registration requested bit.
	RegistrationRequested bool
	// GUTI is the new 5G-GUTI, nil when the command assigns none.
	GUTI *GUTI5G
	// HasTAIList and HasAllowedNSSAI report a new TAI list and a new
	// allowed NSSAI.
	HasTAIList      bool
	HasAllowedNSSAI bool
}

// ParseConfigurationUpdateCommand reads a decoded Configuration Update
// Command.
func ParseConfigurationUpdateCommand(m *f5nas.Message) (*ConfigurationUpdate, error) {
	if m.GmmMessage == nil || m.ConfigurationUpdateCommand == nil {
		return nil, fmt.Errorf("not a Configuration Update Command")
	}
	c := m.ConfigurationUpdateCommand
	out := &ConfigurationUpdate{HasTAIList: c.TAIList != nil, HasAllowedNSSAI: c.AllowedNSSAI != nil}
	if ind := c.ConfigurationUpdateIndication; ind != nil {
		out.AckRequested = ind.GetACK() == 1
		out.RegistrationRequested = ind.GetRED() == 1
	}
	if g := c.GUTI5G; g != nil {
		n := int(g.Len)
		if n > len(g.Octet) {
			n = len(g.Octet)
		}
		out.GUTI = &GUTI5G{Raw: append([]byte(nil), g.Octet[:n]...)}
	}
	return out, nil
}

// BuildConfigurationUpdateComplete builds the Configuration Update Complete
// (TS 24.501 §8.2.20). Returns the plain message for security wrapping.
func BuildConfigurationUpdateComplete() (*f5nas.Message, error) {
	m, gmm := newGmm(f5nas.MsgTypeConfigurationUpdateComplete)
	c := nasMessage.NewConfigurationUpdateComplete(f5nas.MsgTypeConfigurationUpdateComplete)
	setGmmHeader(&c.ExtendedProtocolDiscriminator, &c.SpareHalfOctetAndSecurityHeaderType,
		&c.ConfigurationUpdateCompleteMessageIdentity, f5nas.MsgTypeConfigurationUpdateComplete)
	gmm.ConfigurationUpdateComplete = c
	return m, nil
}
