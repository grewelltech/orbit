package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The declaration: a PASS and four FAILs, each citing the shall of 10.3.5,
// which is the primary reference; the NG Setup is the stimulus, so the
// check needs no procedure first.
func TestMissingTAListDeclaration(t *testing.T) {
	c := ngSetupMissingMandatoryIE{}
	if err := ValidateTest(c); err != nil {
		t.Fatal(err)
	}
	if WorstVerdict(c) != Fail || len(NeedsOf(c).Procedures) != 0 {
		t.Errorf("worst %s, needs %v; want FAIL and no procedure", WorstVerdict(c), NeedsOf(c).Procedures)
	}
	for _, r := range c.SpecRefs() {
		if r.Primary != (r.Key() == missingTAListRef) {
			t.Errorf("%s: primary %v; the shall of 10.3.5 is the primary reference", r.Key(), r.Primary)
		}
	}
}

// Against the mock AMF over real SCTP, as a suite run scores it: the mock
// accepts an NG SETUP REQUEST without a Supported TA List, which misses the
// shall of 10.3.5 (FAIL, outcome accepted); the core survives, and the
// check's associations close.
func TestMissingTAListAgainstMock(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	env := Env{AMFAddr: front.Addr,
		GNB: gnb.Config{ID: 0x400, IDBits: 24, Name: "orbit-conf", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}}
	res := runOne(context.Background(), ngSetupMissingMandatoryIE{}, env, 10*time.Second)
	if res.Verdict != Fail || res.Outcome != missingTAListAccepted.ID || res.Observed != "NG SETUP RESPONSE (accepted!)" {
		t.Fatalf("= %s %s %q (%s), want FAIL accepted", res.Verdict, res.Outcome, res.Observed, res.Detail)
	}
	if !strings.Contains(res.Detail, "violates TS 38.413 10.3.5 Reject IE bullet 1 (shall)") {
		t.Errorf("detail %q does not name the shall", res.Detail)
	}
	if setups := amf.Setups(); len(setups) != 2 {
		t.Errorf("mock saw %d NG Setups, want the stimulus and the liveness probe", len(setups))
	}
	deadline := time.Now().Add(3 * time.Second)
	for front.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := front.Active(); n != 0 {
		t.Errorf("%d associations still open after the check", n)
	}
}
