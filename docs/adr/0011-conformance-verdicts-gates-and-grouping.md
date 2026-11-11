# ADR-0011: Conformance verdicts, gates and check grouping

- Status: Proposed
- Date: 2026-11-11
- Builds on: ADR-0002 (strict codecs, named core quirks), ADR-0005 (the server owns runs)

## Context

The conformance suite grew from a handful of checks against one core to several
dozen run against three cores of different lineage. That exposed four problems
in how a result is classified and presented.

**FAIL carries three meanings.** A FAIL can mean that a cited requirement was
violated, that the core never got far enough for the requirement to be tested
(it refused a handover the check needed, or did not answer a registration), or
that the core stopped answering. A reader cannot tell which without reading the
detail of each result. The same breakage is also graded differently from check
to check: a refused procedure is ERROR in one, SKIP in another and PASS in a
third.

**Some verdicts have no requirement behind them.** The suite's rule is that
FAIL needs a cited "shall" and DEVIATION a cited "should". Several outcomes
that matter to an operator have neither: the specification describes what the
network does without obliging it, or is silent. Reporting those as a
non-gating INFO-DIFF puts a core that acts on unprotected input in the same
bucket as a harmless quirk.

**Attribution is guessed per check.** When a registration goes unanswered, each
check decides for itself whether the core or the subscriber configuration is at
fault, with its own heuristic.

**Categories mix four axes.** `nas` and `gtpu` are protocols, `mobility` is a
procedure family, `security` is a concern, and `negative-ie` and `procedural`
are kinds of test. Placement is arbitrary as a result, `procedural` is a
catch-all, and `timing` is empty.

A fifth concern follows from running against several cores: a check, its
stimulus or its failure text can quietly encode the behaviour of the one core
it was written against.

The goal is that a user understands a result from the verdict word and the
check's place in the suite, without reading a justification per check.

## Decision

### 1. Three layers

- **Conformance.** Checks that cite a "shall" or a "should". The default run.
- **Characterisation.** Checks where the specification is descriptive or
  silent. They record what a core does. Part of the default run.
- **Core regression packs.** Checks for the behaviour of one named core. A pack
  is never part of the default run and never reports a conformance verdict. A
  pack is keyed by the target's existing core-compatibility profile
  (`core.profile`, ADR-0002), and runs only when the user names it. A pack
  named for a profile other than the target's is refused. A target with no
  profile, or with `strict-3gpp`, has no pack, and naming one is an error
  rather than an empty run.

The test for the first two layers is that the check's provenance file can be
written without naming any core. A check that fails this test belongs in a
pack.

### 2. The class comes from the specification text

A check asserts one requirement. The strength of the sentence it cites fixes
the worst verdict it can give: "shall" allows FAIL, "should" allows DEVIATION,
descriptive or silent text allows WARN or INFO.

The rule is enforced per outcome, not per check. Every outcome a check can
report is declared with its verdict, and a FAIL or DEVIATION outcome names the
reference it violates. The suite refuses to register a check with a FAIL
outcome that cites no "shall", or a DEVIATION outcome that cites no "should".
Neither verdict can exist without its clause.

Every check cites at least one reference, including a characterisation check:
where the specification is silent on the behaviour observed, the check cites,
as descriptive, the clause of the procedure its stimulus belongs to. Exactly
one of a check's references is marked primary in its declaration.

### 3. One vocabulary

| Verdict | Meaning | About | Gates (default / spec) |
|---|---|---|---|
| PASS | The cited requirement was met. | core | no / no |
| FAIL | A cited "shall" was observably violated. | core | yes / yes |
| DEVIATION | A cited "should" was not followed. | core | yes / no |
| WARN | No normative requirement, and the observed behaviour has security or operational impact. | core | yes / no |
| INFO | No normative requirement; behaviour recorded, no impact. | core | no / no |
| BLOCKED | The core did not complete a procedure this check needs before its requirement can be tested. | core | yes / yes |
| CRASH | The core stopped answering after the stimulus. | core | yes / yes |
| SKIP | Not applicable by a declared condition. | neither | no / no |
| ERROR | The tool could not run the check. | ORBIT or testbed | yes / yes |

Each word has this meaning in every check, in the CLI, the JSON output, the
API and the dashboard. INFO-DIFF is removed: whether the core did what the
descriptive text describes is stated in the observed text.

SKIP is limited to three declared conditions: the core's claimed release is
older than the check requires, an input the check needs is not configured (a
UE, a responder, a second address), or the specification explicitly permits
the alternative the core chose.

ERROR says nothing about the core. It tells the user to fix the setup.

### 4. WARN is declared, from a closed list

An outcome is WARN only if it meets one of these criteria:

- **Security.** The core acts on unprotected or unauthenticated input, omits or
  weakens a protection, or accepts something it had the means to reject as
  forged or replayed.
- **Availability.** The core drops or ignores a valid request, loses UE or
  session state, or cannot complete a procedure.
- **Interoperability.** The behaviour breaks a peer that is itself conformant.

The criterion is declared when the check is written, per outcome, and shown in
the generated checks table. Nothing decides it at run time. An outcome that
meets none of the criteria is INFO.

### 5. Every procedure has one owner check

When checks depend on a procedure completing (a registration, a PDU session, an
AN release, an N2 handover preparation), exactly one check owns the question of
whether that procedure completes. Its class follows rule 2. A dependent check
whose prerequisite does not complete reports BLOCKED, naming the procedure and
the owner check. The same breakage is graded once, in one place.

Ownership is declared, from a fixed list of procedure names. A check declares
the procedures it owns and the procedures it needs. The suite refuses to
register when a procedure has two owners, or when a check needs a procedure no
registered check owns, so a renamed or removed owner fails at start-up, not in
a result.

One core fault counts once at the gate. When the owner ran in the same run and
reported the procedure as not completing, its result is the gated finding, and
the BLOCKED results that name it are listed under it and are not counted again.
A BLOCKED result gates on its own only when its owner did not run in that run
(it was not selected, or was skipped) or reported the procedure as completing.

### 6. Attribution is decided once per run

A run starts with a preflight built from what the selected checks declare
they need. Every run completes an NG Setup. A run that selects a check needing
a registered UE also registers and deregisters one; a PDU session is
established only if a selected check needs one, and the UPF is probed only if
one needs the user plane. A run with no UE configured, or that selects only
checks needing none, does the NG Setup alone, and checks that need a UE SKIP
for the missing input as they do today.

If a preflight step fails, the run reports ERROR for the checks that depend on
that step and judges the rest. If it passes, a later failure of the same kind
of setup inside a check is attributed to the core and reported BLOCKED. Checks
do not carry their own attribution heuristics.

### 7. Absence is judged against a declared bound

"No answer" is a violation only where the requirement is to send something. The
wait is a timer from the specification or a bound stated in the check, and the
result says the absence was inferred within that bound.

### 8. Two gate policies

The verdict word never changes with the policy. Only the gate does.

- **default** gates on FAIL, CRASH, BLOCKED, ERROR, DEVIATION and WARN.
- **spec** (`--gate spec`) gates on FAIL, CRASH, BLOCKED and ERROR.

The summary line separates the two, for example "3 spec failures, 2 warnings",
so a run that is red with no violated requirement says so on its face.

An expected-failures list may name FAIL, DEVIATION and WARN outcomes; it may
not name CRASH, BLOCKED or ERROR. An entry carries the verdict it expects next
to the check and the observed text, and matches only a result with all three.
An entry with no verdict expects FAIL, so an existing list keeps its meaning. A
result that matches an entry keeps its verdict word, is marked expected, and is
left out of the gate count, as a listed FAIL is today. An entry that matches no
result is reported as stale, per verdict, as today.

### 9. Checks are grouped by the specification's own titles

A check's group is derived from its primary reference (rule 2): specification,
then the procedure family, then the procedure, using the clause titles. For example
`NGAP / UE Mobility Management / Handover Preparation` or
`NAS / 5GMM common procedures / Security mode control`.

- The mapping from clause to title lives in one table in the repository, taken
  from the specification text per release. A check marks which reference is
  primary; it does not name its group.
- The title is the one in the run's target release. When that release has no
  such clause, or the run has no target release (`--target-release unset`),
  the latest release in which the reference was read is used, so a check has
  one group in any run.
- A check whose primary citation is not a procedure (handling of erroneous
  protocol data, NAS security, TS 33.501) is grouped under that clause's title.
- These groups replace the `--category` values. Selection is by group path or
  any prefix of it, and by check ID.
- The kind of test is a separate, optional tag (`negative`, `security`,
  `timing`), for filtering only. Tags never affect a verdict.

Run phases (main, stranding, disruptive) are unchanged and independent of
groups.

### 10. Core neutrality

- A stimulus is defined by the requirement it exercises, with parameters
  justified from the specification or stated as a sweep. It is not tuned to one
  core's observed behaviour.
- Failure text states the requirement and what was observed. A "likely cause"
  line names no core and no core's source.
- The default target release is stated as a release, not as a core's release.

## Alternatives considered

- **FAIL "by rule" with no clause cited**, for a core that does not carry out
  the procedure under test. Rejected: it makes FAIL mean two things again and
  invites a challenge the suite cannot answer with a clause.
- **Report a core-refused prerequisite as SKIP or ERROR.** Rejected: SKIP hides
  a core fault and ERROR blames the tool.
- **Keep INFO and INFO-DIFF, non-gating.** Rejected: too soft for behaviour
  with security or operational impact, and "differs" is not the property a
  reader cares about.
- **WARN that never gates.** Rejected as the default: a result that does not
  affect the exit code is ignored in automation. Kept as the `spec` policy.
- **A `--strict` flag that turns warnings off.** Rejected: "strict" reads as
  "more failures", the opposite of what it would do.
- **Per-check attribution heuristics.** Rejected: each check drew the line
  differently, which is the inconsistency this ADR removes.
- **Groups by clause number.** Rejected: not readable. **Hand-assigned
  categories.** Rejected: they drift and mix axes.

## Consequences

- A reader learns nine words once. A red run states whether a requirement was
  violated, a warning fired, the core blocked the check, or the tool failed.
- Exit codes change. DEVIATION gates by default where it did not, and a WARN
  gates where an INFO-DIFF did not. A run that passed can now fail; `--gate
  spec` gives the narrower answer.
- The verdict strings on the API and in JSON change: INFO-DIFF goes, WARN,
  BLOCKED and CRASH arrive. The change to the proto is additive for the new
  values; consumers that switch on the verdict string need the new cases.
- `--category` values are replaced. Run files and scripts that name a category
  need the group path.
- Every existing check is re-declared outcome by outcome. Outcomes that are
  FAIL today without a cited "shall" become BLOCKED, WARN or INFO, or gain a
  citation.
- Owner checks are added for each procedure that other checks depend on, and
  the preflight is added to the run.
- The clause-title table must be read from the specification text for each
  release in scope before groups are generated; clause numbers moved between
  releases.
- WARN depends on the three criteria being applied consistently. The table of
  declared outcomes is the review surface for that, and a new criterion needs
  an ADR.
- Known limit: the preflight proves the setup worked once. A core that degrades
  during a run is reported BLOCKED from then on, which is the intended reading
  but depends on the preflight being representative.
