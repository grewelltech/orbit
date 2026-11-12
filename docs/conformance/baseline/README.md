# Conformance baselines

A baseline is one run of the default conformance suite against each core ORBIT
is tested with, recorded with the exact ORBIT build and core versions it used.
It shows where each core stands against the checks at that point, so a later
run of ORBIT or of a core can be compared with it. A baseline reports what the
cores did; it is not a ranking of them.

| Baseline | ORBIT build | Checks | Cores |
|---|---|---|---|
| [2026-11 (v0.2.0)](2026-11-v0.2.0.md) | `feat/conformance-adr-0011-part2` at `752210bd` | 41 | SD-Core (main), free5GC v4.3.0, Open5GS v2.8.0 |

## How a baseline is run

Each core runs in its own LXD testbed built by
[`testing/lxd-testbed`](../../../testing/lxd-testbed/README.md), in the
`minimal` shape (one core node, one RAN node), with the profile named for the
core:

```sh
./testbed.sh -p sdcore all     # likewise -p free5gc, -p open5gs
```

On the RAN node, with the testbed's environment loaded, the suite runs once
against the `lxd-conformance` target, with the responder on N6 and the UDP
echo peer for downlink traffic, at the release the core claims:

```sh
set -a; . /etc/orbit/env; set +a
orbit conformance -t lxd-conformance --responder n6 --traffic $TESTBED_ECHO \
  --target-release <release> --json
```

The target release only decides which checks SKIP for being newer than the
core claims; it never changes what passes
([target-release.md](../target-release.md)). No expected-failures list
([expected-failures.md](../expected-failures.md)) is applied to a baseline, so
every non-PASS result is reported as found. An N2 capture is taken on the RAN
node for the length of each run, and the results that are not PASS are
checked against it.

## Reading the verdicts

Every verdict word means the same thing in every check
([ADR-0011](../../adr/0011-conformance-verdicts-gates-and-grouping.md), rule 3):

| Verdict | Meaning |
|---|---|
| PASS | The cited requirement was met. |
| FAIL | A cited "shall" was observably violated. |
| DEVIATION | A cited "should" was not followed. |
| WARN | No normative requirement applies, and the behaviour has a declared security, availability or interoperability impact. |
| INFO | No normative requirement applies; the behaviour is recorded. |
| BLOCKED | The core did not complete a procedure the check needs before its requirement can be tested; the check that owns that procedure carries the finding. |
| CRASH | The core stopped answering after the stimulus. |
| SKIP | Not applicable, by one of the declared conditions (release, missing input, or an alternative the specification permits). |
| ERROR | ORBIT or the testbed could not run the check; it says nothing about the core. |

How a fault that blocks other checks is counted at the gate is in
[ADR-0012](../../adr/0012-blocked-dependents-gate-under-every-policy.md). Each
check's declared outcomes, verdicts and clauses are in
[checks.md](../checks.md), and the quoted specification text behind each check
is in [provenance/](../provenance/).

## What a baseline page leaves out

A result that shows a security-relevant problem in a core, or a crash, is not
published here until the maintainers of that core have been told and have had
time to respond. Where that applies, the check's row is left out of the
baseline table for every core, so the table does not show which core it
concerns, and the page says how many checks were left out.
