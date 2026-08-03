#!/usr/bin/env bash
#
# Stand up an LXD testbed for ORBIT: three Ubuntu VMs on separated N2/N3/N6
# networks. See README.md for usage, topology and troubleshooting.
#
# Every LXD object created here carries TESTBED_PREFIX, so `down` removes
# exactly what `up` made and nothing else on the host is at risk.

set -euo pipefail

PROG=$(basename "$0")
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=testbed.conf
source "$HERE/testbed.conf"
if [ -n "${TESTBED_LOCAL_CONF:-}" ] && [ -f "$TESTBED_LOCAL_CONF" ]; then
    # shellcheck disable=SC1090
    source "$TESTBED_LOCAL_CONF"
fi

P="$TESTBED_PREFIX"
NET_MGMT="$P-mgmt"; NET_N2="$P-n2"; NET_N3="$P-n3"; NET_N6="$P-n6"
NODE_CORE="$P-core"; NODE_RAN="$P-ran"; NODE_APP="$P-app"
PROFILE="$P"
IMAGE_ALIAS="$P-base"

die()  { echo "$PROG: $*" >&2; exit 1; }
# Progress goes to stderr: some of these functions return a value on stdout,
# and a stray progress line would be captured as part of it.
info() { echo "==> $*" >&2; }
warn() { echo "$PROG: warning: $*" >&2; }

# net_host <cidr> -> the .1 address LXD gives the host on that bridge
net_host() { echo "${1%.*/*}.1/${1##*/}"; }
# net_addr <cidr> <octet> -> a host address inside that subnet
net_addr() { echo "${1%.*/*}.$2"; }

require_lxd() {
    command -v lxc >/dev/null 2>&1 || die "lxc not found — LXD is a prerequisite (see README)"
    lxc info >/dev/null 2>&1 || die "cannot talk to the LXD daemon; is it running and is $USER in the lxd group?"
    lxc info 2>/dev/null | grep -q "qemu" \
        || die "this LXD has no VM (qemu) driver; these nodes are VMs, not containers"
    lxc storage show "$TESTBED_STORAGE_POOL" >/dev/null 2>&1 \
        || die "storage pool '$TESTBED_STORAGE_POOL' not found — set TESTBED_STORAGE_POOL (lxc storage list)"
}

exists_net()      { lxc network show "$1" >/dev/null 2>&1; }
exists_instance() { lxc info "$1" >/dev/null 2>&1; }
exists_profile()  { lxc profile show "$1" >/dev/null 2>&1; }
exists_image()    { lxc image info "$1" >/dev/null 2>&1; }

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

make_net() {
    local name=$1 cidr=$2 desc=$3
    if exists_net "$name"; then
        info "network $name exists, leaving it alone"
        return
    fi
    info "creating network $name ($cidr) — $desc"
    lxc network create "$name" \
        ipv4.address="$(net_host "$cidr")" \
        ipv4.nat="$TESTBED_NAT" \
        ipv6.address=none >/dev/null
}

cmd_networks() {
    require_lxd
    make_net "$NET_MGMT" "$TESTBED_NET_MGMT" "operator access"
    make_net "$NET_N2"   "$TESTBED_NET_N2"   "N2: gNB<->AMF, NGAP/SCTP"
    make_net "$NET_N3"   "$TESTBED_NET_N3"   "N3: gNB<->UPF, GTP-U"
    make_net "$NET_N6"   "$TESTBED_NET_N6"   "N6: UPF<->data network"
}

# ── instances ────────────────────────────────────────────────────────────────

make_profile() {
    exists_profile "$PROFILE" && return
    info "creating profile $PROFILE"
    lxc profile create "$PROFILE" >/dev/null
    lxc profile device add "$PROFILE" root disk pool="$TESTBED_STORAGE_POOL" path=/ >/dev/null
}

cidr_for() {
    case "$1" in
        "$NET_MGMT") echo "$TESTBED_NET_MGMT" ;;
        "$NET_N2")   echo "$TESTBED_NET_N2" ;;
        "$NET_N3")   echo "$TESTBED_NET_N3" ;;
        "$NET_N6")   echo "$TESTBED_NET_N6" ;;
        *) die "unknown network $1" ;;
    esac
}

# Emit this node's netplan. cloud-init writes it to /etc/netplan on first boot,
# so the addressing exists as a file on the node — which is what Aether OnRamp
# and SD-Core read to discover interfaces. A DHCP lease leaves nothing there.
#
# Interfaces are matched by MAC rather than by name so the mapping cannot drift
# from what this script intended; the MACs are read back from LXD after the NICs
# are attached.
netcfg_for() {
    local name=$1; shift
    echo "version: 2"
    echo "ethernets:"
    local i=0 spec net octet cidr
    for spec in "$@"; do
        net=${spec%%:*}; octet=${spec##*:}
        cidr=$(cidr_for "$net")
        echo "  eth$i:"
        echo "    match:"
        echo "      macaddress: $(lxc config get "$name" "volatile.eth$i.hwaddr")"
        echo "    set-name: eth$i"
        echo "    addresses: [$(net_addr "$cidr" "$octet")/${cidr##*/}]"
        # Only management carries a default route and DNS: the 3GPP reference
        # points are deliberately not a path off the testbed.
        if [ "$net" = "$NET_MGMT" ]; then
            echo "    routes:"
            echo "      - to: default"
            echo "        via: $(net_addr "$cidr" 1)"
            echo "    nameservers:"
            echo "      addresses: [$(net_addr "$cidr" 1)]"
        fi
        i=$((i + 1))
    done
}

# make_node <name> <cpu> <mem> <disk> <net:octet>...
make_node() {
    local name=$1 cpu=$2 mem=$3 disk=$4; shift 4
    if exists_instance "$name"; then
        info "instance $name exists, leaving it alone"
        return
    fi
    info "creating $name (${cpu} vCPU, $mem, $disk)"
    lxc init "$IMAGE_ALIAS" "$name" --vm \
        --profile "$PROFILE" \
        -c limits.cpu="$cpu" -c limits.memory="$mem" \
        -d root,size="$disk" >/dev/null 2>&1

    local i=0 spec net octet cidr
    for spec in "$@"; do
        net=${spec%%:*}; octet=${spec##*:}
        cidr=$(cidr_for "$net")
        lxc config device add "$name" "eth$i" nic network="$net" >/dev/null
        printf '      eth%s  %-12s %s\n' "$i" "$net" "$(net_addr "$cidr" "$octet")"
        i=$((i + 1))
    done

    # Set after the NICs exist, so the MACs are known.
    lxc config set "$name" cloud-init.network-config="$(netcfg_for "$name" "$@")"
    if [ -n "${TESTBED_SSH_PUBKEY:-}" ]; then
        [ -f "$TESTBED_SSH_PUBKEY" ] || die "TESTBED_SSH_PUBKEY='$TESTBED_SSH_PUBKEY' not found"
        lxc config set "$name" cloud-init.user-data="$(printf '#cloud-config\nssh_authorized_keys:\n  - %s\n' "$(cat "$TESTBED_SSH_PUBKEY")")"
    fi
    info "$name will write its netplan on first boot"
}

cmd_up() {
    require_lxd
    cmd_networks
    make_profile

    # Interface order is deliberate and matches the reference points each node
    # participates in. mgmt is always eth0 so operator access is predictable.
    make_node "$NODE_CORE" "$TESTBED_CORE_CPU" "$TESTBED_CORE_MEM" "$TESTBED_CORE_DISK" \
        "$NET_MGMT:$TESTBED_HOST_CORE" "$NET_N2:$TESTBED_HOST_CORE" \
        "$NET_N3:$TESTBED_HOST_CORE" "$NET_N6:$TESTBED_HOST_CORE"
    make_node "$NODE_RAN" "$TESTBED_RAN_CPU" "$TESTBED_RAN_MEM" "$TESTBED_RAN_DISK" \
        "$NET_MGMT:$TESTBED_HOST_RAN" "$NET_N2:$TESTBED_HOST_RAN" "$NET_N3:$TESTBED_HOST_RAN"
    make_node "$NODE_APP" "$TESTBED_APP_CPU" "$TESTBED_APP_MEM" "$TESTBED_APP_DISK" \
        "$NET_MGMT:$TESTBED_HOST_APP" "$NET_N6:$TESTBED_HOST_APP"

    local n
    for n in "$NODE_CORE" "$NODE_RAN" "$NODE_APP"; do
        [ "$(lxc info "$n" | awk '/^Status:/{print tolower($2)}')" = "running" ] && continue
        info "starting $n"
        lxc start "$n"
    done

    info "waiting for nodes to report addresses (up to 180s)"
    local deadline=$((SECONDS + 180)) ready=0
    while [ $SECONDS -lt $deadline ]; do
        ready=0
        for n in "$NODE_CORE" "$NODE_RAN" "$NODE_APP"; do
            lxc list "^$n\$" -c 4 --format csv 2>/dev/null | grep -q . && ready=$((ready + 1))
        done
        [ "$ready" -eq 3 ] && break
        sleep 5
    done
    [ "$ready" -eq 3 ] || warn "not every node reported an address; check '$PROG status' and the consoles"
    echo
    cmd_status
}

# ── status / access / teardown ───────────────────────────────────────────────

cmd_status() {
    require_lxd
    echo "networks"
    lxc network list --format csv 2>/dev/null | awk -F, -v p="^$P-" '$1 ~ p {printf "  %-12s %s\n", $1, $4}'
    echo
    echo "nodes"
    lxc list "^$P-" -c ns4 --format csv 2>/dev/null \
        | awk -F, '{printf "  %-14s %-8s %s\n", $1, $2, $3}' \
        || echo "  (none)"
    echo
    echo "access:  $PROG console <core|ran|app>     (autologin root, always works)"
    [ -n "${TESTBED_SSH_PUBKEY:-}" ] && echo "         $PROG ssh <core|ran|app>"
    true
}

node_for() {
    case "$1" in
        core) echo "$NODE_CORE" ;;
        ran)  echo "$NODE_RAN" ;;
        app)  echo "$NODE_APP" ;;
        *)    die "unknown node '$1' (expected core, ran or app)" ;;
    esac
}

cmd_console() {
    [ $# -ge 1 ] || die "usage: $PROG console <core|ran|app>"
    echo "(detach with ctrl-a q)" >&2
    lxc console "$(node_for "$1")"
}

cmd_ssh() {
    [ $# -ge 1 ] || die "usage: $PROG ssh <core|ran|app>"
    local n ip; n=$(node_for "$1")
    ip=$(lxc list "^$n\$" -c 4 --format csv | tr ' ' '\n' | grep -F "$(net_addr "$TESTBED_NET_MGMT" "")" | head -1)
    [ -n "$ip" ] || die "no management address for $n yet"
    shift
    ssh -o StrictHostKeyChecking=accept-new "root@$ip" "$@"
}

cmd_down() {
    require_lxd
    local n
    for n in "$NODE_CORE" "$NODE_RAN" "$NODE_APP"; do
        if exists_instance "$n"; then
            info "deleting $n"
            lxc delete --force "$n" >/dev/null
        fi
    done
    exists_profile "$PROFILE" && { info "deleting profile $PROFILE"; lxc profile delete "$PROFILE" >/dev/null; }
    for n in "$NET_MGMT" "$NET_N2" "$NET_N3" "$NET_N6"; do
        exists_net "$n" && { info "deleting network $n"; lxc network delete "$n" >/dev/null; }
    done
    if [ "${1:-}" = "--image" ] && exists_image "$IMAGE_ALIAS"; then
        info "deleting image $IMAGE_ALIAS"
        lxc image delete "$IMAGE_ALIAS" >/dev/null
    fi
    info "done — nothing prefixed '$P-' remains"
}

usage() {
    cat <<EOF
$PROG — LXD testbed for ORBIT (separated N2/N3/N6)

  $PROG image              cache the base image locally (once)
  $PROG up                 create networks and nodes, then start them
  $PROG status             show networks, nodes and addresses
  $PROG console <node>     attach to a node's console (autologin root)
  $PROG ssh <node> [cmd]   ssh to a node (needs TESTBED_SSH_PUBKEY at build time)
  $PROG down [--image]     remove everything this script created
  $PROG networks           create only the networks

node is one of: core, ran, app
Configuration: testbed.conf, overridden by env or testbed.local.conf.
See README.md.
EOF
}

case "${1:-}" in
    image)    shift; cmd_image "$@" ;;
    up)       shift; cmd_up "$@" ;;
    networks) shift; cmd_networks "$@" ;;
    status)   shift; cmd_status "$@" ;;
    console)  shift; cmd_console "$@" ;;
    ssh)      shift; cmd_ssh "$@" ;;
    down)     shift; cmd_down "$@" ;;
    ""|-h|--help|help) usage ;;
    *) die "unknown command '$1' (try '$PROG help')" ;;
esac
