package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// Every verdict branch of the Deregistration Accept check from a
// constructed outcome.
func TestJudgeDeregAccept(t *testing.T) {
	t0 := time.Unix(1000, 0)
	rel := &engine.UEContextRelease{At: t0.Add(40 * time.Millisecond), HasAMFUENGAPID: true, HasRANUENGAPID: true, AMFUENGAPID: 1, RANUENGAPID: 1}
	noRel := errors.New("the core sent no UE Context Release within 5s")
	tests := []struct {
		name         string
		res          *engine.DeregisterResult
		err          error
		cut          bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		notDetail    []string
	}{
		{name: "accept, then release", res: &engine.DeregisterResult{SentAt: t0, Accepted: true, AcceptAt: t0.Add(12 * time.Millisecond), Release: rel},
			want: Pass, wantObserved: "Deregistration Accept received", wantDetail: []string{"12ms after the request", "recorded, not judged", "the core released the UE context"}},
		{name: "accept, no release", res: &engine.DeregisterResult{SentAt: t0, Accepted: true, AcceptAt: t0.Add(time.Second), ReleaseErr: noRel},
			want: Pass, wantObserved: "Deregistration Accept received", wantDetail: []string{"1s after the request", "the core sent no UE Context Release within 5s", "closed without it"}},
		{name: "accept as the time runs out", res: &engine.DeregisterResult{SentAt: t0, Accepted: true, AcceptAt: t0.Add(time.Second), ReleaseErr: noRel}, cut: true,
			want: Pass, wantObserved: "Deregistration Accept received"},
		{name: "release instead of the accept", res: &engine.DeregisterResult{SentAt: t0, AcceptErr: errors.New("x"), Release: rel},
			want: Fail, wantObserved: "UE Context Release Command without a Deregistration Accept",
			wantDetail: []string{deregAcceptFail, "the release came 40ms after the request"}, notDetail: []string{"switch-off Deregistration Request followed"}},
		{name: "nothing, then released on the switch-off", res: &engine.DeregisterResult{SentAt: t0, AcceptErr: errors.New("x"), Release: rel, SwitchOffFallback: true},
			want: Fail, wantObserved: "no Deregistration Accept within 15s",
			wantDetail: []string{deregAcceptFail, "a switch-off Deregistration Request followed: the core released the UE context"}},
		{name: "nothing at all", res: &engine.DeregisterResult{SentAt: t0, AcceptErr: errors.New("x"), ReleaseErr: noRel, SwitchOffFallback: true},
			want: Fail, wantObserved: "no Deregistration Accept within 15s",
			wantDetail: []string{deregAcceptFail, "a switch-off Deregistration Request followed: the core sent no UE Context Release within 5s"}},
		{name: "cut short", res: &engine.DeregisterResult{SentAt: t0, AcceptErr: errors.New("x"), ReleaseErr: noRel}, cut: true,
			want: Error, wantObserved: "timed out before the Deregistration Accept wait ended", wantDetail: []string{"raise --per-test-timeout to at least 40s"}},
		{name: "not sent", err: errors.New("no 5G-GUTI (AMF assigned none in Registration Accept)"),
			want: Error, wantObserved: "Deregistration Request not sent", wantDetail: []string{"no 5G-GUTI", "still registered"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeDeregAccept(tc.res, tc.err, tc.cut, nasDeregAcceptNotSwitchOff{})
			if v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
			for _, w := range tc.notDetail {
				if strings.Contains(detail, w) {
					t.Errorf("detail %q has %q", detail, w)
				}
			}
		})
	}
}

// The FAIL message is the one the check's issue fixes, with the clause the
// archive reading pinned.
func TestDeregAcceptFailMessage(t *testing.T) {
	const want = "no Deregistration Accept for a non-switch-off deregistration. TS 24.501 5.5.2.2.2. Likely cause: AMF treats every deregistration as switch-off."
	if deregAcceptFail != want {
		t.Errorf("FAIL message %q, want %q", deregAcceptFail, want)
	}
}

// deregMock is a mock AMF behind a real SCTP listener for the
// deregistration checks, assigning a 5G-GUTI unless noGUTI.
func deregMock(t *testing.T, noGUTI bool, opts ...mockamf.Option) (*mockamf.AMF, *sctpfront.Front) {
	t.Helper()
	k, _ := auth.ParseHexKey("Ki", testKi)
	opc, _ := auth.ParseHexKey("OPc", testOPc)
	if !noGUTI {
		opts = append([]mockamf.Option{mockamf.WithGUTI()}, opts...)
	}
	amf, err := mockamf.New(mockamf.Config{
		Ki: k, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return amf, sctpfront.Start(t, amf)
}

// assertNoAssociations waits for every association of front to close.
func assertNoAssociations(t *testing.T, front *sctpfront.Front) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for front.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if front.Active() != 0 {
		t.Errorf("%d associations still open after the check", front.Active())
	}
}

// Every branch against the mock AMF over a real SCTP listener: the Accept
// (with and without the release after it), the Accept withheld (a release
// in its place, or nothing, which draws the switch-off), a UE the mock gave
// no 5G-GUTI, a registration that fails, and no UE. No association stays
// open, and the mock sees the Deregistration Requests the branch sends.
func TestDeregAcceptCheckAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noGUTI       bool
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantDeregs   int64
		wantAccepts  int64
		wantComplete int64
	}{
		{name: "accept, then release", want: Pass, wantObserved: "Deregistration Accept received",
			wantDetail: []string{"the core released the UE context"}, wantDeregs: 1, wantAccepts: 1, wantComplete: 1},
		{name: "accept, no release", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()}, want: Pass,
			wantObserved: "Deregistration Accept received", wantDetail: []string{"the core sent no UE Context Release within 200ms"}, wantDeregs: 1, wantAccepts: 1},
		{name: "accept withheld: a release instead", opts: []mockamf.Option{mockamf.WithNoDeregistrationAccept()}, want: Fail,
			wantObserved: "UE Context Release Command without a Deregistration Accept", wantDetail: []string{deregAcceptFail}, wantDeregs: 1, wantComplete: 1},
		{name: "accept withheld: nothing", opts: []mockamf.Option{mockamf.WithNoDeregistrationAccept(), mockamf.WithNoReleaseOnDeregistration()}, want: Fail,
			wantObserved: "no Deregistration Accept within 300ms",
			wantDetail:   []string{deregAcceptFail, "a switch-off Deregistration Request followed: the core sent no UE Context Release within 200ms"}, wantDeregs: 2},
		{name: "no 5G-GUTI", noGUTI: true, want: Error, wantObserved: "Deregistration Request not sent", wantDetail: []string{"no 5G-GUTI"}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error, wantObserved: "registration failed"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := deregMock(t, tc.noGUTI, tc.opts...)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := nasDeregAcceptNotSwitchOff{registerWait: 3 * time.Second, acceptWait: 300 * time.Millisecond, releaseWait: 200 * time.Millisecond}
			r := runOne(ctx, c, env, 15*time.Second)
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
			if got := amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("the mock got %d Deregistration Requests, want %d", got, tc.wantDeregs)
			}
			if got := amf.DeregistrationAccepts(); got != tc.wantAccepts {
				t.Errorf("the mock sent %d Deregistration Accepts, want %d", got, tc.wantAccepts)
			}
			if got := amf.ReleaseCompletes(); got != tc.wantComplete {
				t.Errorf("the mock got %d UE Context Release Completes, want %d", got, tc.wantComplete)
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			assertNoAssociations(t, front)
		})
	}
}

// The check's time running out during the Accept wait is the harness's
// ERROR, not the core's FAIL.
func TestDeregAcceptCheckCutShort(t *testing.T) {
	_, front := deregMock(t, false, mockamf.WithNoDeregistrationAccept(), mockamf.WithNoReleaseOnDeregistration())
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	c := nasDeregAcceptNotSwitchOff{registerWait: time.Second, acceptWait: time.Minute, releaseWait: 200 * time.Millisecond}
	r := c.Run(ctx, ueEnv(front.Addr, "", "127.0.0.1"))
	if r.Verdict != Error || r.Observed != "timed out before the Deregistration Accept wait ended" {
		t.Errorf("= %s %q (%s), want ERROR for the timeout", r.Verdict, r.Observed, r.Detail)
	}
	assertNoAssociations(t, front)
}

// The registry stamps the check's identity and references on the result.
func TestDeregAcceptCheckIdentity(t *testing.T) {
	c := testByID(t, "NAS-DEREG-ACCEPT-NOT-SWITCH-OFF")
	if c.Category() != Procedural || WorstVerdict(c) != Fail || MinRelease(c) != 15 {
		t.Errorf("category %s, worst verdict %s, min release %s", c.Category(), WorstVerdict(c), MinRelease(c))
	}
	if n := NeedsOf(c); n.UEs != 1 || n.RequiresIdleCore {
		t.Errorf("needs %+v, want one UE and no idle core", n)
	}
	if b, ok := c.(Budgeter); !ok || b.Budget() != 40*time.Second {
		t.Errorf("budget %v, want 40s", c.(Budgeter).Budget())
	}
}

// Every verdict branch of the switch-off release check from a constructed
// outcome.
func TestJudgeSwitchOffRelease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := t0.Add(25 * time.Millisecond)
	noRel := errors.New("the core sent no UE Context Release within 5s")
	result := func(rel *engine.UEContextRelease, relErr error) *engine.DeregisterResult {
		return &engine.DeregisterResult{SwitchOff: true, SentAt: t0, AMFUENGAPID: 7, RANUENGAPID: 9, Release: rel, ReleaseErr: relErr}
	}
	tests := []struct {
		name         string
		res          *engine.DeregisterResult
		err          error
		cut          bool
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "the pair", res: result(&engine.UEContextRelease{At: at, HasAMFUENGAPID: true, HasRANUENGAPID: true, AMFUENGAPID: 7, RANUENGAPID: 9}, nil),
			want: Pass, wantObserved: "UE CONTEXT RELEASE COMMAND naming both UE NGAP IDs", wantDetail: []string{"25ms after the request", "AMF UE NGAP ID 7 and RAN UE NGAP ID 9", "Complete sent"}},
		{name: "the pair as the time runs out", res: result(&engine.UEContextRelease{At: at, HasAMFUENGAPID: true, HasRANUENGAPID: true, AMFUENGAPID: 7, RANUENGAPID: 9}, nil),
			cut: true, want: Pass, wantObserved: "UE CONTEXT RELEASE COMMAND naming both UE NGAP IDs"},
		{name: "the AMF ID alone", res: result(&engine.UEContextRelease{At: at, HasAMFUENGAPID: true, AMFUENGAPID: 7}, nil),
			want: Fail, wantObserved: "UE CONTEXT RELEASE COMMAND without the RAN UE NGAP ID",
			wantDetail: []string{"UE CONTEXT RELEASE COMMAND after switch-off deregistration names the UE by AMF UE NGAP ID 7 alone, with both UE NGAP IDs available. TS 38.413 8.3.3.2. Likely cause: AMF builds the release without the UE NGAP ID pair.", "the connection is AMF UE NGAP ID 7, RAN UE NGAP ID 9"}},
		{name: "no IDs", res: result(&engine.UEContextRelease{At: at}, nil),
			want: Fail, wantObserved: "UE CONTEXT RELEASE COMMAND without UE NGAP IDs", wantDetail: []string{"carries no UE NGAP ID, with both UE NGAP IDs available. TS 38.413 8.3.3.2."}},
		{name: "another pair", res: result(&engine.UEContextRelease{At: at, HasAMFUENGAPID: true, HasRANUENGAPID: true, AMFUENGAPID: 1007, RANUENGAPID: 1009}, nil),
			want: Skip, wantObserved: "a UE CONTEXT RELEASE COMMAND for another UE NGAP ID; not scored",
			wantDetail: []string{"names AMF UE NGAP ID 1007, RAN UE NGAP ID 1009", "the connection is AMF UE NGAP ID 7, RAN UE NGAP ID 9", "recorded, not failed"}},
		{name: "the pair with another RAN ID", res: result(&engine.UEContextRelease{At: at, HasAMFUENGAPID: true, HasRANUENGAPID: true, AMFUENGAPID: 7, RANUENGAPID: 10}, nil),
			want: Skip, wantObserved: "a UE CONTEXT RELEASE COMMAND for another UE NGAP ID; not scored"},
		{name: "another AMF ID alone", res: result(&engine.UEContextRelease{At: at, HasAMFUENGAPID: true, AMFUENGAPID: 8}, nil),
			want: Skip, wantObserved: "a UE CONTEXT RELEASE COMMAND for another UE NGAP ID; not scored", wantDetail: []string{"names AMF UE NGAP ID 8;"}},
		{name: "no release", res: result(nil, noRel), want: Skip, wantObserved: "no UE CONTEXT RELEASE COMMAND within 5s; not scored",
			wantDetail: []string{switchOffNoRelease, "the core sent no UE Context Release within 5s"}},
		{name: "cut short", res: result(nil, noRel), cut: true, want: Error, wantObserved: "timed out before the release wait ended",
			wantDetail: []string{"raise --per-test-timeout to at least 20s"}},
		{name: "not sent", err: errors.New("no 5G-GUTI (AMF assigned none in Registration Accept)"),
			want: Error, wantObserved: "Deregistration Request not sent", wantDetail: []string{"no 5G-GUTI"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, observed, detail := judgeSwitchOffRelease(tc.res, tc.err, tc.cut, ngapCtxReleaseAfterSwitchOff{})
			if v != tc.want || observed != tc.wantObserved {
				t.Errorf("= %s %q (%s), want %s %q", v, observed, detail, tc.want, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// Every branch against the mock AMF over a real SCTP listener: the release
// naming the pair, the AMF UE NGAP ID alone or another pair, the release
// withheld, a UE the mock gave no 5G-GUTI, a registration that fails, and
// no UE. The stimulus is one switch-off Deregistration Request, which
// draws no Deregistration Accept, and no association stays open.
func TestSwitchOffReleaseCheckAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		noGUTI       bool
		noUE         bool
		want         Verdict
		wantObserved string
		wantDetail   []string
		wantDeregs   int64
		wantComplete int64
		wait         time.Duration // the release wait; zero is 300ms
	}{
		{name: "released", want: Pass, wantObserved: "UE CONTEXT RELEASE COMMAND naming both UE NGAP IDs", wantDetail: []string{"the connection's pair"}, wantDeregs: 1, wantComplete: 1},
		{name: "released late, after other PDUs", opts: []mockamf.Option{mockamf.WithNoiseBeforeRelease(), mockamf.WithReleaseDelay(100 * time.Millisecond)}, wait: 3 * time.Second,
			want: Pass, wantObserved: "UE CONTEXT RELEASE COMMAND naming both UE NGAP IDs", wantDeregs: 1, wantComplete: 1},
		{name: "released by the AMF ID alone", opts: []mockamf.Option{mockamf.WithDeregistrationReleaseIDs(mockamf.ReleaseAMFIDOnly)}, want: Fail,
			wantObserved: "UE CONTEXT RELEASE COMMAND without the RAN UE NGAP ID", wantDetail: []string{"alone, with both UE NGAP IDs available. TS 38.413 8.3.3.2."}, wantDeregs: 1, wantComplete: 1},
		{name: "a release for another pair", opts: []mockamf.Option{mockamf.WithDeregistrationReleaseIDs(mockamf.ReleaseOtherPair)}, want: Skip,
			wantObserved: "a UE CONTEXT RELEASE COMMAND for another UE NGAP ID; not scored", wantDeregs: 1, wantComplete: 1},
		{name: "release withheld", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()}, want: Skip,
			wantObserved: "no UE CONTEXT RELEASE COMMAND within 300ms; not scored", wantDetail: []string{switchOffNoRelease}, wantDeregs: 1},
		{name: "no 5G-GUTI", noGUTI: true, want: Error, wantObserved: "Deregistration Request not sent", wantDetail: []string{"no 5G-GUTI"}},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()}, want: Error, wantObserved: "registration failed"},
		{name: "no UE", noUE: true, want: Skip, wantObserved: "no UE configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, front := deregMock(t, tc.noGUTI, tc.opts...)
			env := ueEnv(front.Addr, "", "127.0.0.1")
			if tc.noUE {
				env.UE = UESpec{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			wait := tc.wait
			if wait == 0 {
				wait = 300 * time.Millisecond
			}
			r := runOne(ctx, ngapCtxReleaseAfterSwitchOff{registerWait: 3 * time.Second, releaseWait: wait}, env, 15*time.Second)
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
			if got := amf.Deregistrations(); got != tc.wantDeregs {
				t.Errorf("the mock got %d Deregistration Requests, want %d", got, tc.wantDeregs)
			}
			if got := amf.DeregistrationAccepts(); got != 0 {
				t.Errorf("the mock sent %d Deregistration Accepts to a switch-off", got)
			}
			if got := amf.ReleaseCompletes(); got != tc.wantComplete {
				t.Errorf("the mock got %d UE Context Release Completes, want %d", got, tc.wantComplete)
			}
			if amf.StrayReleaseCompletes() != 0 {
				t.Errorf("%d UE Context Release Completes came before the release they answer", amf.StrayReleaseCompletes())
			}
			if amf.EncodeErrors() != 0 {
				t.Errorf("mock dropped %d downlink PDUs that did not encode", amf.EncodeErrors())
			}
			assertNoAssociations(t, front)
		})
	}
}

// The check's time running out during the release wait is the harness's
// ERROR.
func TestSwitchOffReleaseCheckCutShort(t *testing.T) {
	_, front := deregMock(t, false, mockamf.WithNoReleaseOnDeregistration())
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	r := ngapCtxReleaseAfterSwitchOff{registerWait: time.Second, releaseWait: time.Minute}.Run(ctx, ueEnv(front.Addr, "", "127.0.0.1"))
	if r.Verdict != Error || r.Observed != "timed out before the release wait ended" {
		t.Errorf("= %s %q (%s), want ERROR for the timeout", r.Verdict, r.Observed, r.Detail)
	}
	assertNoAssociations(t, front)
}

// The registry stamps the check's identity: FAIL-class through the UE NGAP
// ID shall, with the release's trigger cited as descriptive.
func TestSwitchOffReleaseCheckIdentity(t *testing.T) {
	c := testByID(t, "NGAP-CTX-RELEASE-AFTER-SWITCH-OFF")
	if c.Category() != Procedural || WorstVerdict(c) != Fail || MinRelease(c) != 15 {
		t.Errorf("category %s, worst verdict %s, min release %s", c.Category(), WorstVerdict(c), MinRelease(c))
	}
	var shall []string
	for _, r := range c.SpecRefs() {
		if r.Strength == Shall {
			shall = append(shall, r.Spec+" "+r.Clause)
		}
	}
	if len(shall) != 1 || shall[0] != "TS 38.413 8.3.3.2" {
		t.Errorf("shall references %v, want TS 38.413 8.3.3.2 alone", shall)
	}
	if n := NeedsOf(c); n.UEs != 1 || n.RequiresIdleCore {
		t.Errorf("needs %+v, want one UE and no idle core", n)
	}
	if b, ok := c.(Budgeter); !ok || b.Budget() != 20*time.Second {
		t.Errorf("budget %v, want 20s", c.(Budgeter).Budget())
	}
}
