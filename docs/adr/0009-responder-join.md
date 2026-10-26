# ADR-0009: App-side setup by join

- Status: Accepted
- Date: 2026-10-26
- Amends: ADR-0007 (its "accepted limits" on install, service units and remote
  deploy)

## Context

UE → UPF → N6 is the primary traffic path for every user-plane test, so an N6
responder is not optional (maintainer decision, 2026-10-26). Setting one up today takes five
manual steps:

1. Install `orbit responder` or `loomd` on the app host.
2. Choose its N6 address.
3. Add the return route for the UE pool through the UPF.
4. Open the firewall: the control port from the management side, and RTP/data
   ports from the UPF's N6 subnet.
5. Copy `peer:`, `token:` and `peer_data_ip:` into every scenario file.

Any step that goes wrong only shows up later, as a test failure that looks like a
core problem.

ADR-0007 deliberately kept `orbit responder` as a foreground process with a
bind address the operator always states. It rejected an SSH `install` command
because that would drag in credential handling and service management. The maintainer has
now decided on one mechanism with three ways to start it: a join one-liner served
by `orbit serve`, SSH bootstrap on the client side only, and discovery as a
doctor. ADR-0007 is Accepted and immutable, so this ADR records which of its
limits change and why. Its security concerns still hold: a responder is a
traffic generator that can be aimed remotely, and this ADR keeps the operator's
explicit consent at each step.

Join depends on ADR-0008's channel (TLS plus an optional bearer token). The script
runs as root, so it must never be fetched over an unauthenticated plaintext link.

## Decision

### 1. Tokens

`orbit join create [--name app1] [--ttl 10m] -t <target>` calls
`JoinService.CreateJoinToken`. That call is authenticated like any other RPC.

- The client sends what the script needs from the resolved target:
  - the UE pool CIDR(s);
  - the UPF's N6 address and subnet;
  - the server's management address as seen by the app host.

  The server keeps these with the token until it expires. It isn't a target store:
  the data is single-purpose and expires with the token.
- A token is 128 random bits, kept hashed, valid for `--ttl` (default 10 min,
  maximum 1 h), and **consumed by the first successful registration**. Fetching
  the script doesn't consume it, so a script that fails halfway can be rerun with
  the same token within the TTL.
- `orbit join list|revoke` shows and cancels pending tokens.
- The command prints the one-liner. It pins the server's key, so a self-signed
  `--tls auto` certificate needs no CA:

  ```
  curl -sf --pinnedpubkey 'sha256//<b64>' https://ran1:8412/join/<token> | sudo sh
  ```

- `serve` refuses the join endpoints unless TLS is on. A plaintext join is never
  offered, not even on loopback, because the script is root code.

### 2. The script

`GET /join/<token>` returns a POSIX `sh` script, generated for that token. It is
idempotent: a second run changes nothing and reports that.

1. It detects the OS and architecture, and refuses anything other than Linux
   amd64/arm64.
2. It downloads `orbit` from `/join/<token>/bin/<arch>` over the same pinned
   connection, and verifies its SHA-256 against a value embedded in the script.
3. It picks the N6 interface and IP: the address with a route to the UPF's N6
   address. It also picks the control bind: the address with a route to the
   server's management address.
4. It works out the changes it will make:
   - the return route: `ip route replace <ue_pool> via <upf_n6>`;
   - the firewall rules: the control port from the server's address, and the
     RTP/data range from the UPF N6 subnet. It uses nftables, or iptables if
     nftables is absent.
   - optionally, a systemd unit for `orbit responder` that also re-applies the
     route in `ExecStartPre`, so the route survives a reboot.
5. It **prints the plan, then applies it.** Before applying, it asks for
   confirmation on `/dev/tty`. With no tty, it applies only if `--yes` was passed
   (`… | sudo sh -s -- --yes`), and otherwise exits after printing. That
   confirmation is the operator's explicit statement of the addresses, which keeps
   ADR-0007's rule that the bind address is never chosen silently for the
   operator. The operator can set `--n6-ip`, `--bind`, `--no-firewall`,
   `--no-route`, `--no-systemd` and `--dry-run`.
6. It starts the responder and registers it (§3). It reports success only once the
   server has acknowledged the registration.

### 3. Registration (the responder dials out)

The responder dials the server and registers through
`JoinService.RegisterResponder`, over TLS pinned to the server's key.

- It presents the join token once and sends:
  - its name;
  - its control address and its N6 data IP;
  - its RTP/data port range;
  - its version.
- The server burns the token and issues a responder ID and a long-lived responder
  credential. The credential is kept in `/etc/orbit/responder.yaml` with mode
  0600.
- The server also generates the loom control token. The responder uses it as its
  `--token`, so no `token:` is ever typed in by hand.
- The responder then holds an outbound heartbeat stream. The server marks a
  responder offline after missed heartbeats, and `orbit responder list` shows it
  as offline.
- Loom control stays as it is: the server dials the responder's control address,
  as for `--peer` today. Only discovery and registration go outbound.
- Targets reference a registered responder **by name**
  (`app.responders: [{name: app1}]`). At `StartRun`, the server resolves that name
  to an address and token. Registered responders are runtime resources of the
  server, not targets, so they don't conflict with ADR-0008's "the server stores no
  targets".
- `orbit responder remove <name>` revokes the credential. Static responders
  (stock `loomd`, with `control`/`token`/`data_ip` in the target) stay fully
  supported.
- When the RAN host itself has an N6 interface, `orbit serve --responder-bind
  <addr>` starts the hosted responder (ADR-0007) and registers it as `local`. No
  join is needed. The operator still states the bind address.

### 4. SSH, on the client only

`orbit target add-responder --ssh <host> [-t <target>]` creates a token, then runs
the same one-liner with `--yes` by executing the user's own `ssh` binary
(`ssh <host> 'curl … | sudo sh -s -- --yes'`).

- The user's `~/.ssh/config`, agent and known_hosts apply unchanged.
- ORBIT links no SSH library, reads no key, and stores nothing.
- No SSH data goes into a target or a `StartRun`. A test asserts this.
- The web UI does no SSH. It shows the one-liner, ready to copy.

### 5. The doctor

`orbit target probe` and `TargetService.Probe` take leases as in ADR-0008 §5. They
then run these steps in order and stop at the first failure, naming what's missing:

| Step | Failure reported as |
|---|---|
| NG Setup | the AMF address, SCTP reachability or PLMN mismatch |
| register one UE | the subscriber or keys (with the reject cause) |
| PDU session | DNN or slice (with the 5GSM cause) |
| responder registered and online | "no responder registered", plus the join command for this target |
| control reachable | the control port or bind address, plus the firewall rule to add |
| UE → N6 data echo | "return route", plus the exact `ip route` line |
| RTP range | the firewall port range, plus the rule to add |

The probe runs from the server's position on the RAN host, so the path it tests
is the path a run will use.

## Alternatives considered

**ADR-0007's position (foreground process only, no install).** The safest posture,
but it leaves the five manual steps, which are where new users get stuck, and the
user-plane path can't be optional. Superseded for install and bootstrap only.

**A server-side SSH deploy (`orbit responder install` from ADR-0007).** Rejected
again: the server would hold SSH credentials. Doing SSH on the client side gets
the same convenience with the user's own credentials, and none of them stored.

**Tunnelling loom control over the responder's outbound stream.** This removes the
inbound control-port rule entirely. It's attractive, but it changes loom's control
transport. Under ADR-0007 that is a loom change, so it's left as an open question
and not built here.

**A token consumed when the script is fetched.** This is stricter, but a script
that fails partway would need a new token, and partial failures are exactly what
idempotency is for. Burning the token at registration keeps "single use" where it
matters, which is the credential.

**Applying changes with no confirmation.** That would be the smoothest one-liner,
but the script would pick routes, firewall rules and a bind address for the
operator as root without asking. That is what ADR-0007 refused. `--yes` gives the
same result as an explicit choice.

## Consequences

Easier:

- One pasted line, or one `--ssh` command, gives a working N6 responder.
- Targets lose `peer`/`token`/`peer_data_ip`.
- A failure gets a specific diagnosis instead of a confusing traffic result.

Harder:

- The server gains a token store, a responder registry, a binary-serving endpoint
  and a script generator.
- The script has to stay correct across distros and firewall tools. That needs an
  LXD test matrix, plus idempotency and dry-run tests.
- The join endpoint is root code delivery, so it is security-sensitive. It is TLS
  only, with a pinned key, tokens kept hashed, and rate limiting.

Accepted limits:

- Linux amd64/arm64 app hosts only.
- The route is persisted through the systemd unit, not the distro's network
  configuration.
- Loom control is still inbound to the responder.

Follow-up: J1 (tokens and the endpoint), J2 (the script and its tests), J3
(registration and heartbeat), J4 (the doctor and `--ssh`). ADR-0007's status line
gains "amended by ADR-0009". A later `orbit node` ADR extends the same join and
registration to RAN hosts.

## Resolved questions

The maintainer answered these on 2026-10-26, when he accepted this ADR.

| # | Question | Decision |
|---|---|---|
| 1 | Confirm the plan before applying? | Yes, as drafted: confirm on `/dev/tty`, `--yes` skips it, and with no tty the script applies only with `--yes`. |
| 2 | Tunnel loom control over the outbound stream? | Not now. Revisit it in the `orbit node` ADR. |

## Open questions

| # | Question | Owner |
|---|---|---|
| 3 | Which firewall tools to support in v1: nftables and iptables only, or also ufw and firewalld? | implementer (J2) |
| 4 | Is there a default responder name scheme when `--name` is omitted (the hostname)? | architect (J1) |
| 5 | The heartbeat interval, and how many missed beats mark a responder offline. | architect (J3) |
