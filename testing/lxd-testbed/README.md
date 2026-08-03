# LXD testbed — separated N2/N3/N6

Stands up a three-node LXD environment for ORBIT on **separated** 3GPP reference
points: N2, N3 and N6 are distinct L2 domains. No interface is collapsed onto
another, which is the point of this profile — see
[`docs/design/ci-testbed.md`](../../docs/design/ci-testbed.md) §5.1 for why the
topology is something to validate rather than a convenience to pick.

Nodes run [testbox](https://github.com/bgrewell/testbox) images: Ubuntu 24.04 on
btrfs with an immutable base and an ephemeral root that is recreated from that
base on every boot. Nothing survives a reboot unless it was explicitly saved as
a layer, so the testbed is clean by default rather than clean-if-torn-down.

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

Addresses are **LXD DHCP reservations**, not in-guest configuration. The testbox
image already DHCPs every wired interface, so nodes come up on predictable
addresses without the image being customised per deployment.

## Prerequisites

- **LXD** with the VM (qemu) driver and a usable storage pool. Assumed present.
- **qemu-utils** — `qemu-img`, to convert the testbox raw image to qcow2.
- To *build* a testbox image (not needed if you have one):
  **mkosi v26+**, plus the host packages testbox lists — `debootstrap`,
  `mtools`, `btrfs-progs`, `systemd-container`, `dosfstools`, `squashfs-tools`,
  `bubblewrap`, `debian-archive-keyring`, `ovmf`, `qemu-system-x86`. Ubuntu
  noble does not ship a new enough mkosi:
  ```sh
  pipx install git+https://github.com/systemd/mkosi.git@v26
  ```
  The build needs `sudo` — testbox loop-mounts the raw image to create its
  `@base` and `@hostid` subvolumes.

## Usage

```sh
cd testing/lxd-testbed

./testbed.sh image     # build the testbox image and import it into LXD
./testbed.sh up        # create networks + nodes and start them
./testbed.sh status    # what is running, and where
```

`image` is the slow step and only needs repeating when the image should change.
Point `TESTBOX_IMAGE` at an existing `testbox.raw` to skip the build entirely.

Tear down:

```sh
./testbed.sh down            # remove nodes, profile and networks
./testbed.sh down --image    # also drop the imported LXD image
```

`down` only removes objects carrying the configured prefix, so an unrelated
environment on the same host is never at risk.

## Access

**Console — always works, needs nothing:**

```sh
./testbed.sh console core     # detach with ctrl-a q
```

The testbox image autologins root on the serial console.

**SSH — needs a key baked in at image build time:**

```sh
TESTBED_SSH_PUBKEY=~/.ssh/id_ed25519.pub ./testbed.sh image
./testbed.sh up
./testbed.sh ssh core
```

The image carries no cloud-init, and LXD's guest agent is not in it, so a key
cannot be injected after the fact — it is present from the build or not at all.
The script stages the key into the image tree for the build and removes it
afterwards, so the testbox checkout is left clean.

> `lxc exec` does **not** work against these nodes. That needs the LXD agent,
> which a custom OS image does not carry. Use `console` or `ssh`.

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

**Secure Boot is disabled** on these VMs. testbox installs its own
`systemd-bootx64.efi` at the UEFI fallback path, and that binary is not signed,
so LXD's default Secure Boot would refuse to boot it.

**Journals do not survive a reboot.** That is the testbox ephemerality
guarantee working as designed, but it means a node that reboots loses its logs.
Collect anything needed before rebooting, or promote the state to a named layer
first (`testbox state save <name>` inside the guest). Tracked upstream as
[testbox#2](https://github.com/bgrewell/testbox/issues/2).

**Layers are local to a node.** testbox cannot yet export or import a saved
layer, so a known-good state cannot be moved between machines. This is the main
gap between this testbed and a multi-runner CI —
[testbox#1](https://github.com/bgrewell/testbox/issues/1).

**Extra gNB source addresses are not configured yet.** Handover needs a distinct
routed source IP per gNB, and LXD reserves one address per NIC. Additional N3
addresses on `orbit-ran` need to be added in-guest, which belongs with the RAN
setup step rather than here.

**Databases on btrfs.** The root filesystem is copy-on-write, which is a known
hazard for database write patterns and could colour benchmark results. Worth
measuring before trusting performance numbers taken here —
[testbox#3](https://github.com/bgrewell/testbox/issues/3).

## Troubleshooting

**A node has no address.** Check the console — the guest DHCPs `en*`/`eth*`, so
a missing lease usually means it has not finished booting, or the image was
built without the expected netplan. `./testbed.sh status` shows what LXD sees.

**`lxc start` fails with a Secure Boot or EFI error.** The profile should carry
`security.secureboot=false`; confirm with
`lxc profile show orbit`.

**`image` fails in mkosi.** mkosi must be v26 or newer (`mkosi --version`);
noble's archive version is too old. The build also needs the host packages
listed above.

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
