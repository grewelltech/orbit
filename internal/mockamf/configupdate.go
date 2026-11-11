package mockamf

import (
	f5nas "github.com/free5gc/nas"
	"github.com/free5gc/nas/nasMessage"
	"github.com/free5gc/nas/nasType"

	"github.com/bgrewell/orbit/internal/nas"
)

// ConfigurationUpdate is the Configuration Update Command the mock sends a
// UE once it is registered (WithConfigurationUpdate).
type ConfigurationUpdate struct {
	// AckRequested sets the Acknowledgement bit of the Configuration
	// update indication IE; without it, and without RegistrationRequested,
	// the IE is left out.
	AckRequested bool
	// RegistrationRequested sets that IE's Registration requested bit.
	RegistrationRequested bool
	// NewGUTI assigns the UE a new 5G-GUTI (ConfigurationUpdateGUTI).
	NewGUTI bool
}

// WithConfigurationUpdate makes the mock start the generic UE configuration
// update procedure (TS 24.501 5.4.4.2) on each Registration Complete: one
// protected Configuration Update Command, as cu describes.
func WithConfigurationUpdate(cu ConfigurationUpdate) Option {
	return func(a *AMF) { a.configUpdate = &cu }
}

// ConfigurationUpdateCompletes reports how many Configuration Update
// Completes the mock has received.
func (a *AMF) ConfigurationUpdateCompletes() int64 { return a.configUpdateCompletes.Load() }

// ConfigurationUpdateGUTI is the 5GS mobile identity value of the 5G-GUTI a
// ConfigurationUpdate with NewGUTI assigns the UE with amfUENGAPID.
func (a *AMF) ConfigurationUpdateGUTI(amfUENGAPID int64) []byte {
	return guti5G(a.plmn, 0x80000000|uint32(amfUENGAPID))
}

// DeregistrationIdentities returns the 5GS mobile identity value of each
// UE-originating Deregistration Request received, oldest first.
func (a *AMF) DeregistrationIdentities() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneAll(a.deregIdentities)
}

// recordDeregistration notes the identity a Deregistration Request names.
func (a *AMF) recordDeregistration(req *nasMessage.DeregistrationRequestUEOriginatingDeregistration) {
	if req == nil {
		return
	}
	id := req.MobileIdentity5GS
	a.mu.Lock()
	a.deregIdentities = append(a.deregIdentities, append([]byte{}, id.Buffer[:min(int(id.Len), len(id.Buffer))]...))
	a.mu.Unlock()
}

// sendConfigurationUpdate sends st the WithConfigurationUpdate command.
func (a *AMF) sendConfigurationUpdate(p *pipe, st *ueState) error {
	cu := a.configUpdate
	m, gmm := plainGmm(f5nas.MsgTypeConfigurationUpdateCommand)
	cmd := nasMessage.NewConfigurationUpdateCommand(f5nas.MsgTypeConfigurationUpdateCommand)
	setPlainHeader(&cmd.ExtendedProtocolDiscriminator, &cmd.SpareHalfOctetAndSecurityHeaderType,
		&cmd.ConfigurationUpdateCommandMessageIdentity, f5nas.MsgTypeConfigurationUpdateCommand)
	if cu.AckRequested || cu.RegistrationRequested {
		ind := nasType.NewConfigurationUpdateIndication(nasMessage.ConfigurationUpdateCommandConfigurationUpdateIndicationType)
		ind.SetIei(nasMessage.ConfigurationUpdateCommandConfigurationUpdateIndicationType)
		if cu.AckRequested {
			ind.SetACK(1)
		}
		if cu.RegistrationRequested {
			ind.SetRED(1)
		}
		cmd.ConfigurationUpdateIndication = ind
	}
	if cu.NewGUTI {
		v := a.ConfigurationUpdateGUTI(st.amfID)
		g := nasType.NewGUTI5G(nasMessage.ConfigurationUpdateCommandGUTI5GType)
		g.SetLen(uint16(len(v)))
		copy(g.Octet[:], v)
		cmd.GUTI5G = g
	}
	gmm.ConfigurationUpdateCommand = cmd
	wrapped, err := st.encodeSecure(m, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	return a.send(p, buildDownlinkNASTransport(st.amfID, a.ranIDOf(st), wrapped))
}
