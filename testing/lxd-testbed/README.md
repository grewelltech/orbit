# LXD testbed — the Ref 2 small site

Stands up an LXD environment for ORBIT in the shape of the sdcore-lab **Ref 2**
small-site reference (`reference-deployments/ref2-site/DESIGN.md` in
[grewelltech/aether-sdcore-infosite](https://github.com/grewelltech/aether-sdcore-infosite)):
one 802.1Q trunk per site, a site router that owns every gateway and NATs only
at the Internet edge, a routed UE pool, and a second RAN machine two routed
hops away over an emulated WAN. ORBIT is measured against that shape because it
is the one the lab has standardised on — a result taken here transfers to the
reference, and a fault found here is a fault in the reference.

Nodes run **stock Ubuntu 24.04 cloud images**. Cleanliness comes from rebuilding
rather than from an ephemeral root: `down` then `up` is fast, so a run always
starts from a known image. Addressing is static and written to disk by
cloud-init, because Aether OnRamp and SD-Core discover interfaces by reading the
network configuration from the filesystem — a DHCP lease leaves nothing there.
Each node's role (router, WAN emulator, build toolchain) is also finished by
cloud-init, so a node that has booted is a node that is ready.

The script creates the machines, the wiring and the routers. Deploying the 5G
core and ORBIT onto them are separate steps, all driven from here.

## Topology

```
                            Internet (lxdbr0, DHCP+NAT)
                                      │ uplink
                               ┌──────┴──────┐
      core site                │ orbit-rsite │ site router: .1 on every core-site VLAN,
      trunk  ══════════════════╡  (nftables) │ routes the UE pool to the UPF, NATs on uplink only
      VLANs 130-133            └──────┬──────┘
        │                             │ wan VLAN 134 (10.134.0.0/30)
        ├─ orbit-ctrl   oam          ┌┴────────────┐
        ├─ orbit-wanem  oam          │ orbit-wanem │ bump in the wire: br-wan, wanemd (delay/jitter/loss)
        ├─ orbit-core   oam n2 n3 n6 └┬────────────┘
        ├─ orbit-ran-near oam n2 n3   │
        └─ orbit-app    oam n6       ┌┴───────────┐
                                     │ orbit-rran │ far-site router: .1 on every far VLAN
      far site                       └┬───────────┘
      trunk  ═════════════════════════╡
      VLANs 135-137                   │
        └─ orbit-ran-far  oam n2 n3   (two routed hops from the core)
```

Defaults — all configurable, see [Configuration](#configuration):

| VLAN | Subnet | Interface | Carries |
|---|---|---|---|
| 130 | `10.130.0.0/24` | `oam` | operator access, Kubernetes API, deployment; no 3GPP traffic |
| 131 | `10.131.0.0/24` | `n2` | gNB ↔ AMF, NGAP over SCTP (core site) |
| 132 | `10.132.0.0/24` | `n3` | gNB ↔ UPF, GTP-U (core site) |
| 133 | `10.133.0.0/24` | `n6` | UPF ↔ data network (app / responder) |
| 134 | `10.134.0.0/30` | `wan` | r-site ↔ r-ran, through wanem |
| 135 | `10.135.0.0/24` | far `n2` | far-site gNBs' N2 |
| 136 | `10.136.0.0/24` | far `n3` | far-site gNBs' N3 — the UPF's `ran_subnet` |
| 137 | `10.137.0.0/24` | far `oam` | far-site management |

**The VLAN id is the second octet.** That is Ref 2's rule (120–127 there), kept
here and moved up by ten so this testbed can run next to a live Ref 2 replica
on the same host without two identical address plans in `lxc list`.
`TESTBED_VLAN_BASE=120` gives a byte-identical replica. The UE pool is
`100.65.0.0/16` for the same reason (Ref 2: `100.64.0.0/16`).

| Node | Role | Addresses (last octet) | Size |
|---|---|---|---|
| `orbit-rsite` | site router, Internet edge | `.1` on oam/n2/n3/n6, wan `.1`; uplink by DHCP | 2 vCPU / 2 GiB |
| `orbit-wanem` | WAN emulator (wanemd) | oam `.5`; `br-wan` has no address | 2 vCPU / 2 GiB |
| `orbit-rran` | far-site router | wan `.2`; `.1` on far n2/n3/oam | 2 vCPU / 2 GiB |
| `orbit-ctrl` | aether-ops | oam `.6` | 4 vCPU / 8 GiB / 40 GiB |
| `orbit-core` | SD-Core | oam `.10`, n2 `.10` (AMF); **n3/n6 no address** (UPF `.100` on each via macvlan) | 32 vCPU / 64 GiB / 60 GiB |
| `orbit-ran-near` | ORBIT, on-link with the UPF | oam `.21`; n2 and n3 `.21`–`.30` (ten gNB slots) | 16 vCPU / 32 GiB |
| `orbit-ran-far` | ORBIT, two routed hops away | far oam `.21`; far n2 and n3 `.21`–`.30` | 16 vCPU / 32 GiB |
| `orbit-app` | data network (responder) | oam `.30`, n6 `.30` | 8 vCPU / 16 GiB |

Interface names are Ref 2's: `trunk` is the physical port, `oam`/`n2`/`n3`/`n6`
the VLAN interfaces on it, so `ip addr` on a node reads as the topology and a
netplan written here is reusable on a physical host by swapping the MACs.
`eth0`/`eth1` ordering is an enumeration accident and tells you nothing about
which plane a NIC carries. cloud-init does the renaming via netplan `set-name`,
matched on the MAC LXD assigned.

**The bridges are switches.** The four LXD networks (`orbit-trunk`,
`orbit-ftrunk`, `orbit-wan-a`, `orbit-wan-b`) carry no host address, no DHCP
and no NAT. Every VM port is given its allowed-VLAN list (`vlan.tagged`, or
`vlan` for the WAN emulator's access ports, which must see the WAN untagged
because wanemd's classifier is `protocol ip`), so a port only sees the VLANs its
node belongs on. Every gateway is a router VM, and the only path off the testbed
is `orbit-rsite`'s uplink — so a node can reach the Internet (OnRamp pulls
charts and images) exactly the way a site host would, through the site's NAT,
and nothing else about the host's networking is involved.

The far site matters for ORBIT specifically: a gNB there reaches the AMF and the
UPF across two routers, so the UPF has to route downlink to it rather than ARP
for it. That is the case that exposed the UPF never programming on-link routes
(sdcore-lab UPF-5), and it is why `verify` runs from both machines.

The control node has an `oam` address **only**. It deploys over SSH on the
management plane, and a foot on N2 or N3 would let a bug there put traffic on a
reference point that should carry nothing but gNB and UPF traffic — which is
exactly the class of fault this topology exists to expose.

Each RAN node carries one N2 **and** one N3 address per simulated gNB
(`TESTBED_RAN_GNBS`, default 10, slot *n* = `.20+n`): an AMF keys a gNB's
association on its source address, so handover between two gNBs needs two
distinct ones.

## Prerequisites

- **LXD** with the VM (qemu) driver and a usable storage pool. Assumed present.
- An existing LXD bridge with DHCP and NAT for the site router's uplink
  (`TESTBED_UPLINK_NET`, default `lxdbr0`). It is never modified.
- Network access to pull `ubuntu:24.04` the first time.
- **Go** on the host, to build ORBIT and wanemd (both are pushed as binaries).
- `gh` logged in to GitHub: aether-ops and wanemd are private repositories.

## Usage

```sh
cd testing/lxd-testbed

./testbed.sh image     # pull ubuntu:24.04 into LXD (once)
./testbed.sh up        # networks + nodes; starts r-site first, then the rest
./testbed.sh check     # assert the site routes as designed
./testbed.sh status    # what is running, and where
```

Then, in order: `ctrl` and `keys` to stand up the management plane (SD-Core
only), `wanem` for
the WAN emulator, `core` to deploy the 5G core, `apps` to install ORBIT, and
`verify` to drive a UE through the whole path from both sites. `all` runs the
lot up to `check`.

`up` starts `orbit-rsite` and waits until it routes and NATs before starting
anything else: every other node's first-boot provisioning (packages, the
control node's toolchain) goes through its uplink. `up` is safe to re-run and
leaves existing instances alone.

Tear down:

```sh
./testbed.sh down            # remove nodes, profile and the four bridges
./testbed.sh down --image    # also drop the imported LXD image
```

`down` only removes objects carrying the configured prefix (and never the
uplink bridge), so an unrelated environment on the same host is never at risk.

## Access

The Ubuntu images carry the LXD agent, so:

```sh
lxc exec orbit-core -- bash            # straight in
./testbed.sh console core              # console; detach with ctrl-a q
```

Node names: `rsite`, `wanem`, `rran`, `ctrl`, `core`, `ran-near`, `ran-far`,
`app` (`near`/`far` also work).

The host has no address on any site VLAN, so a browser or SSH client on the
host cannot reach `10.130.0.x` directly. `testbed.sh ssh <node>` jumps through
the site router's uplink address; set `TESTBED_SSH_PUBKEY` before `up` and
cloud-init installs the key for the `ubuntu` user on every node:

```sh
TESTBED_SSH_PUBKEY=~/.ssh/id_ed25519.pub ./testbed.sh up
./testbed.sh ssh core
```

For the dashboards (`orbit serve` on each RAN node, aether-ops on `ctrl`),
either tunnel through the same jump (`ssh -J ubuntu@<rsite-uplink> -L
8412:10.130.0.21:8412 ubuntu@10.130.0.21`) or add a host route to
`10.130.0.0/21` via the site router's uplink address. The ORBIT dashboard is
`https://` with a self-signed certificate (accept it once in the browser) and
asks for the API token. The token is generated on the host once per testbed,
kept at mode 600 in `~/.cache/orbit-lxd-testbed/$TESTBED_PREFIX.api-token` (outside
the repository) and on each RAN node in `/etc/orbit/api-token` (600) and
`/etc/orbit/env`. To use the API from your own machine, through the tunnel or
route above:

```sh
mkdir -p ~/.config/orbit; install -m 600 /dev/null ~/.config/orbit/testbed-token
lxc exec orbit-ran-near -- cat /etc/orbit/api-token > ~/.config/orbit/testbed-token
orbit server trust https://10.130.0.21:8412 --token-file ~/.config/orbit/testbed-token
#   compare the fingerprint with: lxc exec orbit-ran-near -- journalctl -u orbit | grep spki
export ORBIT_SERVER=https://10.130.0.21:8412
```

## Profiles and core kinds

The same site can be built around three cores. A **profile** selects a whole
testbed in one word — its own prefix, VLAN block, UE pool and core — so several
run side by side on one host:

| Profile | Prefix | VLAN block / subnets | UE pool | Core (`TESTBED_CORE_KIND`) | Shape |
|---|---|---|---|---|---|
| *(none)* | `orbit` | 130–137, `10.130`–`10.137.0.0/24` | `100.65.0.0/16` | SD-Core (`sdcore`) | site |
| `sdcore` | `sdcore` | 160–167, `10.160`–`10.167.0.0/24` | `100.68.0.0/16` | SD-Core (`sdcore`) | minimal |
| `free5gc` | `free5gc` | 140–147, `10.140`–`10.147.0.0/24` | `100.66.0.0/16` | free5GC (`free5gc`) | minimal |
| `open5gs` | `open5gs` | 150–157, `10.150`–`10.157.0.0/24` | `100.67.0.0/16` | Open5GS (`open5gs`) | minimal |

```sh
./testbed.sh -p free5gc all        # or: TESTBED_PROFILE=free5gc ./testbed.sh all
./testbed.sh -p free5gc status
./testbed.sh -p free5gc verify
./testbed.sh -p free5gc down
```

Every subcommand works the same under `-p`, and everything in this document
applies with the profile's prefix and addresses in place of `orbit` and
`10.13x`. The topology, the checks, the ORBIT targets on the RAN nodes (`lxd`,
`lxd-conformance`, `lxd-load`) and the host octets are identical; only the core
step differs. A profile is a short file in [`profiles/`](profiles); the free5GC
and Open5GS ones also size the core, RAN and app nodes down, since those cores
are run for conformance rather than load. All three default to the minimal
shape described next.

### Shapes

`TESTBED_SHAPE` says how much of the site is built. `site` is everything in
this document. `minimal`, the profiles' default, is what conformance runs need
and nothing else: **two VMs and one bridge**.

| Node | Interfaces |
|---|---|
| `<prefix>-core` | `uplink` (DHCP on `TESTBED_UPLINK_NET`: management, Internet); `n2` `.10` (AMF); `n3`/`n6`: the UPF at `.100` on each |
| `<prefix>-ran` | `uplink` (DHCP; the ORBIT API and dashboard listen here); `n2` and `n3` `.21`–`.30` (ten gNB slots); `n6` `.30` (the responder) |

- One trunk, `<prefix>-trunk`, carries the N2, N3 and N6 VLANs (the block's
  +1, +2, +3). There is no OAM VLAN, router, far site, WAN emulator, app node
  or control node, and no NAT: the UE pool is reachable only inside the testbed.
- The RAN node stands in for the app node: it holds the app's N6 address and
  runs the responder (`app` installs it there). A UE's packets are built by
  ORBIT in user space and leave inside GTP-U on N3, so they still cross
  N3 → UPF → N6 to reach it, and the replies return through the UPF.
- The UPF's N6 next hop is the responder's address, and the gNBs are on-link
  on N3.
- The `uplink` address is on the host's own LXD bridge, so the dashboard is
  reachable from the host with no route or jump: `status` prints it, and `up`
  makes the DHCP lease static so it survives restarts.
- The RAN node's `lxd-conformance` and `lxd-load` targets take the low half of
  the subscriber pool, as the near site's do; `lxd` spans all of it.

```sh
./testbed.sh -p sdcore all                      # core + RAN, about as fast as the core deploys
TESTBED_SHAPE=site ./testbed.sh -p sdcore all   # the full site around the same profile
```

The parts can also be chosen one by one — `TESTBED_WITH_ROUTER`,
`TESTBED_WITH_FAR`, `TESTBED_WITH_APP`, `TESTBED_WITH_CTRL` (`true`/`false`;
see [`testbed.conf`](testbed.conf)). The far site and the control node need the
router. Every command honours them: `all` skips the steps for absent nodes,
`check` asserts what the shape has, `verify` runs from the RAN nodes that
exist. Build and tear down a testbed with the same settings; `down` removes
every node and bridge carrying the prefix whatever the shape.

### Core kinds

What a core kind changes, beyond how `core` deploys it:

- **Control node.** aether-ops manages SD-Core only. Other kinds have no `ctrl`
  node and no `ctrl`/`keys` steps; `all` skips them. (Nor does the minimal
  shape, for any kind.)
- **Where the UPF sits.** SD-Core's UPF is a pod attached to the core node's
  `n3`/`n6` by macvlan, so the node has no address there. free5GC and Open5GS
  run the UPF on the core node's host network, so the node itself holds the
  UPF's `.100` on `n3` and `n6` (and routes the far site's N3 via r-site).
  Either way the UPF is at the same addresses, so r-site's UE-pool route, the
  app's return route, the targets and `check` do not change shape.

Testbeds share nothing on the host but the uplink bridge and the SD-Core image
builds (tagged by commit): LXD objects carry the prefix, and each testbed has
its own cache directory, `~/.cache/<prefix>-lxd-testbed`, for its tokens, built
binaries and version record.

A core kind is one file in [`cores/`](cores) implementing the contract described
at the top of [`testbed.sh`](testbed.sh) ("core kinds").

## From nothing to ready

```sh
./testbed.sh all      # image + up + ctrl + keys + wanem + core + apps + check
./testbed.sh verify   # then: a UE from each site, end to end
```

Roughly 35 minutes, most of it the core and the aether-ops build. The steps
remain available individually, which is usually what you want: `up` rebuilds
the VMs in a few minutes without touching a working core.

## Checking the site: `check`

`check` is the infrastructure's own assertion suite, independent of the core,
modelled on Ref 2's `lxd/checks.sh`. One `PASS`/`FAIL` line per assertion, exit
status = failures:

- r-site forwards, masquerades **only** on the uplink, and routes the UE pool to
  the UPF's N6 address; r-ran forwards and does not NAT
- from the core: every r-site gateway answers; a near gNB's N2 address is
  on-link (no `via`); a far gNB's N2 address is reached via exactly r-site's N2
  gateway then r-ran's WAN address; a 1472-byte DF ping to the far site passes
  (MTU 1500 end to end); the Internet is reached; `n3`/`n6` are up with no
  address
- from the far RAN node: the AMF's N2 address and r-site's N3 gateway answer
  from every gNB slot; the core-site N3 subnet is routed via r-ran on `n3`
- from the near RAN node: every slot reaches the AMF on-link; **isolation** —
  the app's N6 address does not answer ARP on `n3` or `n2`, and N3 → N6 is
  routed (hop 1 is r-site) rather than switched
- the app routes the UE pool back via r-site; wanem's bridge has two ports and
  no address and wanemd is listening

Run it after `up` (before the core exists) and again whenever a measurement
looks wrong: a FAIL here means the site is miswired and the core is not the
place to look.

## The control node (aether-ops)

```sh
./testbed.sh ctrl     # build and install aether-ops on orbit-ctrl
./testbed.sh keys     # authorize its managed key on core, both RAN nodes and app
```

[aether-ops](https://github.com/aether-gui/aether-ops) is the management daemon
for an Aether deployment: a REST API that registers nodes, reports on a running
core and can drive OnRamp itself. It lives on its own node so that the thing
doing the managing is not also the thing under test — a core node that has to be
rebuilt should not take the tool that rebuilds it with it.

**It is deliberately not in this testbed's deploy path.** `testbed.sh core`
drives OnRamp directly on the core node, and that is the supported route. The
point of this testbed is to measure ORBIT against SD-Core, so the fewer moving
parts between the two, the more a result means — a failure here should implicate
ORBIT or the core, not a third tool's deployment logic. aether-ops is installed
because the control node is genuinely useful for **observing** a running core:
it exposes registered UEs, per-subscriber metrics, NF and UPF status, and
Kubernetes state over a REST API, none of which requires it to own the
deployment.

If you do want it to own the deployment, note that its OnRamp checkout is its
own (`/var/lib/aether-ops/aether-onramp`, from `opennetworkinglab/aether-onramp`)
and entirely separate from the one `testbed.sh core` uses on the core node. Two
tools independently deploying the same core is the failure mode to avoid, so
pick one.

```sh
curl -s http://10.130.0.6:8186/api/v1/meta/version   # from inside the site
# Swagger UI at /docs, OpenAPI at /openapi.json
```

**The repository is private.** The install reads a token from the `gh` CLI's
existing login at the moment it needs one — so nothing is stored in this
repository, and `gh auth login` (with `repo` scope) is the only setup. The token
reaches the node as a mode-0600 file that the install shreds, rather than as a
command argument, which would put it on the host's process list, or as instance
configuration, which `lxc config show` would print back.

aether-ops is built **from source** on the control node at `AETHER_OPS_REF`
(default `main`), which is why that node carries Go and Node from cloud-init and
a 40 GiB disk. The installer does not provide the toolchain — its preflight
installs git/make/ansible, but not Go or Node.

### Why `keys` is a separate step

`--preflight` bootstraps only the node it runs on. There is no supported way to
authorize the daemon's key on the nodes it manages and no password fallback, so
a multi-node deployment cannot reach its targets until the key is distributed by
hand — [AO-2](https://github.com/aether-gui/aether-ops/blob/main/KNOWN_ISSUES.md)
in aether-ops's own known-issues log. `keys` is that step: it creates the
`aether` service account on the core, RAN and app nodes, installs the daemon's
managed public key, and grants it passwordless sudo (the daemon runs unattended,
so a sudo prompt would hang a deploy rather than fail it).

The key itself is generated by `ctrl`, during preflight. The daemon **reads**
that key and never creates one ([ADR-0011]), so an install that skipped
preflight yields a daemon that starts cleanly and cannot reach a single node.
Re-run `keys` after any re-run of `ctrl` that regenerated the key.

[ADR-0011]: https://github.com/aether-gui/aether-ops

## The WAN emulator (wanem)

```sh
./testbed.sh wanem    # build wanemd on the host, install and start it on orbit-wanem
```

`orbit-wanem` is a transparent bump in the wire between the two site routers:
its two WAN ports are bridged (`br-wan`, no address), and
[wanemd](https://github.com/bgrewell/wanemd) applies delay, jitter and loss to
what crosses the bridge. Like ORBIT it is built on the host and pushed as a
binary, because the repository is private and the host already holds the
credential.

Profiles are applied on the node and are scoped to the far site's N2 and N3
subnets, so management traffic is never shaped:

```sh
lxc exec orbit-wanem -- /opt/wanemd/bin/profile.sh wan-30ms    # ~30 ms RTT, ±5 ms, ~0.5% loss
lxc exec orbit-wanem -- /opt/wanemd/bin/profile.sh sat-600ms   # GEO-satellite-like backhaul
lxc exec orbit-wanem -- /opt/wanemd/bin/profile.sh clear
```

With a profile applied, the same `verify far` (or any ORBIT run from
`orbit-ran-far`) measures the core over an impaired backhaul while the near
site is untouched — which is the comparison the two RAN machines exist for.

## Deploying the core

```sh
./testbed.sh core           # OnRamp -> Kubernetes -> SD-Core via 5gc-install, ~15 min
./testbed.sh core-status    # pods, host interfaces, the UPF's attachments
```

Automated end to end. OnRamp is pinned by `ONRAMP_VERSION` and the chart by
`SDCORE_CHART_VERSION` (default 5.0.5, what Ref 2 runs); everything the core
needs is installed on the node — nothing is assumed present.

### How the site is expressed

Since [aether-onramp#263](https://github.com/opennetworkinglab/aether-onramp/pull/263)
OnRamp describes the UPF as two sides, each attached to a named network, so the
Ref 2 site is plain configuration. The script writes this `core:` block into
`vars/main.yml` (addresses from `testbed.conf`) and runs the documented
`make 5gc-install`:

```yaml
core:
  networks:
    access:
      iface: n3
      subnet: 10.132.0.0/24
      gateway: 10.132.0.1            # the site router, not the core node
      attach: macvlan
      ran_subnet: 10.136.0.0/24      # far-site gNBs; near-site gNBs are on-link
    core:
      iface: n6
      subnet: 10.133.0.0/24
      gateway: 10.133.0.1
      attach: macvlan
  upf:
    instances:
      default:
        access: { ip: 10.132.0.100 }
        core:   { ip: 10.133.0.100 }
        ue_ip_pool: 100.65.0.0/16
```

Three things in it carry the topology. Each side is a **macvlan child of a
different VLAN interface** (`n3`, `n6`), so the UPF's N3 and N6 are separate
L2 domains by construction while the core node stays an ordinary host on the
site network. `gateway:` on each segment is the **site router**, not the core
node — so OnRamp's router role builds no overlay and installs no NAT, and the
UE pool is routed rather than masqueraded, as on a real site (r-site routes it
to the UPF's N6 address; the app returns via r-site). `ran_subnet` is the
**far site's N3**: the UPF routes downlink for those gNBs via its N3 gateway,
and the near site's gNBs are on-link.

That is the same data path Ref 2 runs. Ref 2's `onramp/main.yml` still
expresses it with its fork's knobs (`router.enabled: false`, `access_iface`,
`core_iface`, `multihop_gnb`, `data_iface`); the block above is what those
became upstream.

Nothing is patched into OnRamp's templates. OnRamp refuses a configuration that
cannot work instead of deploying it, and prints what it resolved ("show the
resolved UPF networking") before anything is deployed — that line is the first
thing to check if the UPF does not come up.

One thing OnRamp does not do yet is on the core node's first boot instead:
`testbed-upf-offloads.service` turns software GRO **and** the NIC's own
receive aggregation (`rx-gro-hw`) off on `trunk`. The af_packet UPF silently
drops receive-aggregated frames, which collapses TCP downlink (sdcore-lab
UPF-14), and OnRamp turns GRO off only on the VLAN interfaces it was given
(`n3`, `n6`), once, at install — aggregation happens on the lower device, and
the setting does not survive a reboot (ONR-4). `check` asserts both flags.

Two things are written into the values template, independent of networking:
the subscriber range (`ueId-start`, `ueId-end` and the device group's IMSI
list), and — with `TESTBED_UPF_END_MARKER=true`, the default — the two
`upf.jsonc` keys that make the BESS UPF send GTP-U End Markers on a path switch
(`enable_end_marker`, `endmarker_sockaddr: /pod-share/pfcpport`), as Ref 2 runs
it and as ORBIT's `GTPU-END-MARKER-ON-PATH-SWITCH` check expects.

### What to expect afterwards

`n3` and `n6` stay on the core node, up and without an address: they are the
UPF's macvlan parents. Inside the pod:

```
# on orbit-core                    # inside upf-0
oam   10.130.0.10/24               access   10.132.0.100/24
n2    10.131.0.10/24               core     10.133.0.100/24
n3    (no address, UP)
n6    (no address, UP)
```

The AMF takes the node's N2 address (`10.131.0.10`) and exposes NGAP on
38412/SCTP, so a gNB reaches it over N2 while user-plane traffic rides N3 to
the UPF — separate VLANs, separate paths, and from the far site two routed hops
on each.

### Subscribers

OnRamp's stock values file provisions **100** subscribers, which runs out for
any population test. `TESTBED_SUB_COUNT` (default 1000) and `TESTBED_SUB_START`
set the range, and the script rewrites `ueId-start` / `ueId-end` and the IMSI
list before installing.

> **Re-running `core` removes and reinstalls the SD-Core release.** OnRamp's
> install task does not *upgrade* an existing release — helm stays at the same
> revision and silently keeps the old values, so a changed subscriber range or
> interface mapping would never take effect. The script removes the release
> first to make the command genuinely repeatable.

### MongoDB

Nothing to do here any more, which is worth stating because it used to be the
single biggest lever on measured attach rate.

The Bitnami chart shipped `cpu: 750m` for mongodb while no other NF was capped
at all, and a registration costs roughly 55 MongoDB operations — so mongo
saturated its quota and was CFS-throttled long before anything else was busy
(measured at 707m of a 750m limit during an attach storm, throughput flat and
latency climbing). The script used to patch the limit, worth about 2x.

The chart has since fixed it. Measured 2026-09-30 against sd-core as deployed by
aether-ops v1.0.3, mongodb ships `limits: cpu 4, memory 2Gi` with
`requests: cpu 2, memory 1Gi` — what the patch was setting anyway. So the patch
is gone, and `testbed.sh core` only *checks* the limit now and warns if it is
low (`TESTBED_MONGO_CPU` is the floor).

Removing the patch removed a whole hazardous sequence with it. Patching a live
StatefulSet recreates the mongo pod, and its datadir is an `emptyDir` — so the
patch wiped every subscriber *and* the NRF's NF registry, leaving a core that
looked healthy and attached nothing. That is why the install used to have to
repopulate immediately afterwards. It no longer does.

`testbed.sh reprovision` remains, because the underlying hazard has **not** been
fixed: mongo's datadir is still an `emptyDir` with no PVC, so anything that
recreates that pod still empties it.

### Helm

OnRamp requires `helm` but does not install it; its k8s role only pins a
version. The script installs it, having watched the stock path fail with
`Failed to find required executable "helm"`. (The Aether reference host carries
a `get_helm.sh` for the same reason.)

## The free5GC core (`-p free5gc`)

```sh
./testbed.sh -p free5gc core           # build and start free5GC, provision subscribers
./testbed.sh -p free5gc core-status    # units, listeners, the UPF's attachment, subscriber count
./testbed.sh -p free5gc reprovision    # restore the subscriber population
```

[`cores/free5gc.sh`](cores/free5gc.sh) builds free5GC from source on the core
node and runs it natively: one systemd unit per NF (`free5gc-nrf`,
`free5gc-amf`, … `free5gc-upf`, `free5gc-webconsole`, grouped by
`free5gc.target`), no containers. The node's own addresses are the core's:

| | Where | |
|---|---|---|
| AMF, NGAP | `CORE_N2:38412` SCTP on `n2` | `10.141.0.10` |
| UPF, GTP-U | `UPF_N3:2152` on `n3` | `10.142.0.100` |
| UPF, N6 | `UPF_N6` on `n6`; UE addresses leave it un-NATed | `10.143.0.100` |
| PFCP (N4), SBI, MongoDB, webconsole API | loopback | `127.0.0.x` |

The first `core` takes about 6 minutes, most of it package installs and the Go
build; a re-run at the same refs rebuilds nothing, re-renders the
configuration, restarts the NFs and re-provisions in about a minute. It is
automatic from a fresh `up`.

**Versions.** Pinned by variables at the top of `cores/free5gc.sh`, overridable
like any other setting (environment, `testbed.free5gc.local.conf`); what each
resolved to is recorded in `/etc/orbit/testbed-versions` on the core node.

| Variable | Default | |
|---|---|---|
| `FREE5GC_REF` | `v4.3.0` | free5GC release (git ref of `FREE5GC_REPO`); every NF is the submodule commit that release pins |
| `GTP5G_REF` | `v0.10.2` | gtp5g kernel module (git ref of `GTP5G_REPO`); go-upf at v4.3.0 accepts `>= 0.9.3, < 0.10.3` |
| `FREE5GC_GO_VERSION` | `1.26.11` | Go toolchain from go.dev (the release's modules need 1.26.2 or later) |
| `FREE5GC_MONGO_SERIES` | `8.0` | MongoDB series from repo.mongodb.org |
| `FREE5GC_NFS` | `nrf udr udm ausf nssf pcf chf amf smf` | control-plane NFs to build and run; the UPF and the webconsole backend always run |
| `FREE5GC_NIA_ORDER` / `FREE5GC_NEA_ORDER` | `NIA2 NIA1 NIA3` / `NEA0 NEA2 NEA1 NEA3` | the AMF's NAS algorithm preference |
| `FREE5GC_GTP_MTU` | `TESTBED_MTU - 64` | MTU of the UPF's GTP device |
| `FREE5GC_AMBR` | `10 Gbps` | session and UE AMBR of every subscriber |
| `FREE5GC_N6_GATEWAY` | *(empty: `GW_N6` if it answers)* | next hop for UE traffic beyond the N6 subnet; `none` for no next hop |
| `FREE5GC_LOG_LEVEL` | `info` | log level of every NF (`journalctl -u free5gc-amf`) |
| `FREE5GC_DIR` | `/opt/free5gc` | source tree, binaries and helper scripts on the core node |

**3GPP release.** free5GC describes itself as a Release 15 core "and beyond".
v4.3.0's NAS and NGAP codecs (`free5gc/nas` v1.3.0, `free5gc/ngap` v1.2.0)
follow TS 24.501 and TS 38.413 at Release 15/16 level. Run the conformance
suite against it with `--target-release Rel-15`, so a check that needs a later
release SKIPs; under ORBIT's default (Rel-18, the SD-Core release) such a check
runs and FAILs.

**ORBIT must send its IMEISV.** free5GC's AMF asks for the IMEISV in the
Security Mode Command and, if the Security Mode Complete carries none, sends an
Identity Request for the IMEI before it accepts the registration; there is no
setting for this. An ORBIT whose UE does neither stops at
`AMF requested an Identity … Identity Response not yet implemented`: NG Setup
works, and `verify` and every check that registers a UE fail.

**Configuration.** The NF configuration in `/etc/free5gc` is the pinned
release's own `config/*.yaml` with only the testbed's values replaced — PLMN,
TAC, slice, DNN, the UE pool, the N2 and N3 addresses, the NAS algorithm order
— so a newer release brings its new settings along, and a changed layout fails
the render with a message instead of starting a half-configured core. The NAS
timers are the release's defaults, which are the specification's (T3550, T3560,
T3570 6 s; T3591, T3592 16 s). The SMF sets the End Marker flag on a path
switch and gtp5g sends the End Markers; there is no switch for it.

**The UPF** is go-upf with the `gtp5g` kernel module, built on the node through
DKMS (so a kernel upgrade rebuilds it) and loaded at boot. LXD VMs boot with
Secure Boot on, and a module built on the node is unsigned, so the first `core`
turns Secure Boot off for the core VM and restarts it once. The UPF routes
`TESTBED_UE_POOL` to its GTP device, and `ip_forward` is on. Nothing is NATed
and no firewall rule is added: free5GC's guides add a MASQUERADE rule, this
does not. UE traffic leaves through N6 and nowhere else — a routing rule sends
anything *from* a UE address to a table that holds the N6 subnet and, when the
site has a router there, a default route via it (`core-status` prints both).
The return path is the site's: r-site routes the pool to `UPF_N6`.

**Subscribers** are written through the webconsole's API
(`http://127.0.0.1:5000` on the core node, account `admin` / `free5gc`), which
creates `TESTBED_SUB_COUNT` consecutive IMSIs from `TESTBED_SUB_START` in one
request and writes every collection the UDR and PCF read in the pinned
release's own schema: 5G-AKA with `TESTBED_KI` and `TESTBED_OPC` (an OPc), one
slice and DNN, IPv4, default 5QI 9, offline charging. `reprovision` deletes
whatever subscribers exist and creates the population again; it does not
restart anything. MongoDB keeps its data on the node's disk, so subscribers
survive a restart of the core or the node. Only the webconsole's backend runs;
its web frontend is not built.

**After a reboot** of the core node everything returns by itself: `mongod`, the
module, the routing and `free5gc.target`. An NF registers with the NRF once, at
start, so the control-plane units restart together with `free5gc-nrf`
(`systemctl restart free5gc-nrf`); `systemctl restart free5gc.target` or
`./testbed.sh -p free5gc core` restarts the UPF and the webconsole as well.

## Deploying the core: Open5GS (`-p open5gs`)

```sh
./testbed.sh -p open5gs core           # MongoDB + Open5GS built from source, ~10 min the first time
./testbed.sh -p open5gs core-status    # units, version, interfaces, listeners, subscriber count
./testbed.sh -p open5gs reprovision    # re-apply the subscriber population
```

[`cores/open5gs.sh`](cores/open5gs.sh) builds [Open5GS](https://open5gs.org)
from source on the core node at `OPEN5GS_REF` (default the release tag
`v2.8.0`; any tag, branch or sha) with meson/ninja, installs it under
`/opt/open5gs`, and runs the eleven 5G SA network functions — NRF, SCP, AMF,
SMF, UPF, AUSF, UDM, UDR, PCF, NSSF, BSF — as systemd units
(`open5gs-amfd.service` and so on, the names the Open5GS packages use) on the
node's host network. MongoDB (`OPEN5GS_MONGODB_SERIES`, default 8.0) comes from
MongoDB's own apt repository. No EPC daemon (MME, SGW-C/U, HSS, PCRF) is given
a unit, so nothing but the AMF listens on N2 and nothing but the UPF on N3.

`core` is repeatable and fast to re-run: a node already built at the resolved
commit is not built again (the commit is kept in `/opt/open5gs/.testbed-built`),
so a second run only rewrites the configuration, restarts the NFs and upserts
the subscribers — about 20 seconds. What it resolved is recorded in
`/etc/orbit/testbed-versions` like every core kind's:

```
open5gs v2.8.0 157f611a530e292e40ec50f9d23f0ef5d4fcd6a6
open5gs-3gpp-release v2.8.0 Release-19
mongodb 8.0 8.0.38
```

**Claimed 3GPP release.** Open5GS v2.8.0 describes itself as Release-19
compliant (`docs/_pages/features.md` in its tree, which is where the
`open5gs-3gpp-release` line is read from), and its codecs are generated from
TS 38.413 (NGAP) 19.2.0 and TS 24.501 (5GS NAS) 19.6.2. For ORBIT's
conformance suite that is `--target-release Rel-19`; the suite's default
(Rel-18) only skips checks that need something newer, so it is a safe
under-claim.

### How the site is expressed

Every 3GPP interface is bound by address, from the same variables the rest of
the testbed derives:

| What | Where | From |
|---|---|---|
| AMF NGAP (SCTP 38412) | the core node's N2 address | `CORE_N2` |
| UPF GTP-U (UDP 2152) | the UPF's N3 address | `UPF_N3` |
| SBI, PFCP (SMF `127.0.0.4` ↔ UPF `127.0.0.7`), metrics | loopback, Open5GS's default addresses | — |
| UE pool, on the tun device `ogstun` (gateway = the pool's first address) | `ogstun` | `TESTBED_UE_POOL` |
| PLMN, TAC, slice, DNN | AMF `guami`/`tai`/`plmn_support`, NRF `serving`, NSSF `nsi`, SMF/UPF `session` | `TESTBED_MCC`/`MNC`/`TAC`/`SST`/`SD`/`DNN` |

The UPF is a router between `ogstun` and the node's interfaces:
`net.ipv4.ip_forward` is on and **nothing is NATed** — the MASQUERADE rule of
the Open5GS quickstart is deliberately not added, so a UE's address appears
unchanged on N6, r-site's route for the pool brings the replies back to
`UPF_N6`, and `core-status` prints the number of NAT rules on the node (0).
The tun device, its address and the pool route belong to systemd-networkd
(`/etc/systemd/network/99-open5gs.*`), so they are there before the UPF starts
and after a reboot. UE traffic leaves by the core node's ordinary routing table:
the N6 subnet (the app) is on-link on `n6`.

Nothing in the core file names an interface other than `ogstun`, assumes a
router, or uses the management network: the build needs the node's default
route to reach the Internet (GitHub, Ubuntu's and MongoDB's apt repositories),
whichever interface that is.

### Conformance-relevant settings

- **NAS security.** `security.integrity_order` is `NIA2, NIA1, NIA0` and
  `ciphering_order` `NEA0, NEA2, NEA1` (`OPEN5GS_INTEGRITY_ORDER`,
  `OPEN5GS_CIPHERING_ORDER`), so a UE is given NIA2/NEA0 as on the SD-Core
  testbed.
- **NAS timers** are Open5GS's built-in, spec-default values; only T3512
  (540 s, Open5GS's own default) is written to the configuration.
- **End Marker.** Open5GS has no switch for it: the SMF always asks the UPF to
  send End Markers on the old tunnel after an Xn path switch or an N2 handover.
- **Subscribers** carry OPc (not OP), AMF field `8000`, one slice
  (`TESTBED_SST`/`TESTBED_SD`) with one IPv4 session on `TESTBED_DNN`, 5QI 9,
  ARP 8, 1 Gbps AMBR; SUCI null scheme and profiles A/B (Open5GS's stock home
  network keys) are accepted.
- **Sizing.** `OPEN5GS_MAX_UE` (4096) replaces Open5GS's default of 1024 UE
  contexts per NF, which is no margin over 1000 subscribers.

### Subscribers

`TESTBED_SUB_COUNT` consecutive IMSIs from `TESTBED_SUB_START` are written
straight into MongoDB's `subscribers` collection in one bulk upsert, in the
document shape (`schema_version: 1`) that `open5gs-dbctl add_ue_with_slice`
writes — about a second for 1000, where 1000 `open5gs-dbctl` calls take
minutes. It is an upsert keyed on the IMSI, so `core` and `reprovision` can be
run any number of times, apply a changed key, slice or DNN, and leave any
subscriber outside the range alone (with a warning that it is there). The UDR
reads MongoDB on every request, so no NF is restarted. MongoDB's data is on the
node's disk (`/var/lib/mongodb`) and survives a restart and a reboot.

### ORBIT's UE does not register yet

Open5GS enforces the protection of the initial NAS message (TS 24.501 4.4.6,
TS 33.501 6.4.6): a REGISTRATION REQUEST sent without a NAS security context
may carry only the cleartext IEs, and the AMF answers anything else with
Registration Reject, 5GMM cause #95 (`Non cleartext IEs is included` in the
AMF's journal). Every release from v2.6.6 to v2.8.0 does this and there is no
configuration switch for it. ORBIT's UE puts the 5GMM capability and the
Requested NSSAI in that first message (`internal/ue/nasmm.go`,
`BuildRegistrationRequest`), so on this testbed:

- `verify` gets NG Setup accepted and then fails at `register` on both sites
  with `AMF rejected registration`. The user-plane hint it prints after that
  (a UPF routing fault) does not apply: the UE never registered.
- `orbit conformance -t lxd-conformance` passes the checks that need no
  registered UE (NG Setup, NG Reset, unknown-UE, GTP-U Error Indication) and
  reports ERROR or FAIL for every check that needs one.

The core itself is sound: a UE that sends the cleartext IEs only and the whole
message in the Security Mode Complete's NAS message container (UERANSIM v3.2.7,
run from `ran-far` with a subscriber from this population) registers with
NIA2/NEA0, gets a PDU session and an address from `TESTBED_UE_POOL`, and
exchanges traffic with the app over N3 → UPF → N6, the app seeing the UE's
own address.

### Looking at it

```sh
lxc exec open5gs-core -- journalctl -u open5gs-amfd -f      # NF logs are in the journal
lxc exec open5gs-core -- systemctl status 'open5gs-*'
lxc exec open5gs-core -- cat /opt/open5gs/etc/open5gs/amf.yaml
lxc exec open5gs-core -- mongosh --quiet open5gs --eval 'db.subscribers.countDocuments({})'
```

`/opt/open5gs-testbed` on the core node holds what `core` staged: the settings
(`env`), `install.sh`, `apply.sh`, `subscribers.js`, the build log and the
version record.

## Installing ORBIT and the responder

```sh
./testbed.sh apps          # both of the below
./testbed.sh app           # responder on orbit-app, bound to N6
./testbed.sh ran           # ORBIT on both RAN nodes (or: ran near, ran far)
```

ORBIT is built **on the host** and the binary pushed to the nodes. That keeps
repository credentials off the testbed and is faster than putting a Go
toolchain on each node. `ORBIT_VERSION` selects the ref; empty builds the
working tree as-is, which is what you want mid-change.

Every ORBIT node gets `/etc/orbit/env` (mode 600): PLMN, the subscriber keys as
the deployed core has them, the core's coordinates and the responder token, and
on the RAN nodes the server's listen address, `https://` URL and API token
(`ORBIT_TOKEN`). The units read it, and so does a shell:

Each RAN node also gets an ORBIT **target** named `lxd`, in
`/root/.config/orbit/targets/lxd.yaml` (mode 600). It holds the core, the
provisioned subscriber pool, that node's own N2 and N3 addresses (one per
simulated gNB) and the N6 responder. The keys and the responder token are
`${ORBIT_KI}`, `${ORBIT_OPC}` and `${ORBIT_RESPONDER_TOKEN}` references to
`/etc/orbit/env`, so the file holds no secret ([docs/targets.md](../../docs/targets.md)).
Every command and every example then runs with `-t lxd`:

```sh
lxc exec orbit-ran-far -- bash
set -a; . /etc/orbit/env; set +a
orbit --server $ORBIT_SERVER target probe lxd
orbit --server $ORBIT_SERVER cell ngsetup -t lxd --gnb-id 42
orbit conformance -t lxd
orbit --server $ORBIT_SERVER runs start-fleet examples/fleet-testbed.yaml -t lxd
```

The API-client commands (`cell`, `ue`, `runs`) default to `127.0.0.1:8412`
and the server listens on the node's OAM address, so pass
`--server $ORBIT_SERVER` (the env carries it). `ran` pins the server's
certificate for root on the node (`orbit server trust`, with the fingerprint
computed from the certificate on the node), so the client verifies it.

| Node | Unit | Listens on |
|---|---|---|
| `orbit-ctrl` | `aether-ops` | `0.0.0.0:8186` — API **and** UI |
| `orbit-app` | `orbit-responder` | `10.133.0.30:9551` — **N6 only**, bearer token |
| `orbit-ran-near` | `orbit` | `10.130.0.21:8412` — API **and** dashboard, OAM only, TLS + bearer token |
| `orbit-ran-far` | `orbit` | `10.137.0.21:8412` — API **and** dashboard, far OAM only, TLS + bearer token |

The responder binds its N6 address specifically and requires a token: it is a
remotely-aimable traffic generator and has no business answering on the
management network or for anyone. The API server binds the node's OAM address
rather than `0.0.0.0`: the same port serves the monitoring dashboard, worth
reaching from a browser inside the site, but the API carries subscriber
credentials (Ki / OPc), so it has no business on N2 or N3. Off loopback
`orbit serve` refuses to start without TLS and a token, so the unit runs it
with `--tls auto` (a self-signed certificate kept in `/var/lib/orbit/runs/tls`)
and `--token-file /etc/orbit/api-token`. The responder token is generated once
per testbed and kept on the app node (and in the host cache), so reinstalling a
RAN node keeps working against a running responder; the API token likewise,
kept on the RAN nodes and in the host cache.

Return routing for the UE pool is in the app node's **netplan** (via r-site),
not in a unit: the app knows nothing about the UPF, as on a real site.

The dashboard is embedded into the binary at build time, so a build made
without `make ui` starts fine but logs `dashboard assets not embedded` and
serves a placeholder at `/`.

## Verifying the whole path: `verify`

```sh
./testbed.sh verify          # near site, then far site
./testbed.sh verify far      # just the two-hop case
```

From each RAN node in turn, with its own `/etc/orbit/env`: NG Setup over N2; a
registration with a PDU session (5G-AKA, NAS security, SMF/UPF session); a ping
from the UE through N3 → UPF → N6 to the app; five seconds of traffic to the
responder; deregistration. Fresh gNB IDs per run — the AMF does not cleanly
re-key a reused gNB ID from a new association. The far run is the one that
exercises UPF downlink routing (two hops, `ran_subnet`), which the near run
cannot.

The keys in `/etc/orbit/env` are read from the deployed values template when
the core is up, and fall back to `TESTBED_KI`/`TESTBED_OPC` (OnRamp's standard
test values) otherwise. A mismatch presents as a 5G-AKA MAC failure
(`XMAC-A … not match MAC-A`) at registration, **not** as an
authentication-rejected message — so if that appears, re-run `apps` after the
core so the env is regenerated from the deployed file.

## Configuration

[`testbed.conf`](testbed.conf) holds every tunable with its default and the
reasoning behind it. Ways to override, in increasing precedence:

1. edit `testbed.conf` (tracked — only for changes that suit everyone)
2. a profile, `-p <name>` (tracked — a whole testbed; see
   [Profiles and core kinds](#profiles-and-core-kinds))
3. `testbed.local.conf` beside it (gitignored — for one machine's specifics)
4. `testbed.<profile>.local.conf` (gitignored — one machine's specifics for one
   profile)
5. environment variables

```sh
# another testbed alongside the others, without a profile: its own prefix,
# VLAN block and UE pool
TESTBED_PREFIX=orbit2 TESTBED_VLAN_BASE=170 TESTBED_UE_POOL=100.69.0.0/16 ./testbed.sh up
```

Keep what identifies a testbed (`TESTBED_PREFIX`, `TESTBED_VLAN_BASE`,
`TESTBED_UE_POOL`, `TESTBED_CORE_KIND`, `TESTBED_CACHE_DIR`) out of
`testbed.local.conf` and the environment when using profiles: both outrank a
profile, so a value set there applies to every testbed.

Nothing site-specific is committed. Addresses, paths, sizes and keys are all
parameters, so this file describes a topology rather than one host's setup.

**Prefix.** Everything is named `${TESTBED_PREFIX}-*`, default `orbit`, at most
8 characters (bridge names are interface names, capped at 15). A host may
already carry other 5G environments — the Ref 2 replica uses `ref2-`, the
aether-ops dev environment `aether-` — and they coexist because nothing here
has a host address to collide on.

**Addressing** is one knob: `TESTBED_VLAN_BASE`. Everything else (VLAN ids,
subnets, the nftables set, routes, the OnRamp vars, the ORBIT env, the checks)
derives from it and the per-node host octets, so the pieces cannot disagree.

## Notes and known edges

**Long commands are not wrapped in the wedge timeout.** `lxc_do` bounds every
mutating `lxc` call with `TESTBED_LXC_TIMEOUT` (90s) and retries it, which is
right for control-plane calls that should return instantly. It is actively wrong
for a command whose job is to take minutes — a Kubernetes install, a source
build, a `kubectl rollout status --timeout=300s`. Those go through `lxc_long`
instead, which does not time out and does not retry: re-running the second half
of an interrupted install is worse than reporting the interruption. Putting a
long command back through `lxc_do` makes it fail at 90 seconds while reporting
the LXD client wedge, which is a genuinely confusing place to start debugging.

**Routing between segments is real, not host magic.** A node reaches an
address on a segment it has no interface on only through a router, and `check`
asserts the hops. The earlier shape of this testbed let the LXD host route
between flat bridges, which hid interface-selection bugs; this one does not.

**The host cannot reach the site directly.** By design (the bridges have no
host address). Use `lxc exec`, `testbed.sh ssh`, or a host route via r-site's
uplink address if you need a browser on a dashboard.

**The near site has no user plane until UPF-5 is fixed.** Reproduced here on
2026-10-24 (OnRamp a3928d2, chart 5.0.5, upf rel-2.6.3): a UE on
`orbit-ran-near` registers and gets a PDU session, uplink reaches the app, and
**0/5** pings come back; the same UE on `orbit-ran-far` gets 5/5. The UPF's
route controller programs only kernel routes that have a gateway into BESS, so
downlink to a gNB on the UPF's own N3 subnet has no next hop, while the far
site's `ran_subnet` route via r-site does (sdcore-lab `research/ISSUES.md`
UPF-5). `verify` reports the near-site ping as FAIL for that reason; it is the
core's fault, deliberately not papered over in the testbed. Run user-plane
work from `orbit-ran-far`, control-plane work from either.

**Ref 2 differences.** No `nginx`/`iperf3` service on the app node beyond the
`iperf3` package (ORBIT's responder is the traffic sink); a control node that
Ref 2 does not have; 1000 subscribers rather than Ref 2's provisioned 100; and
OnRamp upstream `a3928d2` with `core.networks` rather than Ref 2's fork pin.
Everything on the wire — VLANs per plane, router-owned gateways, routed pool,
NAT at the edge only, two-hop far site, End Markers on — is the same.

**`lxc profile create` has hung for 20+ minutes** twice on this host while the
daemon stayed responsive to every other command. Killing the stuck client and
re-running completed instantly. See Troubleshooting.

## Troubleshooting

**A node has no address.** cloud-init writes the netplan on first boot, so give
it a moment. Check with `lxc exec <node> -- cat /etc/netplan/50-cloud-init.yaml`
and `lxc exec <node> -- cloud-init status --long`.

**A node has an address but no Internet / `apt` failed at first boot.** Its
path out is `orbit-rsite`. `check` tells you which hop is missing; `lxc exec
orbit-rsite -- nft list ruleset` should show the masquerade rule and
`sysctl net.ipv4.ip_forward` should be 1. If r-site itself has no uplink
address, the `TESTBED_UPLINK_NET` bridge is not handing out DHCP.

**`check` fails the far-site traceroute.** The hops must be exactly r-site's N2
gateway and r-ran's WAN address. A missing first hop means the core node's
route to the far N2 subnet (via `n2`) did not apply; a missing second means
r-site's WAN routes or r-ran's forwarding are off. Nothing on the far site is
reachable from the core until both hold.

**`check` passes but the far UE gets no downlink.** That is the UPF, not the
site: it must route the far N3 subnet via its N3 gateway (`ran_subnet`). Look
at `kubectl -n aether-5gc exec upf-0 -c bessd -- ip route` for a
`10.136.0.0/24 via 10.132.0.1` route.

**`lxc` hangs on an otherwise idle host.** Seen twice at `lxc profile create`,
hung 20+ minutes while the daemon answered every other command instantly and
`lxc operation list` was empty — a stuck client, not a stuck daemon. Killing the
client and re-running succeeded immediately both times.

The script bounds this rather than inheriting it: every mutating `lxc` call
runs under a timeout and is retried (`TESTBED_LXC_TIMEOUT`, default 90s;
`TESTBED_LXC_RETRIES`, default 3). A wedge becomes a slow run instead of an
indefinite stall, and a genuine `lxc` error is not retried. The root cause is
still unknown — the daemon log shows nothing at the hang times, and the host had
17 concurrent LXD connections from another workload, which may or may not be
related.

**Storage pool not found.** Set `TESTBED_STORAGE_POOL` to a pool from
`lxc storage list`.

**"The instance you are starting doesn't have any network attached to it."**
Harmless. LXD prints this at `lxc init` time because the profile carries no NIC;
the script attaches per-node NICs immediately afterwards, which is what keeps
each node on only the VLANs it belongs on.
