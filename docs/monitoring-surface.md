# Monitoring surface — schema sketch

Companion to [ADR-0005](adr/0005-server-owned-run-execution.md) (the server owns
run execution) and [ADR-0006](adr/0006-live-monitoring-surface.md) (how a client
watches a run).

**Status: sketch.** These definitions are deliberately *not* in `proto/` yet.
`system.proto` records the project rule that services are added "when their
engine layers exist, so schemas freeze against working code rather than
guesses". This file is the design to build against and to argue with; it becomes
`.proto` one service at a time, as the engine layer behind it lands.

## Service shape

```proto
// RunService owns test-run execution. Every client — CLI, web, desktop —
// starts and observes runs through it; no client orchestrates a run in its
// own process (ADR-0005).
service RunService {
  rpc StartRun(StartRunRequest) returns (StartRunResponse) {}
  rpc StopRun(StopRunRequest) returns (StopRunResponse) {}
  rpc ListRuns(ListRunsRequest) returns (ListRunsResponse) {}
  rpc GetRun(GetRunRequest) returns (GetRunResponse) {}

  // Periodic self-contained aggregate snapshots. Frames may be dropped;
  // each frame is complete, so loss costs one sample and never accumulates.
  rpc RunTelemetry(RunTelemetryRequest) returns (stream TelemetryFrame) {}

  // Discrete occurrences, sequenced per run. Gaps are reported explicitly
  // rather than passed off as "nothing happened".
  rpc RunEvents(RunEventsRequest) returns (stream RunEvent) {}

  // The authoritative end-of-run result. Live frames are sampled; this is not.
  rpc GetRunReport(GetRunReportRequest) returns (RunReport) {}
}
```

## Run identity and lifecycle

```proto
enum RunKind { RUN_KIND_UNSPECIFIED = 0; LOAD = 1; FLEET = 2; SCENARIO = 3; CONFORMANCE = 4; }

// PENDING → RUNNING → (DRAINING) → COMPLETE | FAILED | CANCELLED
enum RunState { RUN_STATE_UNSPECIFIED = 0; PENDING = 1; RUNNING = 2; DRAINING = 3;
                COMPLETE = 4; FAILED = 5; CANCELLED = 6; }

message Run {
  string run_id = 1;              // server-assigned
  RunKind kind = 2;
  string name = 3;                // scenario name, or a caller-supplied label
  RunState state = 4;
  int64 started_unix_nano = 5;
  int64 ended_unix_nano = 6;      // 0 while running
  string error = 7;               // set when state = FAILED
  // The spec that produced this run, so a run is reproducible from its record.
  oneof spec { LoadSpec load = 8; FleetSpec fleet = 9; ScenarioSpec scenario = 10; }
}
```

A run outlives the client that started it. `StopRun` is explicit; disconnecting
does not cancel.

## Aggregate frames

Complete snapshot, never a delta. Sized independently of UE count.

```proto
message RunTelemetryRequest {
  string run_id = 1;
  // Requested cadence. The server clamps to a supported range and reports
  // what it chose in the first frame.
  uint32 interval_ms = 2;
}

message TelemetryFrame {
  string run_id = 1;
  int64  unix_nano = 2;
  uint32 interval_ms = 3;          // the cadence the server actually applied
  uint64 frame_seq = 4;            // lets a client detect (not recover) dropped frames

  RunState state = 5;
  int64 elapsed_ms = 6;

  UeStateCounts ues = 7;
  ProcedureRates rates = 8;
  Throughput throughput = 9;
  repeated ProcedureLatency latency = 10;   // one per procedure name
  repeated GnbAggregate gnbs = 11;
  ResourceSample resources = 12;
}

// Population by state. Registration and mobility are orthogonal axes
// (ADR-0006 §6): a handed-over UE is still session-active.
message UeStateCounts {
  uint32 deregistered = 1;
  uint32 registering = 2;
  uint32 registered = 3;
  uint32 session_active = 4;
  uint32 failed = 5;
}

message ProcedureRates {
  double attach_per_sec = 1;
  double detach_per_sec = 2;
  double handover_per_sec = 3;
  // Cumulative outcome counters — rates are derivable, totals are not.
  uint64 attach_attempted = 4;
  uint64 attach_succeeded = 5;
  uint64 attach_failed = 6;
  uint64 handover_attempted = 7;
  uint64 handover_succeeded = 8;
  uint64 handover_failed = 9;
  // The rate the ramp scheduler is currently asking for, so offered can be
  // compared against achieved — the two diverging is the interesting signal.
  double offered_per_sec = 10;
}

// Percentiles read live from the run's HDR histogram. procedure is one of
// "attach", "registration", "pdu_session", "handover".
message ProcedureLatency {
  string procedure = 1;
  uint64 count = 2;
  double p50_ms = 3;
  double p90_ms = 4;
  double p99_ms = 5;
  double p999_ms = 6;
  double max_ms = 7;
}

message Throughput {
  uint64 uplink_bytes = 1;      // cumulative
  uint64 downlink_bytes = 2;
  double uplink_bps = 3;        // over the last interval
  double downlink_bps = 4;
  uint64 downlink_drops = 5;    // from datapath.UERx — currently unsurfaced
}

message GnbAggregate {
  uint32 gnb_id = 1;
  string name = 2;
  uint32 ues_attached = 3;
  uint32 sessions_active = 4;
  bool   ng_setup_ok = 5;
}

message ResourceSample {
  uint32 goroutines = 1;
  uint64 rss_bytes = 2;
}
```

## Events

```proto
message RunEventsRequest {
  string run_id = 1;
  // Resume point. 0 = from the oldest retained event.
  uint64 from_seq = 2;
  repeated EventSeverity min_severity = 3;
}

enum EventSeverity { EVENT_SEVERITY_UNSPECIFIED = 0; INFO = 1; WARN = 2; ERROR = 3; }

message RunEvent {
  string run_id = 1;
  uint64 seq = 2;                 // monotonic within the run
  int64  unix_nano = 3;
  EventSeverity severity = 4;
  string kind = 5;                // REGISTER, HANDOVER, NGAP, PDU_SESSION, RUN, SLO
  string supi = 6;                // empty for run-scoped events
  uint32 gnb_id = 7;
  string message = 8;

  // Set on the first message when the requested from_seq had been evicted.
  // The client learns it missed events instead of assuming none occurred.
  uint64 dropped_before_seq = 9;
  uint64 dropped_count = 10;
}
```

`dropped_count` is the point of the design. Delivery is not guaranteed; loss is
*detectable*, which is what a conformance tool needs and what the current
`hub.publish` drop-on-full cannot express.

## Per-UE detail stays pull-based

Frames carry aggregates only. Drill-down uses the existing unary RPCs, extended
so the two orthogonal state axes are both readable:

```proto
message UEStatus {
  string supi = 1;
  string state = 2;             // registration axis: REGISTERING … SESSION_ACTIVE
  string pdu_address = 3;
  int64  amf_ue_ngap_id = 4;

  // Added: currently held in Session.gnbCfg/gnbN3 with no accessor, so no
  // client can answer "where is this UE now?".
  uint32 serving_gnb_id = 5;
  string serving_gnb_name = 6;
  // Mobility axis, independent of state: HANDOVER_STARTED, HANDED_OVER,
  // HANDOVER_FAILED, PATH_SWITCH_COMPLETE. Empty if the UE has never moved.
  string mobility_state = 7;
  int64  mobility_changed_unix_nano = 8;
}
```

## Suggested build order

Each step is independently verifiable against a live core, per the project's
build-in-small-chunks rule.

1. **Engine accessors.** `Session.GNB()`, mobility state on `Session`, and the
   `UEStatus` fields above. Unblocks the per-gNB view and is useful on its own.
2. **Wire `load.Observer`.** `engine.RunLoad` sets `Config.Observer`; live
   attach/failure counters and latency percentiles become readable mid-run.
   Smallest change with real value, and it turns on already-tested code.
3. **Run registry + `RunService` unary RPCs.** Runs get identity, listing, and
   history — still executing where they execute today.
4. **Move `RunLoad`/`RunFleet` onto the server's `Manager`** (the substance of
   ADR-0005). Load and fleet UEs join `List()` and the hub.
5. **`RunTelemetry`.** Incremental aggregation in the engine; snapshot frames.
6. **`RunEvents`.** Per-run sequenced ring, replacing the dashboard's use of
   the lossy `StateStream` for run-scoped events.
7. **CLI becomes a client.** `orbit load`/`orbit run` submit and render.

Steps 1–2 are useful even if ADR-0005 is deferred or reshaped; steps 4–7 depend
on it.

## Open questions

- **Concurrent runs.** Initial policy is one active run per kind. Whether
  multiple concurrent load runs are ever meaningful against a single core is a
  question about the core, not about ORBIT.
- **History retention.** How many completed runs the daemon keeps, and whether
  reports persist across restarts. Sketch assumes in-memory and bounded.
- **HDR concurrency.** `hdrhistogram` is not safe for concurrent read/write;
  live percentiles need a snapshot under the existing mutex or a rotating pair.
