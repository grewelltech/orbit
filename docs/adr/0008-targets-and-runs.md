# ADR-0008: Targets and Runs

- Status: Accepted
- Date: 2026-10-26

## Context

Each way of running a test is configured differently today:

| Entry point | Configured by | Executes |
|---|---|---|
| `orbit conformance` | ~10–12 flags + env | in the CLI process; the UI uses a separate `ConformanceService`; never archived |
| `orbit load` / `runs start-load` | ~9–12 flags + env; SLO gate only on `load` | CLI process / server (`LoadRunSpec`) |
| `orbit run fleet.yaml` / `runs start-fleet` | 50+ lines of YAML; keys via env/flags | CLI process / server (`FleetRunSpec`) |
| `orbit run <steps.yaml>` | YAML | the CLI drives the API; no run identity |

Every command and every example file restates the same facts: the AMF, PLMN, TAC,
slice and DNN, the SUPI range and keys, the N2/N3 source addresses, and the N6
responder. The examples hard-code the LXD testbed's addresses. Load and fleet are
separate commands, protos and code paths, but they differ only in workload shape
and in how long the run lasts. The web UI can start conformance runs, but not load
or fleet runs, and #72 is blocked on how the browser should reference credentials.

ADR-0005 made the server own run execution, and `RunService` now does so for load
and fleet runs. Two pieces of that ADR were never finished. Conformance and step
scenarios still run outside it. And the registry's interim policy of one active
run per kind (`internal/engine/runregistry.go`, `activeOfKindLocked`) stands in
for the real constraint: two runs conflict only when they need the same SUPIs,
gNB IDs or source addresses.

The maintainer settled these decisions in the usability plan
(approved 2026-10-26). This
ADR records them and does not reopen them:

1. A **target** describes *where* a test runs: the core, the subscriber pool, the
   RAN address pools and the N6 responder. Targets live only on the client, as CLI
   files or in browser storage. Each run carries its resolved target inline over a
   secure channel. These are test subscribers, not operational secrets. The server
   stores no targets, and archives keep a redacted copy only.
2. There is one kind of thing: a **Run** `{ target, workloads[], until, gates }`.
   How long a run lives and what it tests are properties of the run. Load and
   fleet merge into a single *population* workload. Conformance becomes a workload.
   `RunKind` becomes a derived display label. The old oneof arms stay for proto
   compatibility.
3. Configuration is layered: built-in default < target < run file < env < flag.
   The resolver records which layer set each field and what that overrode.
4. The server **leases** SUPIs, gNB IDs and source IPs. This replaces "one active
   run per kind".
5. The app side is set up by join (ADR-0009). UE → UPF → N6 is always the primary
   traffic path.
6. Multi-host `orbit node` agents are a later ADR. The schema leaves room for them.

## Decision

### 1. The target schema

A target is a versioned YAML document. The same schema is used in a CLI file, in
browser storage, and on the wire as `ResolvedTarget`.

```yaml
version: 1
kind: target
name: lab1
core:
  amf: 192.0.2.10:38412           # host:port
  plmn: { mcc: "001", mnc: "01" } # strings; leading zeros kept
  tac: 1
  slices:                         # first is the default for PDU sessions
    - { sst: 1, sd: "000001" }
  dnn: internet
  profile: sdcore                 # internal/coreprofile name; default strict-3gpp
subscribers:                      # a list of pools from v1 on
  - name: main
    first_supi: "001010000000001"
    count: 100
    ki:  ${ORBIT_KI}              # expanded when the client loads the file
    opc: ${ORBIT_OPC}
ran:
  node: ""                        # reserved; "" = the server's own host (ADR on orbit node)
  n2_source_ips: [192.0.2.50-192.0.2.59]       # addresses, ranges or CIDRs
  n3_ips: [198.51.100.50]
  gnb_ids: { first: 100, count: 900, bits: 24 }   # optional; else the server's default range
app:
  responders:                     # referenced by workloads by name
    - name: app1                  # a responder registered by join (ADR-0009)
    - name: lab-loomd             # or a static one, for stock loomd
      control: 203.0.113.20:9551
      token: ${ORBIT_LOOM_TOKEN}
      data_ip: 203.0.113.21
```

Rules:

- **Loading.** `-t/--target <name>` or `$ORBIT_TARGET` selects a target. It is
  looked up in `./orbit.yaml` (which may hold one target or a list of them), then
  in `~/.config/orbit/targets/<name>.yaml`. A run file can name a target
  (`target: lab1`) or embed one inline. The highest layer that names a target
  picks it, the same as any other field.
- **`${ENV}` expansion** happens when the client loads the file. It is
  substitution, not the env layer: provenance attributes the value to the target
  layer and notes the variable (`target lab1 via ${ORBIT_KI}`).
- **Secret fields** are tagged in the schema (`ki`, `opc`, `app.responders[].token`).
  Archives, logs, `Get`/`List` replies and provenance all mask them. No other
  field is masked: SUPI ranges and addresses are kept, because "why did this run
  use X" needs them.
- **Room for nodes.** Addresses belong to `ran.node`. In v1 only `""` (the
  server's own host) is accepted, and any other value is rejected with a pointer
  to the future ADR. That ADR fills it in without renaming anything.
- **Unknown keys are errors.** This is strict, as for scenario files.
  `version` gates future changes.
- The existing fleet and step YAML accept `target:` in place of their
  `core`/`credentials`/addressing blocks. Setting both is an error, not a merge.
  (The fleet schema already uses `target` inside `latency` and `traffic` for an
  echo IP. The top-level key doesn't clash, but the docs should say so.)

### 2. The Run (proto)

`Run` is already taken: it is the identity and lifecycle message. The new spec
message is therefore named **`RunPlan`**. In prose and in the UI it is still "a
run".

```proto
message StartRunRequest {
  string name = 1;
  oneof spec {
    LoadRunSpec load = 2;    // deprecated: translated into a RunPlan server-side
    FleetRunSpec fleet = 3;  // deprecated: translated into a RunPlan server-side
    RunPlan plan = 5;
  }
  EventVerbosity event_verbosity = 4;
}

message RunPlan {
  ResolvedTarget target = 1;            // inline, keys included (on the wire only)
  repeated Workload workloads = 2;      // at least one; names unique
  Until until = 3;
  repeated Gate gates = 4;
  repeated FieldProvenance provenance = 5;  // from the client's resolver; informational
}

message Workload {
  string name = 1;
  StartCondition start = 2;             // default: when the run starts
  oneof kind {
    PopulationWorkload population = 10;
    ConformanceWorkload conformance = 11;
    StepsWorkload steps = 12;
  }
}

message PopulationWorkload {            // today's load and fleet, merged
  AttachProfile attach = 1;             // count, rate, ramp, arrival (uniform|poisson), concurrency
  Topology topology = 2;                // gNB count, layout, spacing; addresses come from the target
  bool pdu_session = 3;
  Behaviors behaviors = 4;              // mobility, traffic, latency, bulk; empty = a load test
  string subscriber_pool = 5;           // pool name; default the first
}

message ConformanceWorkload {
  repeated string categories = 1;
  string target_release = 2;
  ExpectedFailures expected = 3;
  uint32 per_test_timeout_ms = 4;
  NASAlgorithms nas = 5;                // #184's flags, as fields
  string responder = 6;                 // for checks that need traffic
}

message StepsWorkload { string scenario_yaml = 1; }   // ADR-0003 schema, minus core/credentials

message StartCondition {                // e.g. conformance under load
  string after = 1;                     // workload name
  Phase phase = 2;                      // STARTED | ATTACHED | COMPLETE
}

message Until {
  oneof stop {
    uint64 duration_ms = 1;
    bool complete = 2;                  // every workload has finished
    bool forever = 3;                   // only an explicit StopRun ends it
  }
}

message Gate {                          // evaluated server-side, in the report
  string workload = 1;                  // "" = the whole run
  oneof check {
    SLOGate slo = 2;                    // metric, op, threshold (reg-p99 <= 500ms, ...)
    ConformanceGate conformance = 3;    // no FAIL outside the expected-failures list
  }
}
```

Semantics:

- **One-off vs long-running** is just `until`. The plan's `until: count` is
  `attach.count` together with `until: complete`; it isn't a separate stop
  condition. Validation rejects a plan that can never finish: `complete` with a
  population that has behaviours, since behaviours run until stopped.
- **The old arms are translated server-side into a `RunPlan`** (one workload, the
  same target fields), so there is one execution path. They stay in the proto,
  marked deprecated, and are never removed in v1.
- **`RunKind` is derived.**
  - A single population with no behaviours is LOAD. With behaviours it is FLEET.
  - A single conformance workload is CONFORMANCE, and a single steps workload is
    SCENARIO.
  - Anything with more than one workload is the new `RUN_KIND_MIXED = 5`.
  - `ListRuns(kind)` matches a run if any of its workloads has that kind.
- **Conformance becomes a workload.** It gets an identity, history, archiving and
  reports. `ConformanceService.RunConformance` stays for compatibility, as a thin
  wrapper: it starts a one-workload plan and streams the run's events as
  `ConformanceUpdate`.
- **Gates.** `RunReport` gains a `repeated GateResult` and an overall `gates_passed`.
  A new `--wait` flag on the thin clients maps them to exit codes, using the exit
  codes `orbit load` gives today when its SLO is breached (it runs in-process and
  exits non-zero). Conformance gets the same mapping unchanged: an unexpected FAIL
  is a gate failure.
- **Archive.** `RunArchive` gains the redacted `RunPlan`, the provenance and the
  leases (§5), plus a per-workload `RunReport`. These are additive fields within
  archive version 1.
- **Typed, not YAML.** `Behaviors` is a typed proto, not YAML, so the UI can build
  it. The fleet YAML parser is changed to produce that typed form, so the file
  schema and the wire schema can't drift apart. `StepsWorkload` keeps YAML,
  because step scenarios are a document and not a form.

### 3. Precedence and provenance

Layers, lowest first: **default < target < run file < env < flag**. `--set
path=value` belongs to the flag layer. Every existing flag keeps working in that
layer, so no command line changes meaning.

- **Fields have canonical paths** (`core.plmn.mcc`, `subscribers[main].ki`,
  `workloads[pop].attach.rate`). Every existing env var and flag maps to one path:
  `ORBIT_MCC` and `--mcc` both map to `core.plmn.mcc`. The mapping table lives in
  `internal/target` and is tested in both directions.
- **Merge rules.** Scalars replace. Maps merge key by key. Lists replace the whole
  list and never merge element-wise, so "which layer set this pool" always has one
  answer. Named lists (`subscribers`, `responders`, `workloads`) merge by `name`.
- **Provenance.** For every field that ends up set, the resolver records:
  - its final layer;
  - the source: a file path and line, an env var name, or a flag name;
  - its value, masked if it's a secret;
  - the entries it overrode, in order.

  The model is a `FieldProvenance { path, layer, source, value, overridden[] }`
  list.
- **One resolver.** It lives in `internal/target`, written in Go.
  - The CLI calls it in-process.
  - The web UI calls a stateless `TargetService.Resolve(layers) → (ResolvedTarget,
    provenance)`. It sends its own layers: default, the stored target, and the
    Launch form as the run-file layer. The browser has no env layer, and it has
    no flag layer unless the user enters `--set` overrides.
  - The server keeps nothing from a `Resolve` call.

  This avoids a second resolver in TypeScript that would drift from the first.
- **Where provenance surfaces:**
  - `--explain` on `orbit test …` and `orbit run …`;
  - `orbit target show --resolved`;
  - the run archive;
  - a "Resolved config" panel on Launch and on each run's detail view.

  An env value that overrides a run-file value also prints a one-line notice
  under `--explain` and in `--wait` output. That catches a forgotten
  `ORBIT_MCC` in the shell without changing the agreed order.
- **Leases are not a layer.** Values the server allocates (§5) appear in
  provenance with source `lease`. They fill only fields the layers left unset or
  gave as a pool, and they never override a value a layer set.

### 4. The credential channel

Keys travel inside `StartRun` and `Resolve`, so the channel has to be fit for
them.

- **Loopback stays the default and stays plaintext.** That's h2c, as today
  (`127.0.0.1:8412`).
- **TLS on `orbit serve`:**
  - `--tls-cert/--tls-key`;
  - or `--tls auto`, which generates a self-signed certificate once into
    `--state-dir` and prints its SHA-256 SPKI fingerprint.

  Clients pin it with `orbit server trust <url>`. That shows the fingerprint and
  asks before it stores anything in `~/.config/orbit/servers.yaml`; there is no
  silent trust-on-first-use. A remote browser accepts the `--tls auto`
  certificate with a one-time exception. A real certificate stays optional.
- **Bearer token.**
  - The server reads it with `--token-file`.
  - Clients send `Authorization: Bearer`, from `$ORBIT_TOKEN` or `servers.yaml`.
  - The UI keeps it in browser storage.
  - With a token set, the server answers every RPC except health with
    UNAUTHENTICATED when the token is missing or wrong.
- **Enforcement:**
  - A non-loopback `--listen` without TLS is refused unless the operator passes
    `--insecure-listen`.
  - Even then, the server rejects with FAILED_PRECONDITION any `StartRun` or
    `Resolve` that carries keys over a plaintext non-loopback connection.
  - A non-loopback listener **refuses to start without a token**, unless the
    operator passes `--no-auth`. With `--no-auth`, the server logs a prominent
    warning at startup, as ADR-0007 does for the responder.
- **Server handling:**
  - Keys exist only in the run's memory.
  - `GetRun`, `ListRuns`, archives, logs and events carry the masked form,
    following the `internal/server/run_spec.go` redaction pattern. A test asserts
    that no reply path returns a key.
  - There is no target store.

### 5. Leasing

`RunRegistry` replaces `activeOfKindLocked` with a lease table. Leases are taken
atomically at `StartRun`, after validation and before the run goes to PENDING.
They're released when the run is terminal and its drain has finished (UEs
deregistered, associations closed).

| Resource | Scope (what must be unique) | How it is assigned |
|---|---|---|
| SUPIs | (PLMN, SUPI) | a contiguous block from the named pool, sized by the workload (population `count`; conformance by its checks' UE needs) |
| gNB IDs | (AMF address, PLMN, gNB ID) | from `ran.gnb_ids`, or the server's default range. The least recently used ID comes first, and the rotation state is persisted in `--state-dir`, so a new run never reuses an ID the AMF may still hold (QUICKSTART §9) |
| N2 source IPs | (server host, IP) | one per gNB from `ran.n2_source_ips`, held exclusively |
| N3 IPs | none | shared; TEIDs keep sessions apart |

- A value that a layer pinned (for example an exact `first_supi` with no pool, or
  one fixed source IP) is a request for that exact lease. If another run holds it,
  `StartRun` fails with FAILED_PRECONDITION, naming the resource and the holding
  run. That replaces today's message naming the kind.
- Two runs conflict only when their leases overlap. Conformance under a separate
  load run on the same server works as long as the pools allow it.
- The leases are recorded on the run. They're shown in `GetRun`, archived, and
  appear in provenance as `lease`.
- Leases live in memory. A server restart already ends all runs (`StopAll`), and
  only the gNB ID rotation is persisted.
- `TargetService.Probe` takes leases like a run does, with one SUPI and one gNB.
  That way the doctor can't disturb a live run.

### 6. CLI surface

```
orbit target init|list|show [--resolved]|probe <name>
orbit test conformance|load|fleet [template] -t <name> [--for 2h] [--gate k=v] [--set k=v] [--wait] [--explain]
orbit run <plan.yaml> [--wait] [--explain]
```

- `orbit test` is a sugar layer. It builds a `RunPlan` from a built-in,
  address-free template plus the layers. `orbit load`, `orbit conformance` and
  `orbit run` stay, become thin clients of the same plan, and keep their flags as
  the flag layer.
- The built-in `load` and `fleet` templates gate on attach success ≥ 0.99 and
  nothing else. Latency is reported in the run report but isn't gated unless the
  user adds a `--gate`.
- `testing/lxd-testbed` writes a target file instead of `/etc/orbit/env`.
- The `examples/` files become address-free and say `target:` instead of
  restating addresses.

## Alternatives considered

**A server-side target store.** This would be one place to edit, and the UI would
need no storage. The maintainer rejected it: the server would hold keys at rest, and that
moves it from a test tool towards a credential store.

**Keeping load and fleet as separate kinds, adding conformance as a third.** This
is less migration, but it keeps three code paths, makes "conformance under load"
impossible, and leaves the duration as a kind instead of a property.

**A resolver in TypeScript for the UI.** This needs no RPC, but two
implementations of the precedence rules would drift. The rules are the part users
most need to trust. A stateless `Resolve` keeps one implementation and stores
nothing.

**Env above flags, or env below the run file.** This is settled as default <
target < run file < env < flag. The notice in §3 covers the main risk of the
chosen order without changing it.

**Fleet behaviours carried as YAML on the wire (as `FleetRunSpec` does).** This
reuses the parser, but the UI can't build or show a YAML blob as a form. Typed
messages with one parser producing them keep the file and wire forms the same.

**Mandatory TLS everywhere.** This is simplest to reason about, but it breaks the
loopback developer path and CI on one host for no gain. The rule instead keys on
loopback versus non-loopback.

## Consequences

Easier:

- A run against a known lab is `orbit test conformance -t lab1`.
- The UI can start every workload, which unblocks #72.
- "Why did this run use X" can be answered from the archive.
- Two runs on one server coexist when their resources don't overlap.
- Conformance gets history and reports.
- The examples stop carrying lab addresses.

Harder:

- The lease table becomes a concurrency-sensitive part of the registry. Each
  workload has to declare its resource needs up front, so conformance must know
  its UE count before it starts.
- Typing `Behaviors` in proto is real work, and the fleet parser has to emit the
  typed form without changing what any existing file means.
- TLS and token handling add a configuration surface and a test matrix: loopback,
  TLS, token, and the combinations of the three.
- Every flag and env var needs an entry in the path-mapping table. A missing one
  shows up as a silent default, so the table needs a test that walks every
  registered flag.

Accepted limits:

- Keys sit in plaintext on the client: in target files (or the environment they
  expand from) and in browser storage. That's acceptable because these are test
  subscribers.
- Provenance sent by the client is informational. The server can't verify it.

Follow-up work, in the roadmap order: T1–T4 (client targets), S1–S4 (channel,
leasing, `Resolve`/`Probe`), R1–R5 (RunPlan), J1–J4 (ADR-0009), U1–U4 (UI). A
later ADR covers `orbit node`.

## Resolved questions

The maintainer answered these on 2026-10-26, when accepting this ADR. The decisions above
already reflect them.

| # | Question | Decision |
|---|---|---|
| 2 | Self-signed `--tls auto` for remote browsers? | Allowed, with a one-time browser exception. A real certificate is optional. |
| 3 | Token on a non-loopback listener? | Required. The listener refuses to start without one, unless `--no-auth` is given. |
| 4 | Default gates for the `load`/`fleet` templates? | Attach success ≥ 0.99 only. Latency is reported, not gated. |
| 8 | The name of the spec message? | `RunPlan`. |

## Open questions

These are resolved in the implementing step. None of them changes a decision
above.

| # | Question | Owner |
|---|---|---|
| 1 | Should N2 source IPs be exclusive per gNB (as proposed) or shareable with ephemeral ports? It depends on how the AMFs we target key NG associations. | architect, with a review of the target AMFs' source (S4) |
| 5 | Confirm the exit codes `orbit load` gives today on an SLO breach, so `--wait`'s gate mapping preserves them exactly. | implementer (R4) |
| 6 | Conformance needs: a static UE count per check, or a declared maximum per category? | conformance owner, implementer (R3) |
| 7 | The default gNB ID range, and how many recent IDs rotation avoids. | architect (S4) |
| 9 | Recurring conformance inside a long run (the roadmap's "every 15m" for R3) isn't expressible with `StartCondition` alone. Add a `repeat_every` to `Workload`, or leave it out of v1? | architect (R3) |
