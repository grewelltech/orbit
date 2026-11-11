# Expected failures

## The verdict rule

The verdicts and what gates are set by
[ADR-0011](../adr/0011-conformance-verdicts-gates-and-grouping.md); in short:

- **FAIL**: a cited **shall** was observably violated. **DEVIATION**: a cited
  **should** was not followed.
- **WARN**: no normative requirement, and the behaviour has security,
  availability or interoperability impact; **INFO**: no requirement and no
  impact.
- **BLOCKED**: the core did not complete a procedure the check needs first
  (counted once, under the check that owns the procedure, when that check ran
  and reported it); **CRASH**: the core stopped answering after the stimulus.
- **PASS**; **SKIP** (not applicable by a declared condition); **ERROR** (the
  tool could not run the check; it says nothing about the core).

Every outcome a check can report is declared with its verdict, and a FAIL or
DEVIATION names the clause it violates. `docs/conformance/checks.md` lists each
check's outcomes.

The default gate fails on FAIL, CRASH, BLOCKED, ERROR, DEVIATION and WARN;
`--gate spec` on FAIL, CRASH, BLOCKED and ERROR only. The verdict word never
changes with the policy.

A core can show a known FAIL, DEVIATION or WARN that the operator has decided
to live with for now. Such a result is recorded on an **expected-failures
list** for that core; it is not softened into a different verdict. The report
still says FAIL (or DEVIATION, or WARN), and the gate counts only the results
that are not on the list.

## The list

The list is one YAML file per core:

```yaml
# Expected conformance failures for one core. Each entry tolerates one
# FAIL, DEVIATION or WARN mode of one check; anything else still gates.
core: Example core (lab A)                  # required, free text
version: "AMF 1.2.3, UPF 4.5.6"             # required, free text
entries:
  - check: NGAP-UNKNOWN-UE-SURVIVES          # required, a registered check ID
    verdict: FAIL                            # FAIL (when left out), DEVIATION or WARN
    observed: "silently dropped; association survived"  # required, the exact Observed text
    finding: EX-1                            # required, the finding that tracks it
    added: 2026-10-12                        # required, YYYY-MM-DD
    reason: "example: the AMF drops the message without ERROR INDICATION"  # required
```

The repository's example is
[`internal/conformance/testdata/expected-failures.yaml`](../../internal/conformance/testdata/expected-failures.yaml).
Its findings and reasons are synthetic. A real core's list is kept by whoever
runs that core.

Loading refuses the whole file, naming the file and the entry index, when:

- `core` or `version` is missing;
- an entry is missing a field, or has an empty one;
- `check` is not a registered check ID;
- `verdict` is not FAIL, DEVIATION or WARN (CRASH, BLOCKED and ERROR always
  gate), or the check never reports that verdict;
- `added` is not a `YYYY-MM-DD` date;
- two entries have the same `check`, `verdict` and `observed`;
- any key is not one of those above.

An empty `entries` list is valid.

## Matching

An entry matches a result only when all of these hold:

- its check ID equals `check`;
- its verdict equals `verdict` (FAIL for an entry that gives none, so a list
  written before entries carried a verdict keeps its meaning);
- its Observed text equals `observed` exactly.

So an entry for a silent drop never hides an `association reset` from the same
check, nor a core that stopped answering afterwards (`AMF unresponsive after
the message (...)`), nor the same text under another verdict. An entry never
matches a PASS, INFO, SKIP, BLOCKED, CRASH or ERROR.

The liveness probes' own texts cannot be listed at all, so loading refuses an
entry whose `observed` starts with any of them:

- `AMF unresponsive after the message`
- `liveness probe could not run`
- `UPF unresponsive after the G-PDU (no Echo Response)`
- `UPF did not answer a GTP-U Echo Request before the test`
- `liveness probe cut short by the per-test timeout` (an ERROR: the per-test
  timeout expired during a probe, so the core's state is unknown; raise
  `--per-test-timeout`)

A crash always gates: it is CRASH in every check.

`GTPU-UNKNOWN-TEID-ERRIND` sends a GTP-U Echo Request before and after its
G-PDU, so `no Error Indication` means a UPF that was alive before and after
and still sent nothing.

- **No Echo Response before:** ERROR. A wrong `--upf-n3` (it must be the
  UPF's primary N3 address) or a UPF without Echo support can never be
  listed.
- **No Echo Response after:** CRASH, which no entry matches.

`GTPU-ECHO-RESPONSE` asserts the Echo Response itself, so there its absence
is the finding: FAIL, `no Echo Response`, which can be listed for a UPF
without Echo handling. That check has nothing else to probe the UPF with, so
such an entry also tolerates a UPF that `--upf-n3` does not reach.

`observed` must be the exact text the check reports, as it appears in the
text output and in the JSON's `observed` field. Some FAIL texts carry values
that change between runs, such as End Marker's `N G-PDU(s) after`; those cannot
be listed precisely. Fixed texts such as `no End Marker on the old F-TEID` can
be.

A matched result keeps its verdict and carries the entry: `expected_failure` (finding,
added, reason) in the JSON and the API. This field is unrelated to `expected`,
which is the behaviour the specification expects of the core.

## The gate

- **Gate:** a run passes when every result its policy gates is matched by an
  entry (BLOCKED, CRASH and ERROR never are).
- **Stale entries:** an entry whose check ran but did not give that verdict
  with that text (it passed, gave another verdict, or the same verdict with
  other text) is **stale**, reported with the verdict it expected. A stale
  entry prints a warning so the list gets pruned. It does not fail the gate.
- **Neither matched nor stale:** an entry whose check was not selected, SKIPped
  (for example under `--target-release`) or ERRORed. That run says nothing about
  whether the failure is gone.
- **No list:** every gated result counts. A core with known gaps stays red until its list
  is given.

## Using it

From the CLI:

```sh
orbit conformance --amf 10.0.0.1:38412 --mcc 001 --mnc 01 \
    --expected-failures lab-a.yaml
```

The CLI reads and checks the file before the run starts, so a bad file fails
the command (exit 3) before anything is dialled. It sends the list in the
run's plan (`ConformanceWorkload.expected`), so the file need not be on the
server's host, and for that run it takes the place of the server's own list. The text output names the list, marks each
tolerated result, warns about stale entries and counts the expected ones:

```text
target-release: Rel-18 (TS 29.244 at Rel-16)
expected-failures: Example core (lab A) (lab-a.yaml)
FAIL (expected: EX-1) [negative-ie] NGAP-UNKNOWN-UE-SURVIVES   silently dropped; association survived
...
stale expected failure: GTPU-UNKNOWN-TEID-ERRIND EX-2 (expected FAIL): now PASS "Error Indication"

5 checks: 3 pass, 1 fail (1 expected), 0 deviation, 0 warn, 0 info, 0 blocked, 0 crash, 1 skip, 0 error
gate default: passed: 0 spec failures, 0 warnings
```

`--json` carries the following:

- `ExpectedFail`, `ExpectedDeviation`, `ExpectedWarn`: the counts of matched
  results.
- `Stale`: the stale entries.
- `ExpectedFailures`: the list's `core` line.
- `expected_failure` on each matched result.

The run's conformance gate fails on a gated result outside the list,
printing `gate conformance: FAIL …` and exiting 1. With `--local`
(deprecated), the error reads `conformance gate <policy>: failed: N spec
failures, M warnings`.

For the server, `orbit serve --conformance-expected-failures lab-a.yaml` loads
a list at startup, and a bad file stops the server from starting. Dashboard and
API runs then apply that list. A request never names a file.

- **Each result:** `expected_failure` on a matched result.
- **Tally:** `expected_failures` counts matched FAILs.
- **Stale entries:** each carries `expected_verdict`.
- **Every update:** `expected_failures_core`, the list's `core` line.
- **Final update:** `stale`, the stale entries.

On the dashboard, a tolerated FAIL keeps the FAIL colour and is tagged
`expected: EX-1`. The tally reads `failed (N expected)`, and stale entries are
listed under it.

The target release and its pins decide which checks run. The list applies to
whatever ran.
