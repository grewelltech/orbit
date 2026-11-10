package conformance

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Every outcome of the update and of the registration after it. The state
// is judged only after an ACKNOWLEDGE: a registration PASSes, a Reject with
// a tracking-area cause (#12, #13, #15) FAILs with the issue's message, and
// every other registration failure is ERROR. A FAILURE SKIPs with both
// results in Observed; no outcome is ERROR.
func TestJudgeKeepsTA(t *testing.T) {
	g := gnb.Config{MCC: "208", MNC: "93", TAC: 7}
	ack := &gnb.RANConfigUpdateResult{Acknowledged: true}
	failure := &gnb.RANConfigUpdateResult{Cause: "misc(5)"}
	const ta = "TAC 7 of PLMN 208/93"
	tests := []struct {
		name         string
		o            keepsTAOutcome
		want         Verdict
		wantObserved string
		wantDetail   string
	}{
		{name: "ack, registered", o: keepsTAOutcome{update: ack, registered: true}, want: Pass,
			wantObserved: "registration succeeded in " + ta + " after the ACKNOWLEDGE", wantDetail: "RAN CONFIGURATION UPDATE ACKNOWLEDGE after 20ms"},
		{name: "ack after other PDUs, registered", o: keepsTAOutcome{update: &gnb.RANConfigUpdateResult{Acknowledged: true, Skipped: 2}, registered: true}, want: Pass,
			wantObserved: "registration succeeded in " + ta + " after the ACKNOWLEDGE", wantDetail: "(2 PDU(s) of other procedures before it)"},
		{name: "ack, reject #12", o: keepsTAOutcome{update: ack, sawReject: true, rejectCause: 12}, want: Fail,
			wantObserved: "Registration Reject with 5GMM cause #12 (Tracking area not allowed) in " + ta + " after the ACKNOWLEDGE", wantDetail: keepsTAFailText},
		{name: "ack, reject #13", o: keepsTAOutcome{update: ack, sawReject: true, rejectCause: 13}, want: Fail,
			wantObserved: "Registration Reject with 5GMM cause #13 (Roaming not allowed in this tracking area) in " + ta + " after the ACKNOWLEDGE", wantDetail: keepsTAFailText},
		{name: "ack, reject #15", o: keepsTAOutcome{update: ack, sawReject: true, rejectCause: 15}, want: Fail,
			wantObserved: "Registration Reject with 5GMM cause #15 (No suitable cells in tracking area) in " + ta + " after the ACKNOWLEDGE", wantDetail: keepsTAFailText},
		{name: "ack, reject #11", o: keepsTAOutcome{update: ack, sawReject: true, rejectCause: 11}, want: Error,
			wantObserved: "Registration Reject with 5GMM cause #11 (PLMN not allowed) after the ACKNOWLEDGE", wantDetail: "5GMM cause #11 is not tied to a tracking area"},
		{name: "ack, reject #27", o: keepsTAOutcome{update: ack, sawReject: true, rejectCause: 27}, want: Error,
			wantObserved: "Registration Reject with 5GMM cause #27 after the ACKNOWLEDGE", wantDetail: "5GMM cause #27 is not tied to a tracking area"},
		{name: "ack, reject #62", o: keepsTAOutcome{update: ack, sawReject: true, rejectCause: 62}, want: Error,
			wantObserved: "Registration Reject with 5GMM cause #62 (No network slices available) after the ACKNOWLEDGE", wantDetail: "does not show the AMF stopped serving " + ta},
		{name: "ack, unreadable reject", o: keepsTAOutcome{update: ack, sawReject: true, rejectErr: errors.New("MAC mismatch")}, want: Error,
			wantObserved: "Registration Reject that could not be read after the ACKNOWLEDGE", wantDetail: "MAC mismatch"},
		{name: "ack, authentication reject", o: keepsTAOutcome{update: ack, authReject: true}, want: Error,
			wantObserved: "Authentication Reject after the ACKNOWLEDGE", wantDetail: "before the tracking area could matter"},
		{name: "ack, released", o: keepsTAOutcome{update: ack, released: true}, want: Error,
			wantObserved: "UE context released during the registration after the ACKNOWLEDGE", wantDetail: "UE CONTEXT RELEASE COMMAND"},
		{name: "ack, registration unanswered", o: keepsTAOutcome{update: ack, noAnswer: true}, want: Error,
			wantObserved: "registration unanswered after the ACKNOWLEDGE", wantDetail: "no Registration Accept or Reject within 10s"},
		{name: "ack, another failure", o: keepsTAOutcome{update: ack, err: errors.New("boom")}, want: Error,
			wantObserved: "registration failed after the ACKNOWLEDGE", wantDetail: "boom"},
		{name: "failure, registered", o: keepsTAOutcome{update: failure, registered: true}, want: Skip,
			wantObserved: "RAN CONFIGURATION UPDATE FAILURE (cause misc(5)); registration succeeded", wantDetail: "TS 38.413 8.7.2.3 allows"},
		// After a FAILURE even a tracking-area Reject is not judged.
		{name: "failure, reject #12", o: keepsTAOutcome{update: failure, sawReject: true, rejectCause: 12}, want: Skip,
			wantObserved: "RAN CONFIGURATION UPDATE FAILURE (cause misc(5)); Registration Reject with 5GMM cause #12 (Tracking area not allowed)", wantDetail: "the state was not judged"},
		{name: "failure, registration unanswered", o: keepsTAOutcome{update: failure, noAnswer: true}, want: Skip,
			wantObserved: "RAN CONFIGURATION UPDATE FAILURE (cause misc(5)); registration unanswered", wantDetail: "no Registration Accept or Reject within 10s"},
		{name: "no outcome", o: keepsTAOutcome{updateErr: context.DeadlineExceeded, updateTimedOut: true}, want: Error,
			wantObserved: "no response to the RAN CONFIGURATION UPDATE", wantDetail: "within 5s of the update (absence inferred within this bound)"},
		{name: "association lost", o: keepsTAOutcome{updateErr: io.EOF}, want: Error,
			wantObserved: "association lost during the RAN CONFIGURATION UPDATE", wantDetail: "EOF"},
		{name: "per-test timeout during the update", o: keepsTAOutcome{updateErr: context.DeadlineExceeded, updateCutShort: true}, want: Error,
			wantObserved: "per-test timeout expired during the RAN CONFIGURATION UPDATE", wantDetail: "raise --per-test-timeout"},
		{name: "never reached the update", o: keepsTAOutcome{err: errors.New("NG Setup rejected: misc(3)")}, want: Error,
			wantObserved: "setup failed", wantDetail: "NG Setup rejected: misc(3)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.updateAfter = 20 * time.Millisecond
			v, observed, detail := judgeKeepsTA(tc.o, g, keepsTAUpdateWait, keepsTARegisterWait)
			if v != tc.want || observed != tc.wantObserved || !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want %s %q with detail containing %q", v, observed, detail, tc.want, tc.wantObserved, tc.wantDetail)
			}
			if (v == Fail) != strings.Contains(detail, keepsTAFailText) {
				t.Errorf("detail %q: the issue's message belongs on a FAIL only", detail)
			}
		})
	}
}

// Every verdict against the mock AMF over real SCTP, scored as a suite run
// scores it. The update carries only the RAN Node Name, goes out before the
// UE's Registration Request and on the same association; a registered UE is
// deregistered; the check's associations close.
func TestKeepsTACheckAgainstMock(t *testing.T) {
	const dereg = "UE deregistered"
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantUpdates  int
		wantRegReqs  int
		wantDeregs   int64
		wantDead     bool
	}{
		{name: "ack, TA kept", want: Pass, wantObserved: "registration succeeded in TAC 1 of PLMN 208/93 after the ACKNOWLEDGE",
			wantDetail:  []string{"RAN CONFIGURATION UPDATE ACKNOWLEDGE after", `carrying only RAN Node Name "orbit-conf"`, dereg, "requires Rel-15 or later; TS 38.413 8.7.2.2"},
			wantUpdates: 1, wantRegReqs: 1, wantDeregs: 1},
		{name: "ack, TA cleared, reject #12", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(12)}, want: Fail,
			wantObserved: "Registration Reject with 5GMM cause #12 (Tracking area not allowed) in TAC 1 of PLMN 208/93 after the ACKNOWLEDGE",
			wantDetail:   []string{keepsTAFailText}, wantUpdates: 1, wantRegReqs: 1},
		{name: "ack, TA cleared, reject #15", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(15)}, want: Fail,
			wantObserved: "Registration Reject with 5GMM cause #15 (No suitable cells in tracking area) in TAC 1 of PLMN 208/93 after the ACKNOWLEDGE",
			wantDetail:   []string{keepsTAFailText}, wantUpdates: 1, wantRegReqs: 1},
		{name: "ack, reject with another cause", opts: []mockamf.Option{mockamf.WithRANConfigUpdateDroppingTAs(11)}, want: Error,
			wantObserved: "Registration Reject with 5GMM cause #11 (PLMN not allowed) after the ACKNOWLEDGE",
			wantDetail:   []string{"not tied to a tracking area"}, wantUpdates: 1, wantRegReqs: 1},
		{name: "ack, authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error,
			wantObserved: "Authentication Reject after the ACKNOWLEDGE", wantUpdates: 1, wantRegReqs: 1},
		{name: "ack, registration unanswered", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()}, want: Error,
			wantObserved: "registration unanswered after the ACKNOWLEDGE", wantDetail: []string{"within 500ms"}, wantUpdates: 1, wantRegReqs: 1},
		{name: "failure", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.FailRANConfigUpdate)}, want: Skip,
			wantObserved: "RAN CONFIGURATION UPDATE FAILURE (cause misc(5)); registration succeeded",
			wantDetail:   []string{"TS 38.413 8.7.2.3 allows", dereg}, wantUpdates: 1, wantRegReqs: 1, wantDeregs: 1},
		{name: "no response", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.IgnoreRANConfigUpdate)}, want: Error,
			wantObserved: "no response to the RAN CONFIGURATION UPDATE",
			wantDetail:   []string{"within 300ms of the update", "no registration was attempted"}, wantUpdates: 1},
		{name: "the update brings the core down", opts: []mockamf.Option{mockamf.WithRANConfigUpdate(mockamf.CrashOnRANConfigUpdate)}, want: Fail,
			wantObserved: ObservedAMFUnresponsive + " (association lost during the RAN CONFIGURATION UPDATE)",
			wantDetail:   []string{"the AMF stopped completing NG Setup after the RAN CONFIGURATION UPDATE"}, wantUpdates: 1, wantDead: true},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := auth.ParseHexKey("Ki", testKi)
			opc, _ := auth.ParseHexKey("OPc", testOPc)
			amf, err := mockamf.New(mockamf.Config{
				Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithGUTI()}, tc.opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			check := ranConfigUpdateKeepsTA{updateWait: 300 * time.Millisecond, registerWait: 500 * time.Millisecond}
			r := runOne(ctx, check, env, 15*time.Second)
			if r.Verdict != tc.want || r.Observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Observed, r.Detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}

			updates := amf.RANConfigUpdates()
			if len(updates) != tc.wantUpdates {
				t.Fatalf("mock received %d RAN CONFIGURATION UPDATEs, want %d", len(updates), tc.wantUpdates)
			}
			for _, u := range updates {
				if len(u.IEs) != 1 || !u.HasIE(ngapType.ProtocolIEIDRANNodeName) || u.RANNodeName != "orbit-conf" {
					t.Errorf("update carried IEs %v (name %q), want only the RAN Node Name \"orbit-conf\"", u.IEs, u.RANNodeName)
				}
			}
			if got := len(amf.RegistrationRequests()); got != tc.wantRegReqs {
				t.Errorf("mock received %d Registration Requests, want %d", got, tc.wantRegReqs)
			}
			// One association carried the NG Setup, the update and the
			// registration; the only other NG Setup is the liveness probe's.
			// A mock that is down reads no later NG Setup.
			wantSetups := 2
			switch {
			case tc.noUE:
				wantSetups = 0
			case tc.wantDead:
				wantSetups = 1
			}
			if setups := amf.Setups(); len(setups) != wantSetups {
				t.Errorf("mock saw %d NG Setups, want %d (the check's, and the liveness probe's from a running core)", len(setups), wantSetups)
			}
			deadline := time.Now().Add(3 * time.Second)
			for amf.Deregistrations() < tc.wantDeregs && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := amf.Deregistrations(); n != tc.wantDeregs {
				t.Errorf("mock saw %d Deregistration Requests, want %d", n, tc.wantDeregs)
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			deadline = time.Now().Add(3 * time.Second)
			for front.Active() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if front.Active() != 0 {
				t.Errorf("%d associations still open after the check", front.Active())
			}
			actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer acancel()
			if alive, err := env.Alive(actx); err != nil || alive == tc.wantDead {
				t.Errorf("liveness after the check: alive %v, err %v; want alive %v", alive, err, !tc.wantDead)
			}
		})
	}
}

// What stops the check before the update is an ERROR that opens nothing
// more: an env the harness refuses, and an AMF that rejects the NG Setup.
func TestKeepsTACheckSetupErrors(t *testing.T) {
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	amf, err := mockamf.New(mockamf.Config{Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"}, mockamf.WithGUTI())
	if err != nil {
		t.Fatal(err)
	}
	front := sctpfront.Start(t, amf)
	noN3 := ueEnv(front.Addr, "", "")
	unserved := ueEnv(front.Addr, "", "127.0.0.1")
	unserved.GNB.MCC, unserved.GNB.MNC, unserved.UE.SUPI = "001", "01", "001010000000011"
	tests := []struct {
		name         string
		env          Env
		wantObserved string
		wantDetail   string
	}{
		{name: "no gNB N3 address", env: noN3, wantObserved: "harness setup failed", wantDetail: "--gnb-n3"},
		{name: "NG Setup rejected", env: unserved, wantObserved: ObservedProbeFailed + " (setup failed)", wantDetail: "liveness probe rejected by the AMF"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			check := ranConfigUpdateKeepsTA{updateWait: 300 * time.Millisecond, registerWait: 500 * time.Millisecond}
			r := runOne(context.Background(), check, tc.env, 10*time.Second)
			if r.Verdict != Error || r.Observed != tc.wantObserved || !strings.Contains(r.Detail, tc.wantDetail) {
				t.Fatalf("= %s %q (%s), want ERROR %q with detail containing %q", r.Verdict, r.Observed, r.Detail, tc.wantObserved, tc.wantDetail)
			}
		})
	}
	if got := len(amf.RANConfigUpdates()); got != 0 {
		t.Errorf("mock received %d RAN CONFIGURATION UPDATEs from checks that stopped before the update", got)
	}
}

// The check declares its UE and a budget that covers the update, the
// registration, the deregistration and the liveness probe.
func TestKeepsTACheckNeedsAndBudget(t *testing.T) {
	c := ranConfigUpdateKeepsTA{}
	if n := NeedsOf(c); n.UEs != 1 || n.RequiresIdleCore {
		t.Errorf("needs = %+v, want one UE and no idle core", n)
	}
	if got, want := c.Budget(), 25*time.Second; got != want {
		t.Errorf("Budget = %s, want %s", got, want)
	}
	if got, want := (ranConfigUpdateKeepsTA{updateWait: time.Second, registerWait: 2 * time.Second}).Budget(), 13*time.Second; got != want {
		t.Errorf("shortened Budget = %s, want %s", got, want)
	}
}
