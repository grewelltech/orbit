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

| Network | Subnet | Carries |
|---|---|---|
| `orbit-mgmt` | `10.60.0.0/24` | operator access only; no 3GPP traffic |
| `orbit-n2` | `10.60.2.0/24` | gNB ↔ AMF, NGAP over SCTP |
| `orbit-n3` | `10.60.3.0/24` | gNB ↔ UPF, GTP-U |
| `orbit-n6` | `10.60.6.0/24` | UPF ↔ data network |

| Node | mgmt | N2 | N3 | N6 | Size |
|---|---|---|---|---|---|
| `orbit-core` | `.10` | `.10` | `.10` | `.10` | 8 vCPU / 16 GiB / 60 GiB |
| `orbit-ran` | `.20` | `.20` | `.20` | — | 4 vCPU / 8 GiB / 30 GiB |
| `orbit-app` | `.30` | — | — | `.30` | 2 vCPU / 4 GiB / 20 GiB |

The host holds `.1` on every bridge, so all four networks are reachable from the
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
