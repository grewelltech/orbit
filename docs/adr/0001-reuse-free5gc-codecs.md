# ADR-0001: Reuse free5gc codecs; build the differentiators clean-room

- Status: Accepted
- Date: 2026-07-02 (backfilled 2026-07-07)

## Context

ORBIT speaks real 5G control and user planes (NGAP/SCTP, NAS-5GS, GTP-U). The
protocol codecs are large, spec-exact, and unforgiving — APER (X.691) has
exactly one legal encoding per value, and a single wrong tag makes a peer
undecodable. Writing them from scratch would consume the whole project and add
no differentiation. Mature Go implementations exist: **free5gc**'s
`ngap`/`nas`/`aper` are Apache-2.0 and field-proven. **UERANSIM** is a strong
behavioral reference but is **AGPL-3.0**.

## Decision

Reuse **free5gc**'s `ngap`, `nas`, and `aper` (and its `util` crypto:
milenage, ueauth, nas/security) as the codec and crypto substrate. Build only
the **differentiating** layers clean-room from the 3GPP specs: the gNB/UE state
machines, mobility synthesis, the conformance harness, and the API. Treat
**UERANSIM as a behavioral reference only — never copy its code** (AGPL).

## Alternatives considered

- **Write codecs from scratch** — spec-accurate APER/NAS is months of work with
  high defect risk; no user value over free5gc.
- **Use omec-project forks as the base** — what SD-Core field-uses, but they
  carry real divergences (see ADR-0002); kept as a *reference/decode* option,
  not the base.

## Consequences

- Fast path to spec-accurate wire behavior; effort concentrates on the parts
  that make ORBIT distinct.
- ORBIT tracks free5gc's release coverage (~Rel-15/16, some 17) — newer-release
  IEs need codec work first (bounds the conformance suite's release reach).
- The grounding rule stands: every protocol claim is verified against a TS, real
  library source, or a live capture — never memory.
