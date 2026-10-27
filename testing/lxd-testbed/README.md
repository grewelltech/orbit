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

Then, in order: `ctrl` and `keys` to stand up the management plane, `wanem` for
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
`10.130.0.0/21` via the site router's uplink address.

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
on the RAN nodes the server's listen address. The units read it, and so does a
shell:

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
`--server $ORBIT_SERVER` (the env carries it).

| Node | Unit | Listens on |
|---|---|---|
| `orbit-ctrl` | `aether-ops` | `0.0.0.0:8186` — API **and** UI |
| `orbit-app` | `orbit-responder` | `10.133.0.30:9551` — **N6 only**, bearer token |
| `orbit-ran-near` | `orbit` | `10.130.0.21:8412` — API **and** dashboard, OAM only |
| `orbit-ran-far` | `orbit` | `10.137.0.21:8412` — API **and** dashboard, far OAM only |

The responder binds its N6 address specifically and requires a token: it is a
remotely-aimable traffic generator and has no business answering on the
management network or for anyone. The API server binds the node's OAM address
rather than `0.0.0.0`: the same port serves the monitoring dashboard, worth
reaching from a browser inside the site, but the API is unauthenticated and
carries subscriber credentials (Ki / OPc), so it has no business on N2 or N3.
The token is generated once per testbed and kept on the app node (and in the
host cache), so reinstalling a RAN node keeps working against a running
responder.

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
reasoning behind it. Three ways to override, in increasing precedence:

1. edit `testbed.conf` (tracked — only for changes that suit everyone)
2. `testbed.local.conf` beside it (gitignored — for one machine's specifics)
3. environment variables

```sh
# a second testbed alongside the first: its own prefix and its own VLAN block
TESTBED_PREFIX=orbit2 TESTBED_VLAN_BASE=140 ./testbed.sh up
```

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
