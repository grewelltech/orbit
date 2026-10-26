package mockamf

import "github.com/bgrewell/orbit/internal/nas"

// WithUndecodableAccept makes the mock answer the Security Mode Complete
// with a Registration Accept that does not decode: a protected 7e 00 42
// with no 5GS registration result.
func WithUndecodableAccept() Option { return func(a *AMF) { a.undecodableAccept = true } }

// Deregistrations returns how many Deregistration Requests the mock has
// received, plain or protected.
func (a *AMF) Deregistrations() int { return int(a.deregs.Load()) }

// sendUndecodableAccept sends WithUndecodableAccept's Accept in an Initial
// Context Setup Request.
func (a *AMF) sendUndecodableAccept(p *pipe, st *ueState) error {
	wrapped, err := st.sec.EncodeSecureRaw([]byte{0x7e, 0x00, 0x42}, nas.SecHdrIntegrityCiphered)
	if err != nil {
		return err
	}
	return a.send(p, buildInitialContextSetupRequest(st.amfID, st.ranID, wrapped, a.plmn, a.cfg.SST, a.sdBytes))
}
