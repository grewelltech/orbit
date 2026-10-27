# ORBIT usage

How to drive ORBIT against a live 5G core. New here? Start with
[QUICKSTART.md](QUICKSTART.md) — this page is the reference. For the
architecture and design rationale see [DESIGN.md](DESIGN.md); for known
core-side issues and the `--core-profile` quirks see
[interop/sdcore.md](interop/sdcore.md).

## Prerequisites

- A reachable 5G SA core (an AMF's N2/SCTP endpoint), and subscriber
  credentials provisioned in the core: `Ki` and `OPc` (32 hex digits each) for
  the SUPIs you use, plus the core's PLMN (`--mcc`/`--mnc`), TAC, and slice
  (`--sst`/`--sd`) and DNN.
- For the **user plane and handover**, a host on the core's N3 access network
  (see "Where things must run" below).
- `make build` → `bin/orbit`.

## Where things must run (topology)

- **Control plane** (attach, status, handover signalling) works from any host
  that can reach the AMF's N2 endpoint.
- **User plane** (`ping`, `traffic`, `latency`) and the **handover downlink**
  need the gNB's N3 address to be reachable *from the UPF*. Register with
  `--gnb-n3 <ip>` set to an address on the UPF's access network; the UPF returns
  downlink there. On a split testbed (e.g. ATB-01) that address lives on the RAN
  node, not the control-plane host, so run those commands from there.
- **Handover** additionally needs each gNB on a **distinct routed source IP**
  (`--bind <ip>`) — the AMF distinguishes gNBs by association address — and a
  **fresh gNB ID per run**: the AMF does not cleanly re-key a reused gNB ID from
  a new address (a stale association makes it drop the Handover Request). See
  [interop/sdcore.md](interop/sdcore.md).

### Choosing the N2 source address (`--bind`)

Every command that opens an N2 association — `cell ngsetup`, `ue register`,
`ue handover` / `xn-handover`, `load`, `runs start-load` and `conformance` —
takes `--bind <ip[:port]>`: the local address the gNB's SCTP association is
bound to. A bare IP means an ephemeral port (`10.0.0.21` is `10.0.0.21:0`); an
explicit port is kept as given; IPv6 is written bare (`fd00::21`, zoned
`fe80::1%eth0`) or bracketed with a port (`[fd00::21]:0`); a hostname is
resolved at dial time. Unset, the association is bound to the single address
the host routes to the AMF from (with an ephemeral port), so the INIT
advertises one address and the AMF sees exactly that; the `n2-source=` line
reports it. Use `--bind` when the gNB should use a different address (a RAN
host with several N2 addresses) or a fixed port. A host with no route to the
AMF fails at dial with an error that names `--bind`; ORBIT never falls back to
advertising every host address. A dial to an AMF that never answers the INIT
fails within a bound (`no INIT-ACK after 10.0s (bound 10s; AMF unreachable or
black-holed)`) instead of waiting out the kernel's retransmit schedule for
minutes; the kernel is told to abandon the exchange at about the same time,
so nothing keeps retransmitting afterwards. The bound is per command: `ue
register`, `ue handover` / `xn-handover`, `load` and `runs start-load` use the
20 s dial default; `cell ngsetup --timeout` bounds its dial plus the NG Setup
exchange (default 10 s); `conformance --per-test-timeout` bounds each check
(default 15 s). The printed bound is the deadline remaining when the dial
started, which is why `ngsetup` says `bound 10s` and `register` `bound 20s`.

**Port rule:** a fixed (non-zero) port is legal only when exactly one
association will use that `ip:port`; port 0 is always legal because every dial
gets a fresh ephemeral port. So a `--bind` shared by several `load` gNBs must
be a bare IP or `ip:0`, two gNBs may not name the same fixed `ip:port`, and
`conformance --bind` must be port 0 because each check dials afresh and the
liveness probe opens a second association from the same address. These are
rejected before anything is dialled.

`--bind` is the **N2** source; the user plane has its own flags (`--gnb-n3` on
the UE and load commands, `--n3-bind` on `conformance`, whose `--upf-n3` must be
the UPF's primary N3 address: the GTP-U check confirms the UPF is alive with a
GTP-U Echo Request, and a UPF may answer Echo only there). Each command prints
the N2 source it actually used as `n2-source=<ip:port>`, with the real port,
so a capture or an AMF-side log can be matched to the run. `conformance --json`
records the configured bind as `N2Source` in `ip:0` form: every check dials its
own association on its own ephemeral port, so there is no single port to show.

`load` and `runs start-load` take a list: none (route-selected source), one (shared by
every gNB, which leaves the AMF telling them apart by gNB ID only — the command
says so), or exactly `--gnb-count` addresses, one per gNB in order. Any other
count is an error. Addresses are never auto-incremented from a base; list the
ones the host owns, as fleet scenarios do with `topology.gnbs.source_ips`.

```sh
orbit cell ngsetup --amf <host:port> --mcc 001 --mnc 01 --bind 172.17.50.21
orbit ue register  ... --bind 172.17.50.21 --gnb-n3 172.17.50.21
orbit load         ... --gnb-count 3 --bind 172.17.50.21,172.17.50.22,172.17.50.23
orbit conformance  --amf <host:port> --bind 172.17.50.21 --json
```

`cell ngsetup --local` is the old name for the same flag; it still works but is
deprecated, and `--bind` wins if both are given.

`cell ngsetup` is a probe, not a persistent gNB: it runs one NG Setup and
closes the association as soon as the answer arrives, and `ue register` opens
its own gNB association for the UE procedures. To watch what the AMF does with
an idle association (its SCTP heartbeat, an AMF Configuration Update, anything
else it sends unprompted), add `--hold <duration>`: the association stays open
that long after a successful NG Setup, inbound NGAP is drained and logged by
procedure code on the server, and the output ends with `held: <duration>`.
Ctrl-C ends the hold early. The server caps a hold at 10 minutes, and a
rejected NG Setup is never held.

```sh
orbit cell ngsetup --amf <host:port> --mcc 001 --mnc 01 --bind 172.17.50.21 --hold 30s
```

### Network identity (`--mcc --mnc --tac --sst --sd --dnn`)

Every command that dials a core takes the same identity flags with the same
meaning, help and defaults, and prints the identity it used on one line:

```
network: plmn=001/01 tac=1 snssai=1/010203 dnn=internet
```

- **PLMN is required and never guessed.** `--mcc` (3 digits) and `--mnc` (2 or
  3 digits, leading zeros kept) come from the flags or, when both are absent,
  from `$ORBIT_MCC`/`$ORBIT_MNC`, exactly as the keys fall back to
  `$ORBIT_KI`/`$ORBIT_OPC`. Neither present is one error naming both sources.
  A wrong PLMN produces an NG Setup Failure that reads like a core finding,
  which is why the tool carries no default.
- **Defaults for the rest:** `--tac 1`, `--sst 1`, `--sd ""` (no slice
  differentiator), `--dnn internet` (where a PDU session is involved). SD-Core
  wants `--sd 010203`; pass it explicitly, as every example here does. The
  values are whatever your core is configured with.
- **Handover inherits.** `ue handover` / `xn-handover` take the same flags but
  with no defaults and no environment fallback: anything left unset is copied
  from the UE's serving gNB (`$ORBIT_MCC`/`$ORBIT_MNC` are ignored here, since
  the target is in the serving PLMN by definition), so a same-network handover
  needs only `--gnb-id`, `--bind` and `--gnb-n3`. A field given explicitly
  overrides the inherited one.
- **Scenario files** need `core.plmn.mcc` and `core.plmn.mnc` (quote them, so
  `"01"` is read as the string it is; `${ORBIT_MCC}`/`${ORBIT_MNC}` expand);
  `core.tac`, `core.slice.sst` and `core.dnn` take the defaults above when
  omitted.
- `conformance --json` records the identity as `Network{MCC, MNC, TAC, SST,
  SD}` next to `N2Source`, so a verdict says which network it was taken under.

```sh
export ORBIT_MCC=001 ORBIT_MNC=01
orbit cell ngsetup --amf <host:port> --sd 010203                 # PLMN from the environment
orbit ue register  --amf <host:port> --supi <imsi> --sd 010203 --gnb-id 1
orbit ue xn-handover --supi <imsi> --amf <host:port> --gnb-id 2 --bind <ip> --gnb-n3 <ip>  # inherits
```

### Targets (`-t`/`--target`, `--explain`)

([targets.md](targets.md) is the full reference.) A target is a YAML file that
describes *where* tests run: the core, the
subscriber pool, the RAN address pools and the N6 responder (ADR-0008 §1 has
the schema). `-t <name>` (or `$ORBIT_TARGET`) loads it from `./orbit.yaml`,
which may hold one target or a list of them, or else from
`~/.config/orbit/targets/<name>.yaml`. Each command then takes its flags'
values from the target:

```yaml
version: 1
kind: target
name: lab1
core:
  amf: 192.0.2.10:38412
  plmn: { mcc: "001", mnc: "01" }
  tac: 1
  slices:
    - { sst: 1, sd: "010203" }
subscribers:
  - name: main
    first_supi: "001010000000001"
    ki:  ${ORBIT_KI}     # expanded when the file loads
    opc: ${ORBIT_OPC}
ran:
  n2_source_ips: [192.0.2.50-192.0.2.59]
  n3_ips: [198.51.100.50]
  gnb_ids: { first: 100, bits: 24 }
```

```sh
orbit conformance -t lab1                      # no other flags needed
orbit cell ngsetup -t lab1 --tac 9             # a flag still overrides one field
orbit conformance -t lab1 --supi ""            # an empty flag clears a field: here, no mobility UE
```

Four commands manage targets:

```sh
orbit target init lab1 --amf <host:port> --mcc 001 --mnc 01 --sd 010203 --first-supi 001010000000001
orbit target list                 # ./orbit.yaml and ~/.config/orbit/targets, with any load errors
orbit target show lab1            # as loaded, keys <redacted>
orbit target show lab1 --resolved # each field's value after default < target < $ORBIT_*, and where it came from
orbit target probe lab1           # NG Setup, one registration, one PDU session, through the server
```

- **`init`** runs one NG Setup through the orbit server, so from the host runs
  execute on. It records the N2 source that NG Setup used as the target's
  `n2_source_ips` (and `n3_ips`, unless `--gnb-n3`). `--bind` picks the
  source instead, and `--no-probe --bind <ip>` skips the NG Setup.
  - It writes `~/.config/orbit/targets/<name>.yaml` with mode 0600. `--here`
    writes `./orbit.yaml` only when that file doesn't exist yet.
  - Keys are written as `${ORBIT_KI}`/`${ORBIT_OPC}` unless `--write-keys`.
  - A rejected NG Setup writes nothing, and an existing file is not
    overwritten; `--force` overrides both.
  - Without `--first-supi` there is no subscriber pool, and `probe` stops
    after NG Setup.
  - On a terminal, `init` asks for the AMF, PLMN and first SUPI when the flags
    leave them out.
- **`list`** shows every target with its file and line, and the error of any
  that doesn't load. One bad entry in an `./orbit.yaml` list never stops the
  others. A target in the config directory with the same name as one in
  `./orbit.yaml` is marked shadowed.
- **`probe`** stops at the first failing step and names what to check. It
  exits non-zero on a failure. With no subscriber pool, the UE steps are
  SKIPped and it passes on NG Setup alone.
  - NG Setup: the AMF address and the N2 route, or the PLMN and TAC the AMF
    doesn't serve.
  - Registration: whether the subscriber and its keys are provisioned in the
    core.
  - PDU session: the DNN and slice.

  The core's reject causes aren't reported yet. Until leasing, `init` uses
  the target range's first gNB ID (or 1), and `probe` uses the first three,
  one per association, plus the pool's first SUPI. A run using the same ones
  at the same time collides.

  `probe` sends the subscriber keys to `--server`. Until the server has TLS,
  keep it on loopback, or the keys cross the network in plaintext.
- `init` never writes through a symlink: a link at `./orbit.yaml` or at
  `targets/<name>.yaml` is refused. A `--force` overwrite replaces the file
  through a temp file and a rename.

- **Precedence**, lowest first: built-in default < target < `$ORBIT_*` < flag,
  field by field. A target therefore never overrides a flag you typed or an
  `$ORBIT_MCC` exported in your shell.
- **One address from a pool.** A flag that takes one address takes the first
  entry of the target's list until the server leases from the pool (a later
  step). A range gives its low end, and a CIDR its first host address.
  `--bind` keeps the port if the entry has one; `--gnb-n3` drops it;
  `--n3-bind` adds GTP-U's 2152 when the entry has none.
- **Host names** are accepted wherever the commands accept them: in
  `ran.n2_source_ips` (`--bind`), `ran.n3_ips` (`--n3-bind`; a pool like the
  N2 one, ranges and CIDRs included) and a responder's
  `data_ip`, with or without a port. They pass unresolved, as a typed
  `--bind lab-n2` does. Ranges and CIDRs are IP-only.
- **Slices.** `--sst` and `--sd` set the first slice's fields
  (`core.slices[0].sst`, `.sd`), not the whole list. So `-t lab1 --sd 000005`
  keeps the target's SST, and its other slices stay in the target.
- **Lists of pools or responders** use the first: `--supi`/`--base-imsi` come
  from the first pool's `first_supi`, and `--peer`/`--token`/`--peer-data-ip`
  from the first responder.
- **`--explain`** prints, to stderr, each field the command uses: its value,
  the layer and source that set it (`target orbit.yaml:7`, `env $ORBIT_MCC`,
  `flag --tac`), what it overrode, and which flag took it. It also lists the
  target's fields the command does not use. Keys and tokens show as
  `<redacted>`. The command then runs as usual. `--explain` works without a
  target as well, and then it only reports: a value the resolver would
  reject shows as a row marked `⚠`, and the command runs exactly as without
  `--explain`. With a target named, such a value is an error.
- **A stale `$ORBIT_TARGET`** naming a target that no longer exists fails
  every command that takes target fields with `target not found`, even when
  every flag is typed: run `unset ORBIT_TARGET`.
- **Commands that take no target field** (`version`, `runs list`, …) refuse
  `-t` and `--explain`, and ignore `$ORBIT_TARGET`, so exporting it never
  breaks them.
- **Handover's destination gNB is not the target.** On `ue handover` and
  `ue xn-handover`, `--bind`, `--gnb-*`, `--mcc`, `--mnc`, `--tac`, `--sst` and
  `--sd` describe the gNB the UE moves to, and they keep inheriting from the
  serving gNB. Only `--amf` comes from the target.
- **`ue latency` and `ue traffic`** already have a `--target` flag: the echo
  destination, unchanged. `-t` is refused there with an error that says so.
- **A number with a leading zero is an error** in every numeric flag that maps
  to a target or run field (`--tac 0010`, `--count 007`): write `10`, or `0x…`
  for hex. Before, `--tac 0010` was read as octal 8. `0x`, `0o` and plain `0`
  are accepted, as in a target file.

## The server

Most commands are Connect API clients; start the server first:

```sh
orbit serve --listen 127.0.0.1:8412 [--log-level info] [--core-profile strict-3gpp]
```

- `/metrics` (Prometheus) and `/healthz` are served on the same listener.
- `--conformance-expected-failures <file>` loads a core's expected-failures
  list at startup (a bad file stops startup); dashboard and API conformance runs
  apply it. See [docs/conformance/expected-failures.md](conformance/expected-failures.md).
- `--core-profile` selects core-compatibility quirks. Default `strict-3gpp`
  (byte-exact 3GPP, zero quirks). Use `sdcore` to enable the documented N2
  handover-transfer workaround for SD-Core; the set of quirks a core needs is a
  conformance signal, not silent tuning.
- `--server <url>` on every client command selects the endpoint. When the flag
  is omitted it comes from `$ORBIT_SERVER` the same way keys come from
  `$ORBIT_KI` / `$ORBIT_OPC`; when that is unset too, `http://127.0.0.1:8412`.
  An explicit flag always wins. The value must be an absolute `http://` or
  `https://` URL (a bare `host:port` is rejected, naming where it came from).
  When the environment chose the server, the command says so on stderr before
  its first request: `server: http://10.127.0.21:8412 (from $ORBIT_SERVER)`.
  `serve`, `load`, `conformance`, `responder`, `version` and `run` with a
  `kind: fleet` scenario never read it (fleet mode drives the engine
  in-process; the step form of `run` is an API client and does), and
  `serve --listen` keeps its loopback default.

### TLS and tokens

On loopback (the default `127.0.0.1:8412`) the server is plaintext and needs no
token. Subscriber keys (Ki/OPc) and app tokens travel in API requests, so
anything else needs a protected channel (ADR-0008 §4):

```sh
# A token: 32 characters or more, first line of a file only you can read.
mkdir -p ~/.config/orbit; (umask 077; openssl rand -hex 32 > ~/.config/orbit/api-token)

# Off loopback: TLS and the token. --tls auto makes a self-signed certificate
# once, keeps it in --state-dir/tls, and logs its fingerprint at startup.
orbit serve --listen 10.0.0.5:8412 --tls auto --token-file ~/.config/orbit/api-token
#   ... msg=TLS spki=sha256/LnPV5keF...

# On each client: pin that certificate (it shows the fingerprint and asks
# before trusting it; compare with the line serve logged) and keep the token.
orbit server trust https://10.0.0.5:8412 --token-file ~/.config/orbit/api-token
export ORBIT_SERVER=https://10.0.0.5:8412
orbit ue list
```

- **`serve` refuses to start** on a non-loopback `--listen` without TLS
  (`--tls auto`, or `--tls-cert`/`--tls-key` for a certificate you already
  have) unless you pass `--insecure-listen`, and without a token unless you pass
  `--no-auth`, which logs a warning. `0.0.0.0` and `:8412` count as non-loopback.
  A host name is resolved once, at startup: if its address later changes to a
  non-loopback one, restart `serve` so the rule is applied again.
- **The token** (`--token-file`, mode 600 or tighter; at least 32 characters
  and 8 distinct ones, so use `openssl rand -hex 32`) is required on every API
  call and on `/metrics`; `/healthz` and the dashboard's page itself stay open.
  A missing token gets `unauthenticated` ("a bearer token is required"), a
  wrong one `unauthenticated` ("invalid bearer token"). Ten wrong tokens from
  one address (an IPv6 /64 counts as one) within 10 s lock it out for 30 s:
  every request from it with a token, the right one included, gets
  `resource_exhausted` without the token being checked. A request without a
  token is never counted, so a dashboard left open before its token is
  entered cannot lock you out.
- **Clients** send `$ORBIT_TOKEN`, else the token `orbit server trust` kept in
  `~/.config/orbit/servers.yaml` for that server. `$ORBIT_TOKEN` wins for
  every server, so unset it before switching `--server` to one with another
  token. `orbit server trust` says which token it kept (`--token-file`,
  `$ORBIT_TOKEN`, or the previous pin's). They verify a pinned server
  by its key alone. `orbit server list` shows the pins (tokens masked) and
  `orbit server forget <url>` removes one. A script that cannot answer the
  prompt pins with `--fingerprint sha256/...`, and it pins only when the
  fingerprint matches: nothing is trusted on first use.
- **Keys never go in the clear off the host.** The CLI refuses to send keys, app
  tokens or the bearer token over `http://` to a host that is not loopback;
  there is no override. Behind `--insecure-listen` the server refuses such a
  request too (`failed_precondition`), for clients other than the CLI.
- **The dashboard** asks for the token the first time the server refuses it
  and keeps it for that browser tab only (sessionStorage): a new tab, or the
  same one after it is closed, asks again. With `--tls auto`, accept the
  self-signed certificate once in the browser.
- **Terminate TLS in `orbit serve`, not in a proxy on the same host.** Behind a
  TLS-terminating reverse proxy on the same machine every request reaches orbit
  from `127.0.0.1`, so it looks like loopback, plaintext requests are allowed
  through, and the keys check above can no longer tell where they came from.
  Use `--tls auto` or `--tls-cert`/`--tls-key` instead.

### Run as a service

To avoid backgrounding `orbit serve`, install it as a systemd service. One
script does install, upgrade, and uninstall (needs root):

```sh
sudo bash scripts/orbit.sh install       # from a checkout, or straight from GitHub:
curl -fsSL https://raw.githubusercontent.com/grewelltech/orbit/main/scripts/orbit.sh | sudo bash -s -- install

sudo bash scripts/orbit.sh upgrade       # rebuild/replace the binary + restart
sudo bash scripts/orbit.sh uninstall     # add --purge to also remove config
sudo bash scripts/orbit.sh status
```

It installs the `orbit` binary to `/usr/local/bin`, a unit at
`/etc/systemd/system/orbit.service`, and config at `/etc/orbit/orbit.env` — set
`ORBIT_ARGS` there (e.g. `--core-profile sdcore`), then
`systemctl restart orbit`. Manage it with `systemctl {status,restart,stop}
orbit` and follow logs with `journalctl -u orbit -f`. (`make install-service` /
`upgrade-service` / `uninstall-service` wrap the script.) Install needs Go to
build, or set `VERSION=` to download a release. The unit as installed serves
loopback only: a non-loopback `--listen` needs TLS and a token (see "TLS and
tokens" above), which the unit does not yet provision. When it does, export
`ORBIT_SERVER=https://<listen>` in the shell you run client commands from, so
they find the service without `--server` on every call.

## UE lifecycle

```sh
# Attach one UE; add --pdu-session (+ --gnb-n3) for a data session.
# --ki/--opc may be omitted when $ORBIT_KI/$ORBIT_OPC are exported (every
# command that takes credentials falls back to them).
orbit ue register --amf <host:port> --supi <imsi> --ki <hex> --opc <hex> \
    --mcc 001 --mnc 01 --sst 1 --sd 010203 --gnb-id 1 \
    --pdu-session --dnn internet --gnb-n3 <ip-reachable-from-upf>

orbit ue status     --supi <imsi>
orbit ue list
orbit ue watch      [--supi <imsi>]      # live StateStream: FSM + mobility events
orbit ue deregister --supi <imsi>        # UE-originated switch-off deregistration
```

`--supi` takes the IMSI as digits or in the NAI spelling `imsi-<digits>`
(5 to 15 digits either way); ORBIT normalises it once at the server, so a UE
registered as `imsi-001010100007500` is the same session as `001010100007500`
and is stored and shown as digits. The same holds for `--base-imsi` and for
scenario `supi`, `range.base` and `supi_base` values.

`watch` streams lifecycle states (`REGISTERING`, `AUTHENTICATED`,
`SECURITY_ESTABLISHED`, `REGISTERED`, `SESSION_ACTIVE`, `DEREGISTERED`) and
mobility states (`HANDOVER_STARTED`, `PATH_SWITCH_COMPLETE`, `HANDED_OVER`,
`HANDOVER_FAILED`). Mobility events are also logged by `orbit serve` at info
level with timestamps and gNB/UE identifiers.

## User-plane data

Run these from a host on the UPF access network (see topology above).

```sh
orbit ue ping    --supi <imsi> --dst 8.8.8.8 --count 3            # ICMP over N3
orbit ue latency --supi <imsi> --target 8.8.8.8 --probes 20       # RTT/jitter/loss (loom)
orbit ue traffic --supi <imsi> --target 8.8.8.8:9999 \            # throughput (loom)
    --rate 20Mbps --packet-size 1200 --duration-ms 5000
orbit ue stats   --supi <imsi>                                    # per-QFI UL/DL counters
```

`traffic` and `latency` embed loom over the UE's GTP-U tunnel: `traffic`
reports bytes/packets and achieved Mbps; `latency` reports replies over the
probes sent, loss % among them, min/mean/max RTT plus jitter, and the elapsed
time with the effective probe rate:

```text
2912/2913 replies (0% loss); 87 not sent: UE <imsi> has no open N3 data path  rtt min/mean/max 2.10/2.80/9.40 ms  jitter 0.40 ms  over 37.4 s, 77.9/s
stopped: UE <imsi> has no open N3 data path
```

A probe whose send fails is counted as *not sent* (with the first send
error), never as loss. If the UE is deregistered or loses its data path
mid-run, the run stops there and says why (`stopped:`) rather than spending
its remaining probes; a transient send error does not stop it.
`--spacing-ms` is the pause after each reply or timeout, so the probe rate is
below 1000/spacing by the round trip: wait for `latency` to exit before
deregistering the UE, or size the wait by the measured rate, not
probes × spacing. `--rate` empty means unlimited.
`stats` reads the tunnel's per-QFI uplink/downlink packet and byte counters
(cumulative since the data path opened; empty until the first
ping/traffic/latency run touches it).

All UEs on one gNB share a single N3 socket (one bind per gNB N3 address);
each UE stamps its own TEID/QFI on uplink and the downlink is demultiplexed
back to it by TEID, so any number of UEs on the same gNB can run data
commands — and hold app sessions — concurrently.

## Application traffic (VoIP / MOS)

`orbit ue app voip` places a real RTP/RTCP call from a registered UE through
its GTP-U data path to a **stock loomd agent** (loom ≥ v0.10) on the N6
network, and scores it with the ITU-T G.107 E-model:

```sh
# On the N6 box (once), either:
loomd --token $TOKEN                                   # stock loom agent
orbit responder --bind 0.0.0.0:9551 --token $TOKEN     # same agent, orbit binary

orbit ue app voip --supi <imsi> --peer <n6-host>:9551 \
    --codec g711 --ptime 20ms --jb 40 --duration 60s [--json]
```

`orbit responder` hosts loom's agent inside the ORBIT binary, so a run needs
only one binary (ADR-0007). It is interchangeable with stock `loomd` — same
control plane, same app engines — and the two are chosen freely per
deployment. `--bind` is **required and has no default**: the responder is a
remotely-aimable traffic generator, so its reachability is always stated
explicitly. Binding a routable address without `--token` warns loudly and
leaves the control plane open to anyone who can reach it.

`--peer` is loomd's control address on the **management** network; if the N6
media address differs, add `--peer-data-ip`. A server-level default can be
set once at startup with `orbit serve --loom-agent <n6-host>:9551
[--loom-token $TOKEN]`, after which `--peer`/`--token` may be omitted
per call (per-call values override the defaults). The CLI streams one line per
interval from *both* ends of the call — MOS/R, jitter, loss, jitter-buffer
discard, RTT, and one-way delay labeled with its method and error bar
(`owd 0.61±0.05ms (timesync)`) — with correlation events (handover phases,
GTP-U End Markers) printed inline as they arrive, then a both-end report with
media-gap summaries and the annotated event timeline. The exit code is
non-zero when the call fails, e.g. a media handshake timeout because RTP
cannot reach the N6 box (the firewall must allow loomd's control port from
the management network and the RTP port range from the UPF's N6 subnet).
One call per UE at a time; UEs on the same gNB call concurrently over the
gNB's shared N3 socket, and a call survives that UE's handover (TEID rebind,
with the UPF's End Marker on the old path reported as a correlation event).

## Application traffic (HTTP / video QoE)

`orbit ue app http` and `orbit ue app video` run real TCP applications from a
registered UE: loom's HTTP/1.1 (+ optional TLS 1.3 / HTTP/2) client, and its
ABR video player, ride a per-gNB userspace TCP/IP stack over the UE's GTP-U
tunnel — real SYN and TLS ClientHello bytes travel as inner IP packets — to
the HTTPOrigin a **stock loomd agent** (loom ≥ v0.11) serves on the N6
network. The video far end *is* the http origin (loom's player is
client-only): one `loomd` serves voip, http, and video sessions alike.

```sh
# Web objects in a think-time loop; TTFB/goodput/error interval lines:
orbit ue app http  --supi <imsi> --peer <n6-host>:9551 \
    --object-size 100KB --think 200ms..2s --duration 60s \
    [--url-path /object/65536] [--objects N] [--tls [--h2] --tls-insecure] \
    [--port-min 40200 --port-max 40210] [--json]

# A generated HLS ladder under the ABR buffer model; buffer/stall/startup lines:
orbit ue app video --supi <imsi> --peer <n6-host>:9551 \
    --ladder 240p:400k,480p:1200k,720p:2500k --duration 180s \
    [--seg-duration 4s] [--segments 150] [--abr throughput|buffer] \
    [--start-threshold 2s] [--buffer-target 12s] [--rebuffer-target 4s] \
    [--tls ...] [--port-min ...] [--json]
```

`--peer`/`--token`/`--peer-data-ip` and the server-level defaults work exactly
as for voip. Parameters travel verbatim to **both** ends (each engine reads
only the keys it documents), so `--ladder`/`--seg-duration`/`--segments`
configure the origin's generated manifest *and* pin the player's expectation —
one grammar, both sides. The firewall matrix extends accordingly: the origin's
data port (advertised back as `media port` in the report) must be reachable
from the UPF's N6 subnet; pin it with `--port-min`/`--port-max` like the RTP
range.

The `http` CLI streams one line per interval from both ends — requests,
errors, TTFB p95, goodput, plus TCP-connect/TLS-handshake means when new
connections were opened (the origin end serves counts and goodput only; it
measures no client-side latencies) — and the report carries the whole-run
percentiles. The `video` CLI streams buffer level, interval bitrate, stall
count/time, and startup delay; the report summarizes startup, stalls with
their timestamped events, rebuffer ratio, and average bitrate.

**TLS, honestly:** the origin generates a self-signed certificate per flow,
and loom's control plane does not expose it remotely (`CertificatePEM` is an
in-process accessor; the Configure response carries only the data port), so
orbit cannot pin a remote origin's certificate for you — and neither can you:
every session Configures a *new* flow with a *new* certificate, so no PEM
obtained out of band ever matches, and a pinned session would pass setup and
then burn its whole duration on 100%-error samples. orbit therefore **refuses
`--tls-ca`** (and a `tls_ca` param in fleet cohorts) up front. Use plain
HTTP/1.1, or `--tls --tls-insecure` (an explicit lab-only opt-in; verification
is off, which is the only working TLS path to a stock loomd origin today).
The flag returns when loom can hand out the per-flow origin certificate over
the control plane.

**Handover semantics (stated, not hidden):** an *intra*-gNB handover swaps
TEIDs below the TCP stack — established connections survive and see only
delay/loss. An *inter*-gNB handover moves the UE's address between gNB stacks,
which **aborts** connections live during the move (the `TCP_CONNS_RESET`
correlation event names the count); the http client's next request reconnects
via the target gNB, and the video player retries the failed segment — visible
as a TTFB spike or a stall, which is the realism the tool exists to measure.
Handover correlation covers the TCP apps too: annotations join the handover
with buffer drain → stall → ABR downshift (video) or error-burst/TTFB-spike
evidence (http), the latter labeled *coincident* — temporal overlap, not
causal proof.

## Mobility (handover)

The UE must already be registered (ideally with a session). Use a distinct
`--bind`/`--gnb-n3` and a fresh `--gnb-id` for the target.

```sh
# N2 (AMF-mediated) handover:
orbit ue handover    --supi <imsi> --amf <host:port> \
    --gnb-id 2 --bind 172.17.50.13 --gnb-n3 172.17.50.13

# Xn handover (NGAP PathSwitch; Xn prep stubbed in-process):
orbit ue xn-handover --supi <imsi> --amf <host:port> \
    --gnb-id 2 --bind 172.17.50.13 --gnb-n3 172.17.50.13
```

Both move the UE's session to the target gNB and emit mobility states on
`watch`; Xn additionally emits `PATH_SWITCH_COMPLETE` when the AMF's
PathSwitchRequestAcknowledge arrives (the downlink cutover point — N2 has no
RAN-visible equivalent). On SD-Core, **Xn** completes with user-plane continuity; **N2**
completes the control-plane handover but the downlink does not follow — an open
upstream SMF bug (not the decode issue the `sdcore` profile fixes). The
[N2 example](../examples/attach-and-handover-n2.yaml) lays out all three N2
issues; see also [interop/sdcore.md](interop/sdcore.md).

## Load / performance

`orbit load` is a direct-drive benchmark (it does not go through the API
server). It reports **sim** vs **integration** capability separately — run it
against a mock for the former and the real core for the latter.

```sh
# Rate-controlled attach storm with an SLO gate (non-zero exit on breach):
orbit load --amf <host:port> --base-imsi 001010100007500 --count 100 \
    --ki <hex> --opc <hex> --mcc 001 --mnc 01 --sd 010203 \
    --rate 20 --concurrency 64 \
    --slo-min-success 0.99 --slo-reg-p99 3s

# Linear ramp instead of a fixed rate ("find the knee"):
orbit load ... --ramp 5:80:30          # 5→80 attach/s over 30s
orbit load ... --rate 175 --arrival poisson   # exponential inter-arrivals

`--arrival` selects the arrival process for the offered rate. The default
`constant` meters arrivals evenly, which is the easy case for a core: a uniform
175/s never presents the transient bursts a real 175/s does. `poisson` draws
exponentially distributed inter-arrival times about the same mean, which is how
independent subscriber attaches actually arrive. Near saturation the difference
is material — measured against SD-Core at an offered 175/s, registration P50 was
190 ms under `constant` and 244 ms under `poisson`, the same load presented
honestly. Set `ORBIT_LOAD_SEED` to make the arrival sequence reproducible.

# Multi-gNB muxing and per-UE data sessions:
orbit load ... --gnb-count 4 --pdu-session --gnb-n3 <ip>

# One N2 source address per gNB (or one shared address; see "Choosing the N2
# source address" above):
orbit load ... --gnb-count 2 --bind 172.17.50.21,172.17.50.22

# Soak: sustain for a duration with a resource trend:
orbit load ... --duration 10m --sample-interval 15s
```

Output is per-procedure latency (P50/P99/P99.9), achieved rate, success/failure
counts, and (for soak) a goroutine/RSS trend. The `--slo-*` flags turn a run
into a CI gate.

## Conformance / regression

`orbit conformance` runs the spec-cited regression suite in-process against a
live core and exits non-zero on any failure — an integration-CI gate.

```sh
orbit conformance --amf <host:port> [--bind <ip[:port]>] [--json] \
    [--category negative-ie] [--gnb-base 0x400] [--per-test-timeout 15s] \
    [--target-release Rel-16] [--expected-failures <core-list.yaml>]
```

The `mobility` category registers a real UE and hands it over by Xn, so it
needs a subscriber and a gNB N3 address reachable from the UPF. Without
`--supi` its checks SKIP with "no UE configured":

```sh
export ORBIT_KI=... ORBIT_OPC=...       # or --ki / --opc
orbit conformance --amf 172.17.50.11:38412 --mcc 001 --mnc 01 --sd 010203 \
    --category mobility --supi 001010100007500 --dnn internet \
    --gnb-n3 172.17.50.12 [--traffic 192.168.250.1:7] --per-test-timeout 20s
```

Every `--gnb-n3` (here and on `ue register`, `ue handover`, `load` and
`runs`) must be an IPv4 address, and an IPv4-mapped form is accepted. IPv6 N3
is not supported yet (#156).

`GTPU-END-MARKER-ON-PATH-SWITCH` registers the UE with a PDU session on one
gNB and hands it over to a second gNB. The second gNB uses the same `--bind`
and `--gnb-n3` and a gNB ID 0x8000 above the first. The check then waits
5 s after the Path Switch Request Acknowledge. Give `--per-test-timeout`
room for the registration, the handover and that wait. It passes when a GTP-U
End Marker arrives on the old downlink tunnel at the source gNB, and no
G-PDU arrives on that tunnel after it. `--traffic` names an optional UDP echo
peer the UE sends to across the handover, so downlink is flowing on the old
path. Run with `--traffic` so the run has a G-PDU positive control. Without it
the check can still FAIL or PASS on the marker, but the run carries no
downlink evidence. Both gNBs share `--gnb-n3`, so after the switch only the
downlink tunnel's TEID changes, not its address. The check keys on the old
TEID at the source gNB's N3 socket, so the shared address does not make the
old and new tunnels indistinguishable. The detail records the old tunnel, the
End Marker counts, whether the marker carried a QFI (recorded, not asserted),
and the downlink seen before the switch. It never records the keys.

The `nas` category also needs the UE flags (`--supi`, the keys and
`--gnb-n3`), and SKIPs without them. `NAS-T3560-AUTH-RETRANSMIT` registers the
UE without a PDU session and withholds its Authentication Response. It passes
when the AMF retransmits the Authentication Request four times, one T3560 apart,
and then releases the UE with a UE Context Release Command
(TS 24.501 5.4.1.3.7 b)). Operators may configure the timer, so the check
does not assume the 6 s default:
- It waits up to 1.5 × 24 s (the largest value in Table 10.2.2) for the first
  retransmission.
- After that it expects each next request, and finally the release, within
  1.5 × the observed timer.

The detail gives each gap and the estimated timer, and notes a gap far from it
or from the 6 s default. The check can take up to 190 s, so it raises its own
timeout.

`NAS-T3550-ACCEPT-RETRANSMIT` withholds the Registration Complete instead. It
passes when the AMF retransmits the Registration Accept four times, one T3550
apart, and then stops (TS 24.501 5.5.1.2.8 c)). A release is recorded but not
required. It SKIPs if the core's Registration Accept assigns no 5G-GUTI,
because T3550 then never starts. Its timing is estimated the same way, with
18 s as the largest T3550, so it can take up to 145 s. A core at the default finishes in about 33 s, because the release
ends the wait. `--per-test-timeout` is a floor for every check, and a check
that needs longer says `per-test timeout raised to 3m10s for this check` in
its detail.

```sh
orbit conformance --amf 172.17.50.11:38412 --mcc 001 --mnc 01 --sd 010203 \
    --category nas --supi 001010100007500 --gnb-n3 172.17.50.12
```

The `security` category's `NAS-SMC-NO-NIA0-REPLAYED-CAPS` also needs the UE
flags. It registers the UE twice, each time stopping at the Security Mode
Command. The first registration offers the usual algorithms, and the AMF must
not select NIA0 and must replay the UE's capabilities exactly. The second
offers only NIA0 and NIA3, and the AMF must not select NIA0 or skip the
command (selecting NIA3, rejecting or releasing all pass). Neither
registration completes, so the check leaves no UE registered.

Checks are framed as graceful-rejection / crash-safety regression guards. Each
names the exact clauses it asserts and the 3GPP releases they come from,
verified in the 3GPP archive through Rel-19, and every result ends with the
release it requires and those clauses, for example
`requires Rel-15 or later; TS 38.413 10.3.5 Reject IE bullet 1 (Rel-15 onward, wording unchanged through Rel-19)`.
`--target-release` names the release the core claims: a check that requires a
newer one reports SKIP, never FAIL. It defaults to Rel-18, the SD-Core
release, with TS 29.244 (PFCP) pinned to Rel-16; an explicit value replaces
Rel-18 and the pin still applies where it is older, and `--target-release unset`
runs every check. The output and `--json` record the targets
(`target-release: Rel-18 (TS 29.244 at Rel-16)`, and `SpecTargets` in JSON).
The evidence and caveats are in
[docs/conformance/target-release.md](conformance/target-release.md).
Every missed **shall** is FAIL and DEVIATION is reserved for a missed
**should**. `--expected-failures <file>` names the core's list of known,
tolerated FAILs, each matched exactly on check and observed text. A listed
FAIL still prints FAIL, tagged `(expected: <finding>)`, but does not fail the
gate. Entries whose check now gives something else print as stale. With no
list, every FAIL gates. The format and rules are in
[docs/conformance/expected-failures.md](conformance/expected-failures.md).
`--json` emits per-check verdict, expected/observed, the rendered `spec_ref`,
the structured `spec_refs` (clause, strength, since, verified versions,
changes), `min_release` and, on a listed FAIL, `expected_failure`. The full table is
[docs/conformance/checks.md](conformance/checks.md), with the quoted normative
text per check in [docs/conformance/provenance/](conformance/provenance/).

## Scenarios (`orbit run`)

Instead of long, repetitive command lines, describe a whole test declaratively
in YAML and run it: `orbit run scenario.yaml`. The runner is an ordinary API
client (it drives the same operations as the `ue` commands), so it needs a
running server (`orbit serve`).

A scenario holds the workload: its **gNBs**, **UEs** and an ordered **steps**
list. The core, keys and addresses come from a target: `-t`, `$ORBIT_TARGET`,
or a `target:` line in the file ([targets.md](targets.md#scenario-files)):

```yaml
name: attach-and-handover
target: lab1                 # or -t lab1 on the command line
gnbs:
  - { name: gnb-1 }          # gNB IDs and N2/N3 addresses come from the target
  - { name: gnb-2 }
ues:
  - { name: ue1, gnb: gnb-1, pdu_session: true }   # SUPIs come from the target's pool
  - { gnb: gnb-1, range: { count: 3 } }            # the next three
steps:
  - register: all                                  # or one UE by name
  - ping:     { ue: ue1 }                          # echoes the target's responder
  - latency:  { ue: ue1, probes: 20 }
  - traffic:  { ue: ue1, target: 203.0.113.21:9999, rate: 20Mbps, duration: 5s }
  - handover: { ue: ue1, to: gnb-2, type: xn }     # or type: n2
  - deregister: all
```

A file may instead set its own `core`, `credentials`, `gnbs[].id`/`bind`/`n3`
and `ues[].supi`/`range.base`. It is then self-contained, as before: `-t` on it
is an error and `$ORBIT_TARGET` is ignored. A file can't do both.

Steps run in order and stop at the first failure (a step fails on an RPC error,
or on a natural assertion like a ping with zero replies). `wait: 5s` pauses
between steps. A ready-to-edit example is in
[`examples/attach-and-handover-xn.yaml`](../examples/attach-and-handover-xn.yaml)
(and an N2 variant,
[`attach-and-handover-n2.yaml`](../examples/attach-and-handover-n2.yaml)).
The same topology rules apply — data-plane and handover steps must run where the
UPF's N3 is reachable, with a fresh gNB ID per run.

### Fleet mode (preview)

A `kind: fleet` scenario ([`examples/fleet.yaml`](../examples/fleet.yaml))
*generates* a topology of gNBs and a UE population running continuous behaviours
(mobility + a traffic mix) for a duration, rather than listing explicit steps —
for scale / soak / mass-mobility runs (design in
[docs/adr/0004-fleet-population-mode.md](adr/0004-fleet-population-mode.md)).
Its core, keys, gNB IDs and per-gNB N2/N3 addresses come from the target, whose
`ran.n2_source_ips` must be addresses the host owns, one per gNB. `orbit run` on a fleet
file prints the plan, **attaches the population** across the gNBs, then runs
**mobility** — each mobile UE moves along a track across the gNB grid and hands
over (Xn) when a neighbour cell becomes stronger by the a3-offset (RSRP/A3
model), not on a timer — and **traffic** (one loom flow per non-mobile UE over
its gNB's shared N3 socket) for `run.duration`, then reports (attach / handovers
/ traffic / deregister).

#### Bulk uplink and downlink

`traffic.bulk` reserves an exact number of non-mobile UEs for constant-rate UDP
load. The rates are **per UE**, so this example offers 200 Mbps uplink and
500 Mbps downlink in aggregate:

```yaml
fleet:
  count: 80
  pdu_session: true
behaviors:
  traffic:
    bulk:
      ues: 10
      uplink_rate: 20Mbps
      downlink_rate: 50Mbps
      peer: 172.17.192.2:9551       # N6 loomd control address
      token: ${LOOM_TOKEN}          # omit for an unauthenticated loomd
      # peer_data_ip: 172.17.192.2  # when control and data addresses differ
      # packet_size: 1200           # UDP payload bytes; default 1200
```

The N6 loom agent runs one shared uplink receiver and one downlink sender per
UE. Downlink senders target the UEs' PDU addresses, so the N6 host must have the
normal route to the UE pool through the UPF. Bulk UEs are reserved before the
optional percentage-based `mix`; mix shares still sum to 1.0 and divide the
remaining non-mobile UEs. At least one direction must be configured.

#### App cohorts (real application traffic at fleet scale)

Traffic mix entries with `app:` instead of `profile:` declare **application
cohorts**: their share of the fleet runs loom's real protocol engines
(`voip` — RTP/RTCP with G.107 MOS; `http` — HTTP/TLS over the per-gNB gVisor
netstack; `video` — the ABR player against the http origin) through their
GTP-U data paths to a stock `loomd` on the N6 network, exactly like
`orbit ue app …` but for a population:

```yaml
behaviors:
  traffic:
    mix:
      - { profile: web, share: 0.7 }                       # synthetic flows
      - { app: voip, name: voip-calls, share: 0.2, peer: 172.17.60.10:9551,
          params: { codec: pcmu, port_min: "40000", port_max: "40100" } }
      - { app: http, name: web-real, share: 0.1, peer: 172.17.60.10:9551,
          params: { object_size: 64KB, think: 200ms, tls: "true", tls_insecure: "true" } }
```

Cohorts need `fleet.pdu_session: true`; `peer` is the loomd control address
(`token`, `peer_data_ip`, and `params` match the `orbit ue app` grammar; the
firewall matrix above applies unchanged). The far-end plumbing is shared, not
multiplied: **one control connection and one TimeSync loop per distinct
loomd** across the whole fleet, **one shared http origin per cohort** (the
origin serves any number of clients), and **one voip answerer per member** —
the answerer latches a single source, so a voip cohort must fit inside its
`port_min..port_max` range (an oversized cohort is refused up front with the
required range). TCP cohorts share each gNB's single gVisor netstack; voip
rides the lightweight dgram path, so fleet voice never pays the gVisor cost.
RTCP reporting is randomized per RFC 3550 §6.3/A.7 inside loom, so thousands
of concurrent calls do not synchronise into report storms.

Cohort results are **distributions** (p5/p50/p95 across members): MOS for
voip, TTFB and goodput for http, stall time and rebuffer ratio for video —
printed in the fleet report and, with `orbit run --metrics-listen host:port`,
served live on `/metrics` as `orbit_fleet_app_*` gauges labeled
`{cohort, q}` (cohort names, never SUPIs — bounded cardinality).

## The API

The server exposes a Connect API (gRPC, gRPC-Web, and JSON/REST on one port).
The CLI is a thin client of it; the same operations are reachable directly, e.g.
over JSON:

```sh
curl -s http://127.0.0.1:8412/orbit.v1.UEService/List \
    -H 'Content-Type: application/json' -d '{}'
```

With a token (see "TLS and tokens"), add `-H "Authorization: Bearer $ORBIT_TOKEN"`;
for a `--tls auto` server, `curl -k --pinnedpubkey 'sha256//<base64>' https://...`
pins the same key: the fingerprint `orbit serve` logs, with `sha256//` for
`sha256/`.

Services: `UEService` (Register, Deregister, Status, List, Ping, Traffic,
Latency, Handover, XnHandover, StateStream, DataStats, StartApp, AppStream,
StopApp), `CellService` (RunNGSetup),
`SystemService` (GetInfo). Schema in [`proto/orbit/v1`](../proto/orbit/v1);
regenerate the Go/Connect bindings with `make gen`.

## Testing

```sh
make test          # unit-CI: headless, no core required
make integration   # integration-CI: needs a live core; ORBIT_AMF_N2 overrides the AMF
```

Integration tests are `//go:build integration`-tagged. The user-plane and
handover integration tests read `ORBIT_TEST_KI`/`ORBIT_TEST_OPC` and expect to
run from a host on the UPF access network with distinct routed source IPs.
