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
	duplicateReset      = "second association reset"
	duplicateFirstKept  = "first association kept"
	duplicateFirstEnded = "first association dropped"
)

// duplicateRANNodeID holds one association that has completed NG Setup and
// opens a second, from another local address (Env.SecondBind), on which it
// sends an NG SETUP REQUEST with the same Global RAN Node ID. It reports
// what the AMF does with the duplicate: accepts it, rejects it, leaves it
// unanswered or ends the new association; and whether the first association
// is still up afterwards.
//
// TS 38.413 says nothing of a second association presenting a Global RAN
// Node ID already in use: 9.3.1.5 says only that the ID identifies an NG-RAN
// node, and 8.7.1.2 describes the answer to an NG SETUP REQUEST ("The AMF
// responds with an NG SETUP RESPONSE message"). So the check is
// informational. An NG SETUP RESPONSE with the first association kept is
// INFO, the answer 8.7.1.2 describes with nothing else changed; every other
// combination is INFO-DIFF, Observed saying which. Neither is a judgement.
//
// The first association is watched at transport level only, for
// duplicateFirstWait: no PDU is sent on it, so an AMF that keeps the
// association open but no longer serves it is reported as keeping it. With
// no second source address, or one this host does not hold, the check
// SKIPs. Both associations are closed afterwards, and a fresh NG Setup is
// tried (observeAlive): a core that no longer completes it did not survive
// the duplicate and is FAIL.
type duplicateRANNodeID struct {
	// setupWait and firstWait override duplicateSetupWait and
	// duplicateFirstWait; tests shorten them.
	setupWait, firstWait time.Duration
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

func (duplicateRANNodeID) ID() string         { return "NGAP-DUPLICATE-RAN-NODE-ID" }
func (duplicateRANNodeID) Category() Category { return Procedural }

// Needs declares the second N2 source address the duplicate is sent from,
// beside the check's own gNB ID and the liveness probe's.
func (duplicateRANNodeID) Needs() CheckNeeds {
	return CheckNeeds{GNBRoles: []GNBRole{RoleOwn, RoleLiveness}, SecondN2Source: true}
}

// Informational declares the check informational: it cites descriptive
// text only.
func (duplicateRANNodeID) Informational() bool { return true }

// SpecRefs cites the description of the NG Setup answer (8.7.1.2) and the
// definition of the Global RAN Node ID (9.3.1.5). Neither requires anything
// of an AMF that is shown an ID twice.
func (duplicateRANNodeID) SpecRefs() []SpecRef {
	return []SpecRef{
		{Spec: "TS 38.413", Clause: "8.7.1.2", Strength: Descriptive, Since: 15, Verified: ts38413Verified,
			Changed: []SpecChange{
				{Release: 19, Note: "in V19.4.0 the same words are a bullet under \"If the NG Setup procedure is executed between the NG-RAN node and the AMF\""},
			}},
		{Spec: "TS 38.413", Clause: "9.3.1.5", Strength: Descriptive, Since: 15, Verified: ts38413Verified, WordingUnchanged: true},
	}
}

func (c duplicateRANNodeID) Run(ctx context.Context, env Env) Result {
	r := Result{Expected: "NG SETUP RESPONSE for an NG SETUP REQUEST, from a second source address, with a Global RAN Node ID a live association already presents, and that first association kept (TS 38.413 8.7.1.2, descriptive; the specification is silent on the duplicate)"}
	if !env.HasSecondSource() {
		r.Verdict, r.Observed = Skip, "no second N2 source address configured"
		r.Detail = "the check opens two associations from two source addresses: give the target a second address in ran.n2_source_ips"
		return r
	}
	req, err := gnb.BuildNGSetupRequest(env.GNB)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "stimulus could not be built", err.Error()
		return r
	}
	first, err := env.DialSetup(ctx)
	if err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "setup failed", err.Error()
		return r
	}
	defer func() { _ = first.Close() }()
	second, err := env.DialSecond(ctx)
	switch {
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		// The address is in the target's pool but not on this host: the
		// check has nowhere to send the duplicate from.
		r.Verdict, r.Observed = Skip, "second N2 source address is not on this host"
		r.Detail = fmt.Sprintf("%s cannot be bound here (%v); the second address in ran.n2_source_ips must be one this host holds", env.SecondBind, err)
		return r
	case err != nil:
		r.Verdict, r.Observed, r.Detail = Error, "second association could not be opened", err.Error()
		return r
	}
	defer func() { _ = second.Close() }()

	// NG SETUP REQUEST is non-UE-associated: stream 0.
	if err := gnb.SendPDU(second, sctp.StreamNonUEAssociated, req); err != nil {
		r.Verdict, r.Observed, r.Detail = Error, "stimulus could not be sent", err.Error()
		return r
	}
	sent := time.Now()
	setupWait, firstWait := c.waits()
	sctx, cancel := context.WithTimeout(ctx, setupWait)
	resp, err := gnb.ReadPDU(sctx, second)
	cancel()
	outcome, detail := classifyDuplicateSetup(resp, err, time.Since(sent), setupWait)

	fctx, cancel := context.WithTimeout(ctx, firstWait)
	pdu, err := gnb.ReadPDU(fctx, first)
	cancel()
	kept, firstDetail := classifyFirstAssociation(pdu, err, firstWait)

	r.Verdict, r.Observed = judgeDuplicate(outcome, kept)
	r.Detail = joinDetail(detail, firstDetail)
	r.Detail = joinDetail(r.Detail, fmt.Sprintf("gNB ID %d presented from %s and then from %s", env.GNB.ID, first.LocalAddr(), second.LocalAddr()))

	// The probe's NG Setup must not meet the two associations: they close
	// first, as they would at the end of the check.
	_ = second.Close()
	_ = first.Close()
	return observeAlive(ctx, env, r, "the duplicate NG SETUP REQUEST")
}

// classifyDuplicateSetup names the outcome of the second NG Setup, one of
// the duplicate* Observed texts, with a detail; after is how long after the
// request it came and wait the bound a missing answer is inferred within.
func classifyDuplicateSetup(resp *ngapType.NGAPPDU, err error, after, wait time.Duration) (outcome, detail string) {
	after = after.Round(time.Millisecond)
	switch {
	case err == nil && isNGSetupResponse(resp):
		return duplicateAccepted, fmt.Sprintf("NG SETUP RESPONSE %s after the second NG SETUP REQUEST", after)
	case err == nil && isNGSetupFailure(resp):
		return duplicateRejected, fmt.Sprintf("NG SETUP FAILURE, cause %s, %s after the second NG SETUP REQUEST", ngSetupFailureCause(resp), after)
	case err == nil:
		return duplicateUnanswered, fmt.Sprintf("answered with %s %s after the second NG SETUP REQUEST, neither NG SETUP RESPONSE nor NG SETUP FAILURE", ngapName(resp), after)
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

// judgeDuplicate is the informational verdict and Observed text for the
// second NG Setup's outcome and the first association's fate: INFO only for
// an accepted setup with the first association kept.
func judgeDuplicate(outcome string, firstKept bool) (Verdict, string) {
	first := duplicateFirstEnded
	if firstKept {
		first = duplicateFirstKept
	}
	v := InfoDiff
	if outcome == duplicateAccepted && firstKept {
		v = Info
	}
	return v, outcome + "; " + first
}
