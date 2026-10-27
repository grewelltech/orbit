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
: "${TESTBED_LOCAL_CONF:=$HERE/testbed.local.conf}"
if [ -f "$TESTBED_LOCAL_CONF" ]; then
    # shellcheck disable=SC1090
    source "$TESTBED_LOCAL_CONF"
fi
# shellcheck source=testbed.conf
source "$HERE/testbed.conf"

P="$TESTBED_PREFIX"
# LXD bridges are Linux interfaces, capped at 15 characters; `-ftrunk` is the
# longest suffix below.
[ ${#P} -le 8 ] || { echo "$PROG: TESTBED_PREFIX '$P' is longer than 8 characters" >&2; exit 1; }

# Four bridges with no address, no DHCP and no NAT: they are 802.1Q switches,
# and every gateway lives on a router VM. The site trunk carries the core-site
# VLANs, the far trunk the far-site VLANs, and the two WAN segments are the
# cables on either side of the WAN emulator.
NET_TRUNK="$P-trunk"; NET_FTRUNK="$P-ftrunk"; NET_WAN_A="$P-wan-a"; NET_WAN_B="$P-wan-b"
NETS=("$NET_TRUNK" "$NET_FTRUNK" "$NET_WAN_A" "$NET_WAN_B")

NODE_RSITE="$P-rsite"; NODE_WANEM="$P-wanem"; NODE_RRAN="$P-rran"
NODE_CTRL="$P-ctrl"; NODE_CORE="$P-core"; NODE_RANN="$P-ran-near"; NODE_RANF="$P-ran-far"; NODE_APP="$P-app"
# Every node, in the order a status table should read them: the path off the
# testbed first, then the operator's entry point, the core under test, and the
# load that exercises it.
NODES=("$NODE_RSITE" "$NODE_WANEM" "$NODE_RRAN" "$NODE_CTRL" "$NODE_CORE" "$NODE_RANN" "$NODE_RANF" "$NODE_APP")
PROFILE="$P"
IMAGE_ALIAS="$P-base"

V_OAM=$TESTBED_VLAN_OAM; V_N2=$TESTBED_VLAN_N2; V_N3=$TESTBED_VLAN_N3; V_N6=$TESTBED_VLAN_N6
V_WAN=$TESTBED_VLAN_WAN; V_FN2=$TESTBED_VLAN_FAR_N2; V_FN3=$TESTBED_VLAN_FAR_N3; V_FOAM=$TESTBED_VLAN_FAR_OAM

die()  { echo "$PROG: $*" >&2; exit 1; }
# Progress goes to stderr: some of these functions return a value on stdout,
# and a stray progress line would be captured as part of it.
info() { echo "==> $*" >&2; }
warn() { echo "$PROG: warning: $*" >&2; }

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
    lxc_do network create "$name" ipv4.address=none ipv6.address=none >/dev/null
}

cmd_networks() {
    require_lxd
    exists_net "$TESTBED_UPLINK_NET" \
        || die "uplink network '$TESTBED_UPLINK_NET' not found — it is the site router's path to the Internet (set TESTBED_UPLINK_NET)"
    make_net "$NET_TRUNK"  "core-site 802.1Q trunk: VLANs $V_OAM-$V_N6 (oam, n2, n3, n6)"
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
nics_for() {
    case "$1" in
        "$NODE_RSITE") echo "uplink $TESTBED_UPLINK_NET -"
                       echo "trunk $NET_TRUNK t:$V_OAM,$V_N2,$V_N3,$V_N6"
                       echo "wanlink $NET_WAN_A t:$V_WAN" ;;
        "$NODE_WANEM") echo "trunk $NET_TRUNK t:$V_OAM"
                       echo "wan-a $NET_WAN_A u:$V_WAN"
                       echo "wan-b $NET_WAN_B u:$V_WAN" ;;
        "$NODE_RRAN")  echo "wanlink $NET_WAN_B t:$V_WAN"
                       echo "trunk $NET_FTRUNK t:$V_FN2,$V_FN3,$V_FOAM" ;;
        "$NODE_CTRL")  echo "trunk $NET_TRUNK t:$V_OAM" ;;
        "$NODE_CORE")  echo "trunk $NET_TRUNK t:$V_OAM,$V_N2,$V_N3,$V_N6" ;;
        "$NODE_RANN")  echo "trunk $NET_TRUNK t:$V_OAM,$V_N2,$V_N3" ;;
        "$NODE_RANF")  echo "trunk $NET_FTRUNK t:$V_FN2,$V_FN3,$V_FOAM" ;;
        "$NODE_APP")   echo "trunk $NET_TRUNK t:$V_OAM,$V_N6" ;;
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
        printf '    dhcp4: true\n'
    else
        printf '    dhcp4: false\n    link-local: []\n'
    fi
    printf '    dhcp6: false\n    accept-ra: false\n'
}

# vlan_block <name> <id> <link> <addresses|""> <default-gw|""> ["to via"]...
# Only an interface with a default route gets resolvers: a node has exactly one
# path off the testbed, and the 3GPP reference points are deliberately not it.
vlan_block() {
    local name=$1 id=$2 link=$3 addrs=$4 gw=$5; shift 5
    printf '  %s:\n    id: %s\n    link: %s\n    link-local: []\n' "$name" "$id" "$link"
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
        printf '  br-wan:\n    interfaces: [wan-a, wan-b]\n    dhcp4: false\n    dhcp6: false\n    accept-ra: false\n    link-local: []\n    parameters:\n      stp: false\n      forward-delay: 0\n'
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
            # n3 and n6 are UP with no address: they are the macvlan parents
            # for the UPF pod. The far site's N2 is reached via the N2 gateway
            # so the AMF answers far gNBs from its N2 address, not via OAM.
            vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" "$TESTBED_HOST_CORE")" "$GW_OAM"
            vlan_block n2  "$V_N2"  trunk "$(cidr_in "$n2" "$TESTBED_HOST_CORE")" "" "$fn2 $GW_N2"
            vlan_block n3  "$V_N3"  trunk "" ""
            vlan_block n6  "$V_N6"  trunk "" "" ;;
        "$NODE_RANN")
            vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" "$TESTBED_HOST_RAN")" "$GW_OAM"
            vlan_block n2  "$V_N2"  trunk "$(addr_list "$n2" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" ""
            vlan_block n3  "$V_N3"  trunk "$(addr_list "$n3" "$TESTBED_HOST_RAN" "$TESTBED_RAN_GNBS")" "" ;;
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
            vlan_block oam "$V_OAM" trunk "$(cidr_in "$oam" "$TESTBED_HOST_APP")" "$GW_OAM"
            vlan_block n6  "$V_N6"  trunk "$(cidr_in "$n6" "$TESTBED_HOST_APP")" "" "$TESTBED_UE_POOL $GW_N6" ;;
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
" ;;
        "$NODE_RANN"|"$NODE_RANF") packages+=" iputils-arping" ;;
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
            lxc list "^$n\$" -c 4 --format csv 2>/dev/null | grep -qF "$(oam_of "$n")" && ready=$((ready + 1))
        done
        [ "$ready" -eq ${#NODES[@]} ] && break
        sleep 5
    done
    [ "$ready" -eq ${#NODES[@]} ] || warn "not every node reported its address; check '$PROG status' and the consoles"

    # The control node's toolchain download is the slow one and `ctrl` waits
    # for it itself; everything else is a package install.
    for n in "${NODES[@]}"; do
        [ "$n" = "$NODE_CTRL" ] && continue
        wait_cloud_init "$n"
    done
    echo
    cmd_status
}

# oam_of <node> -> the node's management address (far-site nodes are on the
# far OAM VLAN; the routers answer on their first segment).
oam_of() {
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
# deliberately 0: they are the UPF's macvlan parents.
expect_for() {
    local g=$TESTBED_RAN_GNBS
    case "$1" in
        "$NODE_RSITE") printf 'oam 1\nn2 1\nn3 1\nn6 1\nwan 1\n' ;;
        "$NODE_WANEM"|"$NODE_CTRL") printf 'oam 1\n' ;;
        "$NODE_RRAN")  printf 'wan 1\noam 1\nn2 1\nn3 1\n' ;;
        "$NODE_CORE")  printf 'oam 1\nn2 1\nn3 0\nn6 0\n' ;;
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
    [ ${#nodes[@]} -gt 0 ] || nodes=("$NODE_RANN" "$NODE_RANF")
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

# ── core (SD-Core via OnRamp) ────────────────────────────────────────────────

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

cmd_core() {
    require_lxd
    exists_instance "$NODE_CORE" || die "$NODE_CORE not found — run '$PROG up' first"
    check_ue_pool_fits

    info "installing OnRamp prerequisites on $NODE_CORE"
    on_core "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git make ansible >/dev/null" \
        || die "could not install prerequisites (does $NODE_CORE reach the Internet via $NODE_RSITE? try '$PROG check')"

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

    # Empty SDCORE_CHART_VERSION keeps whatever OnRamp pins; the whole core:
    # block is replaced below, so the pin has to be read out first.
    local chart=$SDCORE_CHART_VERSION
    if [ -z "$chart" ]; then
        chart=$(on_core "cd $TESTBED_ONRAMP_DIR && sed -n '/^core:/,/^gnbsim:/p' vars/main.yml | sed -n 's/^ *chart_version: *\([^ #]*\).*/\1/p' | head -1" | tr -d '\r')
        [ -n "$chart" ] || die "could not read OnRamp's chart_version pin; set SDCORE_CHART_VERSION"
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
    # near site's gNBs are on-link.
    on_core "cat > /tmp/onramp-core.yml <<'YML'
core:
  values_file: \"$vf_dir/$vf\"
  networks:
    access:
      iface: n3
      subnet: $TESTBED_NET_N3
      gateway: $GW_N3                 # the site router, not this host
      attach: macvlan
      ran_subnet: $TESTBED_NET_FAR_N3   # far-site gNBs; near-site gNBs are on-link
    core:
      iface: n6
      subnet: $TESTBED_NET_N6
      gateway: $GW_N6
      attach: macvlan
  helm:
    local_charts: false
    chart_ref: oci://ghcr.io/omec-project/sd-core
    chart_version: $chart
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
cd $TESTBED_ONRAMP_DIR && python3 - <<'PY'
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

    info "UPF networking as configured:"
    # Everything a wrong deployment turns on, printed before the 15 minutes
    # are spent: which VLAN interface carries which plane, the gateways, the
    # UPF's own addresses and the UE pool. OnRamp prints what it resolved from
    # this ("show the resolved UPF networking") as the install starts.
    on_core "cd $TESTBED_ONRAMP_DIR && sed -n '/^  networks:/,/^  amf:/p' vars/main.yml | sed 's/^/    /'"

    info "installing Kubernetes (slow)"
    on_core "cd $TESTBED_ONRAMP_DIR && make k8s-install" || die "k8s install failed"

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

    cmd_core_status
}

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

# cmd_reprovision repopulates a core whose MongoDB has been emptied.
#
# Mongo's datadir is an emptyDir, which is destroyed when the POD is recreated —
# patching the StatefulSet, deleting the pod, or reinstalling the release. It
# survives a core-node reboot, because the pod object and its directory outlive
# the restart; that was verified rather than assumed. The core then looks
# healthy while every attach fails, because two separate things are gone: the
# subscriber population, and the NRF's registry of which NFs exist.
#
# Cheaper than `testbed.sh core`, which reinstalls the whole helm release.
cmd_reprovision() {
    require_lxd
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

    case "$subs" in
        ''|*[!0-9]*) warn "could not read the subscriber count from MongoDB; check '$PROG core-status'" ;;
        "$TESTBED_SUB_COUNT") info "subscribers provisioned: $subs" ;;
        *) warn "subscribers provisioned: $subs, expected $TESTBED_SUB_COUNT — a short population fails PDU session establishment, not registration" ;;
    esac
}

cmd_core_status() {
    require_lxd
    echo "core pods"
    lxc exec "$NODE_CORE" -- bash -lc 'kubectl -n aether-5gc get pods --no-headers 2>/dev/null' 2>/dev/null | sed 's/^/  /' || echo "  (kubectl not answering)"
    echo
    echo "host interfaces (n3/n6 stay up with no address: they are the UPF's macvlan parents)"
    lxc exec "$NODE_CORE" -- ip -br -4 addr show 2>/dev/null | sed 's/^/  /'
    echo
    echo "UPF attachments"
    lxc exec "$NODE_CORE" -- bash -lc 'kubectl -n aether-5gc exec upf-0 -c bessd -- ip -br -4 addr show 2>/dev/null | grep -E "^(access|core) "' 2>/dev/null | sed 's/^/  /' || echo "  (upf-0 not answering)"
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
    if exists_instance "$NODE_APP"; then
        t=$(lxc exec "$NODE_APP" -- sh -c 'sed -n "s/^ORBIT_RESPONDER_TOKEN=//p" /etc/orbit/env 2>/dev/null' 2>/dev/null | tr -d '\r') || t=""
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
    local ki="" opc="" tpl="$TESTBED_ONRAMP_DIR/deps/5gc/roles/core/templates"
    if exists_instance "$NODE_CORE" && is_running "$NODE_CORE"; then
        # One file, no filename prefixes, first match only: the 4G template
        # beside it carries its own keys.
        ki=$(on_core "cat $tpl/sdcore-5g-values.yaml $tpl/radio-5g-values.yaml 2>/dev/null | grep -m1 -oE '^\s*key: *\"?[0-9a-fA-F]{32}' | grep -oE '[0-9a-fA-F]{32}' | head -1" 2>/dev/null | tr -d '\r') || ki=""
        opc=$(on_core "cat $tpl/sdcore-5g-values.yaml $tpl/radio-5g-values.yaml 2>/dev/null | grep -m1 -oE '^\s*opc: *\"?[0-9a-fA-F]{32}' | grep -oE '[0-9a-fA-F]{32}' | head -1" 2>/dev/null | tr -d '\r') || opc=""
    fi
    if [ -n "$ki" ] && [ "$ki" != "$TESTBED_KI" ]; then warn "deployed Ki differs from TESTBED_KI; using the deployed value"; fi
    if [ -n "$opc" ] && [ "$opc" != "$TESTBED_OPC" ]; then warn "deployed OPc differs from TESTBED_OPC; using the deployed value"; fi
    echo "${ki:-$TESTBED_KI} ${opc:-$TESTBED_OPC}"
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
            echo "ORBIT_LISTEN=$oam:$ORBIT_PORT"
            echo "ORBIT_SERVER=https://$oam:$ORBIT_PORT"
            echo "ORBIT_TOKEN=$(api_token)" ;;
    esac
}

install_orbit_env() {
    local node=$1
    orbit_env_for "$node" > "$TESTBED_CACHE_DIR/env.$node"
    push_file "$node" "$TESTBED_CACHE_DIR/env.$node" /etc/orbit/env 600
    rm -f "$TESTBED_CACHE_DIR/env.$node"
}

cmd_app() {
    require_lxd
    exists_instance "$NODE_APP" || die "$NODE_APP not found — run '$PROG up' first"
    local bin; bin=$(build_orbit)
    push_orbit "$NODE_APP" "$bin"
    install_orbit_env "$NODE_APP"

    # Bound to the N6 address specifically, not 0.0.0.0, and behind a token:
    # the responder is a remotely-aimable traffic generator and has no business
    # on the management network. Return routing to the UE pool is in the
    # node's netplan (via r-site), so there is no route unit here.
    info "starting the responder on $RESPONDER"
    lxc_do exec "$NODE_APP" -- bash -c "cat > /etc/systemd/system/orbit-responder.service <<'EOF'
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
    lxc_do exec "$NODE_APP" -- systemctl is-active orbit-responder >/dev/null \
        && info "responder active" || warn "responder did not come up; check journalctl -u orbit-responder"
}

# cmd_ran [near|far] — ORBIT on one or both RAN nodes (default both).
cmd_ran() {
    require_lxd
    local targets=()
    case "${1:-}" in
        ''|all) targets=("$NODE_RANN" "$NODE_RANF") ;;
        near)   targets=("$NODE_RANN") ;;
        far)    targets=("$NODE_RANF") ;;
        *) die "usage: $PROG ran [near|far]" ;;
    esac
    local bin; bin=$(build_orbit)
    local node
    for node in "${targets[@]}"; do
        exists_instance "$node" || die "$node not found — run '$PROG up' first"
        push_orbit "$node" "$bin"
        install_orbit_env "$node"

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

    check "$NODE_RSITE" "forwarding on, masquerade only on the uplink" \
        "sysctl -n net.ipv4.ip_forward | grep -qx 1 && nft list ruleset | grep -q 'oifname \"uplink\" ip saddr @internal masquerade'"
    check "$NODE_RSITE" "UE pool $TESTBED_UE_POOL routed to the UPF N6 $UPF_N6" \
        "ip route get $(ip_in "$TESTBED_UE_POOL" 1) | grep -q 'via $UPF_N6 dev n6'"
    check "$NODE_RRAN" "forwarding on, no NAT" \
        "sysctl -n net.ipv4.ip_forward | grep -qx 1 && ! nft list ruleset 2>/dev/null | grep -q masquerade"

    local gw; for gw in "$GW_OAM" "$GW_N2" "$GW_N3" "$GW_N6" "$WAN_RSITE"; do
        check "$NODE_CORE" "ping r-site $gw" "$PING $gw"
    done
    check "$NODE_CORE" "near gNB N2 $near_n2 is on-link" \
        "$PING $near_n2 && ip route get $near_n2 | grep -q 'dev n2 src $CORE_N2' && ! ip route get $near_n2 | grep -q via"
    check "$NODE_CORE" "far gNB N2 $far_n2 via r-site, WAN, r-ran" \
        "$PING $far_n2 && t=\$(traceroute -n -q1 -w2 $far_n2 | awk 'NR>1{print \$2}' | tr '\n' ' '); echo \"\$t\"; [ \"\$t\" = '$GW_N2 $WAN_RRAN $far_n2 ' ]"
    check "$NODE_CORE" "1472-byte DF ping to the far site (MTU 1500 end to end)" "ping -c2 -W2 -q -M do -s 1472 $far_n2"
    check "$NODE_CORE" "Internet via r-site NAT" "$PING 8.8.8.8"
    check "$NODE_CORE" "receive aggregation off on trunk (gro, rx-gro-hw)" \
        "ethtool -k trunk | grep -E '^(generic-receive-offload|rx-gro-hw):' | grep -qv ': on'"
    check "$NODE_CORE" "n3/n6 up with no address (macvlan parents)" \
        "! ip -4 -o addr show dev n3 | grep -q inet && ! ip -4 -o addr show dev n6 | grep -q inet && ip -br link show n3 | grep -q UP && ip -br link show n6 | grep -q UP"

    check "$NODE_RANF" "ping the AMF's N2 $CORE_N2" "$PING $CORE_N2"
    check "$NODE_RANF" "ping r-site N3 $GW_N3 (the UPF's gateway)" "$PING $GW_N3"
    check "$NODE_RANF" "every far N2 slot .$s-.$e reaches the AMF" \
        "for i in \$(seq $s $e); do $PING -I $(ip_in "$TESTBED_NET_FAR_N2" '')\$i $CORE_N2 || exit 1; done"
    check "$NODE_RANF" "core N3 subnet routed via r-ran on n3" "ip route get $GW_N3 | grep -q 'via $GW_FN3 dev n3'"
    check "$NODE_RANF" "Internet via two routers" "$PING 8.8.8.8"

    check "$NODE_RANN" "ping r-site OAM and the Internet" "$PING $GW_OAM && $PING 8.8.8.8"
    check "$NODE_RANN" "every near N2 slot .$s-.$e reaches the AMF on-link" \
        "for i in \$(seq $s $e); do $PING -I $(ip_in "$TESTBED_NET_N2" '')\$i $CORE_N2 || exit 1; done"
    # VLAN isolation: the app's N6 address must not answer ARP on N3 or N2, and
    # N3 -> N6 traffic is routed through r-site (hop 1), not switched.
    check "$NODE_RANN" "isolation: no ARP reply for app N6 $APP_N6 on n3" "! arping -c2 -w3 -I n3 -s $near_n3 $APP_N6"
    check "$NODE_RANN" "isolation: no ARP reply for app N6/OAM on n2" \
        "! arping -c2 -w3 -I n2 -s $near_n2 $APP_N6 && ! arping -c2 -w3 -I n2 -s $near_n2 $APP_OAM"
    check "$NODE_RANN" "N3 -> app N6 is routed (hop 1 r-site, hop 2 app)" \
        "$PING -I $near_n3 $APP_N6 && t=\$(traceroute -I -n -q2 -w2 -s $near_n3 $APP_N6 | awk 'NR>1{print \$2}' | tr '\n' ' '); echo \"\$t\"; [ \"\$t\" = '$GW_N3 $APP_N6 ' ]"

    check "$NODE_APP" "ping r-site N6 $GW_N6 and the Internet" "$PING $GW_N6 && $PING 8.8.8.8"
    check "$NODE_APP" "UE pool routed back via r-site" "ip route get $(ip_in "$TESTBED_UE_POOL" 1) | grep -q 'via $GW_N6 dev n6'"

    check "$NODE_WANEM" "ping r-site OAM and the Internet" "$PING $GW_OAM && $PING 8.8.8.8"
    check "$NODE_WANEM" "br-wan has no address and two ports" \
        "! ip -o addr show dev br-wan | grep -q inet; ip -br link show master br-wan | grep -c . | grep -qx 2"
    check "$NODE_WANEM" "wanemd active, gRPC :6201 listening" "systemctl is-active -q wanemd && ss -ltn | grep -q :6201"

    echo "---- $CHECK_PASS passed, $CHECK_FAIL failed"
    return "$CHECK_FAIL"
}

# ── verify: the whole path, with ORBIT ────────────────────────────────────────
#
# From each RAN node in turn: NG Setup over N2, a registration with a PDU
# session (5G-AKA, NAS security, SMF/UPF session), a ping from the UE through
# N3 -> UPF -> N6 to the app, a 5-second traffic flow to the responder, and a
# deregistration. The far node does it two routed hops from the core, which is
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
    if [ "$which" = all ] || [ "$which" = far ]; then
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
    cmd_ctrl
    cmd_keys
    cmd_wanem
    cmd_core
    cmd_apps
    echo
    cmd_check || true
    echo
    info "testbed ready — '$PROG verify' drives ORBIT through it from both sites"
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
        printf '  %-16s %-8s oam %s\n' "$n" "$st" "$(oam_of "$n")"
    done
    echo
    echo "access:  lxc exec <node> -- bash;  $PROG console <node>  (autologin root)"
    [ -n "${TESTBED_SSH_PUBKEY:-}" ] && echo "         $PROG ssh <node>  (via $NODE_RSITE)"
    echo "node is one of: rsite wanem rran ctrl core ran-near ran-far app"
    true
}

node_for() {
    case "$1" in
        rsite|r-site) echo "$NODE_RSITE" ;;
        wanem)        echo "$NODE_WANEM" ;;
        rran|r-ran)   echo "$NODE_RRAN" ;;
        ctrl)         echo "$NODE_CTRL" ;;
        core)         echo "$NODE_CORE" ;;
        ran-near|near|ran) echo "$NODE_RANN" ;;
        ran-far|far)  echo "$NODE_RANF" ;;
        app)          echo "$NODE_APP" ;;
        *) die "unknown node '$1' (expected rsite, wanem, rran, ctrl, core, ran-near, ran-far or app)" ;;
    esac
}

cmd_console() {
    [ $# -ge 1 ] || die "usage: $PROG console <node>"
    echo "(detach with ctrl-a q)" >&2
    lxc console "$(node_for "$1")"
}

# The host has no address on any site VLAN (the bridges are switches), so SSH
# goes through the site router's uplink address as a jump host.
cmd_ssh() {
    [ $# -ge 1 ] || die "usage: $PROG ssh <node> [cmd]"
    local n jump; n=$(node_for "$1"); shift
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
    for n in "${NODES[@]}"; do
        if exists_instance "$n"; then
            info "deleting $n"
            lxc_do delete --force "$n" >/dev/null
        fi
    done
    exists_profile "$PROFILE" && { info "deleting profile $PROFILE"; lxc profile delete "$PROFILE" >/dev/null; }
    for n in "${NETS[@]}"; do
        exists_net "$n" && { info "deleting network $n"; lxc network delete "$n" >/dev/null; }
    done
    if [ "${1:-}" = "--image" ] && exists_image "$IMAGE_ALIAS"; then
        info "deleting image $IMAGE_ALIAS"
        lxc image delete "$IMAGE_ALIAS" >/dev/null
    fi
    info "done — nothing prefixed '$P-' remains ($TESTBED_UPLINK_NET was not touched)"
}

usage() {
    cat <<EOF
$PROG — LXD testbed for ORBIT in the Ref 2 small-site shape
            (VLAN-trunked OAM/N2/N3/N6, site routers, routed UE pool, far site over a WAN)

  $PROG image              cache the base image locally (once)
  $PROG up                 create networks and nodes, start them (r-site first)
  $PROG status             show networks, nodes and addresses
  $PROG check              assert the site routes as designed (gateways, hops,
                                NAT, isolation); exit status = failures
  $PROG ctrl               install aether-ops on the control node
  $PROG keys               authorize the control node's managed SSH key on
                                the core, RAN and app nodes (aether-ops
                                cannot do this itself — see AO-2 in its
                                KNOWN_ISSUES.md)
  $PROG wanem              build wanemd on this host and install it on the
                                WAN emulator node
  $PROG core               deploy SD-Core onto the core node via OnRamp
  $PROG core-status        show the core pods, interfaces and UPF attachments
  $PROG app                install the ORBIT responder on the app node (N6)
  $PROG ran [near|far]     install ORBIT on the RAN node(s) (default both)
  $PROG apps               app + ran
  $PROG verify [near|far]  drive ORBIT through the whole path from each site:
                                NG Setup, registration + PDU session, UE ping,
                                traffic, deregistration
  $PROG all                image + up + ctrl + keys + wanem + core + apps + check
  $PROG reprovision        repopulate MongoDB after a mongo or core-node
                                restart (its datadir is an emptyDir, so a
                                restart empties both the subscribers and the
                                NRF registry)
  $PROG addrs [node...]    re-apply addressing to running nodes, attaching any
                                NIC whose network was removed (default: both
                                RAN nodes)
  $PROG console <node>     attach to a node's console (autologin root)
  $PROG ssh <node> [cmd]   ssh to a node via r-site (needs TESTBED_SSH_PUBKEY
                                at build time)
  $PROG down [--image]     remove everything this script created
  $PROG networks           create only the networks

node is one of: rsite, wanem, rran, ctrl, core, ran-near, ran-far, app
Configuration: testbed.conf, overridden by env or testbed.local.conf.
See README.md.
EOF
}

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
