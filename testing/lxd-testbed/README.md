# LXD testbed — separated N2/N3/N6

Stands up a three-node LXD environment for ORBIT on **separated** 3GPP reference
points: N2, N3 and N6 are distinct L2 domains. No interface is collapsed onto
another, which is the point of this profile — see
[`docs/design/ci-testbed.md`](../../docs/design/ci-testbed.md) §5.1 for why the
topology is something to validate rather than a convenience to pick.

Nodes run **stock Ubuntu 24.04 cloud images**. Cleanliness comes from rebuilding
rather than from an ephemeral root: `down` then `up` is fast, so a run always
starts from a known image. Addressing is static and written to disk by
cloud-init, because Aether OnRamp and SD-Core discover interfaces by reading the
network configuration from the filesystem — a DHCP lease leaves nothing there.

This creates the machines and the wiring only. Deploying a 5G core onto them is
a separate step.

## Topology

```
                 ┌──────────────┐
   mgmt ─────────┤              │
                 │  orbit-core  │  5G core (AMF, SMF, UPF …)
   N2 ───────────┤              │
                 │              │
   N3 ───────────┤              │
                 │              │
   N6 ───────────┤              │
                 └──────────────┘
                 ┌──────────────┐
   mgmt ─────────┤              │
                 │  orbit-ran   │  ORBIT: gNB + UE simulation
   N2 ───────────┤              │  N2 = NGAP/SCTP to the AMF
                 │              │  N3 = GTP-U to the UPF
   N3 ───────────┤              │
                 └──────────────┘
                 ┌──────────────┐
   mgmt ─────────┤              │
                 │  orbit-app   │  data-network side (responder / app server)
   N6 ───────────┤              │
                 └──────────────┘
```

Defaults — all configurable, see [Configuration](#configuration):

| Network | Subnet | Guest interface | Carries |
|---|---|---|---|
| `orbit-mgmt` | `10.100.0.0/16` | `eth.mgmt` | operator access only; no 3GPP traffic |
| `orbit-n2` | `10.102.0.0/16` | `eth.n2` | gNB ↔ AMF, NGAP over SCTP |
| `orbit-n3` | `10.103.0.0/16` | `eth.n3` | gNB ↔ UPF, GTP-U |
| `orbit-n6` | `10.106.0.0/16` | `eth.n6` | UPF ↔ data network |

**The addressing states which interface it belongs to.** The second octet
encodes the reference point — `10.102` is N2, `10.103` is N3, `10.106` is N6 —
so a packet capture, a routing table or a log line identifies its plane at a
glance, with nothing to look up. Management is `10.100`, the odd one out.

Each network is a `/16`, not a `/24`, so a run can grow into thousands of UE or
gNB addresses without renumbering; the third octet is free for that.

Interfaces are renamed to match, so `ip addr` on a node reads as the topology:

```
eth.mgmt   UP   10.100.0.10/16
eth.n2     UP   10.102.0.10/16
eth.n3     UP   10.103.0.10/16
eth.n6     UP   10.106.0.10/16
```

`eth0`/`eth1` ordering is an enumeration accident and tells you nothing about
which plane a NIC carries; a mis-wired interface is obvious with these names and
invisible without them. cloud-init does the renaming via netplan `set-name`,
matched on the MAC LXD assigned.

(172.16/12 would encode the same way — 172.22, 172.23, 172.26 — but 172.26.0.0
is already carrying traffic on this host.)

| Node | mgmt | N2 | N3 | N6 | Size |
|---|---|---|---|---|---|
| `orbit-core` | `.0.10` | `.0.10` | `.0.10` | `.0.10` | 8 vCPU / 16 GiB / 60 GiB |
| `orbit-ran` | `.0.20` | `.0.20` | `.0.20` | — | 4 vCPU / 8 GiB / 30 GiB |
| `orbit-app` | `.0.30` | — | — | `.0.30` | 2 vCPU / 4 GiB / 20 GiB |

The host holds `.0.1` on every bridge, so all four networks are reachable from the
host with no extra routing.

Addresses are **static netplan**, injected per node as `cloud-init.network-config`
and written to `/etc/netplan/50-cloud-init.yaml` on first boot. Interfaces are
matched by the MAC LXD assigned rather than by name, so the mapping cannot drift
from what the script intended. Only the management interface carries a default
route and DNS.

## Prerequisites

- **LXD** with the VM (qemu) driver and a usable storage pool. Assumed present.
- Network access to pull `ubuntu:24.04` the first time. Nothing is built locally.

## Usage

```sh
cd testing/lxd-testbed

./testbed.sh image     # pull ubuntu:24.04 into LXD (once)
./testbed.sh up        # create networks + nodes and start them
./testbed.sh status    # what is running, and where
```

`image` just caches the base image locally; `up` is safe to re-run and leaves
existing instances alone. Point `TESTBED_IMAGE` elsewhere for a different base.

Tear down:

```sh
./testbed.sh down            # remove nodes, profile and networks
./testbed.sh down --image    # also drop the imported LXD image
```

`down` only removes objects carrying the configured prefix, so an unrelated
environment on the same host is never at risk.

## Access

The Ubuntu images carry the LXD agent, so:

```sh
lxc exec orbit-core -- bash            # straight in
./testbed.sh console core              # console; detach with ctrl-a q
```

For SSH, set `TESTBED_SSH_PUBKEY` before `up` and cloud-init installs the key
for the `ubuntu` user:

```sh
TESTBED_SSH_PUBKEY=~/.ssh/id_ed25519.pub ./testbed.sh up
./testbed.sh ssh core
```

## From nothing to ready

```sh
./testbed.sh all      # image + up + core + apps
```

Roughly 20 minutes, most of it the core. The steps remain available
individually, which is usually what you want: `up` rebuilds the VMs in ~2
minutes without touching a working core.

## Deploying the core

```sh
./testbed.sh core           # OnRamp -> Kubernetes -> SD-Core, ~15 min
./testbed.sh core-status    # pods and interfaces
```

Automated end to end. Version is pinned by `ONRAMP_VERSION`, and everything the
core needs is installed on the node — nothing is assumed present.

### Why this is not stock OnRamp

OnRamp deploys **one collapsed data interface** by default. Separated N3/N6
needs three deviations, matching the Aether team's own reference setup:

1. **Skip `5gc-router-install`.** It assumes the collapsed model. The script
   calls `make 5gc-core-install` directly rather than `5gc-install`, so no
   vendored Makefile has to be edited.
2. **A `data_iface_n6` variable**, which stock OnRamp does not have.
   `data_iface` becomes N3, `data_iface_n6` becomes N6.
3. **`cniPlugin: macvlan` → `host-device`** on both UPF sides. This is the
   change that actually separates them, and the one most easily missed:
   `macvlan` makes a virtual child of one parent NIC so several pods can share
   it — which is exactly what lets the collapsed model work. `host-device`
   **moves the real NIC into the pod's namespace**, so each plane needs its own.

The script applies all three and prints the resulting mapping before installing,
so a wrong interface is visible before it costs 15 minutes.

### What to expect afterwards

**`eth.n3` and `eth.n6` disappear from the core node.** `host-device` moves
them into the UPF pod, addresses and all. This is correct, not a fault:

```
# on orbit-core                    # inside the UPF pod
eth.mgmt   10.100.0.10/16          access   10.103.0.100/16
eth.n2     10.102.0.10/16          core     10.106.0.100/16
```

The AMF takes the node's N2 address (`10.102.0.10`) and exposes NGAP on
38412/SCTP, so a gNB on `orbit-ran` reaches it over N2 while user-plane traffic
rides N3 to the UPF — genuinely separate paths.

### Subscribers

OnRamp's stock values file provisions only **10** subscribers, which runs out
immediately for any population test. `TESTBED_SUB_COUNT` (default 100) and
`TESTBED_SUB_START` set the range, and the script rewrites `ueId-start` /
`ueId-end` before installing.

> **Re-running `core` removes and reinstalls the SD-Core release.** OnRamp's
> install task does not *upgrade* an existing release — helm stays at the same
> revision and silently keeps the old values, so a changed subscriber range or
> interface mapping would never take effect. The script removes the release
> first to make the command genuinely repeatable. **This reset path is written
> but not yet verified end to end.**

### Helm

OnRamp requires `helm` but does not install it; its k8s role only pins a
version. The script installs it, having watched the stock path fail with
`Failed to find required executable "helm"`. (The Aether reference host carries
a `get_helm.sh` for the same reason.)

## Installing ORBIT and the responder

```sh
./testbed.sh apps          # both of the below
./testbed.sh app           # responder on orbit-app, bound to N6
./testbed.sh ran           # ORBIT on orbit-ran
```

ORBIT is built **on the host** and the binary pushed to the nodes. That keeps
repository credentials off the testbed and is faster than putting a Go
toolchain on each node. `ORBIT_VERSION` selects the ref; empty builds the
working tree as-is, which is what you want mid-change.

Both run as systemd units, so they survive a reboot and a failure is visible in
`systemctl` rather than a lost process:

| Node | Unit | Listens on |
|---|---|---|
| `orbit-app` | `orbit-responder` | `10.106.0.30:9551` — **N6 only** |
| `orbit-ran` | `orbit` | `127.0.0.1:8412` |

The responder binds its N6 address specifically rather than `0.0.0.0`: it is a
remotely-aimable traffic generator and has no business answering on the
management network. The API server binds loopback because it carries subscriber
credentials and every client command runs on that node anyway.

## Verifying the whole path

With the core deployed and ORBIT installed, this exercises N2 for real:

```sh
lxc exec orbit-ran -- orbit cell ngsetup --amf 10.102.0.10:38412 \
    --mcc 001 --mnc 01 --tac 1 --sst 1 --sd 010203 --gnb-id 1
# NG Setup accepted by AMF "AMF"
```

The deployed core uses PLMN `001/01`, TAC 1, slice `sst=1 sd=010203` — read
from the values file with
`grep -iE '^\s*(mcc|mnc|tac|sst|sd):' /opt/aether-onramp/deps/5gc/roles/core/templates/radio-5g-values.yaml`.

## Configuration## Installing ORBIT and the responder

```sh
./testbed.sh apps          # both of the below
./testbed.sh app           # responder on orbit-app, bound to N6
./testbed.sh ran           # ORBIT on orbit-ran
```

ORBIT is built **on the host** and the binary pushed to the nodes. That keeps
repository credentials off the testbed and is faster than putting a Go
toolchain on each node. `ORBIT_VERSION` selects the ref; empty builds the
working tree as-is, which is what you want mid-change.

Both run as systemd units, so they survive a reboot and a failure is visible in
`systemctl` rather than a lost process:

| Node | Unit | Listens on |
|---|---|---|
| `orbit-app` | `orbit-responder` | `10.106.0.30:9551` — **N6 only** |
| `orbit-ran` | `orbit` | `127.0.0.1:8412` |

The responder binds its N6 address specifically rather than `0.0.0.0`: it is a
remotely-aimable traffic generator and has no business answering on the
management network. The API server binds loopback because it carries subscriber
credentials and every client command runs on that node anyway.

## Verifying the whole path

With the core deployed and ORBIT installed, this exercises N2 for real:

```sh
lxc exec orbit-ran -- orbit cell ngsetup --amf 10.102.0.10:38412 \
    --mcc 001 --mnc 01 --tac 1 --sst 1 --sd 010203 --gnb-id 1
# NG Setup accepted by AMF "AMF"
```

The deployed core uses PLMN `001/01`, TAC 1, slice `sst=1 sd=010203` — read
from the values file with
`grep -iE '^\s*(mcc|mnc|tac|sst|sd):' /opt/aether-onramp/deps/5gc/roles/core/templates/radio-5g-values.yaml`.

## Configuration

[`testbed.conf`](testbed.conf) holds every tunable with its default and the
reasoning behind it. Three ways to override, in increasing precedence:

1. edit `testbed.conf` (tracked — only for changes that suit everyone)
2. `testbed.local.conf` beside it (gitignored — for one machine's specifics)
3. environment variables

```sh
# a second testbed alongside the first
TESTBED_PREFIX=orbit2 TESTBED_NET_N2=10.61.2.0/24 ./testbed.sh up
```

Nothing site-specific is committed. Addresses, paths, sizes and keys are all
parameters, so this file describes a topology rather than one host's setup.

**Prefix.** Everything is named `${TESTBED_PREFIX}-*`, default `orbit`. A host
may already carry an unrelated 5G environment — the aether-ops dev environment
uses `aether-` and its own `aether-n2/n3/n6` bridges — so the two coexist
without interfering.

**NAT** is off by default (`TESTBED_NAT=false`). A testbed that cannot reach the
internet cannot quietly come to depend on it. Turn it on if a core install path
needs to pull images during bring-up.

## Notes and known edges

**The host routes between the bridges.** Each node's default route points at the
management gateway, which is the host, and the host holds an address on all four
bridges — so a node can reach an address on a network it has no interface on
(`orbit-ran` can ping the core's N6 address). The L2 separation that matters for
catching interface-selection bugs still holds: each node only has NICs on the
reference points it belongs to, and traffic it *originates* leaves by the right
interface. If a test needs true isolation, drop forwarding between the bridges
on the host.

**Extra gNB source addresses are not configured.** Handover needs a distinct
routed source IP per gNB. Adding them is a netplan edit on `orbit-ran`, which
belongs with RAN setup rather than here.

**`lxc profile create` has hung for 20+ minutes** twice on this host while the
daemon stayed responsive to every other command. Killing the stuck client and
re-running completed instantly. See Troubleshooting.

## Troubleshooting

**A node has no address.** cloud-init writes the netplan on first boot, so give
it a moment. Check with `lxc exec <node> -- cat /etc/netplan/50-cloud-init.yaml`
and `lxc exec <node> -- cloud-init status`.

**`lxc` hangs on an otherwise idle host.** Seen twice at `lxc profile create`,
hung 20+ minutes while the daemon answered every other command instantly and
`lxc operation list` was empty — a stuck client, not a stuck daemon. Killing the
client and re-running succeeded immediately both times.

The script now bounds this rather than inheriting it: every mutating `lxc` call
runs under a timeout and is retried (`TESTBED_LXC_TIMEOUT`, default 90s;
`TESTBED_LXC_RETRIES`, default 3). A wedge becomes a slow run instead of an
indefinite stall, and a genuine `lxc` error is not retried. The root cause is
still unknown — the daemon log shows nothing at the hang times, and the host had
17 concurrent LXD connections from another workload, which may or may not be
related.

**Storage pool not found.** Set `TESTBED_STORAGE_POOL` to a pool from
`lxc storage list`.

**`lxc` hangs for minutes on an otherwise idle host.** Seen once during
bring-up: a `lxc profile create` client wedged for 20+ minutes while the daemon
itself stayed responsive to every other command. Killing the stuck client and
re-running worked, and the operation then completed instantly. If a step appears
to hang, check `lxc operation list` — an empty list with a stuck client means
the client, not the daemon.

**"The instance you are starting doesn't have any network attached to it."**
Harmless. LXD prints this at `lxc init` time because the profile carries no NIC;
the script attaches per-node NICs immediately afterwards, which is what keeps
each node on only the reference points it belongs on.
