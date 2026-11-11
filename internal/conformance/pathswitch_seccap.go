package conformance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
)

func init() {
	builtins = append(builtins, ngapPathSwitchSecCapMismatch{})
}

// pathSwitchSecCapFailure is the check's failure message.
const pathSwitchSecCapFailure = "the AMF did not send its locally stored UE security capabilities in the PATH SWITCH REQUEST ACKNOWLEDGE after a PATH SWITCH REQUEST whose UE Security Capabilities differ from them. TS 33.501 6.7.3.1. Likely cause: the AMF does not compare the capabilities of the Path Switch Request with its stored copy."

// Observed texts of NGAP-PATH-SWITCH-SECCAP-MISMATCH.
const (
	secCapStored  = "PATH SWITCH REQUEST ACKNOWLEDGE carries the AMF's stored UE security capabilities"
	secCapAbsent  = "PATH SWITCH REQUEST ACKNOWLEDGE without UE Security Capabilities"
	secCapEchoed  = "PATH SWITCH REQUEST ACKNOWLEDGE echoes the mismatching UE security capabilities"
	secCapOther   = "PATH SWITCH REQUEST ACKNOWLEDGE carries UE security capabilities that are neither the stored nor the sent ones"
	secCapNoAck   = "no PATH SWITCH REQUEST ACKNOWLEDGE"
	secCapNoStore = "no UE Security Capabilities in the INITIAL CONTEXT SETUP REQUEST"
)

// ngapPathSwitchSecCapMismatch registers a UE with a PDU session on gNB A
// and hands it over by Xn to gNB B with a PATH SWITCH REQUEST whose UE
// Security Capabilities differ from the ones the AMF supplied in the
// INITIAL CONTEXT SETUP REQUEST. TS 33.501 6.7.3.1: "The AMF shall verify
// that the UE's 5G security capabilities received from the target gNB are
// the same as the UE's 5G security capabilities that the AMF has locally
// stored. If there is a mismatch, the AMF shall send its locally stored 5G
// security capabilities of the UE to the target gNB in the Path-Switch
// Acknowledge message."
//
// PASS is an Acknowledge whose UE Security Capabilities IE equals what the
// AMF supplied at registration. An Acknowledge without the IE, one that
// echoes the capabilities sent, one that carries others, and no Acknowledge
// at all (a PATH SWITCH REQUEST FAILURE included) are FAIL. The sent
// capabilities are the supplied ones with the bit of algorithm 3 inverted
// in both NR bitmaps, so they always differ. The UE is deregistered
// afterwards and a fresh NG Setup is tried.
type ngapPathSwitchSecCapMismatch struct {
	// ackWait overrides pathSwitchAckWait (tests only).
	ackWait time.Duration
}

func (ngapPathSwitchSecCapMismatch) ID() string         { return "NGAP-PATH-SWITCH-SECCAP-MISMATCH" }
func (ngapPathSwitchSecCapMismatch) Category() Category { return Security }
func (ngapPathSwitchSecCapMismatch) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 33.501", Clause: "6.7.3.1", Strength: Shall, Since: 15, Verified: ts33501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 names the target \"gNB\" and ends \"Additionally, the AMF may log the event and may take additional measures, such as raising an alarm\"; by V15.20.0 the target is \"gNB/ng-eNB\" and the last sentence \"The AMF shall support logging capabilities for this event and may take additional measures, such as raising an alarm\"; the two cited sentences are otherwise unchanged through V19.7.0"},
			}},
	}
}

func (c ngapPathSwitchSecCapMismatch) waitAck() time.Duration {
	if c.ackWait > 0 {
		return c.ackWait
	}
	return pathSwitchAckWait
}

// Budget covers the registration, the wait for the Acknowledge, the
// deregistration and the liveness probe, also against a core that has
// stopped answering.
func (c ngapPathSwitchSecCapMismatch) Budget() time.Duration {
	return c.waitAck() + 20*time.Second
}

// mismatchingCapabilities is caps with the bit of algorithm 3 (NEA3, NIA3)
// inverted in the NR encryption and NR integrity bitmaps.
func mismatchingCapabilities(caps *ngapType.UESecurityCapabilities) *ngapType.UESecurityCapabilities {
	out := gnb.CloneUESecurityCapabilities(caps)
	for _, b := range [][]byte{out.NRencryptionAlgorithms.Value.Bytes, out.NRintegrityProtectionAlgorithms.Value.Bytes} {
		if len(b) > 0 {
			b[0] ^= 0x20
		}
	}
	return out
}

// describeCapabilities renders the four bitmaps of a UE Security
// Capabilities IE.
func describeCapabilities(c *ngapType.UESecurityCapabilities) string {
	if c == nil {
		return "none"
	}
	return fmt.Sprintf("NR encryption %x, NR integrity %x, E-UTRA encryption %x, E-UTRA integrity %x",
		c.NRencryptionAlgorithms.Value.Bytes, c.NRintegrityProtectionAlgorithms.Value.Bytes,
		c.EUTRAencryptionAlgorithms.Value.Bytes, c.EUTRAintegrityProtectionAlgorithms.Value.Bytes)
}

func (c ngapPathSwitchSecCapMismatch) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "PATH SWITCH REQUEST ACKNOWLEDGE carrying the AMF's locally stored UE security capabilities, for a PATH SWITCH REQUEST whose UE Security Capabilities differ from them (TS 33.501 6.7.3.1)"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()
	if _, err := h.Register(ctx); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "UE setup on the source gNB failed", err.Error()
		return r
	}
	stored, err := h.SuppliedCapabilities()
	switch {
	case err != nil:
		r.Verdict, r.Observed, r.Detail = Error, "UE setup on the source gNB failed", err.Error()
		return r
	case stored == nil:
		r.Verdict, r.Observed = Error, secCapNoStore
		r.Detail = "the AMF supplied no UE Security Capabilities at registration, so there is no stored copy to compare the Acknowledge with"
		return r
	}
	sent := mismatchingCapabilities(stored)
	hctx, cancel := context.WithTimeout(ctx, c.waitAck())
	got, err := h.XnHandoverWithCapabilities(hctx, sent)
	cancel()
	switch {
	case err != nil && errors.Is(err, engine.ErrPathSwitchNotAcknowledged) && ctx.Err() == nil:
		r.Verdict, r.Observed = Fail, secCapNoAck
		r.Detail = fmt.Sprintf("%s Recorded: no Acknowledge within %s of starting the handover: %v", pathSwitchSecCapFailure, c.waitAck(), err)
	case err != nil:
		r.Verdict, r.Observed, r.Detail = Error, "Xn path switch failed", err.Error()
		return r
	default:
		r.Verdict, r.Observed, r.Detail = classifySecCapAck(stored, sent, got)
	}
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("supplied by the AMF at registration: %s; sent in the PATH SWITCH REQUEST: %s; in the Acknowledge: %s",
		describeCapabilities(stored), describeCapabilities(sent), describeCapabilities(got)))
	// The UE leaves before the probe: it is deregistered from the gNB it
	// is on, the target after an Acknowledge and the source without one.
	h.Close()
	r.Detail = joinDetail(r.Detail, "state left: the UE was deregistered (switch-off) and its associations closed")
	if ctx.Err() != nil {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the PATH SWITCH REQUEST with mismatching UE security capabilities")
}

// classifySecCapAck judges the UE Security Capabilities IE of the
// Acknowledge, got (nil when absent), against the capabilities the AMF
// supplied, stored, and the ones the request carried, sent.
func classifySecCapAck(stored, sent, got *ngapType.UESecurityCapabilities) (Verdict, string, string) {
	switch {
	case got == nil:
		return Fail, secCapAbsent, pathSwitchSecCapFailure
	case gnb.SameUESecurityCapabilities(got, stored):
		return Pass, secCapStored, "the Acknowledge's UE Security Capabilities equal the ones the AMF supplied in the INITIAL CONTEXT SETUP REQUEST"
	case gnb.SameUESecurityCapabilities(got, sent):
		return Fail, secCapEchoed, pathSwitchSecCapFailure
	}
	return Fail, secCapOther, pathSwitchSecCapFailure
}
