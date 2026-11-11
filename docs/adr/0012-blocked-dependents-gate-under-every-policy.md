# ADR-0012: A core fault that blocks other checks gates under every policy

- Status: Accepted
- Date: 2026-11-11
- Amends: ADR-0011 (rule 5, counting a fault once at the gate)

## Context

ADR-0011 rule 5 counts one core fault once at the gate. When the check that
owns a procedure reports that the procedure did not complete, its result is the
gated finding, and the BLOCKED results of the checks that needed the procedure
are listed under it and not counted again.

Under the `spec` gate policy that rule has a gap. An owner whose verdict is WARN
(the specification gives the network no obligation to complete the procedure,
for example a refused handover) does not gate under `spec`. Its BLOCKED
dependents are counted under it, so they do not gate either. A spec run then
passes although some of its checks never ran, because the core broke the
procedure they needed.

A strict run cannot claim conformance for checks it could not run. The owner
of the project ruled that such a run must not pass.

## Decision

The fault still counts once. When an owner reports a procedure as not
completing and at least one BLOCKED result is counted under it, the finding
gates under every policy:

- if the owner's own verdict gates under the active policy, the finding gates
  as that verdict, as ADR-0011 rule 5 says;
- otherwise the finding gates once as BLOCKED, and the summary names the owner
  and the number of checks it blocked.

An owner that reports the procedure as not completing with no BLOCKED results
under it is gated by its own verdict alone, as before. Nothing else in rule 5
changes: BLOCKED still comes only from a dependent's own attempt, an owner's
SKIP never produces BLOCKED, and a BLOCKED result whose owner was not selected
or reported the procedure as completing gates on its own.

## Alternatives considered

- **Keep ADR-0011 as written.** Rejected: a spec run would pass with checks
  that never ran because of the core.
- **Gate every BLOCKED result separately under `spec`.** Rejected: one core
  fault would count as many failures as it has dependents, which rule 5 exists
  to prevent.
- **Upgrade the owner's verdict to FAIL when it has dependents.** Rejected: the
  verdict word would depend on which other checks were selected, and ADR-0011
  keeps the word independent of the gate.

## Consequences

- A `--gate spec` run that has an owner WARN with blocked dependents fails, with
  one finding. Runs with no such case gate exactly as under ADR-0011.
- The summary line distinguishes a blocked finding from spec failures and
  warnings.
- Tests cover both policies with an owner whose verdict does and does not gate.
