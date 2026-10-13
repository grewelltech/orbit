# Expected failures

## The verdict rule

A conformance check scores what the specification says:

- **FAIL**: the core missed a **shall**. This includes a missed shall that
  leaves the core running, such as a silent drop where an ERROR INDICATION is
  required.
- **DEVIATION**: the core missed a **should**. It does not fail the gate.
- **PASS**, **ERROR** (the check could not run) and **SKIP** (not applicable,
  or newer than the target release) are unchanged.

A check's worst verdict comes from the clauses it cites: FAIL if it cites a
shall, DEVIATION if it cites only shoulds. The suite enforces this whatever a
check reports. `docs/conformance/checks.md` lists each check's verdict class.

A core can miss a shall for a known reason that the operator has decided to
live with for now. Such a failure is recorded on an **expected-failures list**
for that core; it is not softened into a different verdict. The report still
says FAIL, and the gate fails only on FAILs that are not on the list.

## The list

The list is one YAML file per core:

```yaml
# Expected conformance failures for one core. Each entry tolerates one
# FAIL mode of one check; anything else still fails the gate.
core: Example core (lab A)                  # required, free text
version: "AMF 1.2.3, UPF 4.5.6"             # required, free text
entries:
  - check: NGAP-UNKNOWN-UE-SURVIVES          # required, a registered check ID
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
- `added` is not a `YYYY-MM-DD` date;
- two entries have the same `check` and `observed`;
- any key is not one of those above.

An empty `entries` list is valid.

## Matching

An entry matches a result only when all of these hold:

- the result's verdict is **FAIL**;
- its check ID equals `check`;
- its Observed text equals `observed` exactly.

So an entry for a silent drop never hides an `association reset` from the same
check, nor a core that stopped answering afterwards (`AMF unresponsive after
the message (...)`). An entry never matches an ERROR, a DEVIATION or a PASS.

The liveness probes' own texts cannot be listed at all, so loading refuses an
entry whose `observed` starts with any of them:

- `AMF unresponsive after the message`
- `liveness probe could not run`
- `UPF unresponsive after the G-PDU (no Echo Response)`
- `UPF did not answer a GTP-U Echo Request before the test`

A crash always gates. On a check citing only shoulds it stays FAIL rather than
becoming DEVIATION.

`GTPU-UNKNOWN-TEID-ERRIND` sends a GTP-U Echo Request before and after its
G-PDU, so `no Error Indication` means a UPF that was alive before and after
and still sent nothing.

- **No Echo Response before:** ERROR. A wrong `--upf-n3` (it must be the
  UPF's primary N3 address) or a UPF without Echo support can never be
  listed.
- **No Echo Response after:** FAIL with the unresponsive text, which no entry
  matches.

`observed` must be the exact text the check reports, as it appears in the
text output and in the JSON's `observed` field. Some FAIL texts carry values
that change between runs, such as End Marker's `N G-PDU(s) after`; those cannot
be listed precisely. Fixed texts such as `no End Marker on the old F-TEID` can
be.

A matched result stays FAIL and carries the entry: `expected_failure` (finding,
added, reason) in the JSON and the API. This field is unrelated to `expected`,
which is the behaviour the specification expects of the core.

## The gate

- **Gate:** a run passes when every FAIL is matched by an entry and nothing
  ERRORed.
- **Stale entries:** an entry whose check ran but did not give that FAIL (it
  passed, deviated, or failed differently) is **stale**. A stale entry prints a
  warning so the list gets pruned. It does not fail the gate.
- **Neither matched nor stale:** an entry whose check was not selected, SKIPped
  (for example under `--target-release`) or ERRORed. That run says nothing about
  whether the failure is gone.
- **No list:** every FAIL gates. A core with known gaps stays red until its list
  is given.

## Using it

From the CLI:

```sh
orbit conformance --amf 10.0.0.1:38412 --mcc 001 --mnc 01 \
    --expected-failures lab-a.yaml
```

The file is loaded before any check runs, so a bad file fails the command
before anything is dialled. The text output names the list, marks each
tolerated FAIL, warns about stale entries and counts the expected FAILs:

```text
target-release: Rel-18 (TS 29.244 at Rel-16)
expected-failures: Example core (lab A) (lab-a.yaml)
FAIL (expected: EX-1) [negative-ie] NGAP-UNKNOWN-UE-SURVIVES   silently dropped; association survived
...
stale expected failure: GTPU-UNKNOWN-TEID-ERRIND EX-2: now PASS "Error Indication"

5 checks: 3 pass, 1 fail (1 expected), 0 deviation, 0 error, 1 skip
```

`--json` carries the following:

- `ExpectedFail`: the count of matched FAILs.
- `Stale`: the stale entries.
- `ExpectedFailures`: the list's `core` line.
- `expected_failure` on each matched result.

The gate error reads `conformance gate failed: F fail (E expected), R error`.

For the server, `orbit serve --conformance-expected-failures lab-a.yaml` loads
a list at startup, and a bad file stops the server from starting. Dashboard and
API runs then apply that list. A request never names a file.

- **Each result:** `expected_failure` on a matched result.
- **Tally:** `expected_failures` counts matched FAILs.
- **Every update:** `expected_failures_core`, the list's `core` line.
- **Final update:** `stale`, the stale entries.

On the dashboard, a tolerated FAIL keeps the FAIL colour and is tagged
`expected: EX-1`. The tally reads `failed (N expected)`, and stale entries are
listed under it.

The target release and its pins decide which checks run. The list applies to
whatever ran.
