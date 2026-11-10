package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
)

func init() {
	builtins = append(builtins, nasRetransmitNewCount{})
}

// retransmitCountFailure is the check's failure message.
const retransmitCountFailure = "AMF retransmitted a protected NAS message without increasing the NAS COUNT. TS 24.501 4.4.3.1. Likely cause: the retransmission resends the stored PDU instead of re-protecting it."

// nasRetransmitNewCount uses the stimulus of NAS-T3550-ACCEPT-RETRANSMIT:
// the UE withholds its Registration Complete, and the AMF retransmits the
// Registration Accept on each T3550 expiry. For the first Accept and each
// retransmission it takes the downlink NAS COUNT the PDU's MAC verifies at
// (engine.NASCount: the PDU's sequence number with the stored overflow
// counter, or with the next one, so a wrap of the sequence number counts as
// an increase). TS 24.501 4.4.3.1 has the sender increase the NAS COUNT by
// one "after each new or retransmitted outbound" protected message, so a
// retransmission whose COUNT is not above the transmission before it is
// FAIL. One that verifies at neither candidate is ERROR. How many
// retransmissions come, and when, is NAS-T3550-ACCEPT-RETRANSMIT's to
// grade: with none, this check SKIPs. It stops at the fourth
// retransmission, or at the first one that decides the verdict.
type nasRetransmitNewCount struct {
	// t3550, tmax and acceptWait override the nominal timer, the bound on
	// the first retransmission and the wait for the first Registration
	// Accept (tests only), as for nasT3550AcceptRetransmit.
	t3550, tmax, acceptWait time.Duration
}

func (nasRetransmitNewCount) ID() string         { return "NAS-RETRANSMIT-NEW-COUNT" }
func (nasRetransmitNewCount) Category() Category { return Security }
func (nasRetransmitNewCount) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "4.4.3.1", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 reads \"security protected NAS message\"; from V15.1.0 \"SECURITY PROTECTED 5GS NAS MESSAGE message\"; the increase by one after each new or retransmitted message is in every version"},
				{Release: 16, Note: "from V16.5.1 the overflow counter is \"the stored NAS overflow counter\", and estimation rules for the receiver follow the sentence"},
				{Release: 18, Note: "\"subclause\" becomes \"clause\""},
			}},
		{Spec: "TS 24.501", Clause: "5.5.1.2.8 c)", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 has the AMF enter 5GMM-DEREGISTERED on the abort, 5GMM-REGISTERED from V15.7.0; the retransmission wording is unchanged through Rel-19"},
			}},
	}
}

// timers is the T3550 check with this check's overrides, for its defaults.
func (c nasRetransmitNewCount) timers() nasT3550AcceptRetransmit {
	return nasT3550AcceptRetransmit(c)
}

// Budget is the wait for the first Registration Accept plus four waits of
// 1.5 × t3550Max, one per retransmission watched: 118 s. A 6 s core ends in
// about 25 s.
func (c nasRetransmitNewCount) Budget() time.Duration {
	t := c.timers()
	return t.waitAccept() + 4*(t.maxTimer()+t.maxTimer()/2)
}

// acceptTx is one transmission of the Registration Accept.
type acceptTx struct {
	// count is the NAS COUNT its MAC verifies at; verified is false when it
	// verifies at neither of candidates.
	count      uint32
	verified   bool
	candidates [2]uint32
	raw        []byte
}

// acceptCounts collects the Registration Accept's transmissions from
// AttachProbe.OnNAS and paces the wait through a retransmitObservation.
type acceptCounts struct {
	obs *retransmitObservation

	mu sync.Mutex
	tx []acceptTx
}

// note records a downlink NAS PDU. A protected PDU that deciphers to a
// Registration Accept at the COUNT its MAC verifies at is a transmission.
// So is, after the first Accept, a protected PDU that verifies at neither
// candidate: nothing else is due then, and it cannot be read. A
// retransmission that decides the verdict ends the attach.
func (a *acceptCounts) note(ev engine.NASEvent) {
	if ev.Count == nil {
		a.obs.noteNAS(ev) // a plain message: a reject is recorded
		return
	}
	a.mu.Lock()
	count, mt, ok := ev.Count.Verifies()
	seen := len(a.tx)
	if (ok && mt != f5nas.MsgTypeRegistrationAccept) || (!ok && seen == 0) {
		a.mu.Unlock()
		a.obs.noteNAS(ev)
		return
	}
	tx := acceptTx{count: count, verified: ok, candidates: ev.Count.Candidates, raw: ev.Raw}
	decided := seen > 0 && (!ok || count <= a.tx[seen-1].count)
	a.tx = append(a.tx, tx)
	a.mu.Unlock()
	// The pacing sees every transmission as a Registration Accept, whatever
	// the security context made of it.
	a.obs.noteNAS(engine.NASEvent{At: ev.At, Type: f5nas.MsgTypeRegistrationAccept})
	if decided {
		a.obs.cancel()
	}
}

func (a *acceptCounts) transmissions() []acceptTx {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]acceptTx(nil), a.tx...)
}

func (c nasRetransmitNewCount) Run(ctx context.Context, env Env) Result {
	t := c.timers()
	r := Result{Expected: "with the Registration Complete withheld, each retransmission of the Registration Accept is protected at a NAS COUNT higher than the transmission before it (TS 24.501 4.4.3.1; the retransmissions are those of 5.5.1.2.8 c))"}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run the UE security checks"
		return r
	}
	h, err := NewUEHarness(env, nil)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "harness setup failed", err.Error()
		return r
	}
	defer h.Close()

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The fourth retransmission ends the observation: one more than
	// retransmissions cancels at once.
	o := &retransmitObservation{cancel: cancel, msgType: f5nas.MsgTypeRegistrationAccept,
		nominal: t.timer(), tmax: t.maxTimer(), retransmissions: t3550Retransmissions - 1}
	counts := &acceptCounts{obs: o}
	noAccept := time.AfterFunc(t.waitAccept(), func() {
		if !o.seen() {
			cancel()
		}
	})
	var smc []byte
	_, err = h.RegisterWithProbe(actx, &engine.AttachProbe{
		Withhold: map[uint8]bool{f5nas.MsgTypeRegistrationComplete: true},
		OnDownlink: func(ev engine.DownlinkEvent) {
			if ev.NASType == f5nas.MsgTypeSecurityModeCommand && smc == nil {
				smc = append([]byte(nil), ev.NAS...)
			}
			o.noteRelease(ev)
		},
		OnNAS: counts.note,
	})
	noAccept.Stop()
	o.stop()
	snap := o.snapshot()
	switch {
	case err == nil:
		r.Verdict, r.Observed = Error, "registration completed with the Registration Complete withheld"
		r.Detail = "the harness did not withhold the reply"
		return r
	case errors.Is(err, engine.ErrUnsupportedAlgorithms):
		if f, perr := parseSMC(smc); perr == nil && f.integrity == nas.NIA0 {
			r.Verdict, r.Observed = Skip, "the core selected 5G-IA0; the UE harness refuses it, so no protected Registration Accept was seen"
			r.Detail = "the UE harness never registers for emergency and refuses 5G-IA0 at the security mode control procedure; the NAS COUNT of a retransmission cannot be read without integrity protection"
			return r
		}
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed before the Registration Accept", err.Error()
		return r
	case errors.Is(err, engine.ErrReleasedByCore), errors.Is(err, context.Canceled) && ctx.Err() == nil:
		// A release, the window's end or a decided verdict: judged below.
	case ctx.Err() != nil:
		r.Verdict, r.Observed = Error, "timed out before the retransmissions were read"
		r.Detail = fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget())
		return r
	case snap.reject != "":
		// A reject: ERROR, judged below.
	default:
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed before the Registration Accept", err.Error()
		return r
	}
	r.Verdict, r.Observed, r.Detail = judgeRetransmitCounts(counts.transmissions(), snap.reject, t.waitAccept())
	if r.Verdict == Skip || (r.Verdict == Error && ctx.Err() != nil) {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the Registration Accept retransmissions")
}

// judgeRetransmitCounts turns the Registration Accept's transmissions into
// a verdict, Observed and Detail. ERROR when registration never reached a
// verified Registration Accept, a reject ended it, or a retransmission
// verifies at neither candidate COUNT; SKIP with no retransmission; FAIL
// for the first retransmission whose COUNT is not above the transmission
// before it; else PASS. Observed lists each transmission's COUNT.
func judgeRetransmitCounts(tx []acceptTx, reject string, acceptWait time.Duration) (Verdict, string, string) {
	switch {
	case reject != "":
		return Error, reject + " before the Registration Accept",
			fmt.Sprintf("the AMF sent %s; this check needs a subscriber the core registers", reject)
	case len(tx) == 0:
		return Error, "registration did not reach a Registration Accept that verifies",
			fmt.Sprintf("no Registration Accept whose MAC verifies within %s of the Initial UE Message", acceptWait)
	case len(tx) == 1:
		return Skip, "no retransmission of Registration Accept within the bound",
			fmt.Sprintf("the Registration Accept came once, at NAS COUNT %d; whether and when the AMF retransmits it is graded by NAS-T3550-ACCEPT-RETRANSMIT (an Accept without a 5G-GUTI starts no T3550)", tx[0].count)
	}
	counts := []string{fmt.Sprint(tx[0].count)}
	for i := 1; i < len(tx); i++ {
		cur, prev := tx[i], tx[i-1]
		if !cur.verified {
			return Error, fmt.Sprintf("retransmission %d of Registration Accept verifies at no candidate NAS COUNT", i),
				fmt.Sprintf("after transmissions at NAS COUNT %s, a protected downlink PDU fails the MAC check at both candidate COUNTs, %d and %d (its sequence number with the stored overflow counter, and with the next one); they cover every COUNT from 255 below the stored one to 256 above it, so the search stops there. NAS %x",
					strings.Join(counts, ", "), cur.candidates[0], cur.candidates[1], cur.raw)
		}
		counts = append(counts, fmt.Sprint(cur.count))
		if cur.count > prev.count {
			continue
		}
		how := "lower than"
		if cur.count == prev.count {
			how = "the same as"
		}
		detail := retransmitCountFailure
		if bytes.Equal(cur.raw, prev.raw) {
			detail += " The retransmission is byte-identical to the transmission before it."
		}
		return Fail, fmt.Sprintf("retransmission %d of Registration Accept at NAS COUNT %d, %s the transmission before it (COUNTs %s)", i, cur.count, how, strings.Join(counts, ", ")),
			detail
	}
	return Pass, fmt.Sprintf("%d retransmissions of Registration Accept, each at a higher NAS COUNT (COUNTs %s)", len(tx)-1, strings.Join(counts, ", ")),
		"each transmission's MAC verifies at the COUNT listed; the number and timing of the retransmissions are graded by NAS-T3550-ACCEPT-RETRANSMIT"
}
