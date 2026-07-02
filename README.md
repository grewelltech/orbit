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

> **Status: early — in design / scaffolding.** The full grounded design, phased
> plan, discovery spikes, and risks are in **[docs/DESIGN.md](docs/DESIGN.md)**.

## Build

```sh
make build      # -> bin/orbit
```

## Relationship to the-earlier-project

ORBIT began as the "Release 2" concept of **the-earlier-project** — the existing
UERANSIM-based 5G digital twin — and is now its own project. the-earlier-project remains the
scenario-driven twin *over* UERANSIM; ORBIT is the purpose-built engine that owns
the protocol stack end to end. The two share concepts (the scenario model) and can
interoperate, but ORBIT does not depend on the-earlier-project.

## License

[Apache-2.0](LICENSE).
