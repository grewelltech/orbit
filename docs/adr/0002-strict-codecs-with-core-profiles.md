# ADR-0002: Strict-by-default codecs; core quirks are opt-in and named

- Status: Accepted
- Date: 2026-07-04 (backfilled 2026-07-07)

## Context

SD-Core's SMF (omec `ngap/v2`) cannot decode a spec-conformant
`HandoverRequestAcknowledgeTransfer`: its generated type drops the `optional`
tag on a spec-OPTIONAL field, so its APER decoder rejects the canonical encoding
("align Bit is not zero"). ORBIT needs to interoperate with such a core **without
becoming SD-Core-specific** — it must stay a general tool that reports the truth
about whatever core it drives (`docs/interop/sdcore.md`).

## Decision

Codecs stay **strict 3GPP/X.691 by default** (the `strict-3gpp` core profile —
zero quirks). Core-specific workarounds are **opt-in, named quirks** applied at
the message-build boundary (`internal/coreprofile`, e.g. the `sdcore` profile),
never baked into the codecs. The **set of quirks a core needs is a conformance
scorecard**, not silent tuning; every quirk names the defect, core, version, and
upstream report, and is deleted when the core is fixed. The conformance suite's
**`DEVIATION`** verdict extends the same principle: a benign spec "shall" the
core misses is *documented*, not hidden and not failed.

## Alternatives considered

- **Fork the codec to match SD-Core** — silently non-conformant; breaks against
  any other/strict core; hides the defect.
- **Emit the workaround unconditionally** — same problem, and it makes ORBIT lie
  about what a conformant peer sends.

## Consequences

- ORBIT stays honest and portable; a conformant core needs no profile and gets
  byte-exact 3GPP.
- Quirks are observable (`Profile.Active()`), upstream-first, and feed the
  conformance scorecard.
- A small amount of per-quirk machinery to maintain; acceptable for the honesty
  it buys.
