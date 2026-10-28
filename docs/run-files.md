# Run files

A run file says what to run: its workloads, when the run stops, and the
gates that judge it (ADR-0008 §2). It holds no addresses or keys. Those
come from a [target](targets.md), so one run file runs against any lab:

```sh
orbit run soak.yaml -t lab1          # waits for the end; exit code is the result
orbit run soak.yaml -t lab1 --detach # starts it, prints the run ID, returns
orbit run soak.yaml -t lab1 --explain  # where each field came from; starts nothing
```

`orbit test <template>` runs a built-in run file (see
[USAGE](USAGE.md#built-in-tests-orbit-test)). `orbit test --list load` prints
one, as a starting point for your own.

## The document

A run file is one YAML document. A second one after `---` is refused, naming
its line, whether it parses or not; `orbit run` used to ignore it.

```yaml
kind: run                  # required: this is a run file, not a scenario
version: 1                 # optional; 1 is the only version
target: lab1               # optional; -t and $ORBIT_TARGET win over it
run:
  name: nightly-soak       # the run's label
  event_verbosity: normal  # or verbose
until: {duration: 2h}      # one of: duration (or duration_ms), complete, forever
workloads:
  - name: pop
    attach: {count: 500, rate: 20, concurrency: 64}
    topology: {gnb_count: 4}
  - name: suite
    start: {after: pop, phase: attached}
    conformance: {categories: [registration], expected: expected-failures.yaml}
gates:
  - name: reg
    workload: pop
    slo: {metric: registration.p99_ms, op: "<=", threshold: 500ms}
  - name: suite
    workload: suite
    conformance: true
```

Unknown keys are errors, and so are the target's own fields (`core`,
`subscribers`, `ran`, `app`): a run file names its target, it does not
restate it. `${ENV}` expands in string values, as in a target file.

### `until`

How the run stops. Give one of:

| Key | Meaning |
|---|---|
| `duration: 2h` (or `duration_ms`) | stop after this long. A storm keeps attaching, cycling its count's SUPIs. A population that keeps its UEs (`behaviors`) holds them until then, even with no behaviours or behaviours that end sooner, then deregisters them |
| `complete: true` | stop when every workload completes (the default) |
| `forever: true` | run until stopped (`orbit runs stop`); gates are judged only on a finish |

### `workloads`

Each workload has a unique `name` and is one of two kinds, decided by the
fields it sets. Setting both kinds' fields is an error.

A **population** attaches the target's subscribers:

| Field | Meaning |
|---|---|
| `attach.count`, `rate`, `concurrency` | UEs, attaches per second (0: as fast as `concurrency` allows), attaches in flight |
| `attach.ramp` | `start:end:seconds`, instead of `rate` |
| `attach.arrival` | `uniform` (the default) or `poisson` |
| `attach.seed`, `distribution`, `distribution_seed` | arrival and gNB-distribution randomness |
| `topology.gnb_count`, `layout`, `spacing_m` | gNBs to spread the UEs over, and their layout |
| `topology.gnb_ids` | `{first: N}` pins the IDs, `{assign: true}` takes them from the server's rotation |
| `pdu_session` | establish a PDU session per UE |
| `behaviors` | keep the UEs and run behaviours on them (a fleet; `{}` keeps them idle until `until.duration`). A whole value: write it in the run file, since `--set` does not reach inside it |
| `subscriber_pool` | which of the target's `subscribers` pools to use, by name |
| `sample_interval_ms` | the resource-sample cadence of a soak |

Whether `behaviors` is there decides what happens to the UEs. A population
without it is a storm: each UE attaches and is released. One with
`behaviors`, even `behaviors: {}`, keeps its UEs and holds them until
`until.duration`.

A **conformance** workload runs the suite, its fields under `conformance`:
`categories`, `target_release`, `per_test_timeout_ms`, `traffic`,
`responder`, `subscriber_pool`, `n3_bind`, `gnb_ids` (as above),
`nas.integrity` and `nas.ciphering` (the UE's NAS algorithms, as
`--nas-integrity` and `--nas-ciphering` give them; a list left out, or
empty, is the default, NIA2 or NEA0), and
`expected`. `expected` is the
[expected-failures list](conformance/expected-failures.md): a path, read
relative to the run file, or the list inline.

A latency probe (`behaviors.latency.target`) echoes an IPv4 address. The
`fleet` template of `orbit test` fills it from the target's first
responder's `data_ip`. A target with no responder, or whose first responder
has no `data_ip`, runs the fleet idle with a note. A `data_ip` that is a
host name, not an IPv4 address, fails the run before it starts, as it does
in a scenario.

`start: {after: <workload>, phase: started | attached | complete}` holds a
workload back until another reaches that phase.

### `gates`

A gate judges the run. A failed gate exits 1 under `--wait`; a workload that
did not complete exits 2. A suite cut short by its deadline or a finish
exits 2, because it did not complete.

- An **SLO gate** compares a population's metric with a threshold:
  `slo: {metric, op, threshold}`. The metrics are `attach.success_ratio`,
  `attach.achieved_rate`, and `<procedure>.<quantile>_ms` (quantiles `p50`,
  `p90`, `p99`, `p999`, `max`), such as `registration.p99_ms`. `op` is
  `<=`, `<`, `>=` or `>`. Left out, it is `>=` for a ratio or rate and `<=`
  for a latency. A latency threshold may be a duration (`500ms`).
- A **conformance gate** (`conformance: true`) fails on a check that failed
  outside the expected list, or on an ERROR.

`workload` scopes a gate to one workload. Left out, it judges the run.

### Gates from the command line

`--gate` adds a gate, or replaces the one of the same name, on every
command that starts a run: `orbit run`, `orbit test`, `orbit runs
start-load` and `start-fleet`.

```sh
--gate 'registration.p99_ms<=500ms'     # named registration_p99_ms
--gate 'reg=registration.p99_ms<=500ms' # named reg
--gate conformance                      # a conformance gate on the run
```

A gate without a name is named after its metric, with `.` as `_`, so
`attach.success_ratio` is `attach_success_ratio`. The `--slo-*` flags are
aliases for these gates:

| Flag | Gate |
|---|---|
| `--slo-min-success 0.99` | `attach.success_ratio>=0.99` |
| `--slo-reg-p99 500ms` | `registration.p99_ms<=500ms` |
| `--slo-attach-p99 1s` | `attach.p99_ms<=1s` |

`--set path=value` sets any run-file field, such as
`--set workloads[pop].attach.count=1000`. The path is the field's path in
this document, with a list element selected by its name. A whole value
(`behaviors`, `start`, `conformance.expected`) is set
whole, as YAML (`--set 'workloads[pop].behaviors={}'`). `--set` does not
reach inside one, so to change a field in it, write it in a run file.

Without gates, exit 0 means the run completed, not that the core passed.
Add gates to judge it.

## From the dashboard

The dashboard's Launch page sends the server a stored target and the run
file it builds, as text, with the `${VAR}` values the tab holds.
`RunService.PlanRun` resolves them as `orbit run` resolves the same files
(no `$ORBIT_*`, and no file read: give an expected-failures list inline)
and builds the plan the page starts.

`RunService.RenderRun` gives the same run as a command:
- `orbit test <template> --set …` when the page started from a template
  and the flags express the changes;
- otherwise `orbit run <target>.run.yaml`, with the run file to save
  beside the command.

Each argument is quoted for a POSIX shell. A secret written out in the run
file is refused: write it as a `${VAR}`.

- **No keys in `--set`.** Both RPCs refuse a `--set` to a key or token
  field, or a whole value with a key in it. They also refuse a `${VAR}` there,
  since `--set` expands none. Keys go in the target.
- **Safe to paste.** A value a terminal would act on when pasted (a control
  character such as Ctrl-C, ESC, CR or a tab), one that would show other text
  than it runs (a bidi or zero-width character, U+2028, U+2029), or an
  argument over 4 KiB never goes in the command:
  - from the run file, the command becomes `orbit run <file>`, and the file
    keeps the value;
  - a `--set` or `--gate` holding one is refused.
- **The fleet's latency probe.** When the fleet template has to go as
  `orbit run <file>`, the file keeps its latency probe as
  `behaviors: {latency: {}}`, as long as the target gives the probe a
  target (a first responder with a `data_ip`, or one registered by join).
  The server fills it from that responder when the run starts. It does the
  same for the plan the page starts. The command gives the same plan
with no `$ORBIT_*` exported, since `--explain` shows what those change.

## Precedence

A field's value comes from the highest of these layers that sets it: the
built-in defaults, the target, the run file (or template), `$ORBIT_*`, then
the flags. `--explain` lists each field with the layer that set it and the
values it overrode. Keys, and any whole-value field holding one (a
`behaviors` cohort's token, say), are masked.

An env var that overrides a value from a file (the target or this run file)
is easy to miss: a forgotten `export ORBIT_MCC` changes every run from that
shell. `--explain`, and a run started without `--detach`, print a notice for
each one:

```text
notice: core.plmn.mcc: env $ORBIT_MCC (208) overrode target lab/orbit.yaml:6 (001)
```

A flag is meant, so it gets no notice. The server keeps the provenance with
the run: `RunService.GetRunConfig` returns a run's plan, every field's
provenance and these notices, during the run and after it, from its archive.
