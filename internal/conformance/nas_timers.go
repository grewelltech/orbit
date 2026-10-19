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

// t3560Default is T3560's normal value in TS 24.501 Table 10.2.2, the same
// in every release read (V15.0.0 to V19.8.0).
const t3560Default = 6 * time.Second

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
// The spacing of the retransmissions is recorded, not asserted: 6 s is the
// table's default, and an operator may configure another value.
type nasT3560AuthRetransmit struct {
	// t3560 is the nominal timer the windows are computed from; zero is
	// t3560Default (tests inject a short value).
	t3560 time.Duration
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

func (c nasT3560AuthRetransmit) authRequestWait() time.Duration {
	if c.authWait > 0 {
		return c.authWait
	}
	return authRequestWait
}

// window is how long after the first Authentication Request the check waits
// for the release: six T3560 periods with half a period of slack each.
func (c nasT3560AuthRetransmit) window() time.Duration { return 9 * c.timer() }

// Budget is the window plus the wait for the first Authentication Request
// and the liveness probe after it.
func (c nasT3560AuthRetransmit) Budget() time.Duration { return c.window() + c.authRequestWait() }

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
	o := &t3560Observation{cancel: cancel, window: c.window()}
	noRequest := time.AfterFunc(c.authRequestWait(), func() {
		if !o.sawRequest() {
			cancel()
		}
	})
	_, err = h.RegisterWithProbe(actx, &engine.AttachProbe{
		Withhold:   map[uint8]bool{f5nas.MsgTypeAuthenticationResponse: true},
		OnDownlink: o.note,
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

// t3560Observation is what the probe saw during the attach. note runs on
// the attach goroutine, the window timer on its own.
type t3560Observation struct {
	cancel context.CancelFunc
	window time.Duration

	mu     sync.Mutex
	timer  *time.Timer
	snap   t3560Snapshot
	closed bool
}

// t3560Snapshot is the observation once the attach is over.
type t3560Snapshot struct {
	requests     []time.Time // each Authentication Request, the first included
	release      time.Time   // the UE Context Release Command; zero if none
	reject       string      // a reject that ended the procedure early
	windowClosed bool        // the window ran out without a release
}

func (o *t3560Observation) note(ev engine.DownlinkEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case ev.Procedure == "UEContextRelease":
		o.snap.release = ev.At
	case ev.NASType == f5nas.MsgTypeAuthenticationRequest:
		o.snap.requests = append(o.snap.requests, ev.At)
		if len(o.snap.requests) == 1 {
			o.timer = time.AfterFunc(o.window, func() {
				o.mu.Lock()
				if !o.closed {
					o.snap.windowClosed = true
				}
				o.mu.Unlock()
				o.cancel()
			})
		}
	case ev.NASType == f5nas.MsgTypeAuthenticationReject:
		o.snap.reject = "Authentication Reject"
	case ev.NASType == f5nas.MsgTypeRegistrationReject:
		o.snap.reject = "Registration Reject"
	}
}

func (o *t3560Observation) sawRequest() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.snap.requests) > 0
}

// stop ends the window timer once the attach has returned.
func (o *t3560Observation) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	if o.timer != nil {
		o.timer.Stop()
	}
}

func (o *t3560Observation) snapshot() t3560Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.snap
	s.requests = append([]time.Time(nil), o.snap.requests...)
	return s
}

// classifyT3560 judges a T3560 observation for nominal timer t. ERROR when
// the procedure never reached an Authentication Request or a reject ended it;
// otherwise PASS for exactly four retransmissions then a UE Context Release
// Command no later than 1.5 T3560 after the fourth, and FAIL naming the
// first part that missed: no retransmission, the wrong count, or no release.
// The detail gives both parts, each retransmission's gap, and whether the
// gaps match the default timer.
func classifyT3560(s t3560Snapshot, t, authWait time.Duration) (Verdict, string, string) {
	if s.reject != "" {
		return Error, s.reject + " before the T3560 window",
			fmt.Sprintf("the AMF sent %s after %d Authentication Request(s); this check needs a subscriber the core authenticates", s.reject, len(s.requests))
	}
	if len(s.requests) == 0 {
		return Error, "registration did not reach Authentication Request",
			fmt.Sprintf("no Authentication Request within %s of the Initial UE Message", authWait)
	}
	n := len(s.requests) - 1
	last := s.requests[len(s.requests)-1]
	releaseBy := last.Add(t + t/2)
	released := !s.release.IsZero() && !s.release.After(releaseBy)

	var details []string
	retransmitOK := n == t3560Retransmissions
	if !retransmitOK {
		details = append(details, fmt.Sprintf("AMF did not retransmit Authentication Request on T3560 expiry (observed %d retransmissions, want %d). TS 24.501 5.4.1.3.7 b). Likely cause: T3560 not implemented in the AMF.", n, t3560Retransmissions))
	}
	releaseOK := retransmitOK && released
	if !released {
		details = append(details, fmt.Sprintf("AMF did not release the N1 NAS signalling connection on the fifth T3560 expiry (no UE Context Release Command within %s, 0.5 x T3560, after the inferred fifth expiry, taken as one T3560 after the last Authentication Request). TS 24.501 5.4.1.3.7 b). Likely cause: the AMF aborts locally without releasing.", t/2))
	}
	details = append(details, t3560Timing(s, t))

	switch {
	case n == 0:
		return Fail, "no retransmission of Authentication Request", strings.Join(details, "; ")
	case !retransmitOK:
		return Fail, fmt.Sprintf("%d retransmissions of Authentication Request, want %d", n, t3560Retransmissions), strings.Join(details, "; ")
	case !releaseOK:
		return Fail, "no UE Context Release Command after the fifth T3560 expiry", strings.Join(details, "; ")
	}
	return Pass, fmt.Sprintf("%d retransmissions of Authentication Request, then UE Context Release Command", n), strings.Join(details, "; ")
}

// t3560Timing records the gaps between Authentication Requests and when the
// release came, and notes gaps outside [0.5, 1.5] T3560.
func t3560Timing(s t3560Snapshot, t time.Duration) string {
	var gaps []string
	differs := false
	for i := 1; i < len(s.requests); i++ {
		g := s.requests[i].Sub(s.requests[i-1])
		gaps = append(gaps, seconds(g))
		if g < t/2 || g > t+t/2 {
			differs = true
		}
	}
	var b strings.Builder
	if len(gaps) > 0 {
		fmt.Fprintf(&b, "gaps %s s", strings.Join(gaps, "/"))
	} else {
		b.WriteString("one Authentication Request")
	}
	if !s.release.IsZero() {
		fmt.Fprintf(&b, ", UE Context Release Command %s s after the last request", seconds(s.release.Sub(s.requests[len(s.requests)-1])))
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
