---
title: Deploying a responder
description: Set up the N6 host that answers user-plane and application traffic from ORBIT's UEs.
---

A **responder** is the far end of application and bulk traffic runs. ORBIT's simulated UEs send traffic through the
core's user plane; the responder sits on the data network (N6) behind the UPF, receives it, and
answers. It is the `orbit` binary running as a service on a host you choose.

| Run | Needs a responder |
| --- | --- |
| Conformance suite | no |
| Attach storm (`orbit load`) | no |
| Application sessions (`orbit ue app voip`, `http`, `video`) | yes |
| Fleet bulk uplink/downlink and app cohorts | yes |
| Fleet latency probing | only without an explicit `behaviors.latency.target`: it then echoes the responder's data address |
| `orbit target probe` beyond `--core-only` | yes |

## Before you start

- **A host on the N6 network.** Linux `amd64` or `arm64`, with root, `curl` and (by default)
  systemd. It must reach the UPF's N6 address directly, on the same subnet, and reach the ORBIT
  server.
- **The server on TLS, off loopback.** The join runs as root on the host, so the server offers it
  only over TLS, and the host has to reach the server's address. Start the server as in
  [Quickstart step 2](/quickstart/#serving-on-a-lab-server):

  ```sh
  orbit serve --state-dir ~/.local/state/orbit \
    --listen 10.0.0.5:8412 --tls auto --token-file ~/.config/orbit/api-token
  ```

  Keep `--state-dir`: registered responders are stored there, and without it every host must join
  again after a server restart.
- **The N6 side of the core in the target.** `orbit target init` does not ask for these; add them
  to the target file (`~/.config/orbit/targets/<name>.yaml`):

  ```yaml
  core:
    ue_pools: [10.250.0.0/16]    # the pools the core gives UEs their addresses from
    upf_n6: 203.0.113.1          # the UPF's N6 address: the next hop of the host's return route
    n6_subnet: 203.0.113.0/24    # the UPF's N6 network: what the host's firewall opens to
  ```

  Or pass them to the join commands below as `--ue-pool`, `--upf-n6` and `--n6-subnet`.

## Join the host

Pick one of three ways. All of them run the same join script on the host.

The host is told to reach the server at its `--listen` address. If the server listens on every
interface (`--listen 0.0.0.0:8412`), or the host reaches it some other way, add
`--mgmt <host>:8412` to the two commands below.

### Over ssh

With ssh access to the host, one command does the whole join from where you run the CLI:

```sh
orbit target add-responder --ssh app1.lab -t lab1 --name app1
```

It copies the script over your own `ssh` (your config, agent and known_hosts apply), shows the plan
of changes, and after you confirm applies it with `sudo`. Add `--yes` to skip the question, which
is required without a terminal.

### A one-line installer

Without ssh from the CLI's machine, issue a short-lived join token and paste the line it prints on
the host:

```sh
orbit join create -t lab1 --name app1
# join token jt-… for responder app1, expires … (in 10m0s).
# curl -q -fsS --proto '=https' -k --pinnedpubkey 'sha256//…' 'https://10.0.0.5:8412/join/oj1_…' | sudo sh
```

The line pins the server's key, so `-k` only skips the CA check a self-signed certificate cannot
pass. It carries a live token: paste it only on the intended host, and revoke one you do not use.

```sh
orbit join list            # pending tokens
orbit join revoke jt-…     # cancel one
```

Success is the line `orbit join: applied N changes.` (or `orbit join: nothing to change`), not the
exit status: `curl … | sudo sh` exits 0 even when the download fails.

### From the dashboard

On the **Targets** page, **Add an N6 host** makes the same one-liner for a target. Once the host
has joined, **use &lt;name&gt; in &lt;target&gt;** adds it to the target for you.

## What the join changes on the host

The script works out every change first, prints it — each command as it will run, each file in
full — and changes nothing until you confirm.

1. **The binary**, installed as `/usr/local/lib/orbit/orbit` after its SHA-256 is checked.
2. **Return routes** for each UE pool through the UPF's N6 address, so replies go back through the
   user plane.
3. **Firewall rules**, only where input is filtered: the control port (9551) from the server, and
   the data ports from the UE pools and the N6 subnet. With `ufw` or `firewalld` it prints the
   rules to add instead of editing them; rerun with `--no-firewall` once they are in.
4. **A systemd unit**, `orbit-responder.service`, running as the system user `orbit`. It
   re-applies the routes and rules at boot.
5. **The registration** with the server, written to `/etc/orbit/responder.yaml` (mode 0600).
6. **The start**: the service is enabled and started, and the script waits for the server to see
   its heartbeat.

Running it again is safe: steps already in place are listed as `ok`.

Options go after `sh -s --` on the one-liner; `add-responder` takes the same ones as flags.

| Option | Effect |
| --- | --- |
| `--yes` | apply without asking |
| `--dry-run` | print the plan only |
| `--n6-ip IP` | the host's N6 address, instead of the source of its route to the UPF |
| `--bind HOST:PORT` | the responder's control address, instead of the source of its route to the server, port 9551 |
| `--no-route` | leave the UE-pool routes alone |
| `--no-firewall` | leave the firewall alone |
| `--no-systemd` | install no unit (required on a host without systemd) |
| `--reregister` | register again with a new token, rotating the responder's credentials |

`--dry-run` and `--reregister` are script options only: `add-responder` has neither flag. It
always shows the script's dry run first, and its own `--yes` skips the question that follows.

## Use it in a target

A join registers the responder with the server; a target then names it. Add it to the target
file — the name is enough, the server fills in its addresses and token:

```yaml
app:
  responders:
    - name: app1
```

## Check it

```sh
orbit responder list       # name, state, last seen, control and data addresses
orbit target probe lab1    # the core, then the user plane to the responder
```

The probe runs seven steps and stops at the first failure, naming what to fix. Steps 4–7 cover the
responder:

| Step | A failure means |
| --- | --- |
| 4 responder | the target names no responder, or it is not registered or is offline |
| 5 control | the responder's control port is unreachable from the server |
| 6 UE to N6 echo | the return route on the host is missing, or ICMP is blocked |
| 7 data ports | the data ports are blocked, or the route |

A responder that stops sending its heartbeat shows offline after 30 s. On the host:

```sh
systemctl status orbit-responder
journalctl -u orbit-responder
```

To remove one — its credentials stop working and the service stops:

```sh
orbit responder remove app1
```

## Running a responder by hand

Without the join, start the responder yourself on the N6 host. `--bind` is required and has no
default, and a routable address needs a token:

```sh
(umask 077; openssl rand -hex 32 > loom-token)
orbit responder --bind 203.0.113.20:9551 --token-file loom-token
```

Then give the target its control address, token and data address. The token is read from the
environment of the shell that runs `orbit` commands, so copy it there over a channel you trust
(`scp`, not a chat window) and export it:

```sh
export ORBIT_LOOM_TOKEN="$(cat loom-token)"
```

```yaml
app:
  responders:
    - name: n6
      control: 203.0.113.20:9551
      token: ${ORBIT_LOOM_TOKEN}   # expanded from your shell when the target loads
      data_ip: 203.0.113.20
```

The routes back to the UE pools and any firewall rules are yours to add:

```sh
ip route replace 10.250.0.0/16 via 203.0.113.1 dev <n6-interface>
```

A stock loom `loomd` agent is interchangeable with `orbit responder` here.

## More

The full reference — token limits, the firewall cases, registration and heartbeat details — is in
[USAGE.md](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md#n6-app-hosts-orbit-join).
