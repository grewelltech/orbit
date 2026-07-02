# ORBIT — instructions for Claude

**ORBIT** (Open Radio Benchmark and Integration Testbed) is a from-scratch,
Go-native, PHY-less 5G SA gNB + UE simulator and test harness for benchmarking,
conformance-checking, and integration-testing a real 5G core (Aether SD-Core). It
speaks the real control and user planes — NGAP/SCTP (N2), NAS-5GS (N1), GTP-U (N3)
— with no RF PHY.

## Read first — the source of truth

**`docs/DESIGN.md`** is the grounded master plan: cellular primer, architecture,
reuse-vs-build table, pivotal decisions, the phased plan (P0→P7) with per-phase
exit criteria, discovery spikes (D-1..D-11) that gate specific phases, and risks.
Read it fully before writing code; treat its sequencing as the roadmap; **do not
skip a discovery spike that gates a phase**.

## Non-negotiable rules

1. **Grounding rule.** Verify every cellular/protocol claim (message structure,
   IE, procedure, key derivation, wire format) against the relevant 3GPP TS, real
   library source, or a live packet capture — **never from memory** — and cite it
   (TS/section, repo path, URL). Known traps: NGAP SCTP **PPID is the integer 60**
   (not `0x3c000000`); the **GTP-U header is 12 bytes** when carrying the `0x85`
   PDU Session Container + QFI. When unsure, spike it against the real core.
2. **Reuse-first.** Reuse mature Apache-2.0 Go codecs — **free5gc `ngap`/`nas`/`aper`**
   — and build only the differentiating layers clean-room from the specs (gNB/UE
   state machines, mobility synthesis, conformance harness, API). **UERANSIM is
   AGPL-3.0 → behavioral reference only, never copy code.** For the conformance
   decode path, evaluate the `omec-project` ngap/nas forks (what SD-Core
   field-uses) — D-11.
3. **Traffic/performance engine = loom.** Embed **`github.com/bgrewell/loom`**
   (source `~/repos/bgrewell/loom`, active development) as a Go library. If it
   lacks an API or behavior you need, **drive the change in loom** — don't work
   around it. No iperf3.
4. **Honest scope.** Emulated mobility depends on the core actually supporting
   handover — SD-Core likely can't today, so mobility is gated on **D-1** with a
   free5gc fallback and a documented degraded mode. "VIAVI/Spirent-class" is a
   *style*, not a numbers claim: SD-Core's ceiling is ~5,000 UEs @ 10 attach/s, so
   always report **sim-capability** (vs a mock core) separately from
   **integration-capability** (bounded by the core under test). Don't oversell.
5. **Educate as you go.** The maintainer is an experienced tool-builder but a
   cellular novice — when a decision hinges on a cellular concept, explain it
   concretely (no jargon dumps). Build in small, independently-verifiable chunks;
   verify each against reality before layering the next.

## Test target

A live Aether SD-Core runs on a two-node testbed reachable from this host
(grewell01). **Confirm current coordinates before the first NG Setup test.** Last
known: AMF N2 `172.17.50.11:38412`; PLMN mcc=208 mnc=93; TAC 1; S-NSSAI sst=1
sd=010203; DNN `internet`; test IMSIs 208930100007500–599 (Ki/OPc from the
maintainer). Early quick check = **D-8**: confirm default auth (expected 5G-AKA +
null-scheme SUCI) and the SD-Core version.

## Git / GitHub conventions

- Git identity for this path resolves to `bgrewell@gmail.com` via an `includeIf`
  rule — **never override `user.name`/`user.email`**.
- **No AI attribution** in commits, trailers, PRs, or comments. Conventional,
  imperative commit messages describing what changed and why.
- GitHub CLI: both `bgrewell` and `bengrewell` are authed here; for ORBIT GitHub
  ops run `gh auth switch -u bgrewell`, and switch back to `bengrewell` when done.
- Branch per change; keep CI green (`gofmt`/`vet`/`test`/`build`); open a PR; the
  maintainer merges. Binaries build to `bin/` (gitignored) via `make build`.
