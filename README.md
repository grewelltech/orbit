# ORBIT

**Open Radio Benchmark and Integration Testbed.**

ORBIT is a from-scratch, Go-native 5G SA RAN + UE simulator and test harness for
**benchmarking, conformance-checking, and integration-testing a real 5G core**.
It speaks the actual control and user planes — NGAP over SCTP (N2), NAS-5GS (N1),
and GTP-U (N3) — with the radio replaced by a software model instead of an RF PHY.
That lets it drive genuine signaling and user data against the core under test
while scaling to a large fleet of simulated devices.

Where existing no-PHY simulators stop, ORBIT is built to be feature-complete:

- **Signaling-level emulated mobility** — synthesized measurements driving real
  N2/Xn handover against the core (no radio required).
- **Core conformance / regression probing** — structured pass/fail with 3GPP spec
  citations.
- **Performance / load testing** — rate-controlled attach storms and per-UE +
  aggregate throughput/latency/jitter; traffic generation via
  [loom](https://github.com/bgrewell/loom).
- **API-first** — a gRPC/Connect + REST API, with a CLI that only ever calls the
  API.
- **First-class observability** (structured logs, metrics, traces, live state
  streaming) and **CI/CD-native** operation from day one.

> **Status: Phase 0 (foundations).** A gNB can complete an **NG Setup** exchange
> against a live AMF over the full CLI → API → engine path. The grounded design,
> phased plan, discovery spikes, and risks are in
> **[docs/DESIGN.md](docs/DESIGN.md)**.

## Build

```sh
make build      # -> bin/orbit
```

## Try it (Phase 0)

Run the API server, then drive an NG Setup exchange against an AMF through it:

```sh
./bin/orbit serve &
./bin/orbit cell ngsetup \
    --amf 172.17.50.11:38412 \
    --mcc 208 --mnc 93 --tac 1 --sst 1 --sd 010203 \
    --gnb-id 66 --name orbit-gnb-1
# -> NG Setup accepted by AMF "AMF"
```

The CLI never touches the engine directly — it is a Connect client of the API,
so every capability is machine-reachable (`--server` selects the endpoint).
`/metrics` (Prometheus) and `/healthz` are served on the same listener.

## Layout

| Path | What |
|---|---|
| `proto/`, `gen/` | API schema (Protobuf) and generated Connect/Go code (`make gen`). |
| `internal/sctp`, `internal/ngap`, `internal/nas`, `internal/gtpu` | Thin adapters over the pinned transport/codec substrate. |
| `internal/gnb` | gNB role — NG Setup today; NGAP procedure FSM grows here. |
| `internal/server`, `internal/cli` | Connect API façade and the cobra CLI. |
| `internal/observability` | slog (trace-correlated, credential-redacting), OTLP tracing, Prometheus. |

## Testing

```sh
make test          # unit-CI: headless, no core required
make integration   # integration-CI: needs a live core (ORBIT_AMF_N2 to override the AMF)
```

## Relationship to ran-twin

ORBIT began as the "Release 2" concept of **ran-twin** — the existing
UERANSIM-based 5G digital twin — and is now its own project. ran-twin remains the
scenario-driven twin *over* UERANSIM; ORBIT is the purpose-built engine that owns
the protocol stack end to end. The two share concepts (the scenario model) and can
interoperate, but ORBIT does not depend on ran-twin.

## License

[Apache-2.0](LICENSE).
