package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// classifyUECtxRelease passes a command and reports a missing one as a
// DEVIATION, never a FAIL, recording the cause, what else was read, and the
// cleanup either way.
func TestClassifyUECtxRelease(t *testing.T) {
	tests := []struct {
		name         string
		rel          engine.ContextReleaseResult
		derr         error
		want         Verdict
		wantObserved string
		wantDetail   []string
	}{
		{name: "command", rel: engine.ContextReleaseResult{Released: true, After: 8 * time.Millisecond, Cause: "nas(0)"}, want: Pass,
			wantObserved: "UE CONTEXT RELEASE COMMAND after 8ms",
			wantDetail: []string{"the command's cause is nas(0) (recorded only)", "UE Context Release Complete sent",
				"cleanup: the UE was deregistered with an initial NAS message, the UE having no NG connection"}},
		{name: "command after another PDU, cleanup failed", want: Pass, derr: errors.New("no 5G-GUTI"),
			rel: engine.ContextReleaseResult{Released: true, After: time.Second, Cause: "radioNetwork(10)", Skipped: []string{"DownlinkNASTransport"}},
			wantDetail: []string{"other PDUs while waiting: DownlinkNASTransport",
				"cleanup: the UE was not deregistered with an initial NAS message, the UE having no NG connection: no 5G-GUTI"}},
		{name: "no command", rel: engine.ContextReleaseResult{}, want: Deviation,
			wantObserved: "no UE CONTEXT RELEASE COMMAND within 5s (absence inferred within this bound)",
			wantDetail:   []string{ueCtxReleaseFailure, "cleanup: the UE was deregistered over its NG connection"}},
		{name: "no command, an Error Indication instead", rel: engine.ContextReleaseResult{Skipped: []string{"ErrorIndication"}}, want: Deviation,
			derr:       errors.New("write failed"),
			wantDetail: []string{ueCtxReleaseFailure, "other PDUs while waiting: ErrorIndication", "cleanup: the UE was not deregistered over its NG connection: write failed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, observed, detail := classifyUECtxRelease(tc.rel, 5*time.Second, tc.derr)
			if v := covers(o).Verdict; v != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", v, observed, detail, tc.want)
			}
			if tc.wantObserved != "" && observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", observed, tc.wantObserved)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// The message for a missing command is the issue's, word for word.
func TestUECtxReleaseFailureMessage(t *testing.T) {
	const want = "no UE CONTEXT RELEASE COMMAND after UE CONTEXT RELEASE REQUEST. TS 38.413 8.3.2.2 (should; Rel-18 V18.1.0: split by cause). Likely cause: AMF ignores RAN-initiated release."
	if ueCtxReleaseFailure != want {
		t.Errorf("failure message = %q", ueCtxReleaseFailure)
	}
}

// The check cites a should and no shall, so its worst verdict is DEVIATION.
// Without a UE it SKIPs; an env the harness refuses is an ERROR before
// anything is dialled.
func TestUECtxReleaseCheckWithoutHarness(t *testing.T) {
	if got := WorstVerdict(ngapUECtxReleaseOnRequest{}); got != Deviation {
		t.Errorf("WorstVerdict = %s, want DEVIATION", got)
	}
	if got := OwnsOf(ngapUECtxReleaseOnRequest{}); len(got) != 1 || got[0] != ProcANRelease || ueCtxReleaseNoCommand.NotCompleted != ProcANRelease {
		t.Errorf("owns %v, want the AN release, reported as not completed by the missing command", got)
	}
	r := runOne(context.Background(), ngapUECtxReleaseOnRequest{}, Env{}, time.Second)
	if r.Verdict != Skip || r.Observed != "no UE configured" || !strings.Contains(r.Detail, "--supi") {
		t.Errorf("without a UE: %s / %q / %q, want SKIP", r.Verdict, r.Observed, r.Detail)
	}
	fixed := ueEnv("192.0.2.1:38412", "127.0.0.1:38999", "127.0.0.1")
	if r := runOne(context.Background(), ngapUECtxReleaseOnRequest{}, fixed, time.Second); r.Verdict != Error || r.Observed != "harness setup failed" {
		t.Errorf("with a fixed bind port: %s / %q, want ERROR", r.Verdict, r.Observed)
	}
}

// The check's verdicts over the real-SCTP mock, through the registry's
// scoring: the command PASSes and the UE is then deregistered with an
// initial NAS message; a withheld command is a DEVIATION and the UE
// deregisters over its NG connection. A core that assigned no 5G-GUTI
// leaves the released UE nothing to deregister with, which the detail
// says. No association stays open.
func TestUECtxReleaseCheckAgainstMock(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		opts         []mockamf.Option
		want         Verdict
		wantObserved string
		wantDetail   []string
		deregs, idle int64
		dead         bool
	}{
		{name: "command", net24: testnet.ConformanceRelOnReq, opts: []mockamf.Option{mockamf.WithGUTI()}, want: Pass, deregs: 1, idle: 1,
			wantDetail: []string{"the command's cause is nas(", "cleanup: the UE was deregistered with an initial NAS message"}},
		{name: "command withheld", net24: testnet.ConformanceRelIgnored, opts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithIgnoreUEContextReleaseRequest()},
			want: Deviation, deregs: 1, wantObserved: "no UE CONTEXT RELEASE COMMAND within 400ms (absence inferred within this bound)",
			wantDetail: []string{ueCtxReleaseFailure, "cleanup: the UE was deregistered over its NG connection", "does not follow TS 38.413 8.3.2.2 (should)"}},
		{name: "core crashes at the request", net24: testnet.ConformanceRelOnReqCrash, opts: []mockamf.Option{mockamf.WithGUTI(), mockamf.WithCrashOnUEContextReleaseRequest()},
			want: Crash, dead: true, wantObserved: ObservedAMFUnresponsive + " (association lost waiting for the UE CONTEXT RELEASE COMMAND)",
			wantDetail: []string{ueCtxReleaseCrashed}},
		{name: "command, no 5G-GUTI", net24: testnet.ConformanceRelNoGUTI, want: Pass,
			wantDetail: []string{"cleanup: the UE was not deregistered with an initial NAS message", "no 5G-GUTI"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMockCore(t, tc.net24, mockupf.Options{}, testKi, tc.opts...)
			env := ueEnv(c.front.Addr, "", c.n3)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r := runOne(ctx, ngapUECtxReleaseOnRequest{wait: 400 * time.Millisecond}, env, 15*time.Second)
			if r.Verdict != tc.want {
				t.Fatalf("verdict %s (%s; %s), want %s", r.Verdict, r.Observed, r.Detail, tc.want)
			}
			if tc.wantObserved != "" && r.Observed != tc.wantObserved {
				t.Errorf("observed %q, want %q", r.Observed, tc.wantObserved)
			}
			if tc.want == Pass && !strings.HasPrefix(r.Observed, "UE CONTEXT RELEASE COMMAND after ") {
				t.Errorf("observed %q, want the command", r.Observed)
			}
			for _, w := range tc.wantDetail {
				if !strings.Contains(r.Detail, w) {
					t.Errorf("detail %q lacks %q", r.Detail, w)
				}
			}
			if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
				t.Error("detail carries key material")
			}
			if got := c.amf.ReleaseRequests(); got != 1 {
				t.Errorf("mock AMF saw %d UE Context Release Requests, want 1", got)
			}
			if tc.dead {
				waitNoAssociations(t, c)
				return
			}
			if got, idle := c.amf.Deregistrations(), c.amf.IdleDeregistrations(); got != tc.deregs || idle != tc.idle {
				t.Errorf("mock AMF saw %d deregistrations, %d in an Initial UE Message; want %d and %d", got, idle, tc.deregs, tc.idle)
			}
			if n := c.amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
			c.assertClosed(t, env)
		})
	}
}

// A registration the core refuses is BLOCKED on the registration, naming
// the step, with no association left open.
func TestUECtxReleaseCheckRegistrationFailure(t *testing.T) {
	c := newMockCore(t, testnet.ConformanceRelRegFail, mockupf.Options{}, "000102030405060708090a0b0c0d0e0f")
	env := ueEnv(c.front.Addr, "", c.n3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := runOne(ctx, ngapUECtxReleaseOnRequest{}, env, 15*time.Second)
	if r.Verdict != Blocked || r.BlockedOn != ProcRegistration || r.Observed != "UE registration failed" || !strings.Contains(r.Detail, "registration on gNB 0x100") {
		t.Errorf("result %s on %q / %q / %q, want BLOCKED on the registration, naming the step", r.Verdict, r.BlockedOn, r.Observed, r.Detail)
	}
	if strings.Contains(r.Detail, testKi) || strings.Contains(r.Detail, testOPc) {
		t.Error("detail carries key material")
	}
	c.assertClosed(t, env)
}

// An error from the release request: the per-test timeout, a request that
// could not be sent and a command whose complete could not be sent are
// ERROR, each saying which; the association lost while waiting for the
// command is the core's, DEVIATION.
func TestUECtxReleaseRequestFailed(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		ctxErr  error
		want    Verdict
		outcome string
		// observed, when set, is the result's Observed.
		observed string
	}{
		{name: "per-test timeout", err: fmt.Errorf("await UEContextReleaseCommand: %w", context.DeadlineExceeded), ctxErr: context.DeadlineExceeded, want: Error, outcome: OutcomeError},
		{name: "association lost", err: fmt.Errorf("%w: await UEContextReleaseCommand: EOF", engine.ErrReleaseWaitLost), want: Deviation, outcome: covers(ueCtxReleaseAssociationLost).ID},
		{name: "not sent", err: errors.New("send UEContextReleaseRequest: broken pipe"), want: Error, outcome: OutcomeError,
			observed: "UE CONTEXT RELEASE REQUEST could not be sent"},
		{name: "the complete not sent", err: fmt.Errorf("%w: broken pipe", engine.ErrReleaseCompleteNotSent), want: Error, outcome: OutcomeError,
			observed: "UE CONTEXT RELEASE COMPLETE could not be sent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := releaseRequestFailed(Result{}, tc.err, tc.ctxErr)
			if r.Verdict != tc.want || r.Outcome != tc.outcome || !strings.Contains(r.Detail, tc.err.Error()) {
				t.Errorf("= %s %q (%s), want %s %q", r.Verdict, r.Outcome, r.Detail, tc.want, tc.outcome)
			}
			if tc.observed != "" && r.Observed != tc.observed {
				t.Errorf("observed %q, want %q", r.Observed, tc.observed)
			}
		})
	}
}
