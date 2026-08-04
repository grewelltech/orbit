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

# The networks are /16 with the reference point encoded in the second octet
# (10.102 = N2 and so on), so an address is built from the network's first two
# octets plus a host part like "0.10". The host part carries the remaining two
# octets, which leaves the third free for growth into large populations.
net_prefix() { echo "$1" | cut -d. -f1,2; }
# net_host <cidr> -> the address LXD gives the host on that bridge
net_host() { echo "$(net_prefix "$1").0.1/${1##*/}"; }
# net_addr <cidr> <host-part> -> an address inside that subnet
net_addr() { echo "$(net_prefix "$1").$2"; }

# label_for <network-name> -> the reference point it carries ("n2", "mgmt", ...)
# Names are prefixed, so stripping the prefix leaves the identity.
label_for() { echo "${1#"$P-"}"; }

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
    local attempt=1
    while :; do
        if timeout "$TESTBED_LXC_TIMEOUT" lxc "$@"; then
            return 0
        fi
        local rc=$?
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
    lxc_do network create "$name" \
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
    lxc_do profile create "$PROFILE" >/dev/null
    lxc_do profile device add "$PROFILE" root disk pool="$TESTBED_STORAGE_POOL" path=/ >/dev/null
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
    local spec net octet cidr
    for spec in "$@"; do
        net=${spec%%:*}; octet=${spec##*:}
        cidr=$(cidr_for "$net")
        local label; label=$(label_for "$net")
        echo "  eth.$label:"
        echo "    match:"
        echo "      macaddress: $(lxc config get "$name" "volatile.eth.$label.hwaddr")"
        echo "    set-name: eth.$label"
        echo "    addresses: [$(net_addr "$cidr" "$octet")/${cidr##*/}]"
        # Only management carries a default route and DNS: the 3GPP reference
        # points are deliberately not a path off the testbed.
        if [ "$net" = "$NET_MGMT" ]; then
            echo "    routes:"
            echo "      - to: default"
            echo "        via: $(net_addr "$cidr" 0.1)"
            echo "    nameservers:"
            echo "      addresses: [$(net_addr "$cidr" 0.1)]"
        fi
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
    lxc_do init "$IMAGE_ALIAS" "$name" --vm \
        --profile "$PROFILE" \
        -c limits.cpu="$cpu" -c limits.memory="$mem" \
        -d root,size="$disk" >/dev/null 2>&1

    local spec net octet cidr
    for spec in "$@"; do
        net=${spec%%:*}; octet=${spec##*:}
        cidr=$(cidr_for "$net")
        local label; label=$(label_for "$net")
        lxc_do config device add "$name" "eth.$label" nic network="$net" >/dev/null
        printf '      %-9s %-12s %s\n' "eth.$label" "$net" "$(net_addr "$cidr" "$octet")"
    done

    # Set after the NICs exist, so the MACs are known.
    lxc_do config set "$name" cloud-init.network-config="$(netcfg_for "$name" "$@")"
    if [ -n "${TESTBED_SSH_PUBKEY:-}" ]; then
        [ -f "$TESTBED_SSH_PUBKEY" ] || die "TESTBED_SSH_PUBKEY='$TESTBED_SSH_PUBKEY' not found"
        lxc_do config set "$name" cloud-init.user-data="$(printf '#cloud-config\nssh_authorized_keys:\n  - %s\n' "$(cat "$TESTBED_SSH_PUBKEY")")"
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
        lxc_do start "$n"
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

# ── core (SD-Core via OnRamp) ────────────────────────────────────────────────

# on_core <cmd...> — run a shell command on the core node as root.
on_core() { lxc_do exec "$NODE_CORE" -- bash -lc "$*"; }

cmd_core() {
    require_lxd
    exists_instance "$NODE_CORE" || die "$NODE_CORE not found — run '$PROG up' first"

    local n2 n3p n6p access_ip core_ip
    n2=$(net_addr "$TESTBED_NET_N2" "$TESTBED_HOST_CORE")
    n3p=$(net_prefix "$TESTBED_NET_N3"); n6p=$(net_prefix "$TESTBED_NET_N6")
    access_ip=$(net_addr "$TESTBED_NET_N3" "$TESTBED_UPF_ACCESS_IP")
    core_ip=$(net_addr "$TESTBED_NET_N6" "$TESTBED_UPF_CORE_IP")

    info "installing OnRamp prerequisites on $NODE_CORE"
    on_core "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git make ansible >/dev/null" \
        || die "could not install prerequisites (is NAT on? see TESTBED_NAT)"

    # OnRamp expects helm on PATH but does not install it — its k8s role only
    # pins a version. The Aether reference host carries a get_helm.sh for the
    # same reason.
    if ! on_core "command -v helm >/dev/null"; then
        info "installing helm (OnRamp requires it but does not provide it)"
        on_core "curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null 2>&1" \
            || die "could not install helm"
    fi

    info "fetching OnRamp${ONRAMP_VERSION:+ at $ONRAMP_VERSION}"
    on_core "rm -rf $TESTBED_ONRAMP_DIR && git clone -q $ONRAMP_REPO $TESTBED_ONRAMP_DIR" \
        || die "could not clone OnRamp from $ONRAMP_REPO"
    if [ -n "${ONRAMP_VERSION:-}" ]; then
        on_core "cd $TESTBED_ONRAMP_DIR && git checkout -q $ONRAMP_VERSION" || die "no such OnRamp ref: $ONRAMP_VERSION"
    fi

    # Ansible runs on the node against itself, so there are no SSH keys to manage.
    info "writing inventory and vars"
    on_core "printf '%s\n' '[all]' 'node1 ansible_host=127.0.0.1 ansible_connection=local ansible_user=root' '' '[master_nodes]' 'node1' '' '[worker_nodes]' '' '[gnbsim_nodes]' '' '[router_nodes]' > $TESTBED_ONRAMP_DIR/hosts.ini"

    # data_iface_n6 does not exist in stock OnRamp; the patched values template
    # below is what consumes it.
    on_core "cd $TESTBED_ONRAMP_DIR && python3 -c \"
import re
p='vars/main.yml'; s=open(p).read()
s=re.sub(r'^  data_iface:.*\$', '  data_iface: eth.n3\\n  data_iface_n6: eth.n6', s, flags=re.M)
s=re.sub(r'^  values_file:.*\$', '  values_file: \\\"deps/5gc/roles/core/templates/radio-5g-values.yaml\\\"', s, flags=re.M)
s=re.sub(r'^  ran_subnet:.*\$', '  ran_subnet: \\\"\\\"', s, flags=re.M)
s=re.sub(r'^    ip: .*\$', '    ip: \\\"$n2\\\"', s, flags=re.M)
s=re.sub(r'^        ue_ip_pool:.*\$', '        ue_ip_pool: \\\"$TESTBED_UE_POOL\\\"', s, flags=re.M)
open(p,'w').write(s)
\""

    info "patching the UPF values template for separated interfaces"
    on_core "cd $TESTBED_ONRAMP_DIR && python3 -c \"
p='deps/5gc/roles/core/templates/radio-5g-values.yaml'; s=open(p).read()
# macvlan makes a virtual child of one parent, which is what allows the
# collapsed model. host-device moves the real NIC into the pod, so each plane
# needs its own interface — that is what makes the separation real.
s=s.replace(chr(123)+chr(123)+chr(32)+chr(39)+'vfioveth'+chr(39)+' if core.upf.mode == '+chr(39)+'dpdk'+chr(39)+' else '+chr(39)+'macvlan'+chr(39)+chr(32)+chr(125)+chr(125), 'host-device')
lines=s.split(chr(10)); seen=False
for i,l in enumerate(lines):
    if l.strip()=='core:': seen=True
    if seen and 'core.data_iface' in l and 'data_iface_n6' not in l:
        lines[i]=l.replace('core.data_iface','core.data_iface_n6'); break
s=chr(10).join(lines)
for a,b in [('gateway: '+chr(123)+chr(123)+' access_gw '+chr(125)+chr(125),'gateway: $n3p.0.1'),
            ('ip: '+chr(123)+chr(123)+' access_ip '+chr(125)+chr(125),'ip: $access_ip/16'),
            ('gateway: '+chr(123)+chr(123)+' core_gw '+chr(125)+chr(125),'gateway: $n6p.0.1'),
            ('ip: '+chr(123)+chr(123)+' core_ip '+chr(125)+chr(125),'ip: $core_ip/16')]:
    s=s.replace(a,b)
open(p,'w').write(s)
\""

    info "provisioning $TESTBED_SUB_COUNT test subscribers from $TESTBED_SUB_START"
    on_core "cd $TESTBED_ONRAMP_DIR && python3 -c \"
import re
p='deps/5gc/roles/core/templates/radio-5g-values.yaml'; s=open(p).read()
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

    info "interface mapping as applied:"
    on_core "cd $TESTBED_ONRAMP_DIR && grep -nE 'data_iface|ue_ip_pool|ran_subnet' vars/main.yml | sed 's/^/    /' && grep -nE 'host-device|data_iface' deps/5gc/roles/core/templates/radio-5g-values.yaml | sed 's/^/    /'"

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

    # Deliberately not 5gc-install: that depends on 5gc-router-install, which
    # assumes the collapsed single-interface model.
    info "installing SD-Core, skipping the router step"
    on_core "cd $TESTBED_ONRAMP_DIR && make 5gc-core-install" || die "SD-Core install failed"

    cmd_core_status
}

cmd_core_status() {
    require_lxd
    echo "core pods"
    lxc exec "$NODE_CORE" -- bash -lc 'kubectl -n aether-5gc get pods --no-headers 2>/dev/null' 2>/dev/null | sed 's/^/  /' || echo "  (kubectl not answering)"
    echo
    echo "host interfaces (eth.n3/eth.n6 leave once the UPF pod owns them)"
    lxc exec "$NODE_CORE" -- ip -br -4 addr show 2>/dev/null | sed 's/^/  /'
}

# ── orbit (reflector on the app node, ORBIT on the RAN node) ─────────────────

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
    lxc_do file push "$bin" "$node/usr/local/bin/orbit" --mode 0755 >/dev/null
}

# A unit rather than a bare process, so the node survives a reboot with the
# service still up and the failure mode is visible in systemctl.
install_unit() {
    local node=$1 name=$2 desc=$3 execstart=$4
    lxc_do exec "$node" -- bash -c "cat > /etc/systemd/system/$name.service <<'EOF'
[Unit]
Description=$desc
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$execstart
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now $name.service" >/dev/null
}

cmd_app() {
    require_lxd
    exists_instance "$NODE_APP" || die "$NODE_APP not found — run '$PROG up' first"
    local bin n6
    bin=$(build_orbit)
    n6=$(net_addr "$TESTBED_NET_N6" "$TESTBED_HOST_APP")
    push_orbit "$NODE_APP" "$bin"

    # Bound to the N6 address specifically, not 0.0.0.0: the responder is a
    # remotely-aimable traffic generator and has no business on the management
    # network.
    # Downlink needs a route back to the UE pool via the UPF's N6 address,
    # otherwise traffic reaches the app server and the replies are dropped —
    # the path looks broken in one direction only.
    local upf_n6; upf_n6=$(net_addr "$TESTBED_NET_N6" "$TESTBED_UPF_CORE_IP")
    info "routing the UE pool ($TESTBED_UE_POOL) back via the UPF at $upf_n6"
    install_unit "$NODE_APP" orbit-ue-route "Route the UE pool back through the UPF" \
        "/sbin/ip route replace $TESTBED_UE_POOL via $upf_n6 dev eth.n6"
    lxc_do exec "$NODE_APP" -- bash -c "sed -i 's|^Restart=on-failure|Type=oneshot\nRemainAfterExit=yes|' /etc/systemd/system/orbit-ue-route.service && systemctl daemon-reload && systemctl restart orbit-ue-route" >/dev/null 2>&1 || true

    info "starting the responder on $n6:9551"
    install_unit "$NODE_APP" orbit-responder "ORBIT responder (loom agent) on N6" \
        "/usr/local/bin/orbit responder --bind $n6:9551"
    lxc_do exec "$NODE_APP" -- systemctl is-active orbit-responder >/dev/null \
        && info "responder active" || warn "responder did not come up; check journalctl -u orbit-responder"
}

cmd_ran() {
    require_lxd
    exists_instance "$NODE_RAN" || die "$NODE_RAN not found — run '$PROG up' first"
    local bin
    bin=$(build_orbit)
    push_orbit "$NODE_RAN" "$bin"

    # The API server binds loopback: every orbit client command runs on this
    # node, and the API carries subscriber credentials.
    info "starting the ORBIT API server"
    install_unit "$NODE_RAN" orbit "ORBIT API server" \
        "/usr/local/bin/orbit serve --listen 127.0.0.1:8412"
    lxc_do exec "$NODE_RAN" -- systemctl is-active orbit >/dev/null \
        && info "orbit serve active" || warn "orbit did not come up; check journalctl -u orbit"
}

cmd_apps() { cmd_app; cmd_ran; }

# Everything, in order, from nothing to a testbed ready to exercise.
cmd_all() {
    cmd_image
    cmd_up
    cmd_core
    cmd_apps
    echo
    info "testbed ready"
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
            lxc_do delete --force "$n" >/dev/null
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
  $PROG core               deploy SD-Core onto the core node via OnRamp
  $PROG core-status        show the core pods and interfaces
  $PROG app                install the ORBIT responder on the app node (N6)
  $PROG ran                install ORBIT on the RAN node
  $PROG apps               both of the above
  $PROG all                image + up + core + apps, from nothing to ready
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
    core)     shift; cmd_core "$@" ;;
    core-status) shift; cmd_core_status "$@" ;;
    app)      shift; cmd_app "$@" ;;
    ran)      shift; cmd_ran "$@" ;;
    apps)     shift; cmd_apps "$@" ;;
    all)      shift; cmd_all "$@" ;;
    console)  shift; cmd_console "$@" ;;
    ssh)      shift; cmd_ssh "$@" ;;
    down)     shift; cmd_down "$@" ;;
    ""|-h|--help|help) usage ;;
    *) die "unknown command '$1' (try '$PROG help')" ;;
esac
