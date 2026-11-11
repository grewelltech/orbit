# Target release

A conformance run is gated on the 3GPP release the core under test claims. A
check that requires a newer release reports SKIP and never FAIL. The target
only decides what is SKIPped, never what passes.

## Default: Rel-18, with TS 29.244 at Rel-16

With no `--target-release` (CLI), or `target_release` 0 (API and dashboard), a
run targets **Rel-18**, with **TS 29.244 (PFCP) pinned to Rel-16**. The
default is a release, not a statement about any core: give
`--target-release` the release the core under test claims, which its
documentation or its codecs' specification versions say.

Caveats:

- TS 29.281 (GTP-U) has no pin.
- A core built on a release's codecs need not behave as that release says. The target only decides what is SKIPped, never what passes.

The target release also names the groups: a check's group is read in the
clause titles of the run's target release, or of the latest release its
primary reference was read in when the target has no such clause or is
unset (ADR-0011 rule 9; [checks.md](checks.md)).

No check registered today SKIPs at the defaults; a test enforces that, so a
check that does has to be listed on purpose.

## How a check is gated

Each reference a check cites is gated on its specification's effective target:

- the pin for that specification, when the pin is older than the run target;
- the run target otherwise. A pin only narrows: `--target-release 15` gates
  TS 29.244 at Rel-15 too;
- nothing at all when the run target is unset.

A check SKIPs when a shall or should reference requires a release newer than
its effective target. A descriptive reference never causes a SKIP (unless the
check cites nothing stronger). A check that declares its own minimum release
SKIPs when that is newer than the run target; the declaration is about the
whole check, so pins do not apply to it. The SKIP detail names what bound
it:

- `requires Rel-19 (TS 38.413 <clause>); target release is Rel-18` for the run target;
- `requires Rel-17 (TS 29.244 <clause>); target for TS 29.244 is Rel-16` for a pin.

Every run states what it used: the text output prints
`target-release: Rel-18 (TS 29.244 at Rel-16)` (or `unset`), `--json` carries
`TargetRelease` and `SpecTargets` (the pins that narrowed the run), and every
API update carries `target_release` and `spec_target_releases`.

## Overriding

- `--target-release Rel-17` (or `17`) replaces the run target; the default pins
  still apply where they are older.
- `--target-release unset` gates nothing: every check runs, pins included.
- Over the API, a non-zero `target_release` replaces the run target. There is
  no way to ask for an unset target over the API yet.

Per-specification pins are not configurable yet; they are the defaults above.

The target release only decides which checks run. The verdict rule and a
core's expected-failures list ([expected-failures.md](expected-failures.md))
apply to whatever ran.
