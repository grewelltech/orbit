package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
)

func init() {
	builtins = append(builtins, nasT3560AuthRetransmit{})
}

const (
	// t3560Default is T3560's normal value in TS 24.501 Table 10.2.2, the
	// same in every release read (V15.0.0 to V19.8.0).
	t3560Default = 6 * time.Second
	// t3560Max is the largest value Table 10.2.2 gives T3560: 24 s in
	// WB-N1/CE mode, added in Rel-16. It bounds the wait for the first
	// retransmission, so a core configured slower than the default still
	// gets its retransmissions counted.
	t3560Max = 24 * time.Second
)

const (
	// t3560Retransmissions is how many times 5.4.1.3.7 b) repeats the
	// Authentication Request.
	t3560Retransmissions = 4
	// authRequestWait bounds the wait for the first Authentication Request
	// after the attach starts.
	authRequestWait = 10 * time.Second
)

// nasT3560AuthRetransmit withholds the UE's Authentication Response and
// asserts what TS 24.501 5.4.1.3.7 b) requires of the network: retransmit
// the AUTHENTICATION REQUEST on each T3560 expiry, four times, and on the
// fifth expiry abort and release the N1 NAS signalling connection, which on
// N2 is a UE CONTEXT RELEASE COMMAND for the UE.
//
// The timer is not assumed to be the 6 s default, since an operator may
// configure another value. The first retransmission is awaited for 1.5 ×
// t3560Max. After that the timer is estimated as T_est = max(nominal, the
// mean gap so far), and each next retransmission, or after the fourth the
// release, is due 1.5 × T_est after the last request. The spacing itself is
// recorded, not asserted.
type nasT3560AuthRetransmit struct {
	// t3560 is the nominal timer; zero is t3560Default (tests inject a
	// short value).
	t3560 time.Duration
	// tmax bounds the wait for the first retransmission; zero is t3560Max
	// (tests inject a short value).
	tmax time.Duration
	// authWait bounds the wait for the first Authentication Request; zero
	// is authRequestWait (tests inject a short value).
	authWait time.Duration
}

func (nasT3560AuthRetransmit) ID() string         { return "NAS-T3560-AUTH-RETRANSMIT" }
func (nasT3560AuthRetransmit) Category() Category { return NAS }
func (nasT3560AuthRetransmit) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 24.501", Clause: "5.4.1.3.7 b)", Strength: Shall, Since: 15, Verified: ts24501Verified, WordingUnchanged: true},
		{Spec: "TS 24.501", Clause: "Table 10.2.2 T3560", Strength: Descriptive, Since: 15, Verified: ts24501Verified,
			Changed: []SpecChange{
				{Release: 16, Note: "adds a WB-N1/CE mode value of 24s; the normal value stays 6s"},
				{Release: 17, Note: "adds 11s for access via a satellite NG-RAN cell; the normal value stays 6s"},
			}},
	}
}

func (c nasT3560AuthRetransmit) timer() time.Duration {
	if c.t3560 > 0 {
		return c.t3560
	}
	return t3560Default
}

func (c nasT3560AuthRetransmit) maxTimer() time.Duration {
	if c.tmax > 0 {
		return c.tmax
	}
	return t3560Max
}

func (c nasT3560AuthRetransmit) authRequestWait() time.Duration {
	if c.authWait > 0 {
		return c.authWait
	}
	return authRequestWait
}

// Budget is the wait for the first Authentication Request plus five waits
// of 1.5 × t3560Max (the first retransmission, three more, and the
// release): 190 s. A core at the 6 s default ends in about 33 s, since the
// release closes the window.
func (c nasT3560AuthRetransmit) Budget() time.Duration {
	return c.authRequestWait() + 5*(c.maxTimer()+c.maxTimer()/2)
}

func (c nasT3560AuthRetransmit) Run(ctx context.Context, env Env) Result {
	t := c.timer()
	r := Result{Expected: fmt.Sprintf("with the Authentication Response withheld, %d retransmissions of the Authentication Request at T3560 (%s by default), then a UE Context Release Command on the fifth expiry (TS 24.501 5.4.1.3.7 b))",
		t3560Retransmissions, t3560Default)}
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
	o := &retransmitObservation{cancel: cancel, msgType: f5nas.MsgTypeAuthenticationRequest,
		nominal: t, tmax: c.maxTimer(), retransmissions: t3560Retransmissions, release: true}
	noRequest := time.AfterFunc(c.authRequestWait(), func() {
		if !o.seen() {
			cancel()
		}
	})
	_, err = h.RegisterWithProbe(actx, &engine.AttachProbe{
		Withhold:   map[uint8]bool{f5nas.MsgTypeAuthenticationResponse: true},
		OnDownlink: o.noteRelease,
		OnNAS:      o.noteNAS,
	})
	noRequest.Stop()
	o.stop()
	snap := o.snapshot()
	switch {
	case err == nil:
		r.Verdict, r.Observed = Error, "registration completed with the Authentication Response withheld"
		r.Detail = "the harness did not withhold the reply"
		return r
	case errors.Is(err, engine.ErrReleasedByCore), errors.Is(err, context.Canceled) && ctx.Err() == nil:
		// The release, the window's end or no Authentication Request: judged below.
	case ctx.Err() != nil:
		r.Verdict, r.Observed = Error, "timed out before the T3560 window closed"
		r.Detail = fmt.Sprintf("raise --per-test-timeout to at least %s", c.Budget())
		return r
	case snap.reject != "":
		// An Authentication or Registration Reject: ERROR, judged below.
	default:
		r.Verdict, r.Observed, r.Detail = Error, "UE setup failed during the T3560 procedure", err.Error()
		return r
	}
	r.Verdict, r.Observed, r.Detail = classifyT3560(snap, t, c.authRequestWait())
	// The AMF answered throughout, so its state after the abort is checked
	// the way the NGAP checks do; Manager.Register closed the association.
	return confirmAlive(ctx, env, r, true, "the AMF stopped completing NG Setup after the T3560 procedure")
}

// estimate is T_est: the nominal timer, or the mean gap between the
// messages in times when that is longer.
func estimate(times []time.Time, nominal time.Duration) time.Duration {
	if len(times) < 2 {
		return nominal
	}
	mean := times[len(times)-1].Sub(times[0]) / time.Duration(len(times)-1)
	if mean > nominal {
		return mean
	}
	return nominal
}

// retransmitObservation is what the probe saw of one NAS message the AMF
// retransmits on a timer (msgType), plus any release or reject. note runs
// on the attach goroutine, the deadline timer on its own.
//
// The first message opens a wait of 1.5 × tmax for the first
// retransmission. Each retransmission sets the next deadline at 1.5 × T_est
// after it. After the last expected retransmission (retransmissions), a
// release-expecting procedure (release) keeps listening for 1.5 ×
// max(T_est, tmax), long enough to see a late release; one more
// retransmission, or the release, ends the observation at once. first, if
// set, judges the first message: false ends the attach at once (refused).
// NAS messages come from AttachProbe.OnNAS (noteNAS), typed from the
// decoded message so the observation works under any cipher; the release,
// an NGAP message, from OnDownlink (noteRelease).
type retransmitObservation struct {
	cancel          context.CancelFunc
	msgType         uint8
	nominal, tmax   time.Duration
	retransmissions int
	release         bool
	first           func(engine.NASEvent) bool

	mu     sync.Mutex
	timer  *time.Timer
	snap   retransmitSnapshot
	closed bool
}

// retransmitSnapshot is the observation once the attach is over.
type retransmitSnapshot struct {
	times        []time.Time // each message of the watched type, the first included
	release      time.Time   // the UE Context Release Command; zero if none
	reject       string      // a reject that ended the procedure early
	windowClosed bool        // a deadline ran out
	refused      bool        // first judged the first message unusable
}

// noteRelease records a UE Context Release Command (AttachProbe.OnDownlink).
func (o *retransmitObservation) noteRelease(ev engine.DownlinkEvent) {
	if ev.Procedure != "UEContextRelease" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.snap.release = ev.At
	o.cancel()
}

// noteNAS records a downlink NAS message (AttachProbe.OnNAS).
func (o *retransmitObservation) noteNAS(ev engine.NASEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch mt := ev.MessageType(); mt {
	case o.msgType:
		o.snap.times = append(o.snap.times, ev.At)
		n := len(o.snap.times) - 1 // retransmissions so far
		est := estimate(o.snap.times, o.nominal)
		switch {
		case n == 0 && o.first != nil && !o.first(ev):
			o.snap.refused = true
			o.cancel()
		case n == 0:
			o.arm(o.tmax + o.tmax/2)
		case n < o.retransmissions:
			o.arm(est + est/2)
		case n == o.retransmissions && o.release:
			wait := est
			if o.tmax > wait {
				wait = o.tmax
			}
			o.arm(wait + wait/2)
		case n == o.retransmissions:
			o.arm(est + est/2) // room to see a retransmission too many
		default:
			o.cancel() // a retransmission too many: the count is decided
		}
	case f5nas.MsgTypeAuthenticationReject:
		o.snap.reject = "Authentication Reject"
	case f5nas.MsgTypeRegistrationReject:
		o.snap.reject = "Registration Reject"
	}
}

// arm (re)starts the deadline timer for d; o.mu is held. A replaced
// timer's callback may already be running, blocked on o.mu, when Stop is
// called (Stop then returns false), so each callback acts only while its
// timer is still the current one.
func (o *retransmitObservation) arm(d time.Duration) {
	if o.timer != nil {
		o.timer.Stop()
	}
	var tm *time.Timer
	tm = time.AfterFunc(d, func() {
		o.mu.Lock()
		if o.closed || o.timer != tm {
			o.mu.Unlock()
			return
		}
		o.snap.windowClosed = true
		o.mu.Unlock()
		o.cancel()
	})
	o.timer = tm
}

func (o *retransmitObservation) seen() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.snap.times) > 0
}

// stop ends the deadline timer once the attach has returned.
func (o *retransmitObservation) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	if o.timer != nil {
		o.timer.Stop()
	}
}

func (o *retransmitObservation) snapshot() retransmitSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.snap
	s.times = append([]time.Time(nil), o.snap.times...)
	return s
}

// classifyT3560 judges a T3560 observation for nominal timer t. ERROR when
// the procedure never reached an Authentication Request or a reject ended
// it. Otherwise PASS for exactly four retransmissions then a UE Context
// Release Command no later than 1.5 × T_est after the fourth (0.5 × T_est
// after the inferred fifth expiry), and FAIL naming the first part that
// missed: no retransmission, the wrong count, a late release or none. The
// detail gives both parts, each gap and the estimated timer.
func classifyT3560(s retransmitSnapshot, t, authWait time.Duration) (Verdict, string, string) {
	if s.reject != "" {
		return Error, s.reject + " before the T3560 window",
			fmt.Sprintf("the AMF sent %s after %d Authentication Request(s); this check needs a subscriber the core authenticates", s.reject, len(s.times))
	}
	if len(s.times) == 0 {
		return Error, "registration did not reach Authentication Request",
			fmt.Sprintf("no Authentication Request within %s of the Initial UE Message", authWait)
	}
	n := len(s.times) - 1
	last := s.times[len(s.times)-1]
	est := estimate(s.times, t)
	fifth := last.Add(est)
	releaseBy := fifth.Add(est / 2)
	released := !s.release.IsZero()
	late := released && s.release.After(releaseBy)

	var details []string
	retransmitOK := n == t3560Retransmissions
	if !retransmitOK {
		details = append(details, fmt.Sprintf("AMF did not retransmit Authentication Request on T3560 expiry (observed %d retransmissions, want %d). TS 24.501 5.4.1.3.7 b). Likely cause: T3560 not implemented in the AMF.", n, t3560Retransmissions))
	}
	switch {
	case late:
		details = append(details, fmt.Sprintf("UE Context Release Command %s s after the inferred fifth expiry, later than 0.5 x T_est (%s s). TS 24.501 5.4.1.3.7 b).",
			seconds(s.release.Sub(fifth)), seconds(est/2)))
	case !released:
		details = append(details, fmt.Sprintf("AMF did not release the N1 NAS signalling connection on the fifth T3560 expiry (no UE Context Release Command within %s, 0.5 x T_est, after the inferred fifth expiry, taken as one T_est after the last Authentication Request). TS 24.501 5.4.1.3.7 b). Likely cause: the AMF aborts locally without releasing.", est/2))
	}
	details = append(details, retransmitTiming(s, t, est, "Authentication Request", "request", ""))

	switch {
	case n == 0:
		return Fail, "no retransmission of Authentication Request", strings.Join(details, "; ")
	case !retransmitOK:
		return Fail, fmt.Sprintf("%d retransmissions of Authentication Request, want %d", n, t3560Retransmissions), strings.Join(details, "; ")
	case late:
		return Fail, "UE Context Release Command late after the fifth T3560 expiry", strings.Join(details, "; ")
	case !released:
		return Fail, "no UE Context Release Command after the fifth T3560 expiry", strings.Join(details, "; ")
	}
	return Pass, fmt.Sprintf("%d retransmissions of Authentication Request, then UE Context Release Command", n), strings.Join(details, "; ")
}

// retransmitTiming records the gaps between the watched messages (what,
// "last" naming the last one), the estimated timer est, when a release came
// (with releaseNote after it), and notes gaps outside [0.5, 1.5] × est and
// a timer that differs from the 6 s default of TS 24.501 Table 10.2.2.
func retransmitTiming(s retransmitSnapshot, nominal, est time.Duration, what, last, releaseNote string) string {
	var gaps []string
	uneven, differs := false, false
	for i := 1; i < len(s.times); i++ {
		g := s.times[i].Sub(s.times[i-1])
		gaps = append(gaps, seconds(g))
		if g < est/2 || g > est+est/2 {
			uneven = true
		}
		if g < nominal/2 || g > nominal+nominal/2 {
			differs = true
		}
	}
	var b strings.Builder
	if len(gaps) > 0 {
		fmt.Fprintf(&b, "gaps %s s (estimated timer %s s)", strings.Join(gaps, "/"), seconds(est))
	} else {
		b.WriteString("one " + what)
	}
	if !s.release.IsZero() && len(s.times) > 0 {
		fmt.Fprintf(&b, ", UE Context Release Command %s s after the last %s%s", seconds(s.release.Sub(s.times[len(s.times)-1])), last, releaseNote)
	}
	if uneven {
		b.WriteString("; gaps uneven: outside [0.5, 1.5] x the estimated timer")
	}
	if differs {
		fmt.Fprintf(&b, "; timer differs from the %s default (TS 24.501 Table 10.2.2)", t3560Default)
	}
	return b.String()
}

// seconds renders d in seconds with one decimal, or two below a second.
func seconds(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.2f", d.Seconds())
	}
	return fmt.Sprintf("%.1f", d.Seconds())
}
