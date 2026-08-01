# ADR-0007: ORBIT hosts the loom responder; stock loomd stays supported

- Status: Accepted
- Date: 2026-08-01

## Context

[`docs/design/real-app-traffic.md`](../design/real-app-traffic.md) §8 puts the
far end of every application flow on the N6 network as **stock `loomd`**: loom
owns the protocol engines (`app.Server "voip"`, `HTTPOrigin`, video), ORBIT is a
thin consumer. That split is right — loom does the lifting, the agent is useful
outside ORBIT, and it upgrades on loom's own cadence.

It also means a user standing up a benchmark run installs and operates **two
binaries from two repos** before a single call is placed. For the single-node
demo — where the responder is on the same host — that friction buys nothing.

The enabling fact: `cmd/loomd/main.go` is 83 lines, and all of it is public API
in `github.com/bgrewell/loom/control`, which ORBIT already imports:

```go
lis, _ := net.Listen("tcp", addr)
srv := control.NewServer(version, opts...)   // WithAuthToken, WithTelemetryInterval
gs  := control.NewGRPCServer(srv)
gs.Serve(lis)
```

Hosting the agent inside ORBIT is therefore not a wrapper, a fork, or a
reimplementation — it is calling the same constructor `loomd` calls. This is
what `CLAUDE.md` rule 3 already asks for: embed loom as a library, and drive any
gap into loom rather than working around it.

The security posture is a real constraint, not a detail. `loomd`'s own comment
is explicit that "an agent is a remotely-aimable traffic generator, so it must
not be reachable off-host unless the operator opts in", and it defaults to
`127.0.0.1:9551`, warning when bound to a routable address with no token. A
responder that is easier to start is also easier to start carelessly.

## Decision

ORBIT ships **`orbit responder`**, which runs the loom control-plane agent
in-process via `control.NewServer` / `control.NewGRPCServer`.

- **`--bind host:port` is a required flag with no default.** Rather than
  inheriting `loomd`'s safe-by-default loopback, ORBIT refuses to start until
  the operator states the bind address. Reachability becomes an explicit
  decision every time, so neither exposure nor loopback is ever a surprise.
- Binding a routable address with no `--token` logs a prominent warning, matching
  `loomd`.
- **Stock `loomd` remains fully supported.** `orbit serve --loom-agent` and the
  per-call `--peer` path are unchanged. This is additive.
- ORBIT hosts the agent and passes configuration through. It does not wrap,
  extend, or reimplement responder behaviour: **changing what the responder does
  is a loom change**, not an ORBIT one.

The command is named for its role, not its reference point — the same process
serves HTTP and video responders in later phases, not only N6-side VoIP.

## Alternatives considered

**Leave it at stock `loomd` only.** Zero new code and keeps the repos cleanly
separated, but it keeps the two-install friction for the single-node case, which
is exactly where new users meet ORBIT first.

**`orbit responder install` — deploy over SSH.** Genuinely "deploys" to a remote
N6 host, but drags in credential handling, idempotency, teardown, and service
management: a new failure domain, and a config-management surface ORBIT has no
business owning. For the split deployment, a container image or k8s manifest
fits the existing testbed model far better, and can be added later without
revisiting this decision.

**Auto-start an embedded responder from `orbit serve` when `--peer` is absent.**
The best zero-config experience, and still attractive — but it implies a bind
address chosen *for* the operator, which is the one thing this ADR refuses to
do. Revisit only with an explicit opt-in flag.

**Vendor loom's app servers into ORBIT.** Rejected outright: duplicates the
protocol engines, forks the measurement science, and breaks the single-seam rule
the app-traffic design is built on.

## Consequences

**Easier.** One binary for the common case. The embedded path cannot suffer
loom version skew, because both ends compile from the same pin — the version-skew
gate in the app-traffic design only has to police the stock-`loomd` path.
Demos, CI, and the README single-node walkthrough stop needing a second install.

**Harder.** The embedded responder upgrades on ORBIT's release cadence, not
loom's — a user wanting a newer loom on the responder side must use stock
`loomd`. ORBIT's binary grows the agent's dependencies. Two supported paths mean
two paths to test.

**Accepted limits.** `orbit responder` does not daemonise, install a service
unit, or deploy to a remote host; it is a foreground process, and process
supervision is the operator's (or systemd's, or Kubernetes') job.

**Follow-up.** Revises §8 of the app-traffic design, which stands as written for
the stock-`loomd` path. `docs/USAGE.md` gains the responder in the firewall
matrix and the single-node walkthrough.
