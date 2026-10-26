package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue"
)

func init() {
	builtins = append(builtins, nasT3550AcceptRetransmit{})
}

const (
	// t3550Default is T3550's normal value in TS 24.501 Table 10.2.2, the
	// same in every release read (V15.0.0 to V19.8.0).
	t3550Default = 6 * time.Second
	// t3550Max is the largest value Table 10.2.2 gives T3550: 18 s in
	// WB-N1/CE mode, added in Rel-16. It bounds the wait for the first
	// retransmission.
	t3550Max = 18 * time.Second
)

// t3550Retransmissions is how many times 5.5.1.2.8 c) repeats the
// Registration Accept.
const t3550Retransmissions = 4

// nasT3550AcceptRetransmit withholds the UE's Registration Complete after a
// Registration Accept that assigns a 5G-GUTI, which shall start T3550
// (5.5.1.2.4), and asserts TS 24.501 5.5.1.2.8 c): the AMF retransmits the REGISTRATION
// ACCEPT on each T3550 expiry, four times, and on the fifth expiry aborts.
// No release is required on the abort, so none is asserted; one that comes
// is recorded. The timing is adaptive as for T3560 (the first
// retransmission awaited 1.5 × t3550Max, then each next one, and after the
// fourth a fifth, 1.5 × T_est after the last), and the spacing is recorded,
// not asserted.
type nasT3550AcceptRetransmit struct {
	// t3550 is the nominal timer; zero is t3550Default (tests only).
	t3550 time.Duration
	// tmax bounds the wait for the first retransmission; zero is t3550Max
	// (tests only).
	tmax time.Duration
	// acceptWait bounds the wait for the first Registration Accept; zero
	// is authRequestWait, 10 s (tests only).
	acceptWait time.Duration
}

func (nasT3550AcceptRetransmit) ID() string         { return "NAS-T3550-ACCEPT-RETRANSMIT" }
func (nasT3550AcceptRetransmit) Category() Category { return NAS }
func (nasT3550AcceptRetransmit) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.5.1.2.8 c)", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 has the AMF enter 5GMM-DEREGISTERED on the abort, 5GMM-REGISTERED from V15.7.0; the retransmission wording is unchanged through Rel-19"},
			}},
		{Spec: "TS 24.501", Clause: "5.5.1.2.4 T3550 start", Strength: Shall, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 15, Note: "V15.0.0 starts T3550 when a new 5G-GUTI is allocated (as for a UE registering with a SUCI); from V15.7.0 when a 5G-GUTI or the SOR transparent container is included, with operator-defined access categories, extended emergency numbers, a new configured NSSAI and network slicing subscription change added"},
				{Release: 16, Note: "the 5G-GUTI sentence is unchanged; adds CAG information list, UE radio capability ID (or its deletion) and truncated 5G-S-TMSI configuration triggers"},
				{Release: 17, Note: "the \"ACCCEPT\" typo is fixed; adds extended CAG information list, PEIPS assistance information, UUAA-MM pending and NSSRG triggers; the configured-NSSAI trigger applies only when roaming"},
				{Release: 18, Note: "\"subclause\" becomes \"clause\"; adds on-demand NSSAI, S-NSSAI time and location validity, NSAG and alternative NSSAI triggers; the configured-NSSAI wording leaves 5.5.1.2.4"},
				{Release: 19, Note: "adds LP-WUSPS assistance information and LP-WUS status triggers"},
			}},
		{Spec: "TS 24.501", Clause: "Table 10.2.2 T3550", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "adds a WB-N1/CE mode value of 18s; the normal value stays 6s"},
				{Release: 17, Note: "adds 11s for access via a satellite NG-RAN cell; the normal value stays 6s"},
			}},
	}
}

func (c nasT3550AcceptRetransmit) timer() time.Duration {
	if c.t3550 > 0 {
		return c.t3550
	}
	return t3550Default
}

func (c nasT3550AcceptRetransmit) waitAccept() time.Duration {
	if c.acceptWait > 0 {
		return c.acceptWait
	}
	return authRequestWait
}

func (c nasT3550AcceptRetransmit) maxTimer() time.Duration {
	if c.tmax > 0 {
		return c.tmax
	}
	return t3550Max
}

// Budget is the wait for the first Registration Accept plus five waits of
// 1.5 × t3550Max (the first retransmission, three more, and room for a
// fifth): 145 s. A 6 s core ends in about 33 s.
func (c nasT3550AcceptRetransmit) Budget() time.Duration {
	return c.waitAccept() + 5*(c.maxTimer()+c.maxTimer()/2)
}

func (c nasT3550AcceptRetransmit) Run(ctx context.Context, env Env) Result {
	t := c.timer()
	r := Result{Expected: fmt.Sprintf("with the Registration Complete withheld after a Registration Accept assigning a 5G-GUTI, %d retransmissions of the Registration Accept at T3550 (%s by default), then none (TS 24.501 5.5.1.2.8 c))",
		t3550Retransmissions, t3550Default)}
	if !env.UE.Configured() {
		r.Verdict, r.Observed = Skip, "no UE configured"
		r.Detail = "set --supi, --ki/--opc and --gnb-n3 to run NAS checks"
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
	o := &retransmitObservation{cancel: cancel, msgType: f5nas.MsgTypeRegistrationAccept,
		nominal: t, tmax: c.maxTimer(), retransmissions: t3550Retransmissions, first: acceptAssignsGUTI}
	noAccept := time.AfterFunc(c.waitAccept(), func() {
		if !o.seen() {
			cancel()
		}
	})
	_, err = h.RegisterWithProbe(actx, &engine.AttachProbe{
		Withhold:   map[uint8]bool{f5nas.MsgTypeRegistrationComplete: true},
		OnDownlink: o.note,
	})
	noAccept.Stop()
	o.stop()
	snap := o.snapshot()
	switch {
	case err == nil:
		r.Verdict, r.Observed = Error, "registration completed with the Registration Complete withheld"
		r.Detail = "the harness did not withhold the reply"
		return r
	case errors.Is(err, engine.ErrReleasedByCore), errors.Is(err, context.Canceled) && ctx.Err() == nil:
		// A release, the window's end, no Accept or no 5G-GUTI: judged below.
	case ctx.Err() != nil:
		r.Verdict, r.Observed = Error, "timed out before the T3550 window closed"
		r.Detail = fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget())
		return r
	case snap.reject != "":
		// A reject: ERROR, judged below.
	default:
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed during the T3550 procedure", err.Error()
		return r
	}
	r.Verdict, r.Observed, r.Detail = classifyT3550(snap, t, c.waitAccept())
	if r.Verdict == Skip {
		return r
	}
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the T3550 procedure")
}

// acceptAssignsGUTI reports whether a Registration Accept event assigns a
// 5G-GUTI. Its NAS type was readable, so the inner plain message starts at
// offset 7 of a protected PDU (or at 0 of a plain one).
func acceptAssignsGUTI(ev engine.DownlinkEvent) bool {
	raw := ev.NAS
	if len(raw) > 7 && raw[1]&0x0f != nas.SecHdrPlain {
		raw = raw[7:]
	}
	msg, err := nas.DecodePlain(raw)
	if err != nil {
		return false
	}
	g, err := ue.ParseRegistrationAccept(msg)
	return err == nil && g != nil && len(g.Raw) > 0
}

// classifyT3550 judges a T3550 observation for nominal timer t. ERROR when
// registration never reached a Registration Accept or a reject ended it;
// SKIP when the Accept assigned no 5G-GUTI, so T3550 never started; else
// PASS for exactly four retransmissions and FAIL for none, fewer or more (a
// fifth means the AMF did not abort on the fifth expiry). The detail gives
// the gaps and records any release.
func classifyT3550(s retransmitSnapshot, t, acceptWait time.Duration) (Verdict, string, string) {
	switch {
	case s.reject != "":
		return Error, s.reject + " before the T3550 window",
			fmt.Sprintf("the AMF sent %s; this check needs a subscriber the core registers", s.reject)
	case len(s.times) == 0:
		return Error, "registration did not reach Registration Accept",
			fmt.Sprintf("no Registration Accept within %s of the Initial UE Message", acceptWait)
	case s.refused:
		return Skip, "Registration Accept carried no 5G-GUTI; T3550 not started",
			"TS 24.501 5.5.1.2.4 starts T3550 when the Registration Accept includes a 5G-GUTI; other T3550 triggers (SOR, CAG, NSSAI ...) not evaluated"
	}
	n := len(s.times) - 1
	timing := retransmitTiming(s, t, estimate(s.times, t), "Registration Accept", "Registration Accept", " (recorded only: 5.5.1.2.8 c) requires no release)")
	switch {
	case n == t3550Retransmissions:
		return Pass, fmt.Sprintf("%d retransmissions of Registration Accept", n), timing
	case n > t3550Retransmissions:
		return Fail, fmt.Sprintf("%d retransmissions of Registration Accept, want %d", n, t3550Retransmissions),
			strings.Join([]string{fmt.Sprintf("AMF retransmitted Registration Accept after the fifth T3550 expiry (observed %d retransmissions, want %d); the procedure is aborted on the fifth expiry. TS 24.501 5.5.1.2.8 c).", n, t3550Retransmissions), timing}, "; ")
	}
	observed := fmt.Sprintf("%d retransmissions of Registration Accept, want %d", n, t3550Retransmissions)
	if n == 0 {
		observed = "no retransmission of Registration Accept"
	}
	return Fail, observed, strings.Join([]string{fmt.Sprintf("AMF did not retransmit Registration Accept on T3550 expiry (observed %d retransmissions, want %d). TS 24.501 5.5.1.2.8 c). Likely cause: T3550 not implemented.", n, t3550Retransmissions), timing}, "; ")
}
