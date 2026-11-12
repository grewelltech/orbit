#!/usr/bin/env bash
#
# Stand up an LXD testbed for ORBIT in the shape of the sdcore-lab "Ref 2"
# small site: VLAN-trunked OAM/N2/N3/N6, a site router that owns every gateway,
# a routed UE pool, and a far-site RAN machine two routed hops away over an
# emulated WAN. See README.md for usage, topology and troubleshooting.
#
# Every LXD object created here carries TESTBED_PREFIX, so `down` removes
# exactly what `up` made and nothing else on the host is at risk.

set -euo pipefail

PROG=$(basename "$0")
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# A profile selects a whole testbed in one word: `-p <name>` before the
# command, or TESTBED_PROFILE. It names profiles/<name>.conf, which carries
# what makes that testbed its own — prefix, VLAN block, UE pool, core kind.
# With no profile the testbed is testbed.conf's.
TESTBED_PROFILE=${TESTBED_PROFILE:-}
case "${1:-}" in
    -p|--profile)
        [ $# -ge 2 ] || { echo "$PROG: $1 needs a profile name" >&2; exit 1; }
        TESTBED_PROFILE=$2; shift 2
        # Hints and usage then name commands that reach this testbed.
        PROG="$PROG -p $TESTBED_PROFILE" ;;
esac
PROFILE_CONF=""
if [ -n "$TESTBED_PROFILE" ]; then
    PROFILE_CONF="$HERE/profiles/$TESTBED_PROFILE.conf"
    case "$TESTBED_PROFILE" in *[!a-z0-9_-]*) PROFILE_CONF="" ;; esac
    if [ ! -f "$PROFILE_CONF" ]; then
        echo "$PROG: no profile '$TESTBED_PROFILE' (have:$(for f in "$HERE"/profiles/*.conf; do [ -f "$f" ] && printf ' %s' "$(basename "$f" .conf)"; done))" >&2
        exit 1
    fi
fi

# Four files, read in this order: testbed.<profile>.local.conf,
# testbed.local.conf, profiles/<profile>.conf, testbed.conf (the first and
# third only when a profile is selected). So a profile replaces testbed.conf's
# defaults, a local override replaces either, and the override for one profile
# replaces the override for all of them.
#
# Local overrides are sourced BEFORE testbed.conf, and the order is the whole
# mechanism: every assignment in both files is `: "${VAR:=...}"`, which sets a
# variable only when it is unset. So whichever file is read first wins, and
# testbed.conf's defaults become no-ops for anything already set.
#
# This used to be the other way round, which silently made testbed.local.conf
# inert for every variable testbed.conf defines -- nearly all of them. The
# failure is quiet and expensive: an override appears to be accepted, the run
# succeeds, and it has used the tracked default throughout. Environment
# variables still take precedence over both, since they are set before this
# script starts.
if [ -n "$TESTBED_PROFILE" ]; then
    : "${TESTBED_PROFILE_LOCAL_CONF:=$HERE/testbed.$TESTBED_PROFILE.local.conf}"
    if [ -f "$TESTBED_PROFILE_LOCAL_CONF" ]; then
        # shellcheck disable=SC1090
        source "$TESTBED_PROFILE_LOCAL_CONF"
    fi
fi
: "${TESTBED_LOCAL_CONF:=$HERE/testbed.local.conf}"
if [ -f "$TESTBED_LOCAL_CONF" ]; then
    # shellcheck disable=SC1090
    source "$TESTBED_LOCAL_CONF"
fi
if [ -n "$PROFILE_CONF" ]; then
    # shellcheck disable=SC1090
    source "$PROFILE_CONF"
fi
# shellcheck source=testbed.conf
source "$HERE/testbed.conf"

P="$TESTBED_PREFIX"
# LXD bridges are Linux interfaces, capped at 15 characters; `-ftrunk` is the
# longest suffix below.
[ ${#P} -le 8 ] || { echo "$PROG: TESTBED_PREFIX '$P' is longer than 8 characters" >&2; exit 1; }

# ── core kinds ───────────────────────────────────────────────────────────────
#
# TESTBED_CORE_KIND names the 5G core the testbed is built around. Everything
# outside the core node is the same for every kind; what differs is how the
# core is deployed and two facts about the topology. Both come from
# cores/<kind>.sh, sourced here, after the configuration and before anything
# else is defined.
#
# A core file sets, at source time (assignments only — the helpers below do
# not exist yet, so it must not call anything):
#
#   CORE_NAME           the core's name for messages and usage text.
#   CORE_HAS_CTRL       true: the testbed has a control node (aether-ops) and
#                       the `ctrl` and `keys` steps. false: no such node is
#                       created and those steps refuse to run.
#   CORE_UPF_ATTACH     macvlan: the UPF attaches to the core node's n3 and n6
#                       itself, so those interfaces are up with no address.
#                       host: the UPF runs on the core node's host network, so
#                       the core node holds UPF_N3 on n3 and UPF_N6 on n6, with
#                       the far site's N3 subnet routed via the N3 gateway.
#   CORE_NODE_PACKAGES  extra packages cloud-init installs on the core node at
#                       first boot (space-separated; may be empty).
#
# It may also give defaults for settings of its own, in testbed.conf's form
# (`: "${FREE5GC_VERSION:=v4.0.0}"`), so the environment, the local overrides
# and the profile still win.
#
# And it defines five functions, called with no arguments:
#
#   core_<kind>_deploy       Installs and starts the core on NODE_CORE. Called
#                            by `core` once the node exists and the UE pool is
#                            known to hold the subscribers. Repeatable: a
#                            second run applies the current configuration.
#                            Afterwards the AMF serves NGAP on AMF_N2
#                            (CORE_N2:38412, SCTP), the UPF serves GTP-U on
#                            UPF_N3:2152 and forwards between it and UPF_N6,
#                            UE addresses come from TESTBED_UE_POOL and leave
#                            N6 unNATed (r-site routes the pool to UPF_N6), and
#                            TESTBED_SUB_COUNT subscribers from
#                            TESTBED_SUB_START exist with TESTBED_KI and
#                            TESTBED_OPC, in PLMN TESTBED_MCC/TESTBED_MNC, TAC
#                            TESTBED_TAC, slice TESTBED_SST/TESTBED_SD, DNN
#                            TESTBED_DNN. Dies on failure.
#   core_<kind>_status       Prints the core's health for `core-status`: its
#                            processes or pods, the core node's interfaces,
#                            where the UPF is attached. Read-only, and must
#                            not fail when the core is not deployed.
#   core_<kind>_reprovision  Restores the subscriber population on a deployed
#                            core without redeploying it, for `reprovision`.
#   core_<kind>_keys         Prints "<ki> <opc>" as the deployed core holds
#                            them, `-` for one it cannot read. Called only
#                            while NODE_CORE is running; ORBIT is handed these,
#                            or TESTBED_KI/TESTBED_OPC in place of a `-`.
#   core_<kind>_versions     Prints one "<component> <ref> <resolved commit or
#                            version>" line per component of the core as last
#                            deployed. `core` stores it on the core node as
#                            /etc/orbit/testbed-versions.
#
# Those functions may use anything defined in this file. The parts meant for
# them: on_core and on (run a shell command on a node as root), push_file,
# lxc_do and lxc_long, exists_instance, is_running, wait_cloud_init, info, warn,
# die, ip_in, cidr_in; the node names NODE_CORE, NODE_RSITE and the rest; the
# addresses CORE_OAM, CORE_N2, AMF_N2, UPF_N3, UPF_N6, GW_OAM, GW_N2, GW_N3,
# GW_N6, APP_N6, ECHO_PEER; every TESTBED_* setting; and TESTBED_CACHE_DIR, this testbed's
# own scratch directory on the host. On the core node the interfaces are named
# oam, n2, n3 and n6.
#
# The shape (see "shape" below) changes what surrounds the core node, and a
# deploy has to hold in each: without the site router there is no oam interface
# and no gateway on N2, N3 or N6 — the node reaches the Internet through
# `uplink` (DHCP), `oam_of "$NODE_CORE"` gives its management address in every
# shape (CORE_OAM only with the router), and UPF_N3_GW, UPF_RAN_SUBNET and
# UPF_N6_GW name the UPF's next hops.
CORE_KIND=$TESTBED_CORE_KIND
case "$CORE_KIND" in
    ''|*[!a-z0-9]*) echo "$PROG: TESTBED_CORE_KIND '$CORE_KIND' is not a core kind name" >&2; exit 1 ;;
esac
[ -f "$HERE/cores/$CORE_KIND.sh" ] \
    || { echo "$PROG: unknown TESTBED_CORE_KIND '$CORE_KIND' (no cores/$CORE_KIND.sh)" >&2; exit 1; }
CORE_NAME=$CORE_KIND; CORE_HAS_CTRL=false; CORE_UPF_ATTACH=host; CORE_NODE_PACKAGES=""
# shellcheck disable=SC1090
source "$HERE/cores/$CORE_KIND.sh"
case "$CORE_HAS_CTRL" in true|false) ;; *) echo "$PROG: cores/$CORE_KIND.sh: CORE_HAS_CTRL must be true or false" >&2; exit 1 ;; esac
case "$CORE_UPF_ATTACH" in macvlan|host) ;; *) echo "$PROG: cores/$CORE_KIND.sh: CORE_UPF_ATTACH must be macvlan or host" >&2; exit 1 ;; esac

# ── shape ────────────────────────────────────────────────────────────────────
#
# Which parts of the site exist. TESTBED_SHAPE gives all four their default
# (site: all present; minimal: none), and each TESTBED_WITH_* overrides one.
#
#   WITH_ROUTER  r-site: the gateways, the OAM VLAN and the NAT to the Internet.
#                Without it every node has an `uplink` NIC on TESTBED_UPLINK_NET
#                instead (DHCP, default route) as its management address and
#                Internet path, there is no OAM VLAN, and N2/N3/N6 have no
#                gateway: everything on them is on-link.
#   WITH_FAR     the far site: r-ran, the WAN emulator, the far RAN node and
#                their three bridges. Needs the router. Without it the one RAN
#                node is <prefix>-ran.
#   WITH_APP     the app node. Without it the RAN node also sits on N6, holds
#                the app's address there and runs the responder.
#   WITH_CTRL    the control node, for a core kind that has one. Needs the
#                router.
case "$TESTBED_SHAPE" in
    site)    shape_default=true ;;
    minimal) shape_default=false ;;
    *) echo "$PROG: TESTBED_SHAPE '$TESTBED_SHAPE' is not site or minimal" >&2; exit 1 ;;
esac
WITH_ROUTER=${TESTBED_WITH_ROUTER:-$shape_default}; WITH_FAR=${TESTBED_WITH_FAR:-$shape_default}
WITH_APP=${TESTBED_WITH_APP:-$shape_default};       WITH_CTRL=${TESTBED_WITH_CTRL:-$shape_default}
for flag in WITH_ROUTER WITH_FAR WITH_APP WITH_CTRL; do
    case "${!flag}" in true|false) ;; *) echo "$PROG: TESTBED_$flag must be true or false, not '${!flag}'" >&2; exit 1 ;; esac
done
HAS_CTRL=false
if [ "$CORE_HAS_CTRL" = true ] && [ "$WITH_CTRL" = true ]; then HAS_CTRL=true; fi
if [ "$WITH_ROUTER" != true ] && { [ "$WITH_FAR" = true ] || [ "$HAS_CTRL" = true ]; }; then
    echo "$PROG: the far site and the control node need the site router (TESTBED_WITH_ROUTER=true)" >&2; exit 1
fi
case "$TESTBED_ONRAMP_NET" in
    current) LEGACY_NET=false ;;
    legacy)  LEGACY_NET=true ;;
    *) echo "$PROG: TESTBED_ONRAMP_NET '$TESTBED_ONRAMP_NET' is not current or legacy" >&2; exit 1 ;;
esac
if [ "$LEGACY_NET" = true ] && { [ "$CORE_KIND" != sdcore ] || [ "$WITH_ROUTER" = true ] || [ "$WITH_FAR" = true ] || [ "$WITH_APP" = true ]; }; then
    echo "$PROG: TESTBED_ONRAMP_NET=legacy needs TESTBED_CORE_KIND=sdcore and the minimal shape" >&2; exit 1
fi

# Four bridges with no address, no DHCP and no NAT: they are 802.1Q switches,
# and every gateway lives on a router VM. The site trunk carries the core-site
# VLANs, the far trunk the far-site VLANs, and the two WAN segments are the
# cables on either side of the WAN emulator.
NET_TRUNK="$P-trunk"; NET_FTRUNK="$P-ftrunk"; NET_WAN_A="$P-wan-a"; NET_WAN_B="$P-wan-b"
ALL_NETS=("$NET_TRUNK" "$NET_FTRUNK" "$NET_WAN_A" "$NET_WAN_B")
NETS=("$NET_TRUNK")
if [ "$WITH_FAR" = true ]; then NETS+=("$NET_FTRUNK" "$NET_WAN_A" "$NET_WAN_B"); fi

NODE_RSITE="$P-rsite"; NODE_WANEM="$P-wanem"; NODE_RRAN="$P-rran"
NODE_CTRL="$P-ctrl"; NODE_CORE="$P-core"; NODE_RANN="$P-ran-near"; NODE_RANF="$P-ran-far"; NODE_APP="$P-app"
# Every name a testbed with this prefix can have used, whatever its shape, so
# `down` removes one that was built in another shape.
ALL_NODES=("$NODE_RSITE" "$NODE_WANEM" "$NODE_RRAN" "$NODE_CTRL" "$NODE_CORE" "$P-ran" "$NODE_RANN" "$NODE_RANF" "$NODE_APP")
if [ "$WITH_FAR" != true ]; then NODE_RANN="$P-ran"; fi
# The node the responder runs on: the app node, or the RAN node standing in.
NODE_RESP=$NODE_APP
if [ "$WITH_APP" != true ]; then NODE_RESP=$NODE_RANN; fi
# Every node, in the order a status table should read them: the path off the
# testbed first, then the operator's entry point, the core under test, and the
# load that exercises it. Only the nodes this shape and core kind have.
NODES=()
if [ "$WITH_ROUTER" = true ]; then NODES+=("$NODE_RSITE"); fi
if [ "$WITH_FAR" = true ]; then NODES+=("$NODE_WANEM" "$NODE_RRAN"); fi
if [ "$HAS_CTRL" = true ]; then NODES+=("$NODE_CTRL"); fi
NODES+=("$NODE_CORE" "$NODE_RANN")
if [ "$WITH_FAR" = true ]; then NODES+=("$NODE_RANF"); fi
if [ "$WITH_APP" = true ]; then NODES+=("$NODE_APP"); fi
PROFILE="$P"
IMAGE_ALIAS="$P-base"

V_OAM=$TESTBED_VLAN_OAM; V_N2=$TESTBED_VLAN_N2; V_N3=$TESTBED_VLAN_N3; V_N6=$TESTBED_VLAN_N6
V_WAN=$TESTBED_VLAN_WAN; V_FN2=$TESTBED_VLAN_FAR_N2; V_FN3=$TESTBED_VLAN_FAR_N3; V_FOAM=$TESTBED_VLAN_FAR_OAM

die()  { echo "$PROG: $*" >&2; exit 1; }
# Progress goes to stderr: some of these functions return a value on stdout,
# and a stray progress line would be captured as part of it.
info() { echo "==> $*" >&2; }
warn() { echo "$PROG: warning: $*" >&2; }

# node_names <separator> [last-separator] -> the short names of this testbed's
# nodes, as `console`, `ssh` and `addrs` take them.
node_names() {
    local sep=$1 last=${2:-$1} out="" i n=${#NODES[@]}
    for ((i = 0; i < n; i++)); do
        if [ "$i" -gt 0 ]; then
            if [ "$i" -eq $((n - 1)) ]; then out+=$last; else out+=$sep; fi
        fi
        out+=${NODES[i]#"$P-"}
    done
    echo "$out"
}

# core_call <function> [args] runs core_<kind>_<function> from cores/<kind>.sh.
core_call() { local fn="core_${CORE_KIND}_$1"; shift; "$fn" "$@"; }

require_ctrl() {
    [ "$HAS_CTRL" = true ] || die "this testbed has no control node"
}

# in_nodes <node> — whether this testbed's shape has that node.
in_nodes() {
    local n
    for n in "${NODES[@]}"; do [ "$n" = "$1" ] && return 0; done
    return 1
}

# Every segment is a /24 (the WAN a /30), so an address is the subnet's first
# three octets plus a host octet.
ip_in()   { echo "${1%.*}.$2"; }                 # ip_in 10.130.0.0/24 10 -> 10.130.0.10
cidr_in() { echo "$(ip_in "$1" "$2")/${1##*/}"; } # cidr_in 10.130.0.0/24 10 -> 10.130.0.10/24

# addr_list <cidr> <first-octet> <count> -> "a/len, b/len, ..." for a netplan
# list. A count > 1 is how a RAN node carries one address per simulated gNB:
# handover needs a distinct source IP per gNB (the AMF keys the association on
# it), so N gNBs on one node means N addresses on one interface. Only the last
# octet moves, so a count that would carry past .254 is a configuration error
# rather than something to silently wrap.
addr_list() {
    local cidr=$1 first=$2 count=$3 i out=""
    [ $((first + count - 1)) -le 254 ] || die "address range $cidr .$first +$count overruns .254; shrink TESTBED_RAN_GNBS or move TESTBED_HOST_RAN"
    for ((i = 0; i < count; i++)); do
        [ -n "$out" ] && out+=", "
        out+="$(cidr_in "$cidr" $((first + i)))"
    done
    echo "$out"
}

# Well-known addresses, derived once so the netplans, the OnRamp vars, the
# ORBIT environment and the checks cannot disagree.
GW_OAM=$(ip_in "$TESTBED_NET_OAM" 1);  GW_N2=$(ip_in "$TESTBED_NET_N2" 1)
GW_N3=$(ip_in "$TESTBED_NET_N3" 1);    GW_N6=$(ip_in "$TESTBED_NET_N6" 1)
GW_FOAM=$(ip_in "$TESTBED_NET_FAR_OAM" 1); GW_FN2=$(ip_in "$TESTBED_NET_FAR_N2" 1); GW_FN3=$(ip_in "$TESTBED_NET_FAR_N3" 1)
WAN_RSITE=$(ip_in "$TESTBED_NET_WAN" 1); WAN_RRAN=$(ip_in "$TESTBED_NET_WAN" 2)
CORE_OAM=$(ip_in "$TESTBED_NET_OAM" "$TESTBED_HOST_CORE"); CORE_N2=$(ip_in "$TESTBED_NET_N2" "$TESTBED_HOST_CORE")
UPF_N3=$(ip_in "$TESTBED_NET_N3" "$TESTBED_UPF_ACCESS_IP"); UPF_N6=$(ip_in "$TESTBED_NET_N6" "$TESTBED_UPF_CORE_IP")
APP_OAM=$(ip_in "$TESTBED_NET_OAM" "$TESTBED_HOST_APP"); APP_N6=$(ip_in "$TESTBED_NET_N6" "$TESTBED_HOST_APP")
RANN_OAM=$(ip_in "$TESTBED_NET_OAM" "$TESTBED_HOST_RAN"); RANF_OAM=$(ip_in "$TESTBED_NET_FAR_OAM" "$TESTBED_HOST_RAN")
CTRL_OAM=$(ip_in "$TESTBED_NET_OAM" "$TESTBED_HOST_CTRL"); WANEM_OAM=$(ip_in "$TESTBED_NET_OAM" "$TESTBED_HOST_WANEM")
AMF_N2="$CORE_N2:38412"; RESPONDER="$APP_N6:9551"; ORBIT_PORT=8412
# The UDP echo peer on N6, beside the responder: what a UE's traffic is echoed
# by when a test needs downlink that answers its own uplink (the conformance
# checks' --traffic).
ECHO_PEER="$APP_N6:$TESTBED_ECHO_PORT"

# What the UPF and the responder's host use as next hops, which is where the
# shapes differ. With the router they are its gateways. Without it nothing
# routes: the UPF's N6 next hop is the responder itself, UE-bound replies go
# straight back to the UPF, and the gNBs are on-link on N3.
#
#   UPF_N6_GW       the UPF's default next hop on N6.
#   UPF_N3_GW       the UPF's next hop for UPF_RAN_SUBNET on N3. With no far
#                   site that subnet is N3 itself and the next hop is the RAN
#                   node's first N3 address — every gNB address is on that one
#                   interface, so one neighbour reaches them all.
#   UPF_RAN_SUBNET  the gNB subnet the UPF reaches through UPF_N3_GW.
#   POOL_RETURN_VIA where the responder's host sends traffic for the UE pool.
UPF_N6_GW=$GW_N6; POOL_RETURN_VIA=$GW_N6
if [ "$WITH_ROUTER" != true ]; then UPF_N6_GW=$APP_N6; POOL_RETURN_VIA=$UPF_N6; fi
UPF_N3_GW=$GW_N3; UPF_RAN_SUBNET=$TESTBED_NET_FAR_N3
if [ "$WITH_FAR" != true ]; then UPF_N3_GW=$(ip_in "$TESTBED_NET_N3" "$TESTBED_HOST_RAN"); UPF_RAN_SUBNET=$TESTBED_NET_N3; fi

# The legacy OnRamp layout (TESTBED_ONRAMP_NET=legacy): the UPF sits on
# internal subnets behind the core node, which holds CORE_N3 and CORE_N6 and
# forwards between them and the UPF's macvlans. The gNBs reach the UPF's
# access address through CORE_N3, and the responder sends UE-bound traffic
# to CORE_N6, which routes the pool to the UPF's core address.
#
#   LEGACY_GW_ACCESS, LEGACY_GW_CORE  the core node's addresses on them (.1)
#   UPF_N3, UPF_N6                    the UPF's own (.3), as OnRamp's defaults
CORE_N3=$(ip_in "$TESTBED_NET_N3" "$TESTBED_HOST_CORE"); CORE_N6=$(ip_in "$TESTBED_NET_N6" "$TESTBED_HOST_CORE")
if [ "$LEGACY_NET" = true ]; then
    LEGACY_GW_ACCESS=$(ip_in "$TESTBED_LEGACY_ACCESS_NET" 1); LEGACY_GW_CORE=$(ip_in "$TESTBED_LEGACY_CORE_NET" 1)
    UPF_N3=$(ip_in "$TESTBED_LEGACY_ACCESS_NET" 3); UPF_N6=$(ip_in "$TESTBED_LEGACY_CORE_NET" 3)
    POOL_RETURN_VIA=$CORE_N6
fi

require_lxd() {
    command -v lxc >/dev/null 2>&1 || die "lxc not found — LXD is a prerequisite (see README)"
    lxc info >/dev/null 2>&1 || die "cannot talk to the LXD daemon; is it running and is $USER in the lxd group?"
    lxc info 2>/dev/null | grep -q "qemu" \
        || die "this LXD has no VM (qemu) driver; these nodes are VMs, not containers"
    lxc storage show "$TESTBED_STORAGE_POOL" >/dev/null 2>&1 \
        || die "storage pool '$TESTBED_STORAGE_POOL' not found — set TESTBED_STORAGE_POOL (lxc storage list)"
}

# LXD clients on this host have been seen to wedge indefinitely on a trivial
# write (`lxc profile create`) while the daemon stayed responsive to every other
# command and `lxc operation list` was empty — a stuck client, not a stuck
# daemon. Killing it and retrying then succeeded instantly, twice.
#
# The root cause is not established, so this does not claim to fix it: it bounds
# it. Every mutating call goes through a timeout and is retried, which turns an
# indefinite hang into a slow-but-completing run. Without this, a CI gate built
# on LXD inherits an occasional unbounded stall.
: "${TESTBED_LXC_TIMEOUT:=90}"
: "${TESTBED_LXC_RETRIES:=3}"

lxc_do() {
    local attempt=1 rc
    while :; do
        # Capture the status with `|| rc=$?`, never from `$?` after an `if`.
        # An `if` whose condition fails and which has no `else` exits 0, so
        # reading `$?` there reports success for every failure — which made
        # this function return 0 no matter what lxc did, and silently turned
        # every `|| die` guard built on it into dead code.
        rc=0
        timeout "$TESTBED_LXC_TIMEOUT" lxc "$@" || rc=$?
        if [ "$rc" -eq 0 ]; then
            return 0
        fi
        # 124 is timeout(1)'s "command timed out"; anything else is a real
        # failure from lxc and should not be retried.
        if [ "$rc" -ne 124 ]; then
            return "$rc"
        fi
        if [ "$attempt" -ge "$TESTBED_LXC_RETRIES" ]; then
            die "lxc $1 ${2:-} timed out ${TESTBED_LXC_RETRIES}x after ${TESTBED_LXC_TIMEOUT}s each — see README (LXD client wedge)"
        fi
        warn "lxc $1 ${2:-} timed out after ${TESTBED_LXC_TIMEOUT}s; retrying ($attempt/$TESTBED_LXC_RETRIES)"
        attempt=$((attempt + 1))
    done
}

# lxc_long runs a command that is SUPPOSED to take a long time — a Kubernetes
# install, a source build, a rollout that waits on pods.
#
# The timeout above exists to bound control-plane calls that should return
# almost instantly, so a wedged client becomes a slow run instead of an
# indefinite stall. Applying it to a command whose whole job is to run for
# minutes does the opposite: it guarantees failure at 90 seconds, three times
# over, and then reports a timeout that looks like the wedge it was written for.
#
# These are deliberately not retried either. Re-running the second half of an
# interrupted install is a worse outcome than reporting that it was interrupted.
lxc_long() { lxc "$@"; }

exists_net()      { lxc network show "$1" >/dev/null 2>&1; }
exists_instance() { lxc info "$1" >/dev/null 2>&1; }
exists_profile()  { lxc profile show "$1" >/dev/null 2>&1; }
exists_image()    { lxc image info "$1" >/dev/null 2>&1; }
is_running()      { [ "$(lxc list "^$1\$" -c s --format csv 2>/dev/null)" = "RUNNING" ]; }

# on <node> <cmd...> — run a shell command on a node as root. Long-running by
# nature (installs, builds), so not wrapped in the wedge timeout.
on() { local n=$1; shift; lxc_long exec "$n" -- bash -lc "$*"; }
on_core() { on "$NODE_CORE" "$@"; }
on_ctrl() { on "$NODE_CTRL" "$@"; }

# push_file <node> <local> <remote> [mode] — write a file into a node. Goes
# through `exec ... cat` rather than `lxc file push`, which has uid trouble
# with VMs on this LXD.
push_file() {
    lxc_do exec "$1" -- sh -c "mkdir -p \"\$(dirname '$3')\" && cat > '$3' && chmod ${4:-644} '$3'" < "$2" \
        || die "could not write $3 on $1"
}

# ── image ──────────────────────────────────────────────────────────────────────
# Stock Ubuntu cloud images carry cloud-init and the LXD agent, so there is
# nothing to build: addressing is injected as cloud-init network-config, and
# `lxc exec` works. That is why this is a published image and not a bespoke one.

cmd_image() {
    require_lxd
    info "pulling $TESTBED_IMAGE (cached by LXD after the first use)"
    lxc image copy "$TESTBED_IMAGE" local: --alias "$IMAGE_ALIAS" --vm >/dev/null 2>&1 \
        || info "image already present locally"
    exists_image "$IMAGE_ALIAS" \
        || die "no local image '$IMAGE_ALIAS' after copying $TESTBED_IMAGE — check 'lxc image list'"
    info "image ready: $IMAGE_ALIAS"
}

# ── networks ─────────────────────────────────────────────────────────────────

# Each bridge is a switch and nothing more: no host address, no DHCP, no NAT.
# LXD enables VLAN filtering on a bridge whose ports ask for VLANs, so a port
# only sees the VLANs its NIC is a member of (vlan.tagged / vlan below), exactly
# like a switch port's allowed-VLAN list.
make_net() {
    local name=$1 desc=$2
    if exists_net "$name"; then
        info "network $name exists, leaving it alone"
        return
    fi
    info "creating network $name — $desc"
    lxc_do network create "$name" ipv4.address=none ipv6.address=none bridge.mtu="$TESTBED_MTU" >/dev/null
}

cmd_networks() {
    require_lxd
    exists_net "$TESTBED_UPLINK_NET" \
        || die "uplink network '$TESTBED_UPLINK_NET' not found — it is the site router's path to the Internet (set TESTBED_UPLINK_NET)"
    if [ "$WITH_ROUTER" = true ]; then
        make_net "$NET_TRUNK"  "core-site 802.1Q trunk: VLANs $V_OAM-$V_N6 (oam, n2, n3, n6)"
    else
        make_net "$NET_TRUNK"  "802.1Q trunk: VLANs $V_N2-$V_N6 (n2, n3, n6)"
    fi
    [ "$WITH_FAR" = true ] || return 0
    make_net "$NET_FTRUNK" "far-site 802.1Q trunk: VLANs $V_FN2-$V_FOAM (n2, n3, oam)"
    make_net "$NET_WAN_A"  "WAN cable: r-site <-> wanem (VLAN $V_WAN)"
    make_net "$NET_WAN_B"  "WAN cable: wanem <-> r-ran (VLAN $V_WAN)"
}

# ── node definitions ─────────────────────────────────────────────────────────
#
# nics_for <node> prints one "device network vlans" line per NIC. vlans is
# "t:ID,ID" for a trunk port (tagged members), "u:ID" for an access port (the
# bridge strips the tag towards the VM), or "-" for a plain port on an
# unmanaged network. The device name is also the interface name inside the
# node, so a netplan reads as the topology: `trunk`, `uplink`, `wanlink`.
#
# The WAN emulator must see the WAN untagged: wanemd's tc classifier is
# `protocol ip`, so 802.1Q-tagged frames would silently skip every impairment
# rule. Its two ports are access ports; the routers' WAN ports are trunks.
#
# Without the site router a node's first NIC is `uplink`, on the uplink
# network, and its trunk port carries no OAM VLAN. Without the app node the RAN
# node's trunk port is also a member of N6.
nics_for() {
    local oam="$V_OAM," n6=""
    if [ "$WITH_ROUTER" != true ]; then
        oam=""
        echo "uplink $TESTBED_UPLINK_NET -"
    fi
    if [ "$WITH_APP" != true ]; then n6=",$V_N6"; fi
    case "$1" in
        "$NODE_RSITE") echo "uplink $TESTBED_UPLINK_NET -"
                       echo "trunk $NET_TRUNK t:$V_OAM,$V_N2,$V_N3,$V_N6"
                       if [ "$WITH_FAR" = true ]; then echo "wanlink $NET_WAN_A t:$V_WAN"; fi ;;
        "$NODE_WANEM") echo "trunk $NET_TRUNK t:$V_OAM"
                       echo "wan-a $NET_WAN_A u:$V_WAN"
                       echo "wan-b $NET_WAN_B u:$V_WAN" ;;
        "$NODE_RRAN")  echo "wanlink $NET_WAN_B t:$V_WAN"
                       echo "trunk $NET_FTRUNK t:$V_FN2,$V_FN3,$V_FOAM" ;;
        "$NODE_CTRL")  echo "trunk $NET_TRUNK t:$V_OAM" ;;
        "$NODE_CORE")  echo "trunk $NET_TRUNK t:$oam$V_N2,$V_N3,$V_N6" ;;
        "$NODE_RANN")  echo "trunk $NET_TRUNK t:$oam$V_N2,$V_N3$n6" ;;
        "$NODE_RANF")  echo "trunk $NET_FTRUNK t:$V_FN2,$V_FN3,$V_FOAM" ;;
        "$NODE_APP")   echo "trunk $NET_TRUNK t:$oam$V_N6" ;;
        *) die "no NIC definition for $1" ;;
    esac
}

size_for() {
    case "$1" in
        "$NODE_RSITE"|"$NODE_WANEM"|"$NODE_RRAN") echo "$TESTBED_RTR_CPU $TESTBED_RTR_MEM $TESTBED_RTR_DISK" ;;
        "$NODE_CTRL") echo "$TESTBED_CTRL_CPU $TESTBED_CTRL_MEM $TESTBED_CTRL_DISK" ;;
        "$NODE_CORE") echo "$TESTBED_CORE_CPU $TESTBED_CORE_MEM $TESTBED_CORE_DISK" ;;
        "$NODE_RANN"|"$NODE_RANF") echo "$TESTBED_RAN_CPU $TESTBED_RAN_MEM $TESTBED_RAN_DISK" ;;
        "$NODE_APP")  echo "$TESTBED_APP_CPU $TESTBED_APP_MEM $TESTBED_APP_DISK" ;;
    esac
}

# Interfaces are matched by MAC rather than by name so the mapping cannot drift
# from what this script intended; the MACs are read back from LXD after the NICs
# are attached.
mac_of() { lxc config get "$1" "volatile.$2.hwaddr"; }

# netplan fragments (cloud-init network-config v2 is netplan without the
# `network:` wrapper). Interfaces carry no IPv6 link-local: nothing here speaks
# IPv6, and a stray address on N3 shows up in captures as noise.
eth_block() { # <name> <mac> [dhcp]
    printf '  %s:\n    match:\n      macaddress: "%s"\n    set-name: %s\n' "$1" "$2" "$1"
    if [ "${3:-}" = dhcp ]; then
        # The uplink: the Internet edge is almost certainly 1500, so it keeps
        # the image default rather than TESTBED_MTU — a jumbo frame out there
        # would be dropped.
        printf '    dhcp4: true\n'
    else
        printf '    dhcp4: false\n    link-local: []\n    mtu: %s\n' "$TESTBED_MTU"
    fi
    printf '    dhcp6: false\n    accept-ra: false\n'
}

# vlan_block <name> <id> <link> <addresses|""> <default-gw|""> ["to via"]...
# Only an interface with a default route gets resolvers: a node has exactly one
# path off the testbed, and the 3GPP reference points are deliberately not it.
vlan_block() {
    local name=$1 id=$2 link=$3 addrs=$4 gw=$5; shift 5
    # mtu matches the link's (TESTBED_MTU): a VLAN interface may not exceed its
    # parent, and the parent trunk/WAN NIC carries TESTBED_MTU from eth_block.
    printf '  %s:\n    id: %s\n    link: %s\n    link-local: []\n    mtu: %s\n' "$name" "$id" "$link" "$TESTBED_MTU"
    if [ -n "$addrs" ]; then printf '    addresses: [%s]\n' "$addrs"; fi
    if [ -n "$gw" ] || [ $# -gt 0 ]; then
        printf '    routes:\n'
        if [ -n "$gw" ]; then printf '      - to: default\n        via: %s\n' "$gw"; fi
        local r
        for r in "$@"; do printf '      - to: %s\n        via: %s\n' "${r% *}" "${r#* }"; done
    fi
    if [ -n "$gw" ]; then printf '    nameservers:\n      addresses: [%s]\n' "$TESTBED_DNS"; fi
    return 0
}

# netplan_for <node> -> this node's netplan. cloud-init writes it to
# /etc/netplan on first boot, so the addressing exists as a file on the node —
# which is what Aether OnRamp and SD-Core read to discover interfaces. A DHCP
# lease leaves nothing there.
netplan_for() {
    local node=$1 dev net vl dhcp
    local n2=$TESTBED_NET_N2 n3=$TESTBED_NET_N3 n6=$TESTBED_NET_N6 oam=$TESTBED_NET_OAM
    local fn2=$TESTBED_NET_FAR_N2 fn3=$TESTBED_NET_FAR_N3 foam=$TESTBED_NET_FAR_OAM wan=$TESTBED_NET_WAN
    # Routes that exist only with the far site, and the management VLAN that
    # exists only with the router (without it, `uplink` is the management
    # interface and takes its address and default route from DHCP).
    local far_n2=() far_n3=()
    if [ "$WITH_FAR" = true ]; then far_n2=("$fn2 $GW_N2"); far_n3=("$fn3 $GW_N3"); fi
    oam_block() { # <host octet>
        [ "$WITH_ROUTER" = true ] || return 0
        vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" "$1")" "$GW_OAM"
    }
    echo "version: 2"
    echo "ethernets:"
    while read -r dev net vl; do
        dhcp=""; [ "$dev" = uplink ] && dhcp=dhcp
        eth_block "$dev" "$(mac_of "$node" "$dev")" $dhcp
    done < <(nics_for "$node")
    if [ "$node" = "$NODE_WANEM" ]; then
        # A bump in the wire: the two WAN ports bridged, no address, no STP
        # delay. wanemd shapes what crosses this bridge.
        echo "bridges:"
        printf '  br-wan:\n    interfaces: [wan-a, wan-b]\n    mtu: %s\n    dhcp4: false\n    dhcp6: false\n    accept-ra: false\n    link-local: []\n    parameters:\n      stp: false\n      forward-delay: 0\n' "$TESTBED_MTU"
    fi
    echo "vlans:"
    case "$node" in
        "$NODE_RSITE")
            # The site router: .1 on every core-site VLAN, the Internet edge
            # (default route from DHCP on the uplink), the UE pool routed to
            # the UPF's N6 address, and the far site behind r-ran.
            vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" 1)" ""
            vlan_block n2  "$V_N2"  trunk "$(cidr_in "$n2" 1)"  ""
            vlan_block n3  "$V_N3"  trunk "$(cidr_in "$n3" 1)"  ""
            vlan_block n6  "$V_N6"  trunk "$(cidr_in "$n6" 1)"  "" "$TESTBED_UE_POOL $UPF_N6"
            [ "$WITH_FAR" = true ] || return 0
            vlan_block wan "$V_WAN" wanlink "$(cidr_in "$wan" 1)" "" \
                "$fn2 $WAN_RRAN" "$fn3 $WAN_RRAN" "$foam $WAN_RRAN" ;;
        "$NODE_WANEM")
            vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" "$TESTBED_HOST_WANEM")" "$GW_OAM" ;;
        "$NODE_RRAN")
            # The far-site router: default towards r-site over the WAN, .1 on
            # every far VLAN.
            vlan_block wan "$V_WAN" wanlink "$(cidr_in "$wan" 2)" "$WAN_RSITE"
            vlan_block oam "$V_FOAM" trunk "$(cidr_in "$foam" 1)" ""
            vlan_block n2  "$V_FN2"  trunk "$(cidr_in "$fn2" 1)"  ""
            vlan_block n3  "$V_FN3"  trunk "$(cidr_in "$fn3" 1)"  "" ;;
        "$NODE_CTRL")
            vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" "$TESTBED_HOST_CTRL")" "$GW_OAM" ;;
        "$NODE_CORE")
            # The far site's N2 is reached via the N2 gateway so the AMF
            # answers far gNBs from its N2 address, not via OAM.
            oam_block "$TESTBED_HOST_CORE"
            vlan_block n2  "$V_N2"  trunk "$(cidr_in "$n2" "$TESTBED_HOST_CORE")" "" "${far_n2[@]}"
            if [ "$CORE_UPF_ATTACH" = host ]; then
                # The UPF is a process on this node, so the node holds the
                # UPF's addresses. The far site's N3 is reached via the N3
                # gateway, as its N2 is via the N2 gateway, so downlink GTP-U
                # to a far gNB leaves from the N3 address.
                vlan_block n3  "$V_N3"  trunk "$(cidr_in "$n3" "$TESTBED_UPF_ACCESS_IP")" "" "${far_n3[@]}"
                vlan_block n6  "$V_N6"  trunk "$(cidr_in "$n6" "$TESTBED_UPF_CORE_IP")" ""
            elif [ "$LEGACY_NET" = true ]; then
                # The legacy layout routes for the UPF: the node holds an
                # address on n3 and n6, and OnRamp's router role adds the
                # UPF's access and core macvlans on n3.
                vlan_block n3  "$V_N3"  trunk "$(cidr_in "$n3" "$TESTBED_HOST_CORE")" ""
                vlan_block n6  "$V_N6"  trunk "$(cidr_in "$n6" "$TESTBED_HOST_CORE")" ""
            else
                # n3 and n6 are UP with no address: they are the macvlan
                # parents for the UPF pod.
                vlan_block n3  "$V_N3"  trunk "" ""
                vlan_block n6  "$V_N6"  trunk "" ""
            fi ;;
        "$NODE_RANN")
            oam_block "$TESTBED_HOST_RAN"
            vlan_block n2  "$V_N2"  trunk "$(addr_list "$n2" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" ""
            if [ "$LEGACY_NET" = true ]; then
                vlan_block n3  "$V_N3"  trunk "$(addr_list "$n3" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" "" "$TESTBED_LEGACY_ACCESS_NET $CORE_N3"
            else
                vlan_block n3  "$V_N3"  trunk "$(addr_list "$n3" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" ""
            fi
            if [ "$WITH_APP" != true ]; then
                # Standing in for the app node: the responder's address on N6,
                # and the UE pool routed back the way it came. A UE's packets
                # are built by ORBIT in user space and leave inside GTP-U on
                # N3, so they reach this address only through the UPF, arrive
                # on n6, and are answered out of n6.
                vlan_block n6  "$V_N6"  trunk "$(cidr_in "$n6" "$TESTBED_HOST_APP")" "" "$TESTBED_UE_POOL $POOL_RETURN_VIA"
            fi ;;
        "$NODE_RANF")
            # Two routed hops from the core: N2 and N3 traffic leaves by the
            # right interface because the core-site subnets are routed via the
            # far gateway on that same VLAN, never via OAM.
            vlan_block oam "$V_FOAM" trunk "$(cidr_in "$foam" "$TESTBED_HOST_RAN")" "$GW_FOAM"
            vlan_block n2  "$V_FN2"  trunk "$(addr_list "$fn2" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" "" "$n2 $GW_FN2"
            vlan_block n3  "$V_FN3"  trunk "$(addr_list "$fn3" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" "" "$n3 $GW_FN3" ;;
        "$NODE_APP")
            # Return traffic to UEs goes back via r-site, which routes it to
            # the UPF — the app knows nothing about the UPF, as on a real site.
            oam_block "$TESTBED_HOST_APP"
            vlan_block n6  "$V_N6"  trunk "$(cidr_in "$n6" "$TESTBED_HOST_APP")" "" "$TESTBED_UE_POOL $POOL_RETURN_VIA" ;;
    esac
}

# The site router's firewall. Routing between internal segments is neither
# filtered nor NATed; masquerade applies only to traffic leaving the uplink,
# which is the one place a site NATs. The UE pool is in the set so a UE can
# reach the Internet, as a phone would.
nft_ruleset() {
    cat <<EOF
#!/usr/sbin/nft -f
# Site router ruleset, written by testbed.sh. Internal routing is not filtered
# and not NATed; masquerade happens only on the Internet uplink.
define UPLINK = "uplink"

flush ruleset

table inet filter {
	chain input   { type filter hook input   priority filter; policy accept; }
	chain forward { type filter hook forward priority filter; policy accept; }
	chain output  { type filter hook output  priority filter; policy accept; }
}

table ip nat {
	set internal {
		type ipv4_addr
		flags interval
		elements = { $TESTBED_NET_OAM, $TESTBED_NET_N2, $TESTBED_NET_N3, $TESTBED_NET_N6,
		             $TESTBED_NET_WAN, $TESTBED_NET_FAR_N2, $TESTBED_NET_FAR_N3, $TESTBED_NET_FAR_OAM,
		             $TESTBED_UE_POOL }
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname \$UPLINK ip saddr @internal masquerade
	}
}
EOF
}

indent() { sed "s/^/$1/"; }

# user_data_for <node> -> the #cloud-config for a node.
#
# cloud-init.user-data is a SINGLE document, so everything a node needs at first
# boot has to be emitted together — setting it twice replaces rather than
# merges, which would silently drop whichever went first. Each node's role is
# finished here (router: forwarding and firewall; wanem: bridge netfilter;
# control node: build toolchain), so a node that has booted is a node that is
# ready, with no second pass to sequence.
user_data_for() {
    local node=$1 packages="tcpdump traceroute" files="" runcmd=""

    case "$node" in
        "$NODE_RSITE")
            packages+=" nftables"
            files+="  - path: /etc/sysctl.d/99-testbed-router.conf
    content: |
      net.ipv4.ip_forward = 1
  - path: /etc/nftables.conf
    permissions: '0755'
    content: |
$(nft_ruleset | indent '      ')
"
            runcmd+="  - sysctl -q --system
  - nft -c -f /etc/nftables.conf && systemctl enable nftables && systemctl restart nftables
" ;;
        "$NODE_RRAN")
            files+="  - path: /etc/sysctl.d/99-testbed-router.conf
    content: |
      net.ipv4.ip_forward = 1
"
            runcmd+="  - sysctl -q --system
" ;;
        "$NODE_WANEM")
            # br_netfilter lets wanemd's iptables physdev/mark filters see the
            # bridged WAN frames. Forwarding policy stays ACCEPT.
            packages+=" iptables iproute2"
            files+="  - path: /etc/modules-load.d/testbed-wanem.conf
    content: |
      br_netfilter
  - path: /etc/sysctl.d/99-testbed-wanem.conf
    content: |
      net.bridge.bridge-nf-call-iptables = 1
      net.bridge.bridge-nf-filter-vlan-tagged = 1
"
            runcmd+="  - modprobe br_netfilter
  - sysctl -q --system
" ;;
        "$NODE_CTRL")
            # aether-ops is built from source on the control node and needs Go
            # and Node to do it; neither is in the cloud image and aether-ops's
            # own installer does not provide them (it installs git/make/ansible
            # during preflight, but not the toolchain).
            packages+=" git make curl ca-certificates build-essential"
            runcmd+="  # Node from NodeSource: the distro's nodejs/npm packages conflict on noble.
  - curl -fsSL https://deb.nodesource.com/setup_${AETHER_OPS_NODE_MAJOR}.x | bash -
  - DEBIAN_FRONTEND=noninteractive apt-get install -y nodejs
  # Go from upstream: noble's golang-go is too old to build aether-ops.
  - curl -fsSL https://go.dev/dl/go${AETHER_OPS_GO_VERSION}.linux-amd64.tar.gz -o /tmp/go.tar.gz
  - rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz && rm -f /tmp/go.tar.gz
  - ln -sf /usr/local/go/bin/go /usr/local/bin/go
  - ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
" ;;
        "$NODE_CORE")
            if [ -n "$CORE_NODE_PACKAGES" ]; then packages+=" $CORE_NODE_PACKAGES"; fi
            if [ "$CORE_UPF_ATTACH" = macvlan ]; then
                # The af_packet UPF drops receive-aggregated frames (sdcore-lab
                # UPF-14), and aggregation happens on the trunk NIC, below the n3/n6
                # VLAN interfaces OnRamp turns GRO off on (ONR-4). Both software GRO
                # and the NIC's own (rx-gro-hw) have to be off, at every boot.
                files+="  - path: /etc/systemd/system/testbed-upf-offloads.service
    content: |
      [Unit]
      Description=Turn off receive aggregation on the UPF's trunk NIC (UPF-14 / ONR-4)
      After=sys-subsystem-net-devices-trunk.device network-pre.target
      Wants=sys-subsystem-net-devices-trunk.device
      Before=network-online.target

      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/usr/sbin/ethtool -K trunk gro off rx-gro-hw off

      [Install]
      WantedBy=multi-user.target
"
                runcmd+="  - systemctl daemon-reload && systemctl enable --now testbed-upf-offloads.service
"
            fi ;;
        "$NODE_RANN"|"$NODE_RANF")
            packages+=" iputils-arping"
            if [ "$node" = "$NODE_RESP" ]; then packages+=" iperf3"; fi ;;
        "$NODE_APP") packages+=" iperf3" ;;
    esac

    echo "#cloud-config"
    echo "packages: [${packages// /, }]"
    [ -n "$files" ]  && { echo "write_files:"; printf '%s' "$files"; }
    [ -n "$runcmd" ] && { echo "runcmd:"; printf '%s' "$runcmd"; }
    if [ -n "${TESTBED_SSH_PUBKEY:-}" ]; then
        [ -f "$TESTBED_SSH_PUBKEY" ] || die "TESTBED_SSH_PUBKEY='$TESTBED_SSH_PUBKEY' not found"
        echo "ssh_authorized_keys:"
        echo "  - $(cat "$TESTBED_SSH_PUBKEY")"
    fi
    return 0
}

# ── instances ────────────────────────────────────────────────────────────────

make_profile() {
    exists_profile "$PROFILE" && return
    info "creating profile $PROFILE"
    lxc_do profile create "$PROFILE" >/dev/null
    lxc_do profile device add "$PROFILE" root disk pool="$TESTBED_STORAGE_POOL" path=/ >/dev/null
}

# attach_nics <node> — add any NIC the node is missing, with its VLAN
# membership. Idempotent; also converges the VLAN list of an existing NIC.
attach_nics() {
    local name=$1 dev net vl
    while read -r dev net vl; do
        if ! lxc config device get "$name" "$dev" network >/dev/null 2>&1; then
            lxc_do config device add "$name" "$dev" nic network="$net" >/dev/null \
                || die "could not attach $dev ($net) to $name"
        fi
        case "$vl" in
            t:*) [ "$(lxc config device get "$name" "$dev" vlan.tagged 2>/dev/null)" = "${vl#t:}" ] \
                     || lxc_do config device set "$name" "$dev" vlan.tagged="${vl#t:}" ;;
            u:*) [ "$(lxc config device get "$name" "$dev" vlan 2>/dev/null)" = "${vl#u:}" ] \
                     || lxc_do config device set "$name" "$dev" vlan="${vl#u:}" ;;
        esac
    done < <(nics_for "$name")
}

make_node() {
    local name=$1 cpu mem disk
    if exists_instance "$name"; then
        info "instance $name exists, leaving it alone"
        return
    fi
    read -r cpu mem disk <<<"$(size_for "$name")"
    info "creating $name (${cpu} vCPU, $mem, $disk)"
    # boot.autostart: this is a persistent testbed, not a scratch one. A core
    # that takes 20 minutes to deploy should not need redeploying because the
    # host rebooted, and a node that comes back without its neighbours is a
    # confusing half-testbed rather than an obvious absence. The site router
    # starts first: it is every other node's path to the Internet.
    local prio=0; [ "$name" = "$NODE_RSITE" ] && prio=10
    lxc_do init "$IMAGE_ALIAS" "$name" --vm \
        --profile "$PROFILE" \
        -c limits.cpu="$cpu" -c limits.memory="$mem" \
        -c boot.autostart=true -c boot.autostart.priority="$prio" \
        -d root,size="$disk" >/dev/null 2>&1

    attach_nics "$name"
    local dev net vl
    while read -r dev net vl; do
        printf '      %-8s %-14s %s\n' "$dev" "$net" "${vl#?:}"
    done < <(nics_for "$name")

    # Set after the NICs exist, so the MACs are known.
    lxc_do config set "$name" cloud-init.network-config="$(netplan_for "$name")"
    lxc_do config set "$name" cloud-init.user-data="$(user_data_for "$name")"
    info "$name will write its netplan on first boot"
}

# wait_agent <node> — until `lxc exec` works (the VM has booted far enough).
wait_agent() {
    local node=$1 i
    for i in $(seq 1 60); do
        lxc exec "$node" -- true >/dev/null 2>&1 && return 0
        sleep 3
    done
    die "$node did not come up (no LXD agent after 180s)"
}

# wait_cloud_init <node> — block until first-boot provisioning has finished.
# Racing it means contending for the dpkg lock and, on the control node,
# building against a toolchain that is only half unpacked — which fails in
# ways that look like a broken build rather than a broken sequence.
wait_cloud_init() {
    local node=$1
    info "waiting for cloud-init to finish on $node"
    lxc exec "$node" -- cloud-init status --wait >/dev/null 2>&1 || true
}

wait_internet() {
    local node=$1 i
    for i in $(seq 1 40); do
        lxc exec "$node" -- ping -c1 -W2 8.8.8.8 >/dev/null 2>&1 && return 0
        sleep 3
    done
    return 1
}

cmd_up() {
    require_lxd
    cmd_networks
    make_profile
    local n
    for n in "${NODES[@]}"; do make_node "$n"; done

    if [ "$WITH_ROUTER" = true ]; then
        # The site router first: every other node's packages, toolchains and
        # OnRamp downloads go through its NAT, so a node that boots before it
        # simply fails its first-boot provisioning.
        if ! is_running "$NODE_RSITE"; then
            info "starting $NODE_RSITE (the Internet edge; the rest follow once it routes)"
            lxc_do start "$NODE_RSITE"
        fi
        wait_agent "$NODE_RSITE"
        wait_cloud_init "$NODE_RSITE"
        wait_internet "$NODE_RSITE" || die "$NODE_RSITE has no Internet via $TESTBED_UPLINK_NET"
        lxc exec "$NODE_RSITE" -- nft list ruleset 2>/dev/null | grep -q masquerade \
            || die "$NODE_RSITE has no NAT ruleset — check 'lxc exec $NODE_RSITE -- cloud-init status --long'"
    fi

    for n in "${NODES[@]}"; do
        is_running "$n" && continue
        info "starting $n"
        lxc_do start "$n"
    done

    info "waiting for nodes to report their management address (up to 180s)"
    local deadline=$((SECONDS + 180)) ready=0
    while [ $SECONDS -lt $deadline ]; do
        ready=0
        for n in "${NODES[@]}"; do
            has_mgmt_addr "$n" && ready=$((ready + 1))
        done
        [ "$ready" -eq ${#NODES[@]} ] && break
        sleep 5
    done
    [ "$ready" -eq ${#NODES[@]} ] || warn "not every node reported its address; check '$PROG status' and the consoles"
    if [ "$WITH_ROUTER" != true ]; then
        for n in "${NODES[@]}"; do pin_uplink "$n"; done
    fi

    # The control node's toolchain download is the slow one and `ctrl` waits
    # for it itself; everything else is a package install.
    for n in "${NODES[@]}"; do
        [ "$n" = "$NODE_CTRL" ] && continue
        wait_cloud_init "$n"
    done
    echo
    cmd_status
}

# uplink_addr <node> -> the address DHCP gave the node's uplink NIC, or nothing
# while the node is down or has no lease yet.
uplink_addr() {
    lxc exec "$1" -- sh -c "ip -4 -o addr show dev uplink scope global 2>/dev/null | awk '{print \$4}' | cut -d/ -f1 | head -1" 2>/dev/null | tr -d '\r' || true
}

# pin_uplink <node> makes the address DHCP gave the node's uplink its static
# lease, so it is the same after every restart: the core's Kubernetes node
# address, the ORBIT server's listen address and its pinned certificate all
# name it. This sets a key on the node's own NIC device, nothing on the uplink
# network.
pin_uplink() {
    local a; a=$(uplink_addr "$1")
    [ -n "$a" ] || { warn "$1 has no uplink address to pin"; return 0; }
    [ "$(lxc config device get "$1" uplink ipv4.address 2>/dev/null)" = "$a" ] && return 0
    lxc_do config device set "$1" uplink ipv4.address="$a" >/dev/null 2>&1 \
        || warn "could not pin $1's uplink address $a; it may change when the node restarts"
}

# has_mgmt_addr <node> — whether the node is up far enough to hold its
# management address.
has_mgmt_addr() {
    if [ "$WITH_ROUTER" = true ]; then
        lxc list "^$1\$" -c 4 --format csv 2>/dev/null | grep -qF "$(oam_of "$1")"
    else
        [ -n "$(oam_of "$1")" ]
    fi
}

# oam_of <node> -> the node's management address (far-site nodes are on the
# far OAM VLAN; the routers answer on their first segment). Without the site
# router it is the uplink's DHCP address, read from the running node, and
# empty when there is none to read.
oam_of() {
    if [ "$WITH_ROUTER" != true ]; then uplink_addr "$1"; return 0; fi
    case "$1" in
        "$NODE_RSITE") echo "$GW_OAM" ;;
        "$NODE_WANEM") echo "$WANEM_OAM" ;;
        "$NODE_RRAN")  echo "$GW_FOAM" ;;
        "$NODE_CTRL")  echo "$CTRL_OAM" ;;
        "$NODE_CORE")  echo "$CORE_OAM" ;;
        "$NODE_RANN")  echo "$RANN_OAM" ;;
        "$NODE_RANF")  echo "$RANF_OAM" ;;
        "$NODE_APP")   echo "$APP_OAM" ;;
    esac
}

# expect_for <node> -> "interface count" lines: how many IPv4 addresses each
# VLAN interface must carry once netplan has applied. The core's n3/n6 are
# deliberately 0 when they are the UPF's macvlan parents, and carry the UPF's
# addresses when the UPF runs on the node itself.
expect_for() {
    {
        expect_vlans "$1"
        if [ "$1" = "$NODE_RANN" ] && [ "$WITH_APP" != true ]; then printf 'n6 1\n'; fi
    } | if [ "$WITH_ROUTER" = true ]; then cat; else grep -v '^oam ' || true; fi
}

expect_vlans() {
    local g=$TESTBED_RAN_GNBS
    case "$1" in
        "$NODE_RSITE") printf 'oam 1\nn2 1\nn3 1\nn6 1\nwan 1\n' ;;
        "$NODE_WANEM"|"$NODE_CTRL") printf 'oam 1\n' ;;
        "$NODE_RRAN")  printf 'wan 1\noam 1\nn2 1\nn3 1\n' ;;
        "$NODE_CORE")  if [ "$CORE_UPF_ATTACH" = host ]; then printf 'oam 1\nn2 1\nn3 1\nn6 1\n'
                       else printf 'oam 1\nn2 1\nn3 0\nn6 0\n'; fi ;;
        "$NODE_RANN"|"$NODE_RANF") printf 'oam 1\nn2 %s\nn3 %s\n' "$g" "$g" ;;
        "$NODE_APP")   printf 'oam 1\nn6 1\n' ;;
    esac
}

# cmd_addrs re-applies addressing to RUNNING nodes, so a changed gNB count does
# not mean rebuilding them. It also re-attaches a NIC that has gone missing:
# deleting an LXD network requires first removing every instance device that
# references it, so a node whose network was taken away comes back with the
# device gone too, and starts perfectly happily with one plane simply absent.
#
# The netplan cloud-init wrote at first boot is regenerated from the current
# configuration and replaced in place, then applied — so the change survives a
# reboot instead of living only in the kernel's address list.
cmd_addrs() {
    require_lxd
    local nodes=("$@")
    if [ ${#nodes[@]} -eq 0 ]; then
        nodes=("$NODE_RANN")
        if [ "$WITH_FAR" = true ]; then nodes+=("$NODE_RANF"); fi
    fi
    local target
    for target in "${nodes[@]}"; do
        case "$target" in "$P-"*) : ;; *) target=$(node_for "$target") ;; esac
        exists_instance "$target" || die "$target not found — run '$PROG up' first"
        addrs_one "$target"
    done
}

addrs_one() {
    local name=$1
    info "re-applying addressing to $name"
    attach_nics "$name"

    local cfg; cfg=$(netplan_for "$name")
    # Keep the instance config in step, or a rebuilt node reverts.
    lxc_do config set "$name" cloud-init.network-config="$cfg" >/dev/null

    # cloud-init's network-config is the INNER fragment (version + ethernets);
    # a file under /etc/netplan needs it wrapped in `network:` and indented.
    # Writing the fragment straight out yields "unknown key 'version'".
    printf '%s\n' "$cfg" | sed 's/^/  /' | (echo "network:"; cat) \
        | lxc_do exec "$name" -- tee /etc/netplan/50-cloud-init.yaml >/dev/null
    lxc_do exec "$name" -- chmod 600 /etc/netplan/50-cloud-init.yaml >/dev/null 2>&1 || true

    local out
    if ! out=$(lxc_do exec "$name" -- netplan apply 2>&1); then
        die "netplan apply failed on $name: $out"
    fi

    # netplan can exit 0 having rejected the file, so verify the OUTCOME: count
    # the addresses that actually landed rather than trusting the exit code.
    local iface want got
    while read -r iface want; do
        got=$(lxc_do exec "$name" -- sh -c "ip -4 -o addr show dev $iface 2>/dev/null | grep -c inet" 2>/dev/null | tr -d '\r') || got=0
        [ "${got:-0}" = "$want" ] \
            || die "$name $iface has ${got:-0} address(es), expected $want — netplan reported success but did not apply it${out:+ ($out)}"
        printf '      %-5s %s address(es)\n' "$iface" "$got"
    done < <(expect_for "$name")
    info "addressing verified on $name"
}

# ── core ─────────────────────────────────────────────────────────────────────

# check_ue_pool_fits refuses a subscriber count the UE address pool cannot hold.
#
# Raising TESTBED_SUB_COUNT and leaving TESTBED_UE_POOL behind produces a core
# that looks healthy and fails confusingly: subscribers authenticate and
# register, then PDU session establishment fails once the pool runs dry, and the
# SMF reports an empty "pdu session create:" error having just released the
# address it allocated. Catching it here costs nothing; diagnosing it live cost
# an afternoon.
check_ue_pool_fits() {
    local bits usable
    bits=${TESTBED_UE_POOL##*/}
    case "$bits" in
        ''|*[!0-9]*) die "TESTBED_UE_POOL ($TESTBED_UE_POOL) has no valid prefix length" ;;
    esac
    # Network and broadcast are not assignable.
    usable=$(( (1 << (32 - bits)) - 2 ))
    if [ "$usable" -lt "$TESTBED_SUB_COUNT" ]; then
        die "TESTBED_UE_POOL ($TESTBED_UE_POOL) holds $usable addresses but" \
            "TESTBED_SUB_COUNT is $TESTBED_SUB_COUNT — widen the pool, or those" \
            "UEs will register and then fail PDU session establishment"
    fi
}

# The core commands. Each does what is common to every core kind and hands
# the rest to that kind's function (see "core kinds" at the top).
cmd_core() {
    require_lxd
    exists_instance "$NODE_CORE" || die "$NODE_CORE not found — run '$PROG up' first"
    check_ue_pool_fits
    mkdir -p "$TESTBED_CACHE_DIR"

    core_call deploy
    record_versions

    cmd_core_status
}

# record_versions writes what every core component resolved to, beside the
# core (/etc/orbit/testbed-versions) and into the log: with the core following
# main, these SHAs are the only record of what a run measured.
record_versions() {
    local f="$TESTBED_CACHE_DIR/versions.$NODE_CORE"
    core_call versions > "$f"
    push_file "$NODE_CORE" "$f" /etc/orbit/testbed-versions
    info "core components (/etc/orbit/testbed-versions on $NODE_CORE):"
    sed 's/^/    /' "$f" >&2
    rm -f "$f"
}

cmd_reprovision() {
    require_lxd
    core_call reprovision
}

cmd_core_status() {
    require_lxd
    core_call status
}

# ── SD-Core from main ────────────────────────────────────────────────────────
# The NF images are built here, on the host, and loaded into the core node's
# containerd: the core node needs no registry, the build reuses this host's
# docker cache across rebuilds, and an image built at the same commit is not
# built again.

SDCORE_IMAGES_LIST="$TESTBED_CACHE_DIR/sdcore-images"   # "<image name> <full ref>" per line
SDCORE_VERSIONS="$TESTBED_CACHE_DIR/sdcore-versions"    # what each component resolved to
SDCORE_BUILD_LOG="$TESTBED_CACHE_DIR/sdcore-build.log"

# sdcore_nf_ref <nf> -> the git ref to build it at: SDCORE_REF_<NF>, else SDCORE_NF_REF.
sdcore_nf_ref() {
    local v
    v="SDCORE_REF_$(echo "$1" | tr '[:lower:]-' '[:upper:]_')"
    echo "${!v:-$SDCORE_NF_REF}"
}

# sdcore_nf_images <nf> -> the image names its `make docker-build` produces,
# without registry or tag. The chart's tags name images the same way.
sdcore_nf_images() {
    case "$1" in
        upf)        echo "upf-bess upf-pfcp" ;;
        webconsole) echo "5gc-webui" ;;
        metricfunc|simapp) echo "$1" ;;
        *)          echo "5gc-$1" ;;
    esac
}

# build_sdcore_images builds every NF in SDCORE_NFS at its ref and writes
# SDCORE_IMAGES_LIST. Each repository's own docker-build target is used, so
# the image is the one upstream CI would publish from that commit.
build_sdcore_images() {
    local src="$TESTBED_CACHE_DIR/sdcore-src" nf ref sha tag img all have
    command -v docker >/dev/null 2>&1 \
        || die "docker not found on this host; it builds the SD-Core images (set SDCORE_NF_REF= to run the charts' released images instead)"
    mkdir -p "$src"
    : > "$SDCORE_IMAGES_LIST"
    : > "$SDCORE_BUILD_LOG"
    for nf in $SDCORE_NFS; do
        ref=$(sdcore_nf_ref "$nf")
        if [ ! -d "$src/$nf/.git" ]; then
            git clone -q "$SDCORE_NF_ORG/$nf.git" "$src/$nf" || die "could not clone $SDCORE_NF_ORG/$nf"
        fi
        git -C "$src/$nf" fetch -q --tags --force origin || die "could not fetch $nf"
        git -C "$src/$nf" checkout -q --detach "origin/$ref" 2>/dev/null \
            || git -C "$src/$nf" checkout -q --detach "$ref" 2>/dev/null \
            || die "no such $nf ref: $ref"
        git -C "$src/$nf" clean -qfdx
        sha=$(git -C "$src/$nf" rev-parse HEAD)
        tag="tb-${sha:0:12}"
        all=$(sdcore_nf_images "$nf")
        have=1
        for img in $all; do docker image inspect "$SDCORE_IMAGE_PREFIX$img:$tag" >/dev/null 2>&1 || have=0; done
        if [ $have = 1 ]; then
            info "SD-Core $nf at $ref (${sha:0:12}): already built"
        else
            info "building SD-Core $nf at $ref (${sha:0:12})"
            # The Makefiles split SDCORE_IMAGE_PREFIX as registry + repository;
            # only their concatenation matters.
            ( cd "$src/$nf" && make docker-build \
                DOCKER_REGISTRY="$SDCORE_IMAGE_PREFIX" DOCKER_REPOSITORY= DOCKER_TAG="$tag" \
                CPU="$SDCORE_UPF_CPU" ) >>"$SDCORE_BUILD_LOG" 2>&1 \
                || { tail -n 30 "$SDCORE_BUILD_LOG" >&2; die "building $nf failed; the full log is $SDCORE_BUILD_LOG"; }
        fi
        for img in $all; do
            docker image inspect "$SDCORE_IMAGE_PREFIX$img:$tag" >/dev/null 2>&1 \
                || die "$nf built, but not as $SDCORE_IMAGE_PREFIX$img:$tag — its Makefile's image naming changed"
            echo "$img $SDCORE_IMAGE_PREFIX$img:$tag" >> "$SDCORE_IMAGES_LIST"
        done
        echo "nf/$nf $ref $sha" >> "$SDCORE_VERSIONS"
    done
}

# load_sdcore_images copies the built images into the core node's containerd,
# where the kubelet finds them (the charts pull IfNotPresent). Needs the
# cluster, so it runs after k8s-install.
load_sdcore_images() {
    local tar="$TESTBED_CACHE_DIR/sdcore-images.tar" refs
    refs=$(awk '{print $2}' "$SDCORE_IMAGES_LIST" | tr '\n' ' ')
    info "loading $(wc -l < "$SDCORE_IMAGES_LIST") SD-Core images into $NODE_CORE's containerd"
    # shellcheck disable=SC2086 # one word per image ref
    docker save -o "$tar" $refs || die "docker save failed"
    push_file "$NODE_CORE" "$tar" /var/tmp/sdcore-images.tar 600
    rm -f "$tar"
    on_core "/var/lib/rancher/rke2/bin/ctr --address /run/k3s/containerd/containerd.sock -n k8s.io images import /var/tmp/sdcore-images.tar >/dev/null && rm -f /var/tmp/sdcore-images.tar" \
        || die "could not import the SD-Core images on $NODE_CORE"
}

# sdcore_image_overrides <values template> <chart values root> rewrites the
# image tags in the OnRamp values template so every image built here is the
# one deployed, and every other image keeps the chart's own reference.
#
# The charts compose an image as images.repository + images.tags.<x>, one
# repository per chart. Built images live under a different repository, so
# the override sets repository to "" and writes every tag of that chart as a
# full reference. An image built here that no chart names is an error: it
# means the build list and the charts disagree, and a run would silently use
# the released image.
sdcore_image_overrides() {
    local vf=$1 root=$2
    push_file "$NODE_CORE" "$SDCORE_IMAGES_LIST" /tmp/sdcore-images.txt
    on_core "python3 - '$vf' '$root' <<'PY'
import sys, yaml
vf, root = sys.argv[1], sys.argv[2]
built = dict(l.split() for l in open('/tmp/sdcore-images.txt') if l.strip())
used = set()
# OnRamp values key -> the chart directory holding that subchart's defaults.
charts = {'5g-control-plane': '5g-control-plane', 'omec-sub-provision': 'omec-sub-provision', 'omec-user-plane': 'bess-upf'}
lines = open(vf).read().split(chr(10))
for key, chart in charts.items():
    images = (yaml.safe_load(open('%s/%s/values.yaml' % (root, chart))) or {}).get('images') or {}
    repo, tags = images.get('repository') or '', images.get('tags') or {}
    out = {}
    for k, v in tags.items():
        name = str(v).split(':')[0]
        if name in built:
            out[k] = built[name]; used.add(name)
        else:
            out[k] = repo + str(v)
    if not any(n in built for n in (str(v).split(':')[0] for v in tags.values())):
        continue
    try:
        at = lines.index(key + ':')
    except ValueError:
        sys.exit('values template: no top-level %s: -- OnRamp layout changed' % key)
    end = next((i for i in range(at + 1, len(lines)) if lines[i] and not lines[i][0].isspace() and not lines[i].startswith('#')), len(lines))
    if any(l.startswith('  images:') for l in lines[at + 1:end]):
        sys.exit('values template: %s already sets images -- refusing to override it' % key)
    block = ['  images:', '    repository: \"\"', '    tags:'] + ['      %s: \"%s\"' % (k, v) for k, v in out.items()]
    lines[at + 1:at + 1] = block
unused = sorted(set(built) - used)
if unused:
    sys.exit('built images no chart names: %s' % ' '.join(unused))
open(vf, 'w').write(chr(10).join(lines))
PY" || die "could not point the charts at the images built from $SDCORE_NF_REF"
}

core_sdcore_deploy() {
    # Built first: a failed build costs nothing on the core, and the images are
    # ready to load as soon as the cluster is.
    : > "$SDCORE_VERSIONS"
    if [ -n "$SDCORE_NF_REF" ]; then
        build_sdcore_images
    else
        info "SDCORE_NF_REF is empty: running the images the charts name"
    fi

    info "installing OnRamp prerequisites on $NODE_CORE"
    on_core "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git make ansible >/dev/null" \
        || die "could not install prerequisites (does $NODE_CORE reach the Internet? try '$PROG check')"

    # OnRamp expects helm on PATH but does not install it — its k8s role only
    # pins a version. The Aether reference host carries a get_helm.sh for the
    # same reason.
    if ! on_core "command -v helm >/dev/null"; then
        info "installing helm (OnRamp requires it but does not provide it)"
        on_core "curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null 2>&1" \
            || die "could not install helm"
    fi

    info "fetching OnRamp at $ONRAMP_VERSION"
    on_core "rm -rf $TESTBED_ONRAMP_DIR && git clone -q $ONRAMP_REPO $TESTBED_ONRAMP_DIR" \
        || die "could not clone OnRamp from $ONRAMP_REPO"
    on_core "cd $TESTBED_ONRAMP_DIR && git checkout -q $ONRAMP_VERSION" || die "no such OnRamp ref: $ONRAMP_VERSION"
    echo "onramp $ONRAMP_VERSION $(on_core "git -C $TESTBED_ONRAMP_DIR rev-parse HEAD" | tr -d '\r')" >> "$SDCORE_VERSIONS"

    # Which values template this OnRamp ships is not stable: the fork renamed
    # radio-5g-values.yaml to sdcore-5g-values.yaml when it merged upstream,
    # and every reference below would otherwise have to be edited together.
    # Detect it once, fail here with a clear message if neither is present,
    # rather than part-way through a patch with a bare FileNotFoundError.
    local vf vf_dir="deps/5gc/roles/core/templates"
    vf=$(on_core "cd $TESTBED_ONRAMP_DIR && for f in sdcore-5g-values.yaml radio-5g-values.yaml; do [ -f $vf_dir/\$f ] && { echo \$f; break; }; done" 2>/dev/null | tr -d '\r')
    [ -n "$vf" ] \
        || die "no known values template in $TESTBED_ONRAMP_DIR/$vf_dir (looked for sdcore-5g-values.yaml, radio-5g-values.yaml) — OnRamp layout changed"
    info "values template: $vf"

    # The charts: a published version when SDCORE_CHART_VERSION is set, else
    # sdcore-helm-charts at SDCORE_CHARTS_REF, installed as local charts.
    # Either way the subcharts' own values.yaml end up under $chart_root, which
    # is where the image overrides read each chart's image references from.
    local helm_block chart_root
    if [ -n "$SDCORE_CHART_VERSION" ]; then
        info "SD-Core chart: published $SDCORE_CHART_VERSION"
        chart_root=/opt/sd-core-chart/sd-core/charts
        on_core "rm -rf /opt/sd-core-chart && helm pull oci://ghcr.io/omec-project/sd-core --version $SDCORE_CHART_VERSION --untar --untardir /opt/sd-core-chart >/dev/null && cd $chart_root && for t in *.tgz; do [ ! -e \"\$t\" ] || tar xzf \"\$t\" || exit 1; done" \
            || die "could not fetch SD-Core chart $SDCORE_CHART_VERSION"
        helm_block="    local_charts: false
    chart_ref: oci://ghcr.io/omec-project/sd-core
    chart_version: $SDCORE_CHART_VERSION"
        echo "charts $SDCORE_CHART_VERSION oci://ghcr.io/omec-project/sd-core" >> "$SDCORE_VERSIONS"
    else
        info "SD-Core charts: $SDCORE_CHARTS_REPO at $SDCORE_CHARTS_REF"
        chart_root=/opt/sdcore-helm-charts
        on_core "rm -rf $chart_root && git clone -q $SDCORE_CHARTS_REPO $chart_root && cd $chart_root && git checkout -q $SDCORE_CHARTS_REF" \
            || die "could not fetch sdcore-helm-charts at $SDCORE_CHARTS_REF"
        # OnRamp archives the parent of chart_ref and installs the last path
        # element, so the umbrella's file:// siblings come along.
        helm_block="    local_charts: true
    chart_ref: $chart_root/sdcore-helm-charts"
        echo "charts $SDCORE_CHARTS_REF $(on_core "git -C $chart_root rev-parse HEAD" | tr -d '\r')" >> "$SDCORE_VERSIONS"
    fi

    # Ansible runs on the node against itself, so there are no SSH keys to manage.
    info "writing inventory and vars"
    on_core "printf '%s\n' '[all]' 'node1 ansible_host=127.0.0.1 ansible_connection=local ansible_user=root' '' '[master_nodes]' 'node1' '' '[worker_nodes]' '' '[gnbsim_nodes]' '' '[router_nodes]' > $TESTBED_ONRAMP_DIR/hosts.ini"

    # OnRamp describes the UPF as two sides, each attached to a network
    # (core.networks / core.upf.instances, aether-onramp#263). This is the Ref 2
    # site as configuration: each side is a macvlan child of the core node's
    # VLAN interface, and the gateway on each segment is the site router — so
    # the router role builds no overlay and installs no NAT, and the UPF's N3
    # and N6 are different VLANs by construction. ran_subnet is the far site's
    # N3: the UPF routes downlink to those gNBs via the N3 gateway, while the
    # near site's gNBs are on-link. A shape without the router or the far site
    # has other next hops: see UPF_N3_GW, UPF_RAN_SUBNET and UPF_N6_GW.
    # The OnRamp checkout has to use the layout the node addressing was built
    # for (TESTBED_ONRAMP_NET): the current one names core.networks, the
    # legacy one core.data_iface.
    local has_networks
    has_networks=$(on_core "sed -n '/^core:/,/^[a-z]/p' $TESTBED_ONRAMP_DIR/vars/main.yml | grep -c '^  networks:' || true" 2>/dev/null | tr -d '\r') || has_networks=0
    if [ "$LEGACY_NET" = true ] && [ "${has_networks:-0}" != 0 ]; then
        die "OnRamp $ONRAMP_VERSION uses the current UPF networking layout (core.networks); rebuild with TESTBED_ONRAMP_NET=current"
    fi
    if [ "$LEGACY_NET" != true ] && [ "${has_networks:-0}" = 0 ]; then
        die "OnRamp $ONRAMP_VERSION uses the legacy UPF networking layout (core.data_iface); rebuild with TESTBED_ONRAMP_NET=legacy (minimal shape)"
    fi
    if [ "$LEGACY_NET" = true ]; then
        # The legacy layout, as OnRamp v2.3.0 ships it: the router role puts
        # the access and core macvlans on data_iface (n3) at .1 of each
        # internal subnet, routes the UE pool to the UPF's core address and
        # masquerades the pool out of data_iface; the UPF routes ran_subnet
        # via the access gateway.
        on_core "cat > /tmp/onramp-core.yml <<'YML'
core:
  standalone: true
  data_iface: n3
  values_file: \"$vf_dir/$vf\"
  ran_subnet: \"$TESTBED_NET_N3\"
  helm:
$helm_block
  upf:
    access_subnet: \"$LEGACY_GW_ACCESS/${TESTBED_LEGACY_ACCESS_NET#*/}\"
    core_subnet: \"$LEGACY_GW_CORE/${TESTBED_LEGACY_CORE_NET#*/}\"
    mode: af_packet
    multihop_gnb: false
    default_upf:
      ip:
        access: \"$UPF_N3\"
        core:   \"$UPF_N6\"
      ue_ip_pool: \"$TESTBED_UE_POOL\"
  amf:
    ip: \"$CORE_N2\"
YML
" || die "could not write the legacy core: block"
    else
    on_core "cat > /tmp/onramp-core.yml <<'YML'
core:
  values_file: \"$vf_dir/$vf\"
  networks:
    access:
      iface: n3
      subnet: $TESTBED_NET_N3
      gateway: $UPF_N3_GW
      attach: macvlan
      ran_subnet: $UPF_RAN_SUBNET
    core:
      iface: n6
      subnet: $TESTBED_NET_N6
      gateway: $UPF_N6_GW
      attach: macvlan
  helm:
$helm_block
  upf:
    mode: af_packet
    instances:
      default:
        access: { ip: $UPF_N3 }
        core:   { ip: $UPF_N6 }
        ue_ip_pool: \"$TESTBED_UE_POOL\"
  amf:
    ip: \"$CORE_N2\"
YML
" || die "could not write the core: block"
    fi
    on_core "cd $TESTBED_ONRAMP_DIR && python3 - <<'PY'
import sys
p='vars/main.yml'; s=open(p).read(); block=open('/tmp/onramp-core.yml').read()
# Replace the whole core: block. Anchor on the top-level keys so a changed
# upstream layout fails here rather than deploying half a configuration.
a=s.find(chr(10)+'core:'+chr(10)); b=s.find(chr(10)+'gnbsim:'+chr(10))
if a<0 or b<0 or b<a:
    sys.exit('vars/main.yml: could not find the core: block -- OnRamp layout changed')
open(p,'w').write(s[:a+1]+block+s[b:])
PY" || die "could not write the UPF networking configuration"

    info "provisioning $TESTBED_SUB_COUNT test subscribers from $TESTBED_SUB_START"
    on_core "cd $TESTBED_ONRAMP_DIR && python3 -c \"
import re
p='$vf_dir/$vf'; s=open(p).read()
start=int('$TESTBED_SUB_START'); end=start+$TESTBED_SUB_COUNT-1
w=len('$TESTBED_SUB_START')
s=re.sub(r'ueId-start: \\\"[0-9]+\\\"', 'ueId-start: \\\"%0*d\\\"' % (w,start), s)
s=re.sub(r'ueId-end: \\\"[0-9]+\\\"',   'ueId-end: \\\"%0*d\\\"'   % (w,end),   s)
# The device-group carries an explicit IMSI list, separate from the ueId
# range. The range alone provisions authentication and AM data, but policy
# and SM data come from this list — so a UE outside it authenticates and
# then fails registration. Both have to grow together.
lines=s.split(chr(10)); out=[]; i=0
while i < len(lines):
    if lines[i].strip() == 'imsis:':
        indent = lines[i][:len(lines[i])-len(lines[i].lstrip())] + '  '
        out.append(lines[i]); i += 1
        while i < len(lines) and lines[i].strip().startswith('- \\\"0'):
            i += 1
        for n in range(start, end+1):
            out.append('%s- \\\"%0*d\\\"' % (indent, w, n))
        continue
    out.append(lines[i]); i += 1
s=chr(10).join(out)
open(p,'w').write(s)
\""

    # GTP-U End Markers on a path switch, as Ref 2 runs it. Two keys deep-merged
    # into the UPF's upf.jsonc: enable_end_marker gates the construction, and
    # the socket must be on the volume bessd and pfcp-agent share (/pod-share);
    # the default /tmp/pfcpport is private to each container, so pfcp-agent
    # would log 'dial error:' and skip the end-marker loop.
    if [ "$TESTBED_UPF_END_MARKER" = true ]; then
        info "enabling UPF End Markers (TESTBED_UPF_END_MARKER)"
        on_core "cd $TESTBED_ONRAMP_DIR && python3 - <<'PY'
import sys
p='$vf_dir/$vf'; lines=open(p).read().split(chr(10)); out=[]; done=False
for l in lines:
    out.append(l)
    if not done and l.strip() == 'upf.jsonc:':
        ind = l[:len(l)-len(l.lstrip())] + '  '
        out.append(ind + 'enable_end_marker: true')
        out.append(ind + 'endmarker_sockaddr: \"/pod-share/pfcpport\"')
        done = True
if not done: sys.exit('values template: no upf.jsonc key -- OnRamp layout changed')
open(p,'w').write(chr(10).join(out))
PY" || die "could not enable End Markers in the values template"
    fi

    if [ -n "$SDCORE_NF_REF" ]; then
        info "pointing the charts at the images built from $SDCORE_NF_REF"
        sdcore_image_overrides "$TESTBED_ONRAMP_DIR/$vf_dir/$vf" "$chart_root"
    fi

    info "UPF networking as configured:"
    # Everything a wrong deployment turns on, printed before the 15 minutes
    # are spent: which VLAN interface carries which plane, the gateways, the
    # UPF's own addresses and the UE pool. OnRamp prints what it resolved from
    # this ("show the resolved UPF networking") as the install starts.
    on_core "cd $TESTBED_ONRAMP_DIR && sed -n '/^  \(networks\|data_iface\|ran_subnet\|upf\):/,/^  amf:/p' vars/main.yml | sed 's/^/    /'"

    info "installing Kubernetes (slow)"
    on_core "cd $TESTBED_ONRAMP_DIR && make k8s-install" || die "k8s install failed"
    [ -z "$SDCORE_NF_REF" ] || load_sdcore_images

    # OnRamp's install task does not upgrade an existing release — a re-run
    # leaves helm at the same revision and silently keeps the old values, so a
    # changed subscriber range or interface mapping never takes effect. Remove
    # the release first so this command is genuinely repeatable.
    if on_core "helm -n aether-5gc status sd-core >/dev/null 2>&1"; then
        info "existing SD-Core release found; removing it so new values apply"
        on_core "cd $TESTBED_ONRAMP_DIR && make 5gc-core-uninstall" || die "SD-Core uninstall failed"
    fi

    # The documented path. With both gateways external the router role has no
    # overlay to build and only prepares the host (sysctls, offloads); the
    # resolved networking it prints is the first thing to check if the UPF
    # does not come up.
    info "installing SD-Core (5gc-install: router + core)"
    on_core "cd $TESTBED_ONRAMP_DIR && make 5gc-install" || die "SD-Core install failed"

    check_mongo_cpu
    report_subscribers
}

# core_sdcore_versions prints what OnRamp, the charts and each NF resolved to
# in the last deploy.
core_sdcore_versions() { cat "$SDCORE_VERSIONS"; }

# check_mongo_cpu warns if MongoDB is capped low enough to cap the attach rate.
#
# This used to PATCH the limit. The Bitnami chart shipped `cpu: 750m` while no
# other NF had a limit at all, and a registration costs ~55 MongoDB operations,
# so mongo saturated its quota and was CFS-throttled long before anything else
# was busy — measured at 707m of a 750m limit during an attach storm, with
# throughput flat and latency climbing. Lifting it was worth roughly 2x.
#
# The chart has since fixed this. Measured 2026-09-30 on sd-core as deployed by
# aether-ops v1.0.3, mongodb ships `limits: cpu 4, memory 2Gi` with
# `requests: cpu 2, memory 1Gi` — what the patch was setting anyway. So the
# patch is gone, and with it the reason the install had to reprovision: patching
# recreated the mongo pod, whose datadir is an emptyDir, which wiped every
# subscriber and the NRF's NF registry and forced a repopulate afterwards.
# Removing the patch removes that whole sequence.
#
# The check stays, because the old default is only one chart version away and
# its symptom is a low attach ceiling that reads as a property of the core under
# test — the wrong conclusion to draw from a benchmark, and invisible without
# being told to look.
check_mongo_cpu() {
    local lim
    lim=$(on_core "kubectl -n aether-5gc get sts mongodb -o jsonpath='{.spec.template.spec.containers[0].resources.limits.cpu}'" 2>/dev/null | tr -d '\r') || lim=""
    case "$lim" in
        '')  warn "could not read MongoDB's CPU limit; if the attach rate looks low, check it" ;;
        *m)  warn "MongoDB CPU limit is $lim — a registration costs ~55 mongo operations, so this throttles the attach rate and understates the core's ceiling. Raise it in the chart rather than by patching a live core (patching recreates the pod, and its datadir is an emptyDir)." ;;
        *)   if [ "${lim%%.*}" -lt "$TESTBED_MONGO_CPU" ] 2>/dev/null; then
                 warn "MongoDB CPU limit is $lim, below the $TESTBED_MONGO_CPU this testbed expects — the attach ceiling may be mongo's, not the core's"
             else
                 info "MongoDB CPU limit: $lim (chart default is adequate; no patch needed)"
             fi ;;
    esac
}

# core_sdcore_reprovision repopulates a core whose MongoDB has been emptied.
#
# Mongo's datadir is an emptyDir, which is destroyed when the POD is recreated —
# patching the StatefulSet, deleting the pod, or reinstalling the release. It
# survives a core-node reboot, because the pod object and its directory outlive
# the restart; that was verified rather than assumed. The core then looks
# healthy while every attach fails, because two separate things are gone: the
# subscriber population, and the NRF's registry of which NFs exist.
#
# Cheaper than `testbed.sh core`, which reinstalls the whole helm release.
core_sdcore_reprovision() {
    info "reprovisioning subscribers into MongoDB"
    on_core "kubectl -n aether-5gc rollout restart deploy/simapp" >/dev/null 2>&1 || true
    on_core "kubectl -n aether-5gc rollout status deploy/simapp --timeout=300s" >/dev/null 2>&1 || true

    # Every NF registers with the NRF at startup, and that registry lives in the
    # same emptyDir. After a mongo restart it is empty and discovery fails for
    # every NF pair, so they all have to re-register. The NRF goes first: the
    # others register against it as they come up.
    info "restarting network functions so they re-register with the NRF"
    on_core "kubectl -n aether-5gc rollout restart deploy/nrf" >/dev/null 2>&1 || true
    on_core "kubectl -n aether-5gc rollout status deploy/nrf --timeout=300s" >/dev/null 2>&1 || true
    on_core "for d in amf ausf udm udr pcf nssf smf; do kubectl -n aether-5gc rollout restart deploy/\$d; done" >/dev/null 2>&1 || true
    on_core "for d in amf ausf udm udr pcf nssf smf; do kubectl -n aether-5gc rollout status deploy/\$d --timeout=300s; done" >/dev/null 2>&1 || true

    report_subscribers
}

# report_subscribers states how much of the population actually landed.
#
# A REPORT, not a gate — it must not be able to fail a run. `set -o pipefail` is
# on, so a non-zero from the kubectl exec propagates through the pipe and, under
# `set -e`, would abort the script: a whole successful core install reported as
# failed because a status query was a few seconds early. Observed exactly that.
report_subscribers() {
    # Immediately after a restart the pod is Running before mongod accepts
    # connections, and the count below would report earliness as failure.
    on_core "kubectl -n aether-5gc wait --for=condition=ready pod/mongodb-1 --timeout=300s" >/dev/null 2>&1 || true

    local subs=""
    subs=$(on_core "kubectl -n aether-5gc exec mongodb-1 -c mongodb -- mongosh --quiet --eval 'print(db.getSiblingDB(\"authentication\")[\"subscriptionData.authenticationData.authenticationSubscription\"].countDocuments({}))'" 2>/dev/null | tr -d '\r') || subs=""
    # A MongoDB that requires authentication refuses that query. The webconsole
    # lists the same subscribers on its configuration API.
    case "$subs" in
        ''|*[!0-9]*) subs=$(on_core "curl -s --max-time 10 http://\$(kubectl -n aether-5gc get svc webui -o jsonpath='{.spec.clusterIP}'):5000/api/subscriber | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'" 2>/dev/null | tr -d '\r') || subs="" ;;
    esac

    case "$subs" in
        ''|*[!0-9]*) warn "could not read the subscriber count from MongoDB; check '$PROG core-status'" ;;
        "$TESTBED_SUB_COUNT") info "subscribers provisioned: $subs" ;;
        *) warn "subscribers provisioned: $subs, expected $TESTBED_SUB_COUNT — a short population fails PDU session establishment, not registration" ;;
    esac
}

core_sdcore_status() {
    echo "core pods"
    lxc exec "$NODE_CORE" -- bash -lc 'kubectl -n aether-5gc get pods --no-headers 2>/dev/null' 2>/dev/null | sed 's/^/  /' || echo "  (kubectl not answering)"
    echo
    echo "host interfaces (n3/n6 stay up with no address: they are the UPF's macvlan parents)"
    lxc exec "$NODE_CORE" -- ip -br -4 addr show 2>/dev/null | sed 's/^/  /'
    echo
    echo "UPF attachments"
    lxc exec "$NODE_CORE" -- bash -lc 'kubectl -n aether-5gc exec upf-0 -c bessd -- ip -br -4 addr show 2>/dev/null | grep -E "^(access|core)(@[^ ]*)? "' 2>/dev/null | sed 's/^/  /' || echo "  (upf-0 not answering)"
}

# ── control node (aether-ops) ────────────────────────────────────────────────

# github_token echoes a token with read access to the aether-ops repository.
#
# The repository is private, so the install needs a credential. Rather than ask
# for one to be pasted into a config file, it is read from the `gh` CLI's
# existing login at the moment it is needed. Nothing is stored by this script,
# and the token reaches the node as a mode-0600 file that the install shreds —
# not as an argument, which would put it on the host's process list, and not as
# instance configuration, which `lxc config show` would print back.
github_token() {
    command -v gh >/dev/null 2>&1 \
        || die "gh not found; needed to authenticate to the private $AETHER_OPS_REPO repository"
    local t
    t=$(gh auth token 2>/dev/null) || true
    [ -n "$t" ] || die "gh is not authenticated — run 'gh auth login' (needs 'repo' scope for $AETHER_OPS_REPO)"
    printf '%s' "$t"
}

cmd_ctrl() {
    require_ctrl
    require_lxd
    exists_instance "$NODE_CTRL" || die "$NODE_CTRL not found — run '$PROG up' first"
    wait_cloud_init "$NODE_CTRL"

    # Fail early and specifically: a missing toolchain surfaces 10 minutes into
    # a source build as an unrecognisable compiler error.
    on_ctrl "command -v go >/dev/null && command -v node >/dev/null" \
        || die "go and/or node missing on $NODE_CTRL — cloud-init did not finish; check 'lxc exec $NODE_CTRL -- cloud-init status --long'"
    info "toolchain: $(on_ctrl 'go version' 2>/dev/null | tr -d '\r'), node $(on_ctrl 'node --version' 2>/dev/null | tr -d '\r')"

    info "staging the repository credential on $NODE_CTRL"
    github_token | lxc_do file push - "$NODE_CTRL/root/.aether-token" --mode 0600 >/dev/null \
        || die "could not stage the credential on $NODE_CTRL"

    info "installing aether-ops at $AETHER_OPS_REF (builds from source; several minutes)"
    # --preflight is not optional. It performs the management-node setup
    # (git/make/ansible, the service account) and, critically, generates the
    # daemon's managed ed25519 keypair. The daemon READS that key and never
    # creates one (ADR-0011), so an install without preflight yields a daemon
    # that starts cleanly and cannot reach a single node it is meant to manage.
    lxc_long exec "$NODE_CTRL" -- bash -s -- \
        "$AETHER_OPS_REF" "$AETHER_OPS_FRONTEND_REF" "$AETHER_OPS_PORT" "$AETHER_OPS_REPO" <<'REMOTE'
set -euo pipefail
ref="$1"; frontend_ref="$2"; port="$3"; repo="$4"

GITHUB_TOKEN=$(cat /root/.aether-token); export GITHUB_TOKEN
shred -u /root/.aether-token 2>/dev/null || rm -f /root/.aether-token

# REF makes install.sh build the daemon from that ref. Without it the installer
# fetches its own default (a release tag) and quietly ignores the ref used to
# fetch the installer itself, so the running daemon is not the code asked for.
install_env=(REF="$ref" GITHUB_TOKEN="$GITHUB_TOKEN")
[ -n "$frontend_ref" ] && install_env+=(FRONTEND_REF="$frontend_ref")

curl -fsSL -H "Authorization: token ${GITHUB_TOKEN}" \
    "https://raw.githubusercontent.com/${repo}/${ref}/scripts/install.sh" \
    | env "${install_env[@]}" bash -s -- --preflight --yes

# Bind to every interface so the API and UI are reachable from the lab, not
# just from this node's shell. Rewrite rather than append: appending grows a
# duplicate AETHER_LISTEN line on every re-run, and the file is read by
# systemd's EnvironmentFile, where the last assignment silently wins.
install -d -m 0755 /etc/aether-ops
touch /etc/aether-ops/env
sed -i '/^AETHER_LISTEN=/d' /etc/aether-ops/env
echo "AETHER_LISTEN=0.0.0.0:${port}" >> /etc/aether-ops/env
systemctl restart aether-ops
REMOTE

    lxc_do exec "$NODE_CTRL" -- systemctl is-active aether-ops >/dev/null 2>&1 \
        || die "aether-ops did not come up on $NODE_CTRL — check 'lxc exec $NODE_CTRL -- journalctl -u aether-ops -n 50'"

    info "aether-ops active: $(on_ctrl '/usr/local/bin/aether-ops --version' 2>/dev/null | head -1 | tr -d '\r')"
    info "API and UI on http://$CTRL_OAM:$AETHER_OPS_PORT/ (Swagger at /docs) — reachable from inside the site"
}

# The paths install.sh writes the daemon's managed public key to (ADR-0011):
# the canonical copy under the daemon's key directory, and a sudo-readable
# staged copy. Both are checked because which one exists depends on whether the
# service account existed when preflight ran.
CTRL_PUBKEY_PATHS="/var/lib/aether-ops/keys/aether_ed25519.pub /etc/aether-ops/aether_id_ed25519.pub"

# cmd_keys authorizes the control node's managed key on every managed node.
#
# aether-ops has no supported way to do this itself: its installer bootstraps
# only the node it runs on, and there is no password fallback, so a multi-node
# deployment cannot reach its targets until the key is distributed by hand
# (AO-2 in aether-ops's KNOWN_ISSUES.md). This is that step, done the same way
# aether-ops's own test environment does it.
#
# Idempotent, and safe to re-run after `ctrl` regenerates a key.
cmd_keys() {
    require_ctrl
    require_lxd
    exists_instance "$NODE_CTRL" || die "$NODE_CTRL not found — run '$PROG up' first"

    local pubkey
    pubkey=$(lxc exec "$NODE_CTRL" -- bash -s -- $CTRL_PUBKEY_PATHS <<'REMOTE'
for p in "$@"; do
    [ -s "$p" ] && { cat "$p"; exit 0; }
done
exit 1
REMOTE
    ) || true
    pubkey=$(printf '%s' "$pubkey" | tr -d '\r')
    [ -n "${pubkey// }" ] \
        || die "no managed public key on $NODE_CTRL (looked in: $CTRL_PUBKEY_PATHS) — run '$PROG ctrl' first"
    info "managed key: ${pubkey%% *} ...${pubkey##* }"

    local n
    for n in "$NODE_CORE" "$NODE_RANN" "$NODE_RANF" "$NODE_APP"; do
        in_nodes "$n" || continue
        exists_instance "$n" || { warn "$n not found; skipping"; continue; }
        wait_cloud_init "$n"
        info "authorizing the managed key on $n as '$AETHER_OPS_USER'"
        lxc_do exec "$n" -- bash -s -- "$AETHER_OPS_USER" "$pubkey" <<'REMOTE'
set -euo pipefail
user="$1"; pubkey="$2"

id "$user" >/dev/null 2>&1 || useradd -m -s /bin/bash "$user"
install -d -m 0700 -o "$user" -g "$user" "/home/$user/.ssh"
printf '%s\n' "$pubkey" > "/home/$user/.ssh/authorized_keys"
chown "$user:$user" "/home/$user/.ssh/authorized_keys"
chmod 0600 "/home/$user/.ssh/authorized_keys"

# The daemon runs unattended; a sudo password prompt would hang a deploy
# rather than fail it.
printf '%s ALL=(ALL) NOPASSWD: ALL\n' "$user" > "/etc/sudoers.d/$user"
chmod 0440 "/etc/sudoers.d/$user"
REMOTE
    done
    info "managed nodes reachable as '$AETHER_OPS_USER' from $NODE_CTRL"
}

# ── wanem (WAN emulator between the two site routers) ────────────────────────

# wanemd is built on the host, like ORBIT: the repository is private and the
# host already holds the credential (gh auth), so no node ever sees it.
build_wanemd() {
    local src="$TESTBED_CACHE_DIR/wanemd-src" out="$TESTBED_CACHE_DIR/wanemd"
    mkdir -p "$TESTBED_CACHE_DIR"
    command -v go >/dev/null 2>&1 || die "go not found on this host; needed to build wanemd"
    info "fetching wanemd${WANEMD_VERSION:+ at $WANEMD_VERSION}"
    rm -rf "$src"
    git clone -q "$WANEMD_REPO" "$src" >&2 || die "could not clone $WANEMD_REPO (private: is the host's git authenticated to GitHub? 'gh auth setup-git')"
    if [ -n "${WANEMD_VERSION:-}" ]; then
        git -C "$src" checkout -q "$WANEMD_VERSION" || die "no such wanemd ref: $WANEMD_VERSION"
    fi
    local rev; rev=$(git -C "$src" rev-parse --short HEAD)
    info "building wanemd $rev"
    ( cd "$src" && GOPRIVATE='github.com/BGrewell/*,github.com/bgrewell/*' GOFLAGS=-mod=mod CGO_ENABLED=0 \
        go build -buildvcs=false -ldflags "-X main.rev=$rev" -o "$out" . \
      && GOPRIVATE='github.com/BGrewell/*,github.com/bgrewell/*' GOFLAGS=-mod=mod CGO_ENABLED=0 \
        go build -buildvcs=false -o "$out-cli" ./support/tools/wanemd-cli/go/ ) >&2 \
        || die "wanemd build failed"
    echo "$out"
}

cmd_wanem() {
    [ "$WITH_FAR" = true ] || die "this testbed has no far site, so no WAN emulator"
    require_lxd
    exists_instance "$NODE_WANEM" || die "$NODE_WANEM not found — run '$PROG up' first"
    wait_cloud_init "$NODE_WANEM"
    local bin; bin=$(build_wanemd)

    info "installing wanemd on $NODE_WANEM"
    local f
    for f in wanemd wanemd-cli; do
        local src="$bin"; [ "$f" = wanemd-cli ] && src="$bin-cli"
        push_file "$NODE_WANEM" "$src" "/opt/wanemd/bin/$f.new" 755
        lxc_do exec "$NODE_WANEM" -- mv -f "/opt/wanemd/bin/$f.new" "/opt/wanemd/bin/$f"
    done

    # The impairment profiles are scoped to the far site's cellular subnets so
    # management stays unshaped; the script carries the subnets for this
    # testbed's addressing.
    sed -e "s|__FAR_N2__|$TESTBED_NET_FAR_N2|g" -e "s|__FAR_N3__|$TESTBED_NET_FAR_N3|g" \
        "$HERE/wanem/profile.sh" > "$TESTBED_CACHE_DIR/profile.sh"
    push_file "$NODE_WANEM" "$TESTBED_CACHE_DIR/profile.sh" /opt/wanemd/bin/profile.sh 755

    lxc_do exec "$NODE_WANEM" -- bash -s -- <<'REMOTE'
set -euo pipefail
install -d /etc/wanemd
# ul = towards the far site (wan-a faces r-site), dl = towards the core site.
cat > /etc/wanemd/wanemd.yaml <<'YAML'
grpc_port: 6201
ul_interface: wan-a
dl_interface: wan-b
bridge_name: br-wan
ul_line_rate: 1000mbit
dl_line_rate: 1000mbit
default_queue_handle: 9999
logging_level: info
logging_location: /var/log/wanemd.log
logging_max_size: 100
logging_max_backups: 3
logging_max_age: 30
logging_compress: true
flush_iptables_forward: true
YAML
# Upstream's unit uses StandardOutput=syslog, which systemd no longer accepts;
# otherwise identical.
cat > /etc/systemd/system/wanemd.service <<'UNIT'
[Unit]
Description=wanemd WAN emulator
ConditionPathExists=/opt/wanemd/bin/wanemd
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Restart=on-failure
RestartSec=10
WorkingDirectory=/opt/wanemd/bin
ExecStart=/opt/wanemd/bin/wanemd
StandardOutput=journal
StandardError=journal
SyslogIdentifier=wanemd

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable wanemd >/dev/null 2>&1
systemctl restart wanemd
REMOTE
    sleep 3
    lxc_do exec "$NODE_WANEM" -- systemctl is-active wanemd >/dev/null \
        && info "wanemd active on $NODE_WANEM (gRPC :6201; profiles: /opt/wanemd/bin/profile.sh <wan-30ms|wan-80ms|sat-600ms|clear|show>)" \
        || die "wanemd did not come up; check 'lxc exec $NODE_WANEM -- journalctl -u wanemd -n 50'"
}

# ── orbit (responder on the app node, ORBIT on the RAN nodes) ────────────────

# ORBIT is built here on the host and the binary pushed to the nodes. That
# keeps repository credentials off the testbed, and is faster than installing a
# Go toolchain on each node. ORBIT_VERSION picks the ref; empty builds the
# working tree as-is, which is what you want mid-change.
build_orbit() {
    local repo_root out="$TESTBED_CACHE_DIR/orbit"
    repo_root=$(cd "$HERE/../.." && pwd)
    mkdir -p "$TESTBED_CACHE_DIR"
    command -v go >/dev/null 2>&1 || die "go not found on this host; needed to build ORBIT"

    if [ -n "${ORBIT_VERSION:-}" ]; then
        info "building ORBIT at $ORBIT_VERSION"
        local wt="$TESTBED_CACHE_DIR/orbit-src"
        rm -rf "$wt"
        git -C "$repo_root" worktree prune >/dev/null 2>&1 || true
        git -C "$repo_root" worktree add -q --detach "$wt" "$ORBIT_VERSION" \
            || die "no such ORBIT ref: $ORBIT_VERSION"
        ( cd "$wt" && CGO_ENABLED=0 go build -o "$out" ./cmd/orbit ) >&2 || die "ORBIT build failed"
        git -C "$repo_root" worktree remove --force "$wt" >/dev/null 2>&1 || true
    else
        info "building ORBIT from the working tree"
        ( cd "$repo_root" && CGO_ENABLED=0 go build -o "$out" ./cmd/orbit ) >&2 || die "ORBIT build failed"
    fi
    echo "$out"
}

push_orbit() {
    local node=$1 bin=$2
    info "installing orbit on $node"
    # A running unit holds the binary open, so writing over it fails with
    # "text file busy". Land it beside the target and rename into place: the
    # rename swaps the directory entry, which the running process does not
    # block, and the old inode goes when it exits.
    push_file "$node" "$bin" /usr/local/bin/orbit.new 755
    lxc_do exec "$node" -- mv -f /usr/local/bin/orbit.new /usr/local/bin/orbit \
        || die "could not install orbit on $node"
}

# One responder token per testbed. The app's copy is authoritative so a RAN
# reinstall keeps working against a running responder; the host cache carries
# it across an app rebuild.
responder_token() {
    local f="$TESTBED_CACHE_DIR/$P.responder-token" t=""
    if exists_instance "$NODE_RESP"; then
        t=$(lxc exec "$NODE_RESP" -- sh -c 'sed -n "s/^ORBIT_RESPONDER_TOKEN=//p" /etc/orbit/env 2>/dev/null' 2>/dev/null | tr -d '\r') || t=""
    fi
    if [ -z "$t" ] && [ -s "$f" ]; then t=$(cat "$f"); fi
    if [ -z "$t" ]; then t=$(openssl rand -hex 16); info "generated a new responder token"; fi
    mkdir -p "$TESTBED_CACHE_DIR"; (umask 077; printf '%s\n' "$t" > "$f")
    printf '%s' "$t"
}

# One API token per testbed, for the RAN nodes' `orbit serve --token-file`
# and their clients ($ORBIT_TOKEN). A RAN node's copy is authoritative, so a
# reinstall of one node keeps the token the other is serving with; the host
# cache carries it across a full rebuild.
api_token() {
    local f="$TESTBED_CACHE_DIR/$P.api-token" t="" n
    for n in "$NODE_RANN" "$NODE_RANF"; do
        [ -z "$t" ] && exists_instance "$n" || continue
        t=$(lxc exec "$n" -- sh -c 'sed -n "s/^ORBIT_TOKEN=//p" /etc/orbit/env 2>/dev/null' 2>/dev/null | tr -d '\r') || t=""
    done
    if [ -z "$t" ] && [ -s "$f" ]; then t=$(cat "$f"); fi
    if [ -z "$t" ]; then t=$(openssl rand -hex 32); info "generated a new API token"; fi
    mkdir -p "$TESTBED_CACHE_DIR"; (umask 077; printf '%s\n' "$t" > "$f")
    printf '%s' "$t"
}

# Ki/OPc as the deployed core actually has them, falling back to the configured
# test values when the core is not up. A mismatch presents as a 5G-AKA MAC
# failure at registration, not as a rejected authentication.
core_keys() {
    local ki="" opc=""
    if exists_instance "$NODE_CORE" && is_running "$NODE_CORE"; then
        read -r ki opc <<<"$(core_call keys)" || true
        if [ "$ki" = - ]; then ki=""; fi
        if [ "$opc" = - ]; then opc=""; fi
    fi
    if [ -n "$ki" ] && [ "$ki" != "$TESTBED_KI" ]; then warn "deployed Ki differs from TESTBED_KI; using the deployed value"; fi
    if [ -n "$opc" ] && [ "$opc" != "$TESTBED_OPC" ]; then warn "deployed OPc differs from TESTBED_OPC; using the deployed value"; fi
    echo "${ki:-$TESTBED_KI} ${opc:-$TESTBED_OPC}"
}

# core_sdcore_keys reads Ki/OPc out of the OnRamp values template the core was
# deployed from. One file, no filename prefixes, first match only: the 4G
# template beside it carries its own keys.
core_sdcore_keys() {
    local ki="" opc="" tpl="$TESTBED_ONRAMP_DIR/deps/5gc/roles/core/templates"
    ki=$(on_core "cat $tpl/sdcore-5g-values.yaml $tpl/radio-5g-values.yaml 2>/dev/null | grep -m1 -oE '^\s*key: *\"?[0-9a-fA-F]{32}' | grep -oE '[0-9a-fA-F]{32}' | head -1" 2>/dev/null | tr -d '\r') || ki=""
    opc=$(on_core "cat $tpl/sdcore-5g-values.yaml $tpl/radio-5g-values.yaml 2>/dev/null | grep -m1 -oE '^\s*opc: *\"?[0-9a-fA-F]{32}' | grep -oE '[0-9a-fA-F]{32}' | head -1" 2>/dev/null | tr -d '\r') || opc=""
    echo "${ki:--} ${opc:--}"
}

# orbit_env_for <node> -> /etc/orbit/env. Read by the units (EnvironmentFile)
# and by a shell with `set -a; . /etc/orbit/env`, so the commands in README.md
# work as written on any node.
orbit_env_for() {
    local node=$1 ki opc token oam
    read -r ki opc <<<"$(core_keys)"
    token=$(responder_token)
    oam=$(oam_of "$node")
    cat <<EOF
# ORBIT environment for the LXD testbed, written by testbed.sh. Read by the
# orbit units and by \`set -a; . /etc/orbit/env\` in a shell.
ORBIT_MCC=$TESTBED_MCC
ORBIT_MNC=$TESTBED_MNC
# Subscriber keys as provisioned by OnRamp (every test subscriber shares them).
ORBIT_KI=$ki
ORBIT_OPC=$opc
# Core coordinates (not read by orbit; for the commands in README.md).
TESTBED_AMF=$AMF_N2
TESTBED_UPF_N3=$UPF_N3:2152
TESTBED_RESPONDER=$RESPONDER
# UDP echo peer on N6 (orbit-n6-echo on the responder's node): the conformance
# checks' --traffic.
TESTBED_ECHO=$ECHO_PEER
TESTBED_TAC=$TESTBED_TAC
TESTBED_SST=$TESTBED_SST
TESTBED_SD=$TESTBED_SD
TESTBED_DNN=$TESTBED_DNN
TESTBED_SUPI_START=$TESTBED_SUB_START
TESTBED_SUPI_COUNT=$TESTBED_SUB_COUNT
ORBIT_RESPONDER_TOKEN=$token
EOF
    case "$node" in
        "$NODE_RANN"|"$NODE_RANF")
            echo "# API server and dashboard on this node's management address, over TLS"
            echo "# (--tls auto, pinned in /root/.config/orbit/servers.yaml) with a bearer"
            echo "# token; API client commands (cell, ue, runs) default to it when --server"
            echo "# is omitted."
            [ -n "$oam" ] || die "$node has no management address yet — is it running?"
            echo "ORBIT_LISTEN=$oam:$ORBIT_PORT"
            echo "ORBIT_SERVER=https://$oam:$ORBIT_PORT"
            echo "ORBIT_TOKEN=$(api_token)" ;;
    esac
}

# target_for <ran-node> -> the ORBIT target "lxd" for that node (ADR-0008,
# docs/targets.md): the core, the provisioned subscriber pool, the node's own
# N2/N3 address ranges (one address per simulated gNB) and the N6 responder.
# The keys and the responder token stay ${VAR} references to /etc/orbit/env,
# so the file holds no secret. Written directly rather than through
# `orbit target init`, whose --bind records one address, not a range.
# supi_at <offset> -> TESTBED_SUB_START + offset, zero-padded to the start's
# width, so a SUPI keeps the 15 digits a target expects. The SUPI is decimal:
# 10# forces base 10 so a leading zero is not read as octal.
supi_at() {
    printf '%0*d' "${#TESTBED_SUB_START}" "$((10#$TESTBED_SUB_START + $1))"
}

# check_targets_fit refuses a configuration the three per-node targets cannot
# carve cleanly, before any file is written. The split is only safe when each
# site gets a whole, disjoint half of the subscriber pool and the conformance
# gNBs leave at least one for load.
check_targets_fit() {
    case "$TESTBED_CONF_GNBS" in ''|*[!0-9]*) die "TESTBED_CONF_GNBS ($TESTBED_CONF_GNBS) is not a number" ;; esac
    case "$TESTBED_CONF_SUBS" in ''|*[!0-9]*) die "TESTBED_CONF_SUBS ($TESTBED_CONF_SUBS) is not a number" ;; esac
    [ "$TESTBED_CONF_GNBS" -ge 1 ] && [ "$TESTBED_CONF_GNBS" -lt "$TESTBED_RAN_GNBS" ] \
        || die "TESTBED_CONF_GNBS ($TESTBED_CONF_GNBS) must be between 1 and TESTBED_RAN_GNBS-1 ($((TESTBED_RAN_GNBS - 1)))"
    [ $((TESTBED_SUB_COUNT % 2)) -eq 0 ] \
        || die "TESTBED_SUB_COUNT ($TESTBED_SUB_COUNT) must be even so the two RAN sites take equal halves"
    [ $((TESTBED_SUB_COUNT / 2)) -gt "$TESTBED_CONF_SUBS" ] \
        || die "TESTBED_CONF_SUBS ($TESTBED_CONF_SUBS) must be less than half of TESTBED_SUB_COUNT ($((TESTBED_SUB_COUNT / 2))) so each site has load subscribers too"
}

# target_for <ran-node> <role> -> one ORBIT target document (ADR-0008,
# docs/targets.md). role is full, conformance or load:
#
#   full         name lxd              every gNB, the whole subscriber pool.
#   conformance  name lxd-conformance  the first TESTBED_CONF_GNBS gNBs,
#                                       TESTBED_CONF_SUBS subscribers.
#   load         name lxd-load         the remaining gNBs and subscribers.
#
# The conformance and load slices are disjoint in gNB address, gNB ID and
# subscriber, so the two can run against one node at once. The near and far
# nodes take the low and high halves of the subscriber pool, so a SUPI is
# never registered from both sites. The keys and responder token stay ${VAR}
# references to /etc/orbit/env, so the file holds no secret. Written directly
# rather than through `orbit target init`, whose --bind records one address,
# not a range.
target_for() {
    local node=$1 role=${2:-full} n2 n3
    case "$node" in
        "$NODE_RANF") n2=$TESTBED_NET_FAR_N2; n3=$TESTBED_NET_FAR_N3 ;;
        *)            n2=$TESTBED_NET_N2;     n3=$TESTBED_NET_N3 ;;
    esac
    # The site's half of the subscriber pool: near from offset 0, far from the
    # midpoint. Within a half, conformance takes the first TESTBED_CONF_SUBS.
    local half=$((TESTBED_SUB_COUNT / 2)) site_base=0
    [ "$node" = "$NODE_RANF" ] && site_base=$half

    local name slot_first slot_last first_supi count gnb_first gnb_count how
    case "$role" in
        full)
            name=lxd
            slot_first=$TESTBED_HOST_RAN; slot_last=$((TESTBED_HOST_RAN + TESTBED_RAN_GNBS - 1))
            first_supi=$(supi_at 0); count=$TESTBED_SUB_COUNT
            gnb_first=100; gnb_count=900
            how="orbit target probe lxd" ;;
        conformance)
            name=lxd-conformance
            slot_first=$TESTBED_HOST_RAN; slot_last=$((TESTBED_HOST_RAN + TESTBED_CONF_GNBS - 1))
            first_supi=$(supi_at "$site_base"); count=$TESTBED_CONF_SUBS
            gnb_first=100; gnb_count=450
            how="orbit conformance -t lxd-conformance" ;;
        load)
            name=lxd-load
            slot_first=$((TESTBED_HOST_RAN + TESTBED_CONF_GNBS)); slot_last=$((TESTBED_HOST_RAN + TESTBED_RAN_GNBS - 1))
            first_supi=$(supi_at $((site_base + TESTBED_CONF_SUBS))); count=$((half - TESTBED_CONF_SUBS))
            gnb_first=1000; gnb_count=900
            how="orbit load -t lxd-load" ;;
        *) die "target_for: unknown role '$role'" ;;
    esac
    cat <<TARGET
# ORBIT target for the LXD testbed, written by testbed.sh for $node.
#   $how
version: 1
kind: target
name: $name
core:
  amf: $AMF_N2
  # mcc/mnc as \${ORBIT_MCC}/\${ORBIT_MNC}, like ki/opc below: /etc/orbit/env
  # sets them, so a shell (and the server) that reads a target has the same
  # value in its environment, and the "env overrode target" notice they would
  # otherwise raise for every run is the variable matching its own expansion
  # (suppressed in orbit; see internal/plan/notices.go). The import link
  # inlines them for the browser, which has no environment.
  plmn: { mcc: "\${ORBIT_MCC}", mnc: "\${ORBIT_MNC}" }
  tac: $TESTBED_TAC
  slices:
    - sst: $TESTBED_SST
      sd: "$TESTBED_SD"
  dnn: $TESTBED_DNN
  upf_n3: $UPF_N3:2152
subscribers:
  - name: main
    first_supi: "$first_supi"
    count: $count
    ki: \${ORBIT_KI}
    opc: \${ORBIT_OPC}
ran:
  n2_source_ips: [$(ip_in "$n2" "$slot_first")-$(ip_in "$n2" "$slot_last")]
  n3_ips: [$(ip_in "$n3" "$slot_first")-$(ip_in "$n3" "$slot_last")]
  gnb_ids: { first: $gnb_first, count: $gnb_count, bits: 24 }
app:
  responders:
    - name: n6
      control: $RESPONDER
      token: \${ORBIT_RESPONDER_TOKEN}
      data_ip: $APP_N6
TARGET
}

# The three targets every RAN node carries, full first so a bare `-t lxd` and
# `orbit target probe lxd` keep working.
TARGET_ROLES=(full conformance load)
target_name_for() { case "$1" in full) echo lxd ;; conformance) echo lxd-conformance ;; load) echo lxd-load ;; esac; }

# install_orbit_target <ran-node> writes the node's three targets as root's
# ~/.config/orbit/targets/<name>.yaml (0600) and checks each one loads.
install_orbit_target() {
    local node=$1 role name
    lxc_do exec "$node" -- mkdir -p -m 700 /root/.config/orbit/targets
    for role in "${TARGET_ROLES[@]}"; do
        name=$(target_name_for "$role")
        target_for "$node" "$role" > "$TESTBED_CACHE_DIR/target.$node.$name"
        push_file "$node" "$TESTBED_CACHE_DIR/target.$node.$name" "/root/.config/orbit/targets/$name.yaml" 600
        rm -f "$TESTBED_CACHE_DIR/target.$node.$name"
        lxc_do exec "$node" -- bash -c "set -a; . /etc/orbit/env; orbit target show $name >/dev/null" \
            || die "the $name target on $node does not load; see 'orbit target show $name' there"
    done
    info "targets lxd, lxd-conformance, lxd-load written on $node"
}

# targets_import_link <ran-node> -> a dashboard URL that preloads the node's
# three targets into the Targets page in one click, so the browser needs no
# import or typing either. The targets are carried in the URL's #import=
# fragment as base64url YAML, with the keys inlined (the browser has no
# /etc/orbit/env to expand ${ORBIT_KI} against). A fragment is never sent to
# the server, so the keys stay out of the dashboard's request log; they are
# OnRamp's shared test Ki/OPc and responder token, not production secrets. The
# page shows a confirm banner before it imports, and offers to remember the
# keys in the browser.
targets_import_link() {
    local node=$1 oam role body ki opc token
    oam=$(oam_of "$node")
    read -r ki opc <<<"$(core_keys)"
    token=$(responder_token)
    # A target file the page reads is ONE YAML document: a target, or a LIST of
    # them (docs/targets.md; a second `---` document is refused). So the three
    # targets go out as a YAML sequence, each a list element, keys inlined
    # (the browser has no /etc/orbit/env). The per-target comments are dropped;
    # one header names the file.
    local list="# ORBIT targets for the LXD testbed ($node), preloaded into the dashboard."$'\n'
    for role in "${TARGET_ROLES[@]}"; do
        body=$(target_for "$node" "$role" \
            | sed -e "s|\${ORBIT_KI}|$ki|" -e "s|\${ORBIT_OPC}|$opc|" -e "s|\${ORBIT_RESPONDER_TOKEN}|$token|" \
                  -e "s|\${ORBIT_MCC}|$TESTBED_MCC|" -e "s|\${ORBIT_MNC}|$TESTBED_MNC|" \
            | grep -v '^[[:space:]]*#')
        list+=$(printf '%s\n' "$body" | awk 'NR==1{print "- "$0; next} {print "  "$0}')$'\n'
    done
    local b64; b64=$(printf '%s' "$list" | base64 -w0 | tr '+/' '-_' | tr -d '=')
    echo "https://$oam:$ORBIT_PORT/?targets#import=$b64"
}

install_orbit_env() {
    local node=$1
    orbit_env_for "$node" > "$TESTBED_CACHE_DIR/env.$node"
    push_file "$node" "$TESTBED_CACHE_DIR/env.$node" /etc/orbit/env 600
    rm -f "$TESTBED_CACHE_DIR/env.$node"
}

cmd_app() {
    require_lxd
    exists_instance "$NODE_RESP" || die "$NODE_RESP not found — run '$PROG up' first"
    local bin; bin=$(build_orbit)
    push_orbit "$NODE_RESP" "$bin"
    install_orbit_env "$NODE_RESP"

    # Bound to the N6 address specifically, not 0.0.0.0, and behind a token:
    # the responder is a remotely-aimable traffic generator and has no business
    # on the management network. Return routing to the UE pool is in the
    # node's netplan (via r-site), so there is no route unit here.
    info "starting the responder on $RESPONDER"
    lxc_do exec "$NODE_RESP" -- bash -c "cat > /etc/systemd/system/orbit-responder.service <<'EOF'
[Unit]
Description=ORBIT responder (loom agent) on N6
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/orbit/env
ExecStart=/usr/local/bin/orbit responder --bind \${TESTBED_RESPONDER} --token \${ORBIT_RESPONDER_TOKEN}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable orbit-responder >/dev/null 2>&1 && systemctl restart orbit-responder" >/dev/null
    lxc_do exec "$NODE_RESP" -- systemctl is-active orbit-responder >/dev/null \
        && info "responder active" || warn "responder did not come up; check journalctl -u orbit-responder"
    install_n6_echo
}

# n6_echo_program -> the UDP echo peer: every datagram goes back to its sender
# from the address it was sent to. Python 3 from the image, no dependencies.
n6_echo_program() {
    cat <<'PY'
#!/usr/bin/env python3
"""UDP echo peer for the LXD testbed's N6 network, written by testbed.sh.

Usage: orbit-n6-echo <ip>:<port>
"""
import socket
import sys


def main():
    if len(sys.argv) != 2 or ":" not in sys.argv[1]:
        sys.exit("usage: orbit-n6-echo <ip>:<port>")
    host, _, port = sys.argv[1].rpartition(":")
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind((host, int(port)))
    while True:
        data, peer = sock.recvfrom(65535)
        try:
            sock.sendto(data, peer)
        except OSError:
            pass  # an unreachable sender must not stop the service


if __name__ == "__main__":
    main()
PY
}

# install_n6_echo installs the echo peer on the responder's node as the
# orbit-n6-echo unit, bound to the N6 address only (like the responder, it
# has no business on the management network), and starts it.
install_n6_echo() {
    info "starting the UDP echo peer on $ECHO_PEER"
    n6_echo_program > "$TESTBED_CACHE_DIR/n6-echo.$NODE_RESP"
    push_file "$NODE_RESP" "$TESTBED_CACHE_DIR/n6-echo.$NODE_RESP" /usr/local/bin/orbit-n6-echo 755
    rm -f "$TESTBED_CACHE_DIR/n6-echo.$NODE_RESP"
    lxc_do exec "$NODE_RESP" -- bash -c "cat > /etc/systemd/system/orbit-n6-echo.service <<'EOF'
[Unit]
Description=UDP echo peer on N6 (the conformance checks' --traffic)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/orbit/env
ExecStart=/usr/bin/python3 /usr/local/bin/orbit-n6-echo \${TESTBED_ECHO}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable orbit-n6-echo >/dev/null 2>&1 && systemctl restart orbit-n6-echo" >/dev/null
    lxc_do exec "$NODE_RESP" -- systemctl is-active orbit-n6-echo >/dev/null \
        && info "echo peer active" || warn "the echo peer did not come up; check journalctl -u orbit-n6-echo"
}

# echo_probe -> a shell command for the responder's node: it sends one
# datagram to the echo peer and succeeds when the same bytes come back within
# 2 s from the peer's own address.
echo_probe() {
    cat <<PROBE
python3 -c 'import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(2)
s.sendto(b"orbit-n6-echo-check",("$APP_N6",$TESTBED_ECHO_PORT))
d,p=s.recvfrom(64)
sys.exit(0 if d==b"orbit-n6-echo-check" and p==("$APP_N6",$TESTBED_ECHO_PORT) else 1)'
PROBE
}

# cmd_ran [near|far] — ORBIT on one or both RAN nodes (default both).
cmd_ran() {
    require_lxd
    local targets=()
    case "${1:-}" in
        ''|all) targets=("$NODE_RANN"); if [ "$WITH_FAR" = true ]; then targets+=("$NODE_RANF"); fi ;;
        near)   targets=("$NODE_RANN") ;;
        far)    [ "$WITH_FAR" = true ] || die "this testbed has no far site"
                targets=("$NODE_RANF") ;;
        *) die "usage: $PROG ran [near|far]" ;;
    esac
    check_targets_fit
    local bin; bin=$(build_orbit)
    local node
    for node in "${targets[@]}"; do
        exists_instance "$node" || die "$node not found — run '$PROG up' first"
        push_orbit "$node" "$bin"
        install_orbit_env "$node"
        install_orbit_target "$node"

        # The API server listens on the node's management address: the
        # dashboard it serves on the same port is worth reaching from a browser
        # inside the site, and the API carries subscriber credentials (Ki/OPc),
        # so it has no business on N2 or N3. Off loopback it needs TLS and a
        # token (ADR-0008 §4): a self-signed certificate kept in the state
        # dir, and the testbed's API token.
        local oam; oam=$(oam_of "$node")
        info "starting the ORBIT API server on https://$oam:$ORBIT_PORT"
        api_token | lxc_do exec "$node" -- sh -c 'umask 077; cat > /etc/orbit/api-token' \
            || die "could not write the API token on $node"
        lxc_do exec "$node" -- bash -c "cat > /etc/systemd/system/orbit.service <<'EOF'
[Unit]
Description=ORBIT API server and dashboard
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/orbit/env
ExecStart=/usr/local/bin/orbit serve --listen \${ORBIT_LISTEN} --tls auto --token-file /etc/orbit/api-token --loom-agent \${TESTBED_RESPONDER} --loom-token \${ORBIT_RESPONDER_TOKEN} --state-dir /var/lib/orbit/runs
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
mkdir -p /var/lib/orbit/runs && systemctl daemon-reload && systemctl enable orbit >/dev/null 2>&1 && systemctl restart orbit" >/dev/null
        lxc_do exec "$node" -- systemctl is-active orbit >/dev/null \
            && info "orbit serve active on $node" || warn "orbit did not come up on $node; check journalctl -u orbit"
        trust_orbit "$node" "$oam"
        info "dashboard with the targets preloaded: $(targets_import_link "$node")"
    done
}

# trust_orbit <node> <oam>: pins the node's own API server for its clients.
# The pin is computed from the certificate on the node, not read off the
# wire, so nothing is trusted on first use; the token is kept with it.
trust_orbit() {
    local node=$1 oam=$2
    lxc_do exec "$node" -- bash -c '
        cert=/var/lib/orbit/runs/tls/cert.pem
        for _ in $(seq 1 50); do [ -s "$cert" ] && break; sleep 0.2; done
        [ -s "$cert" ] || { echo "no $cert: orbit serve --tls auto did not start" >&2; exit 1; }
        pin=$(openssl x509 -in "$cert" -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | openssl base64 -A)
        for _ in $(seq 1 50); do
            /usr/local/bin/orbit server trust "https://'"$oam:$ORBIT_PORT"'" --fingerprint "sha256/$pin" --token-file /etc/orbit/api-token --timeout 2s >/dev/null 2>&1 && exit 0
            sleep 0.2
        done
        /usr/local/bin/orbit server trust "https://'"$oam:$ORBIT_PORT"'" --fingerprint "sha256/$pin" --token-file /etc/orbit/api-token --timeout 2s' >/dev/null \
        && info "pinned the API server on $node" || warn "could not pin the API server on $node; check journalctl -u orbit"
}

cmd_apps() { cmd_app; cmd_ran; }

# ── check: does the site route the way it should? ────────────────────────────
#
# Infrastructure assertions, independent of the core: every gateway answers,
# the far site is two hops away and the hops are the routers, the Internet is
# reached through r-site's NAT only, the UE pool is routed where it should be,
# and the VLANs are actually separate (an address on one is not on-link from
# another). One line per assertion; exit status is the number of failures.
CHECK_PASS=0; CHECK_FAIL=0

# Reduces `traceroute -n` output to one word per hop: the address that answered
# at that hop, `*` when no probe was answered, or every distinct address joined
# by commas when more than one router answered. A hop therefore reads as its
# address as long as one probe to it got through.
HOP_ADDRS=$(cat <<'AWK'
awk 'NR>1{o=""; for(i=2;i<=NF;i++) if ($i ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ && index(","o",", ","$i",")==0) o=o (o==""?"":",") $i; print (o==""?"*":o)}'
AWK
)
check() { # check <node> <description> <shell command>
    local node=$1 desc=$2; shift 2
    local out
    if out=$(lxc exec "$node" -- sh -c "$*" 2>&1); then
        printf 'PASS  [%s] %s\n' "${node#"$P-"}" "$desc"; CHECK_PASS=$((CHECK_PASS + 1))
    else
        printf 'FAIL  [%s] %s\n' "${node#"$P-"}" "$desc"; printf '%s\n' "$out" | tail -4 | sed 's/^/        /'
        CHECK_FAIL=$((CHECK_FAIL + 1))
    fi
}

cmd_check() {
    require_lxd
    local n; for n in "${NODES[@]}"; do exists_instance "$n" || die "$n not found — run '$PROG up' first"; done
    local PING='ping -c2 -W2 -q' s=$TESTBED_HOST_RAN e=$((TESTBED_HOST_RAN + TESTBED_RAN_GNBS - 1))
    local near_n2; near_n2=$(ip_in "$TESTBED_NET_N2" "$s")
    local far_n2; far_n2=$(ip_in "$TESTBED_NET_FAR_N2" "$s")
    local near_n3; near_n3=$(ip_in "$TESTBED_NET_N3" "$s")

    if [ "$WITH_ROUTER" = true ]; then
        check "$NODE_RSITE" "forwarding on, masquerade only on the uplink" \
            "sysctl -n net.ipv4.ip_forward | grep -qx 1 && nft list ruleset | grep -q 'oifname \"uplink\" ip saddr @internal masquerade'"
        check "$NODE_RSITE" "UE pool $TESTBED_UE_POOL routed to the UPF N6 $UPF_N6" \
            "ip route get $(ip_in "$TESTBED_UE_POOL" 1) | grep -q 'via $UPF_N6 dev n6'"
    fi
    if [ "$WITH_FAR" = true ]; then
        check "$NODE_RRAN" "forwarding on, no NAT" \
            "sysctl -n net.ipv4.ip_forward | grep -qx 1 && ! nft list ruleset 2>/dev/null | grep -q masquerade"
    fi

    local gw gws=()
    if [ "$WITH_ROUTER" = true ]; then gws=("$GW_OAM" "$GW_N2" "$GW_N3" "$GW_N6"); fi
    if [ "$WITH_FAR" = true ]; then gws+=("$WAN_RSITE"); fi
    for gw in "${gws[@]}"; do
        check "$NODE_CORE" "ping r-site $gw" "$PING $gw"
    done
    check "$NODE_CORE" "near gNB N2 $near_n2 is on-link" \
        "$PING $near_n2 && ip route get $near_n2 | grep -q 'dev n2 src $CORE_N2' && ! ip route get $near_n2 | grep -q via"
    if [ "$WITH_FAR" = true ]; then
        # Three probes per hop, sent one at a time (-N1): the default sends every
        # TTL at once, and the destination rate-limits the burst of port-unreachable
        # replies it owes, so the probe that identifies the last hop can go
        # unanswered. The whole trace is retried before the hop list is judged.
        check "$NODE_CORE" "far gNB N2 $far_n2 via r-site, WAN, r-ran" \
            "$PING $far_n2 || exit 1; for try in 1 2 3; do t=\$(traceroute -n -q3 -N1 -w2 $far_n2 | $HOP_ADDRS | tr '\n' ' '); echo \"\$t\"; [ \"\$t\" = '$GW_N2 $WAN_RRAN $far_n2 ' ] && exit 0; sleep 1; done; exit 1"
        check "$NODE_CORE" "1472-byte DF ping to the far site (MTU 1500 end to end)" "ping -c2 -W2 -q -M do -s 1472 $far_n2"
    fi
    if [ "$WITH_ROUTER" = true ]; then
        check "$NODE_CORE" "Internet via r-site NAT" "$PING 8.8.8.8"
    else
        check "$NODE_CORE" "Internet via the uplink" "$PING 8.8.8.8"
    fi
    if [ "$LEGACY_NET" = true ]; then
        check "$NODE_CORE" "n3/n6 carry the core node's routing addresses $CORE_N3, $CORE_N6" \
            "ip -4 -o addr show dev n3 | grep -q 'inet $CORE_N3/' && ip -4 -o addr show dev n6 | grep -q 'inet $CORE_N6/'"
    fi
    if [ "$CORE_UPF_ATTACH" = host ]; then
        check "$NODE_CORE" "n3/n6 carry the UPF addresses $UPF_N3, $UPF_N6" \
            "ip -4 -o addr show dev n3 | grep -q 'inet $UPF_N3/' && ip -4 -o addr show dev n6 | grep -q 'inet $UPF_N6/'"
        if [ "$WITH_FAR" = true ]; then
                check "$NODE_CORE" "far N3 subnet routed via r-site on n3" \
                    "ip route get $(ip_in "$TESTBED_NET_FAR_N3" "$s") | grep -q 'via $GW_N3 dev n3 src $UPF_N3'"
        fi
    else
        check "$NODE_CORE" "receive aggregation off on trunk (gro, rx-gro-hw)" \
            "ethtool -k trunk | grep -E '^(generic-receive-offload|rx-gro-hw):' | grep -qv ': on'"
        if [ "$LEGACY_NET" != true ]; then
            check "$NODE_CORE" "n3/n6 up with no address (macvlan parents)" \
                "! ip -4 -o addr show dev n3 | grep -q inet && ! ip -4 -o addr show dev n6 | grep -q inet && ip -br link show n3 | grep -q UP && ip -br link show n6 | grep -q UP"
        fi
    fi

    if [ "$WITH_FAR" = true ]; then
        check "$NODE_RANF" "ping the AMF's N2 $CORE_N2" "$PING $CORE_N2"
        check "$NODE_RANF" "ping r-site N3 $GW_N3 (the UPF's gateway)" "$PING $GW_N3"
        check "$NODE_RANF" "every far N2 slot .$s-.$e reaches the AMF" \
            "for i in \$(seq $s $e); do $PING -I $(ip_in "$TESTBED_NET_FAR_N2" '')\$i $CORE_N2 || exit 1; done"
        check "$NODE_RANF" "core N3 subnet routed via r-ran on n3" "ip route get $GW_N3 | grep -q 'via $GW_FN3 dev n3'"
        check "$NODE_RANF" "Internet via two routers" "$PING 8.8.8.8"
    fi

    if [ "$WITH_ROUTER" = true ]; then
        check "$NODE_RANN" "ping r-site OAM and the Internet" "$PING $GW_OAM && $PING 8.8.8.8"
    else
        check "$NODE_RANN" "Internet via the uplink" "$PING 8.8.8.8"
    fi
    check "$NODE_RANN" "every near N2 slot .$s-.$e reaches the AMF on-link" \
        "for i in \$(seq $s $e); do $PING -I $(ip_in "$TESTBED_NET_N2" '')\$i $CORE_N2 || exit 1; done"
    if [ "$WITH_APP" = true ]; then
        # VLAN isolation: the app's N6 address must not answer ARP on N3 or N2, and
        # N3 -> N6 traffic is routed through r-site (hop 1), not switched.
        check "$NODE_RANN" "isolation: no ARP reply for app N6 $APP_N6 on n3" "! arping -c2 -w3 -I n3 -s $near_n3 $APP_N6"
        check "$NODE_RANN" "isolation: no ARP reply for app N6/OAM on n2" \
            "! arping -c2 -w3 -I n2 -s $near_n2 $APP_N6 && ! arping -c2 -w3 -I n2 -s $near_n2 $APP_OAM"
    fi
    if [ "$WITH_ROUTER" = true ] && [ "$WITH_APP" = true ]; then
        check "$NODE_RANN" "N3 -> app N6 is routed (hop 1 r-site, hop 2 app)" \
            "$PING -I $near_n3 $APP_N6 && t=\$(traceroute -I -n -q2 -w2 -s $near_n3 $APP_N6 | awk 'NR>1{print \$2}' | tr '\n' ' '); echo \"\$t\"; [ \"\$t\" = '$GW_N3 $APP_N6 ' ]"
    fi
    if [ "$WITH_APP" != true ]; then
        check "$NODE_RANN" "responder address $APP_N6 on n6, UE pool routed back via $POOL_RETURN_VIA" \
            "ip -4 -o addr show dev n6 | grep -q 'inet $APP_N6/' && ip route get $(ip_in "$TESTBED_UE_POOL" 1) | grep -q 'via $POOL_RETURN_VIA dev n6'"
    fi
    if [ "$LEGACY_NET" = true ]; then
        check "$NODE_RANN" "UPF access subnet $TESTBED_LEGACY_ACCESS_NET routed via the core's N3 $CORE_N3" \
            "ip route get $UPF_N3 | grep -q 'via $CORE_N3 dev n3'"
    fi

    if [ "$WITH_APP" = true ] && [ "$WITH_ROUTER" = true ]; then
        check "$NODE_APP" "ping r-site N6 $GW_N6 and the Internet" "$PING $GW_N6 && $PING 8.8.8.8"
        check "$NODE_APP" "UE pool routed back via r-site" "ip route get $(ip_in "$TESTBED_UE_POOL" 1) | grep -q 'via $GW_N6 dev n6'"
    elif [ "$WITH_APP" = true ]; then
        check "$NODE_APP" "Internet via the uplink" "$PING 8.8.8.8"
        check "$NODE_APP" "UE pool routed back via the UPF N6 $UPF_N6" "ip route get $(ip_in "$TESTBED_UE_POOL" 1) | grep -q 'via $UPF_N6 dev n6'"
    fi

    # The echo peer is installed by `app`; before that the unit is absent and
    # the assertion says so.
    check "$NODE_RESP" "UDP echo peer $ECHO_PEER answers (orbit-n6-echo)" \
        "systemctl is-active -q orbit-n6-echo || { echo 'orbit-n6-echo is not active; run the app step'; exit 1; }; $(echo_probe)"

    if [ "$WITH_FAR" = true ]; then
        check "$NODE_WANEM" "ping r-site OAM and the Internet" "$PING $GW_OAM && $PING 8.8.8.8"
        check "$NODE_WANEM" "br-wan has no address and two ports" \
            "! ip -o addr show dev br-wan | grep -q inet; ip -br link show master br-wan | grep -c . | grep -qx 2"
        check "$NODE_WANEM" "wanemd active, gRPC :6201 listening" "systemctl is-active -q wanemd && ss -ltn | grep -q :6201"
    fi

    echo "---- $CHECK_PASS passed, $CHECK_FAIL failed"
    return "$CHECK_FAIL"
}

# ── verify: the whole path, with ORBIT ────────────────────────────────────────
#
# From each RAN node in turn: NG Setup over N2, a registration with a PDU
# session (5G-AKA, NAS security, SMF/UPF session), a ping from the UE through
# N3 -> UPF -> N6 to the app, a 5-second traffic flow to the responder, a
# 2-second one to the UDP echo peer, and a deregistration. The far node does it two routed hops from the core, which is
# the case that exposed UPF downlink routing (sdcore-lab UPF-5).
verify_from() {
    local node=$1 label=$2 gnb=$3 n3=$4 supi=$5 rc=0
    # Fresh gNB IDs per run: the AMF does not cleanly re-key a reused gNB ID
    # from a new association. Commands run with /etc/orbit/env loaded so the
    # PLMN and keys come from the node's own configuration; the API-client
    # commands default to 127.0.0.1:8412, and the server listens on OAM, so
    # --server is passed explicitly.
    local orbit="set -a; . /etc/orbit/env; set +a; timeout 120 /usr/local/bin/orbit --server \$ORBIT_SERVER"
    echo "### $label ($node): gNB $gnb, N3 $n3, SUPI $supi"
    echo "--- NG Setup"
    lxc exec "$node" -- bash -c "$orbit cell ngsetup --amf \$TESTBED_AMF --mcc \$ORBIT_MCC --mnc \$ORBIT_MNC --tac \$TESTBED_TAC --sst \$TESTBED_SST --sd \$TESTBED_SD --gnb-id $gnb" 2>&1 | tail -2 || rc=1
    echo "--- register + PDU session"
    lxc exec "$node" -- bash -c "$orbit ue register --amf \$TESTBED_AMF --supi $supi --ki \$ORBIT_KI --opc \$ORBIT_OPC --mcc \$ORBIT_MCC --mnc \$ORBIT_MNC --sst \$TESTBED_SST --sd \$TESTBED_SD --dnn \$TESTBED_DNN --gnb-id $((gnb + 1)) --gnb-n3 $n3 --pdu-session" 2>&1 | tail -3 || rc=1
    echo "--- UE ping $APP_N6 (N3 -> UPF -> N6)"
    # The command exits 0 whatever the reply count; 0 replies after a
    # successful registration means the user plane is broken in one
    # direction, which is a failure here. On the near site that is UPF-5
    # (the UPF never programs on-link routes into BESS) until it is fixed.
    local pong; pong=$(lxc exec "$node" -- bash -c "$orbit ue ping --supi $supi --dst $APP_N6 --count 5" 2>&1 | tail -1)
    echo "$pong"
    if ! printf '%s' "$pong" | grep -qE '^[1-9][0-9]*/[0-9]+ replies'; then
        echo "FAIL: no downlink to this gNB${label:+ — $label}; a registered UE with no replies is a UPF routing fault (sdcore-lab UPF-5 for an on-link gNB)"
        rc=1
    fi
    echo "--- traffic 50 Mbps for 5 s to the responder"
    lxc exec "$node" -- bash -c "$orbit ue traffic --supi $supi --target \$TESTBED_RESPONDER --duration-ms 5000 --rate 50Mbps" 2>&1 | tail -2 || rc=1
    echo "--- UDP to the echo peer $ECHO_PEER (the conformance checks' --traffic) for 2 s"
    lxc exec "$node" -- bash -c "$orbit ue traffic --supi $supi --target \$TESTBED_ECHO --duration-ms 2000 --rate 1Mbps" 2>&1 | tail -2 || rc=1
    echo "--- deregister"
    lxc exec "$node" -- bash -c "$orbit ue deregister --supi $supi" 2>&1 | tail -1 || rc=1
    echo
    return $rc
}

cmd_verify() {
    require_lxd
    local which=${1:-all} base supi0 rc=0
    # gNB IDs are 22 bits; a per-run base keeps re-runs from reusing one.
    base=$(( ($(date +%s) % 100000) + 1000 ))
    supi0=$TESTBED_SUB_START
    local w=${#supi0}
    local supi_near supi_far
    supi_near=$(printf '%0*d' "$w" "$((10#$supi0))")
    supi_far=$(printf '%0*d' "$w" "$((10#$supi0 + 1))")
    if [ "$which" = all ] || [ "$which" = near ]; then
        verify_from "$NODE_RANN" "near site, on-link with the UPF" "$base" "$(ip_in "$TESTBED_NET_N3" "$TESTBED_HOST_RAN")" "$supi_near" || rc=1
    fi
    if [ "$which" = far ] && [ "$WITH_FAR" != true ]; then die "this testbed has no far site"; fi
    if [ "$WITH_FAR" = true ] && { [ "$which" = all ] || [ "$which" = far ]; }; then
        verify_from "$NODE_RANF" "far site, two routed hops" "$((base + 100))" "$(ip_in "$TESTBED_NET_FAR_N3" "$TESTBED_HOST_RAN")" "$supi_far" || rc=1
    fi
    [ $rc -eq 0 ] && info "verify: every step passed on every site" \
                  || warn "verify: a step failed; see above"
    return $rc
}

# Everything, in order, from nothing to a testbed ready to exercise.
cmd_all() {
    cmd_image
    cmd_up
    # The control node comes up before the core: it is what deploys and manages
    # the core, and distributing its key is a precondition for it reaching any
    # node at all.
    if [ "$HAS_CTRL" = true ]; then
        cmd_ctrl
        cmd_keys
    fi
    if [ "$WITH_FAR" = true ]; then cmd_wanem; fi
    cmd_core
    cmd_apps
    echo
    cmd_check || true
    echo
    info "testbed ready — '$PROG verify' drives ORBIT through it$(if [ "$WITH_FAR" = true ]; then echo " from both sites"; fi)"
    cmd_status
}

# ── status / access / teardown ───────────────────────────────────────────────

cmd_status() {
    require_lxd
    echo "networks (802.1Q switches; no host address)"
    lxc network list --format csv 2>/dev/null | awk -F, -v p="^$P-" '$1 ~ p {printf "  %-14s %s\n", $1, $NF}'
    echo
    echo "nodes"
    local n
    for n in "${NODES[@]}"; do
        local st; st=$(lxc list "^$n\$" -c s --format csv 2>/dev/null); st=${st:-ABSENT}
        if [ "$WITH_ROUTER" = true ]; then
            printf '  %-16s %-8s oam %s\n' "$n" "$st" "$(oam_of "$n")"
        else
            local a; a=$(oam_of "$n")
            printf '  %-16s %-8s uplink %s\n' "$n" "$st" "${a:--}"
        fi
    done
    echo
    echo "access:  lxc exec <node> -- bash;  $PROG console <node>  (autologin root)"
    if [ -n "${TESTBED_SSH_PUBKEY:-}" ]; then
        if [ "$WITH_ROUTER" = true ]; then echo "         $PROG ssh <node>  (via $NODE_RSITE)"; else echo "         $PROG ssh <node>"; fi
    fi
    echo "node is one of: $(node_names ' ')"
    true
}

node_for() {
    local n
    case "$1" in
        rsite|r-site) n=$NODE_RSITE ;;
        wanem)        n=$NODE_WANEM ;;
        rran|r-ran)   n=$NODE_RRAN ;;
        ctrl)         n=$NODE_CTRL ;;
        core)         n=$NODE_CORE ;;
        ran-near|near|ran) n=$NODE_RANN ;;
        ran-far|far)  n=$NODE_RANF ;;
        app)          n=$NODE_APP ;;
        *) die "unknown node '$1' (expected $(node_names ', ' ' or '))" ;;
    esac
    in_nodes "$n" || die "this testbed has no '$1' node (it has $(node_names ', '))"
    echo "$n"
}

cmd_console() {
    [ $# -ge 1 ] || die "usage: $PROG console <node>"
    local n; n=$(node_for "$1")
    echo "(detach with ctrl-a q)" >&2
    lxc console "$n"
}

# The host has no address on any site VLAN (the bridges are switches), so SSH
# goes through the site router's uplink address as a jump host.
cmd_ssh() {
    [ $# -ge 1 ] || die "usage: $PROG ssh <node> [cmd]"
    local n jump; n=$(node_for "$1"); shift
    if [ "$WITH_ROUTER" != true ]; then
        # The uplink address is on a network the host is on too.
        jump=$(oam_of "$n")
        [ -n "$jump" ] || die "$n has no uplink address yet"
        ssh -o StrictHostKeyChecking=accept-new "ubuntu@$jump" "$@"
        return
    fi
    jump=$(lxc exec "$NODE_RSITE" -- sh -c 'ip -4 -o addr show dev uplink | awk "{print \$4}" | cut -d/ -f1' 2>/dev/null | tr -d '\r')
    [ -n "$jump" ] || die "$NODE_RSITE has no uplink address yet"
    if [ "$n" = "$NODE_RSITE" ]; then
        ssh -o StrictHostKeyChecking=accept-new "ubuntu@$jump" "$@"
    else
        ssh -o StrictHostKeyChecking=accept-new -J "ubuntu@$jump" "ubuntu@$(oam_of "$n")" "$@"
    fi
}

cmd_down() {
    require_lxd
    local n
    for n in "${ALL_NODES[@]}"; do
        if exists_instance "$n"; then
            info "deleting $n"
            lxc_do delete --force "$n" >/dev/null
        fi
    done
    exists_profile "$PROFILE" && { info "deleting profile $PROFILE"; lxc profile delete "$PROFILE" >/dev/null; }
    for n in "${ALL_NETS[@]}"; do
        exists_net "$n" && { info "deleting network $n"; lxc network delete "$n" >/dev/null; }
    done
    if [ "${1:-}" = "--image" ] && exists_image "$IMAGE_ALIAS"; then
        info "deleting image $IMAGE_ALIAS"
        lxc image delete "$IMAGE_ALIAS" >/dev/null
    fi
    info "done — nothing prefixed '$P-' remains ($TESTBED_UPLINK_NET was not touched)"
}

usage() {
    local ctrl_help="" wanem_help="" all_steps="image + up"
    if [ "$WITH_FAR" = true ]; then
        wanem_help="
  $PROG wanem              build wanemd on this host and install it on the
                                WAN emulator node"
    fi
    if [ "$HAS_CTRL" = true ]; then
        all_steps+=" + ctrl + keys"
        ctrl_help="
  $PROG ctrl               install aether-ops on the control node
  $PROG keys               authorize the control node's managed SSH key on
                                the core, RAN and app nodes (aether-ops
                                cannot do this itself — see AO-2 in its
                                KNOWN_ISSUES.md)"
    fi
    if [ "$WITH_FAR" = true ]; then all_steps+=" + wanem"; fi
    all_steps+=" + core + apps + check"
    local sites="each site" ran_args="[near|far]" ran_help="install ORBIT on the RAN node(s) (default both)"
    if [ "$WITH_FAR" != true ]; then sites="the RAN node"; ran_args="          "; ran_help="install ORBIT on the RAN node"; fi
    local up_help="create networks and nodes, start them (r-site first)" ssh_help="ssh to a node via r-site (needs TESTBED_SSH_PUBKEY"
    if [ "$WITH_ROUTER" != true ]; then up_help="create the network and nodes, start them"; ssh_help="ssh to a node (needs TESTBED_SSH_PUBKEY"; fi
    local shape="site"
    if [ "$WITH_ROUTER$WITH_FAR$WITH_APP$WITH_CTRL" != truetruetruetrue ]; then
        shape="router=$WITH_ROUTER far=$WITH_FAR app=$WITH_APP ctrl=$WITH_CTRL"
        if [ "$WITH_ROUTER$WITH_FAR$WITH_APP$WITH_CTRL" = falsefalsefalsefalse ]; then shape="minimal (core + RAN on N2/N3/N6)"; fi
    fi
    cat <<EOF
$PROG — LXD testbed for ORBIT in the Ref 2 small-site shape
            (VLAN-trunked OAM/N2/N3/N6, site routers, routed UE pool, far site over a WAN)

  $PROG image              cache the base image locally (once)
  $PROG up                 $up_help
  $PROG status             show networks, nodes and addresses
  $PROG check              assert the site routes as designed (gateways, hops,
                                NAT, isolation); exit status = failures$ctrl_help$wanem_help
  $PROG core               deploy the core ($CORE_NAME) onto the core node
  $PROG core-status        show the core's health, the core node's interfaces
                                and where the UPF is attached
  $PROG app                install the ORBIT responder on the app node (N6)
  $PROG ran $ran_args     $ran_help
  $PROG apps               app + ran
  $PROG verify $ran_args  drive ORBIT through the whole path from $sites:
                                NG Setup, registration + PDU session, UE ping,
                                traffic, deregistration
  $PROG all                $all_steps
  $PROG reprovision        restore the subscriber population on a deployed
                                core without redeploying it
  $PROG addrs [node...]    re-apply addressing to running nodes, attaching any
                                NIC whose network was removed (default: both
                                RAN nodes)
  $PROG console <node>     attach to a node's console (autologin root)
  $PROG ssh <node> [cmd]   $ssh_help
                                at build time)
  $PROG down [--image]     remove everything this script created
  $PROG networks           create only the networks

node is one of: $(node_names ', ')
Every command takes a profile first: $(basename "$0") -p <profile> <command>
  profiles:$(for f in "$HERE"/profiles/*.conf; do [ -f "$f" ] && printf ' %s' "$(basename "$f" .conf)"; done)  (none: the default testbed)
This testbed: ${TESTBED_PROFILE:+profile '$TESTBED_PROFILE', }prefix '$P', core $CORE_NAME (TESTBED_CORE_KIND=$CORE_KIND), shape $shape.
Configuration: testbed.conf, overridden by a profile, then testbed.local.conf
(testbed.<profile>.local.conf for one profile), then the environment.
See README.md.
EOF
}

# A core file that leaves part of the contract out fails here, not half-way
# through a deploy.
for fn in deploy status reprovision keys versions; do
    declare -F "core_${CORE_KIND}_$fn" >/dev/null \
        || die "core kind '$CORE_KIND' does not define core_${CORE_KIND}_$fn (see cores/$CORE_KIND.sh)"
done

case "${1:-}" in
    image)    shift; cmd_image "$@" ;;
    up)       shift; cmd_up "$@" ;;
    networks) shift; cmd_networks "$@" ;;
    status)   shift; cmd_status "$@" ;;
    check)    shift; cmd_check "$@" ;;
    ctrl)     shift; cmd_ctrl "$@" ;;
    keys)     shift; cmd_keys "$@" ;;
    wanem)    shift; cmd_wanem "$@" ;;
    core)     shift; cmd_core "$@" ;;
    reprovision) shift; cmd_reprovision "$@" ;;
    core-status) shift; cmd_core_status "$@" ;;
    app)      shift; cmd_app "$@" ;;
    ran)      shift; cmd_ran "$@" ;;
    addrs)    shift; cmd_addrs "$@" ;;
    apps)     shift; cmd_apps "$@" ;;
    verify)   shift; cmd_verify "$@" ;;
    all)      shift; cmd_all "$@" ;;
    console)  shift; cmd_console "$@" ;;
    ssh)      shift; cmd_ssh "$@" ;;
    down)     shift; cmd_down "$@" ;;
    ""|-h|--help|help) usage ;;
    *) die "unknown command '$1' (try '$PROG help')" ;;
esac
