package conformance

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

func init() {
	builtins = append(builtins, duplicateRANNodeID{})
}

const (
	// duplicateSetupWait bounds the wait for the second NG Setup's outcome.
	// Its absence within this bound is what "unanswered" reports.
	duplicateSetupWait = ngSetupWait
	// duplicateFirstWait is how long the first association is watched after
	// the second NG Setup's outcome: an association that has not ended
	// within it is reported as kept.
	duplicateFirstWait = 2 * time.Second
)

// Observed texts of NGAP-DUPLICATE-RAN-NODE-ID: what the second NG Setup
// got, then what became of the first association, joined by "; ".
const (
	duplicateAccepted   = "second NG Setup accepted"
	duplicateRejected   = "second NG Setup rejected (NG SETUP FAILURE)"
	duplicateUnanswered = "second NG Setup unanswered"
	duplicateOtherPDU   = "second NG Setup answered with another PDU"
	duplicateReset      = "second association reset"
	duplicateFirstKept  = "first association kept"
	duplicateFirstEnded = "first association dropped"
)

// duplicateRANNodeID holds one association that has completed NG Setup and
// opens a second, from another local address (Env.SecondBind), on which it
// sends an NG SETUP REQUEST with the same Global RAN Node ID. It reports
// what the AMF does with the duplicate: accepts it, rejects it, leaves it
// unanswered, answers it with another PDU or ends the new association; and
// whether the first association is still up afterwards.
//
// TS 38.413 says nothing of a second association presenting a Global RAN
// Node ID already in use: 9.3.1.5 says only that the ID identifies an NG-RAN
// node, and 8.7.1.2 describes the answer to an NG SETUP REQUEST ("The AMF
// responds with an NG SETUP RESPONSE message"). An answer either way is
// recorded: an NG SETUP RESPONSE or an NG SETUP FAILURE with the first
// association kept is INFO, the node still being served. Either with the
// first association dropped is WARN for availability: the node loses the
// association it was served on. An AMF that neither accepts the duplicate
// nor answers it with NG SETUP FAILURE (no answer within the setup wait,
// another PDU, the new association ended) has not accepted the setup and
// has not sent the failure 8.7.1.3 says it should: DEVIATION, whatever
// became of the first association, which Observed states.
//
// The first association is watched at transport level only, for
// duplicateFirstWait: no PDU is sent on it, so an AMF that keeps the
// association open but no longer serves it is reported as keeping it.
// Without a second source address the check SKIPs; with one this host does
// not hold it is ERROR. Both associations are closed afterwards, and a fresh
// NG Setup is tried (observeAlive): a core that no longer completes it did
// not survive the duplicate and is CRASH.
type duplicateRANNodeID struct {
	// setupWait and firstWait override duplicateSetupWait and
	// duplicateFirstWait; tests shorten them.
	setupWait, firstWait time.Duration
}

// The keys of the references the outcomes of NGAP-DUPLICATE-RAN-NODE-ID
// rest on.
const (
	duplicateSuccessRef = "TS 38.413 8.7.1.2"
	duplicateFailureRef = "TS 38.413 8.7.1.3"
)

// The outcomes of NGAP-DUPLICATE-RAN-NODE-ID.
var (
	duplicateAcceptedKept = Outcome{ID: "accepted", Verdict: Info, Refs: []string{duplicateSuccessRef},
		Summary: "accepted the duplicate NG SETUP REQUEST and kept the first association"}
	// A rejection with the first association kept is INFO: the node is
	// still served on the association it set up first, and 8.7.1.3 lets an
	// AMF that cannot accept a setup reject it.
	duplicateRejectedKept = Outcome{ID: "rejected", Verdict: Info,
		Summary: "rejected the duplicate NG SETUP REQUEST with NG SETUP FAILURE and kept the first association"}
	// Dropping the first association is WARN for availability whatever the
	// duplicate got: the node loses the association the AMF served it on,
	// and no requirement covers a duplicate Global RAN Node ID.
	duplicateAcceptedDropped = Outcome{ID: "accepted-first-dropped", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "accepted the duplicate NG SETUP REQUEST and dropped the first association"}
	duplicateRejectedDropped = Outcome{ID: "rejected-first-dropped", Verdict: Warn, Criterion: CriterionAvailability,
		Summary: "rejected the duplicate NG SETUP REQUEST with NG SETUP FAILURE and dropped the first association"}
	duplicateNoAnswer = Outcome{ID: "unanswered", Verdict: Deviation, Refs: []string{duplicateFailureRef},
		Bound:   &Bound{Wait: duplicateSetupWait, Source: "check bound; TS 38.413 sets no timer for the NG Setup outcome"},
		Summary: "sent neither NG SETUP RESPONSE nor NG SETUP FAILURE for the duplicate NG SETUP REQUEST"}
	duplicateOtherAnswer = Outcome{ID: "other-pdu", Verdict: Deviation, Refs: []string{duplicateFailureRef},
		Summary: "answered the duplicate NG SETUP REQUEST with a PDU that is neither NG SETUP RESPONSE nor NG SETUP FAILURE"}
	duplicateSecondLost = Outcome{ID: "association-lost", Verdict: Deviation, Refs: []string{duplicateFailureRef},
		Summary: "dropped the second association instead of answering its NG SETUP REQUEST"}
)

// Outcomes declares what the check can report.
func (duplicateRANNodeID) Outcomes() []Outcome {
	return []Outcome{duplicateAcceptedKept, duplicateRejectedKept, duplicateAcceptedDropped, duplicateRejectedDropped,
		duplicateNoAnswer, duplicateOtherAnswer, duplicateSecondLost}
}

func (c duplicateRANNodeID) waits() (setup, first time.Duration) {
	setup, first = duplicateSetupWait, duplicateFirstWait
	if c.setupWait > 0 {
		setup = c.setupWait
	}
	if c.firstWait > 0 {
		first = c.firstWait
	}
	return setup, first
}

// Budget covers the two NG Setups, the wait for the second one's outcome,
// the watch on the first association and the liveness probe.
func (c duplicateRANNodeID) Budget() time.Duration {
	setup, first := c.waits()
	return 2*setup + first + 5*time.Second
}

func (duplicateRANNodeID) ID() string         { return "NGAP-DUPLICATE-RAN-NODE-ID" }
func (duplicateRANNodeID) Category() Category { return Procedural }

// Needs declares the second N2 source address the duplicate is sent from,
// beside the check's own gNB ID and the liveness probe's.
func (duplicateRANNodeID) Needs() CheckNeeds {
	return CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, SecondN2Source: true, Procedures: []Procedure{ProcNGSetup}}
}

// SpecRefs cites the description of the NG Setup answer (8.7.1.2), the
// definition of the Global RAN Node ID (9.3.1.5) and the should for an AMF
// that cannot accept a setup (8.7.1.3). None says what an AMF does when
// shown an ID twice.
func (duplicateRANNodeID) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.1.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified, Primary: true,
			Changed: []SpecChange{
				{Release: 19, Note: "in V19.4.0 the same words are a bullet under \"If the NG Setup procedure is executed between the NG-RAN node and the AMF\""},
			}},
		{Spec: "TS 38.413", Clause: "9.3.1.5", Strength: Descriptive, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
		{Spec: "TS 38.413", Clause: "8.7.1.3", Strength: Should, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

func (c duplicateRANNodeID) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG SETUP RESPONSE for an NG SETUP REQUEST, from a second source address, with a Global RAN Node ID a live association already presents, and that first association kept (TS 38.413 8.7.1.2, descriptive; the specification is silent on the duplicate)"}
	if !env.HasSecondSource() {
		return r.Skipped(InputSecondN2, "no second N2 source address configured",
			"the check opens two associations from two source addresses: give the target a second address in ran.n2_source_ips")
	}
	req, err := gnb.BuildNGSetupRequest(env.GNB)
	if err != nil {
		return r.Errored("stimulus could not be built", err.Error())
	}
	first, err := env.DialSetup(ctx)
	if err != nil {
		return r.SetupFailed("setup failed", err)
	}
	defer func() { _ = first.Close() }()
	second, err := env.DialSecond(ctx)
	switch {
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		// The address is in the target's pool but not on this host: the
		// check has nowhere to send the duplicate from.
		return r.Errored("second N2 source address is not on this host",
			fmt.Sprintf("%s cannot be bound here (%v); the second address in ran.n2_source_ips must be one this host holds", env.SecondBind, err))
	case err != nil:
		return r.Errored("second association could not be opened", err.Error())
	}
	defer func() { _ = second.Close() }()

	// NG SETUP REQUEST is non-UE-associated: stream 0.
	if err := gnb.SendPDU(second, sctp.StreamNonUEAssociated, req); err != nil {
		return r.Errored("stimulus could not be sent", err.Error())
	}
	sent := time.Now()
	setupWait, firstWait := c.waits()
	sctx, cancel := context.WithTimeout(ctx, setupWait)
	resp, err := gnb.ReadPDU(sctx, second)
	cancel()
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		return r.Errored("timed out waiting for the second NG Setup's outcome",
			fmt.Sprintf("the per-test timeout expired before the %s the check waits; raise --per-test-timeout", setupWait))
	}
	kind, detail := classifyDuplicateSetup(resp, err, time.Since(sent), setupWait)

	fctx, cancel := context.WithTimeout(ctx, firstWait)
	pdu, err := gnb.ReadPDU(fctx, first)
	cancel()
	if ctx.Err() != nil {
		return r.Errored("timed out watching the first association",
			fmt.Sprintf("the per-test timeout expired before the %s the first association is watched; raise --per-test-timeout", firstWait))
	}
	kept, firstDetail := classifyFirstAssociation(pdu, err, firstWait)

	o, observed := judgeDuplicate(kind, kept)
	detail = joinDetail(detail, firstDetail)
	r = r.As(o, observed, joinDetail(detail, fmt.Sprintf("gNB ID %d presented from %s and then from %s", env.GNB.ID, first.LocalAddr(), second.LocalAddr())))

	// The probe's NG Setup must not meet the two associations: they close
	// first, as they would at the end of the check.
	_ = second.Close()
	_ = first.Close()
	return observeAlive(ctx, env, r, "the duplicate NG SETUP REQUEST")
}

// classifyDuplicateSetup names what the second NG Setup got, one of the
// duplicate* Observed texts, with a detail; after is how long after the
// request it came and wait the bound a missing answer is inferred within.
func classifyDuplicateSetup(resp *ngapType.NGAPPDU, err error, after, wait time.Duration) (kind, detail string) {
	after = after.Round(time.Millisecond)
	switch {
	case err == nil && isNGSetupResponse(resp):
		return duplicateAccepted, fmt.Sprintf("NG SETUP RESPONSE %s after the second NG SETUP REQUEST", after)
	case err == nil && isNGSetupFailure(resp):
		return duplicateRejected, fmt.Sprintf("NG SETUP FAILURE, cause %s, %s after the second NG SETUP REQUEST", ngSetupFailureCause(resp), after)
	case err == nil:
		return duplicateOtherPDU, fmt.Sprintf("answered with %s %s after the second NG SETUP REQUEST, neither NG SETUP RESPONSE nor NG SETUP FAILURE", ngapName(resp), after)
	case errors.Is(err, context.DeadlineExceeded):
		return duplicateUnanswered, fmt.Sprintf("no NG SETUP RESPONSE or FAILURE within %s of the second NG SETUP REQUEST (absence inferred within this bound)", wait)
	default:
		return duplicateReset, fmt.Sprintf("the second association was lost %s after its NG SETUP REQUEST (%v)", after, err)
	}
}

// classifyFirstAssociation reports whether the first association is still
// up after the second NG Setup, from one read on it bounded by wait: no PDU
// within wait, or any PDU, is an association that is up; a read error is
// one that ended.
func classifyFirstAssociation(pdu *ngapType.NGAPPDU, err error, wait time.Duration) (kept bool, detail string) {
	switch {
	case err == nil:
		return true, fmt.Sprintf("the first association is up and the AMF sent %s on it", ngapName(pdu))
	case errors.Is(err, context.DeadlineExceeded):
		return true, fmt.Sprintf("the first association did not end within %s (watched at transport level; no PDU was sent on it)", wait)
	default:
		return false, fmt.Sprintf("the first association ended (%v)", err)
	}
}

// judgeDuplicate is the outcome and Observed text for what the second NG
// Setup got (kind, a duplicate* text) and the first association's fate:
// an answer is INFO with the first association kept and WARN without it;
// no answer, another PDU or a lost second association is DEVIATION either
// way.
func judgeDuplicate(kind string, firstKept bool) (Outcome, string) {
	first := duplicateFirstEnded
	if firstKept {
		first = duplicateFirstKept
	}
	observed := kind + "; " + first
	switch {
	case kind == duplicateAccepted && firstKept:
		return duplicateAcceptedKept, observed
	case kind == duplicateAccepted:
		return duplicateAcceptedDropped, observed
	case kind == duplicateRejected && firstKept:
		return duplicateRejectedKept, observed
	case kind == duplicateRejected:
		return duplicateRejectedDropped, observed
	case kind == duplicateOtherPDU:
		return duplicateOtherAnswer, observed
	case kind == duplicateReset:
		return duplicateSecondLost, observed
	}
	return duplicateNoAnswer, observed
}
