# Targets

A target describes **where** tests run: the core, the subscriber pool, the RAN
addresses and the N6 responder. Every command takes one with `-t <name>`, and
scenario files take one with a `target:` line, so the same facts are written
once instead of on every command line and in every scenario. The design is
[ADR-0008](adr/0008-targets-and-runs.md); this page is the reference.

```sh
orbit target init lab1 --amf <amf-host>:38412 --mcc 001 --mnc 01 --sd 010203 --first-supi 001010000000001
orbit target probe lab1
orbit conformance -t lab1
```

## Where targets live

- `./orbit.yaml`: one target, or a YAML list of them.
- `~/.config/orbit/targets/<name>.yaml`: one target per file, named after the
  file. This is where `orbit target init` writes, with mode 0600.

`-t <name>` looks in `./orbit.yaml` first, then in the config directory.
`orbit target list` shows every target in both places, including any that fail
to load, with the error. One bad entry in an `./orbit.yaml` list fails only
when that target is selected. A YAML syntax error fails the whole file.

## The document

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
  dnn: internet
  profile: strict-3gpp
  upf_n3: 198.51.100.10:2152
subscribers:
  - name: main
    first_supi: "001010000000001"
    count: 100
    ki:  ${ORBIT_KI}
    opc: ${ORBIT_OPC}
ran:
  n2_source_ips: [192.0.2.50-192.0.2.59]
  n3_ips: [198.51.100.50-198.51.100.59]
  gnb_ids: { first: 100, count: 900, bits: 24 }
app:
  responders:
    - name: n6
      control: 203.0.113.20:9551
      token: ${ORBIT_LOOM_TOKEN}
      data_ip: 203.0.113.21
```

Unknown keys are errors. `ki`, `opc` and `app.responders[].token` are secret:
they show as `<redacted>` in `--explain`, `target show` and every error.

| Field | What it is | Flags | Env |
|---|---|---|---|
| `core.amf` | AMF N2 endpoint, host:port | `--amf` | |
| `core.plmn.mcc` / `mnc` | PLMN, 3 / 2-3 digits | `--mcc` / `--mnc` | `$ORBIT_MCC` / `$ORBIT_MNC` |
| `core.tac` | tracking area code, 24 bits | `--tac` | |
| `core.slices` | S-NSSAIs; the commands use the first | `--sst`, `--sd` set the first slice's fields | |
| `core.dnn` | data network name | `--dnn` | |
| `core.upf_n3` | the UPF's N3 endpoint, for GTP-U checks | `--upf-n3` (conformance) | |
| `subscribers[].first_supi` | first SUPI of the pool | `--supi` (conformance, ue register), `--base-imsi` | |
| `subscribers[].count` | subscribers in the pool | | |
| `subscribers[].ki` / `opc` | keys, 32 hex digits | `--ki` / `--opc` | `$ORBIT_KI` / `$ORBIT_OPC` |
| `subscribers[].routing_indicator` | SUCI routing indicator | `--routing-indicator` | |
| `ran.n2_source_ips` | N2 (SCTP) source pool | `--bind` | |
| `ran.n3_ips` | gNB N3 (GTP-U) addresses | `--gnb-n3`, `--n3-bind` (conformance) | |
| `ran.gnb_ids` | the gNB ID range: `first`, `count`, `bits` | `--gnb-id`, `--gnb-base`, `--gnb-bits` | |
| `ran.gnb_name` | gNB name prefix | `--gnb-name` | |
| `app.responders[]` | N6 far ends: `control`, `token`, `data_ip` | `--peer`, `--token`, `--peer-data-ip` | |

A list's fields come from its first element: the first pool, the first
responder. On `ue handover` and `ue xn-handover` the gNB flags describe the
destination gNB and are never filled from a target; only `--amf` is.

### Addresses

- **`ran.n2_source_ips`** entries take these forms:
  - an IP: `192.0.2.50`, `fe80::1%eth0`;
  - an ip:port, where port 0 is an ephemeral port and any other port is that
    exact port: `192.0.2.50:0`;
  - a host name, with or without a port: `ran-n2.lab`, `ran-n2.lab:0`. It is
    passed on unresolved, as a typed `--bind` is;
  - a range: `192.0.2.50-192.0.2.59`. Both ends share a family and an IPv6
    zone;
  - a CIDR: `192.0.2.0/28`.

  Ranges and CIDRs are IP-only and take no port.
- **`ran.n3_ips`** is a pool in the same forms as `ran.n2_source_ips`: IPs, host
  names, ip:port/host:port, and IP-only ranges and CIDRs, expanded by the same
  rules. `--gnb-n3` needs an IP: a host-name entry fills `--n3-bind`, leaves
  `--gnb-n3` unfilled, and says so.

### gNB IDs for a conformance run

A conformance suite presents three gNB IDs per check: the check's own, a
handover target, and a liveness probe. When the target bounds its IDs
(`ran.gnb_ids.count` set), a suite run from a plan packs them into the block
from its base: with N registered checks, check i takes base+i, its handover
target base+N+i, and its probe base+2N+i. The block must hold 3N IDs (27
today), or the run is refused, naming the size it needs. Without a count, the
suite keeps the legacy layout: base+i, with the handover target 0x8000 and
the probe 0x9000 above it.
- **One address from a pool.** Until the server leases from the pool, a flag
  that takes one address takes the first entry:
  - a range gives its low end;
  - a CIDR gives its first host address (IPv4 /30 and wider skip the network
    address);
  - `--bind` keeps the entry's port, `--gnb-n3` drops it, and `--n3-bind` adds
    the GTP-U port 2152 when the entry has none.
- **One address per gNB.** `load` and `runs start-load` take `--gnb-count`
  addresses for their per-gNB `--bind` list, and a scenario takes one per gNB.
  The pool is walked in order, ranges and CIDRs address by address, capped at
  65 536.
  - A pool of one address is shared.
  - A longer pool with too few addresses is an error naming both counts:
    `ran.n2_source_ips has 3 addresses, orbit load needs 4 (one per gNB)`.

### Numbers and text

- **A number with a leading zero is an error**, in a file, an env var or a flag:
  `0010 has a leading zero; write 10, or 0x… for hex`. `0x`, `0o`, `0b` and a
  plain `0` are accepted. YAML would read `0010` as octal 8, and a reader takes
  it as decimal 10, so neither guess is made.
- **A quoted number is an error** in a numeric field: `"10" is quoted; write the
  number unquoted`.
- **Text fields keep their text as written.** `mcc: 001`, `mnc: 01` and an
  unquoted `first_supi` keep their digits, and `sd: 010203` stays `010203`.
- **`${ENV}` expands in text fields only**, when the file loads. An unset
  variable is an error naming it. The value is attributed to the target, with
  the variable noted: `target lab1.yaml:16 via ${ORBIT_KI}`. A number takes no
  `${VAR}` (`${TAC} is not expanded in numeric fields`); override one from the
  environment with its `$ORBIT_*` binding, or with a flag.

## Precedence

Layers, lowest first: **built-in default < target < `$ORBIT_*` < flag**, field
by field. So a target never overrides a flag you typed, or an `$ORBIT_MCC` in
your shell. When two flags set the same field (`--gnb-n3` and `--n3-bind`), the
one you typed fills the other.

- **`--explain`** on any command prints each field the command uses: its value,
  the layer and source that set it, what it overrode, and the flag that took it.
  It also lists the target fields the command does not use.
  - Without a target it only reports: a value the resolver would reject is a
    row marked `⚠`, and the command runs as it would without `--explain`.
- **An explicit empty flag clears a field**: `--bind ""` is the route-selected
  source, and `--supi ""` turns a conformance target's UE off.
- **A pool's `first_supi` must be in the target's PLMN** (its leading MCC+MNC
  digits), or the target doesn't resolve.
- **An explicit `-t ""`** clears target selection: `$ORBIT_TARGET` is ignored,
  and a command runs with no target. A scenario file's own `target:` still
  applies.
- **`$ORBIT_TARGET`** names the target when `-t` doesn't. Commands that take no
  target field ignore it, so an export never breaks `orbit runs list`.
  - A stale `$ORBIT_TARGET` naming a missing target fails every command that
    takes target fields: run `unset ORBIT_TARGET`.

## Scenario files

A step or fleet scenario holds the workload: counts, rates, steps and
behaviours. It takes its core, keys and addresses from a target.

- **The `target:` line** is either a name (`target: lab1`) or a target document
  written inline, whose `version`/`kind`/`name` may be omitted. A fleet
  behaviour's own `target:` (an echo address in `latency` or `traffic`) is a
  different key.
- **Which target wins:** `-t`, then `$ORBIT_TARGET`, then the file's `target:`.
  Overriding the file's choice prints a line saying so:
  `target lab2 (flag -t) overrides the file's target: lab1`.
- **A file uses a target or its own setup, not both.** A file that names a
  target cannot also set any of these; it is an error naming the key:
  - `core`, `credentials`;
  - in a fleet: `topology.gnbs.source_ips`/`n3_ips`/`id_base`,
    `fleet.supi_base`, a behaviour's `peer`/`token`/`peer_data_ip`;
  - in a step file: `gnbs[].id`/`bind`/`n3`, `ues[].supi`/`range.base`.

  A file that sets them and names no target is **self-contained**, as before.
  `-t` on it is an error, and `$ORBIT_TARGET` is ignored with a notice. A file
  with neither is an error: `x.yaml names no target: add target: <name> or pass
  -t`.
- **What the target fills:**
  - the core (with its first slice) and the first pool's keys;
  - a fleet's first SUPI and `id_base`, and one N2 and N3 address per gNB;
  - a step file's gNBs, which take the range's first ID, then +1 and so on, and
    the pools' addresses in order;
  - ports in the pools: a gNB's N3 address is an IP, so an N3 ip:port entry
    gives the IP alone, on fleets and step files alike, and `--explain` notes
    "port dropped". A fleet's gNB sources take an IP too: an N2 `ip:0` is the
    IP, and a fixed port is an error naming the entry ("fleet gNB sources take
    an IP; remove :38412 from ran.n2_source_ips[2]"), since it may be
    deliberate;
  - step UEs' SUPIs, allocated from the pool's first in file order, so no two
    entries overlap. A scenario larger than the pool's `count` (when set) is an
    error. Whatever the count, the SUPIs keep the first one's width and its
    PLMN prefix: `subscribers[main]: 001019999999999 + 4 UEs leaves PLMN
    001/01` is an error, on fleets too;
  - app cohorts and bulk traffic, which take the first responder.
- **Echo destinations:** a fleet's `latency.target`, and a step's `ping.dst` and
  `latency.target`, take the first responder's `data_ip` when the file omits
  them. With no responder that is an error naming the step.
- **Naming UEs in steps.** A step names its UE (`ues: [{ name: ue1, … }]`, then
  `ping: { ue: ue1 }`), since the SUPIs are allocated. A literal SUPI in a step
  is an error with a target. Range entries are reached through `all`.
- **On the server.** `runs start-fleet` applies the target on the client and
  sends the scenario with it applied, the keys in the request. The server stores
  no targets and refuses a scenario that still names one.

## Commands

- **`orbit target init <name>`** writes a target. It runs one NG Setup through
  the server and records the N2 source it used; `--bind` picks the source
  instead, and `--no-probe --bind <ip>` skips the NG Setup.
  - Keys are written as `${ORBIT_KI}`/`${ORBIT_OPC}` unless `--write-keys`.
  - `--responder name=control[,data_ip]` adds an N6 responder (repeatable).
  - It never writes through a symlink, and replaces a file only with `--force`.
  - `--here` writes `./orbit.yaml` when that file doesn't exist.
- **`orbit target list`** shows every target with its file:line and load error.
  A config-directory file whose name differs from its target's is an error.
- **`orbit target show <name>`** prints the target as loaded, keys masked.
  `--resolved` shows each field after default < target < env, with its source,
  and `--json` gives the same as JSON.
- **`orbit target probe <name>`** runs, through the server, NG Setup, one
  registration and one PDU session, using the range's first three gNB IDs and
  the pool's first SUPI. It stops at the first failure and names what to check.
  - The keys go to `--server`: keep it on loopback until it has TLS.
