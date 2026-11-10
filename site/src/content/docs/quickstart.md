---
title: Quickstart
description: Install ORBIT, start the server, and run your first test.
---

A short path from nothing to a run. The full walkthrough lives in
[QUICKSTART.md](https://github.com/grewelltech/orbit/blob/main/docs/QUICKSTART.md); the complete
command and config reference is in
[USAGE.md](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md).

## 1. Install

```sh
curl -fsSL https://orbit.grewelltech.com/install.sh | sh
orbit --help
```

## 2. Serve

The server owns run execution; the CLI and the dashboard are both clients of it.

```sh
orbit serve --state-dir ~/.local/state/orbit
```

Open the dashboard at `http://localhost:8412/` — the home page lists every run, live and finished.

### Serving on a lab server

By default the server listens on `127.0.0.1:8412`, reachable only from the machine it runs on. To
use it from other machines — a shared lab server, or separate RAN and app hosts — give it an
address with `--listen`. Subscriber keys travel in API requests, so off loopback the server refuses
to start without TLS and a token:

```sh
# a token: first line of a file only you can read
mkdir -p ~/.config/orbit
(umask 077; openssl rand -hex 32 > ~/.config/orbit/api-token)

# listen on the server's own address, with a self-signed certificate
orbit serve --state-dir ~/.local/state/orbit \
  --listen 10.0.0.5:8412 --tls auto --token-file ~/.config/orbit/api-token
```

`--tls auto` creates the certificate once, keeps it under `--state-dir`, and logs its fingerprint
at startup. Use `--tls-cert`/`--tls-key` instead for a certificate you already have. To listen on
every interface, use `--listen 0.0.0.0:8412`.

The CLI talks to `http://127.0.0.1:8412` unless told otherwise, so every shell that runs `orbit`
commands — on the server itself too — needs the server's address. Pin the certificate once, then export the URL:

```sh
# shows the fingerprint and asks; compare it with the one serve logged
orbit server trust https://10.0.0.5:8412 --token-file ~/.config/orbit/api-token
export ORBIT_SERVER=https://10.0.0.5:8412
```

On another machine, copy the token file over first. Or export the token as `ORBIT_TOKEN` and
leave `--token-file` off:

```sh
export ORBIT_TOKEN=<the token>
orbit server trust https://10.0.0.5:8412
```

Without `ORBIT_SERVER`, commands still go to loopback and fail with
`dial tcp 127.0.0.1:8412: connect: connection refused`. Without the token, the server answers
`unauthenticated`.

The dashboard moves to `https://10.0.0.5:8412/`. Accept the self-signed certificate once; it asks
for the token the first time and keeps it for that browser tab.

See [TLS and tokens](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md#tls-and-tokens)
for the full rules, and
[Run as a service](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md#run-as-a-service)
to install the server under systemd.

## 3. Define a target

A target is where a run points: a core's AMF, PLMN and (optionally) a subscriber pool.

```sh
orbit target init lab1 --amf 127.0.0.1:38412 --mcc 001 --mnc 01 --sd 010203 \
  --first-supi 001010000000001 --count 100
```

`--count` sizes the subscriber pool; it defaults to 1, so set it to at least
the `--count` your load test uses (100 below) or the run is rejected for a pool
that is too small.

Subscriber keys stay out of the target file — `target init` writes `${ORBIT_KI}`
and `${ORBIT_OPC}` in their place, expanded from your shell when the target
loads. Export them before a run, or loading the target will fail on the unset
references:

```sh
export ORBIT_KI=<hex> ORBIT_OPC=<hex>
```

Keeping a testbed's settings in a file instead? Put the `KEY=VALUE` lines in a
dotenv file and pass `--env-file`, which loads them before the run — in place
of `set -a; . file; set +a`. It feeds both the `${ORBIT_KI}`/`${ORBIT_OPC}`
expansion above and the `ORBIT_*` config layer (`ORBIT_MCC`, `ORBIT_TAC`,
`ORBIT_SD`, …); an already-exported variable still wins. See
[Config from an env file](https://github.com/bgrewell/orbit/blob/main/docs/USAGE.md#config-from-an-env-file---env-file).

```sh
orbit conformance -t lab1 --env-file testbed.env
```

## 4. Add a responder

Conformance suites and attach storms need only the core. Application sessions (VoIP, HTTP,
video), fleet bulk traffic and app cohorts, and the user-plane half of `orbit target probe` also
need a **responder**: the far end of the traffic, running on a host in the data network (N6)
behind the UPF. Skip this step if you only run conformance or attach tests. Fleet latency probing
needs one only when the scenario gives no `behaviors.latency.target` of its own to echo.

The server sets the host up for you. It must be serving TLS on a non-loopback address
([step 2](#serving-on-a-lab-server)), and the target needs the N6 side of the core, which
`target init` does not ask for. Add it to `~/.config/orbit/targets/lab1.yaml` under `core:`:

```yaml
core:
  # ...what target init wrote...
  ue_pools: [10.250.0.0/16]    # the pools the core gives UEs their addresses from
  upf_n6: 203.0.113.1          # the UPF's N6 address
  n6_subnet: 203.0.113.0/24    # the UPF's N6 network
```

Then join the host. With ssh access to it, one command does everything — it shows the plan of
changes, asks, and applies it with `sudo`:

```sh
orbit target add-responder --ssh app1.lab -t lab1 --name app1
```

Without ssh, issue a one-line installer and paste it on the host:

```sh
orbit join create -t lab1 --name app1
# prints: curl ... 'https://10.0.0.5:8412/join/...' | sudo sh
```

Last, name the responder in the target file so runs use it:

```yaml
app:
  responders:
    - name: app1
```

Check the whole path — core, responder, and traffic from a UE through the UPF to it:

```sh
orbit responder list       # app1 should be online
orbit target probe lab1    # seven steps; stops at the first failure and says what to fix
```

[Deploying a responder](/responder/) covers what the join changes on the host, its options, and
running a responder by hand.

## 5. Run a test

```sh
# a conformance suite
orbit conformance -t lab1

# an attach storm
orbit load -t lab1 --count 100 --rate 20
```

Conformance results print as a colored table on a terminal (plain output when piped, `--json` for
machines). `orbit conformance` and `orbit load` each wait for the run to finish, so review the
results afterward:

```sh
orbit runs list            # every run, newest first
orbit runs get <run-id>    # the full result of a finished run
```

To follow a run live, run `orbit runs watch` in a second terminal *while* a test is in flight — with
no id it follows the active run, or pass a `<run-id>`. It fails with "no active run" if nothing is
running, which is expected once the commands above have completed.

## Next

- Start a test from the dashboard's **New test** chooser instead of the CLI — they are peers.
- Read [USAGE.md](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md) for targets, run
  files, gates, and the conformance suite in depth.
