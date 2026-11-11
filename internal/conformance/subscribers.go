package conformance

import (
	"fmt"
	"strconv"

	"github.com/bgrewell/orbit/internal/ue"
)

// subscribersOf is how many subscribers check t registers over its run:
// the most it holds at once, or its declared Subscribers when it registers
// more than that one after another.
func subscribersOf(t Test) int {
	n := NeedsOf(t)
	return max(n.UEs, n.Subscribers)
}

// subscriberBlock is where check idx (its position in the full registry,
// so stable across category filters) sits in the suite's subscriber pool:
// its offset from the pool's first SUPI and how many it takes. The blocks
// follow one another in registry order, so no two checks share a
// subscriber: a UE one check leaves half-registered, or still being
// released, is never the next check's UE.
func (r *Registry) subscriberBlock(idx int) (off, span int) {
	for i, t := range r.tests {
		if i == idx {
			return off, subscribersOf(t)
		}
		off += subscribersOf(t)
	}
	return 0, 0
}

// SubscriberSpan is how many subscribers the registered checks and the
// run's preflight take when each has its own: what a pool must hold for
// none to reuse another's.
func (r *Registry) SubscriberSpan() int {
	off, span := r.preflightSubscriber()
	return off + span
}

// preflightSubscriber is where the preflight's one subscriber sits in the
// suite's subscriber pool: after every check's block.
func (r *Registry) preflightSubscriber() (off, span int) {
	for _, t := range r.tests {
		off += subscribersOf(t)
	}
	return off, 1
}

// forCheck is the UE of a check whose block starts off subscribers into
// the pool and takes span of them: SUPI is the block's first and
// Subscribers its size. A pool smaller than the suite's span wraps, so
// later checks reuse earlier ones' subscribers, and a spec with no pool
// (Subscribers of 0 or 1) is returned as it is: every check then
// registers the one SUPI. An unparseable SUPI is left for NewUEHarness to
// report.
func (s UESpec) forCheck(off, span int) UESpec {
	if s.Subscribers <= 1 || span == 0 {
		return s
	}
	off %= s.Subscribers
	supi, err := supiAt(s.SUPI, off)
	if err != nil {
		return s
	}
	s.SUPI, s.Subscribers = supi, min(span, s.Subscribers-off)
	return s
}

// sharedSubscriberNote is what an ERROR says when its check registered a
// subscriber an earlier check of the run registered: the core may still
// hold, or still be releasing, that UE's context, and a registration that
// meets it can go unanswered, so the ERROR may be the suite's doing and
// not the core's answer to this check.
func sharedSubscriberNote(e Env) string {
	return fmt.Sprintf("this check registered the subscriber %s registered earlier in this run: "+
		"a core still holding or releasing that UE's context may leave the registration unanswered; "+
		"a subscriber pool with a subscriber for each UE check keeps them apart", e.sharedWith)
}

// supiAt is the SUPI n after first, as wide as first's digits.
func supiAt(first string, n int) (string, error) {
	norm, err := ue.NormalizeSUPI(first)
	if err != nil {
		return "", err
	}
	v, err := strconv.ParseUint(norm, 10, 64)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", len(norm), v+uint64(n)), nil
}

// UseSubscriber switches the harness to subscriber i of the check's block
// (UESpec.Subscribers; i wraps within it), for a check that registers
// several UEs one after another and wants none to meet the context its
// predecessor left in the core. It is an error while a UE is registered.
func (h *UEHarness) UseSubscriber(i int) error {
	if h.registered || h.probe != nil {
		return fmt.Errorf("a UE is registered as %s", h.supi)
	}
	supi, err := supiAt(h.env.UE.SUPI, i%max(h.env.UE.Subscribers, 1))
	if err != nil {
		return err
	}
	id, err := ue.ParseIdentity(supi, h.env.GNB.MCC, h.env.GNB.MNC, "0")
	if err != nil {
		return err
	}
	h.supi, h.id, h.sub.SUPI = supi, id, supi
	return nil
}
