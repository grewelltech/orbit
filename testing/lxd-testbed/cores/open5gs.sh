# Core kind `open5gs`: Open5GS built from source at a pinned ref and run as
# systemd services on the core node's host network, with MongoDB beside it.
#
# Sourced by testbed.sh; the contract is described there under "core kinds".

CORE_NAME="Open5GS"

# Nothing manages this core but testbed.sh: no control node, no `ctrl` or
# `keys` step.
CORE_HAS_CTRL=false

# The UPF runs on the core node's host network, so the core node itself holds
# UPF_N3 on n3 and UPF_N6 on n6.
CORE_UPF_ATTACH=host

# Extra packages cloud-init installs on the core node at first boot. Empty:
# `core` installs the build dependencies itself, so it also converges a node
# that was created before this list existed.
CORE_NODE_PACKAGES=""

# ── Settings ─────────────────────────────────────────────────────────────────
# In testbed.conf's form, so the environment, the local overrides and the
# profile still win.

# Open5GS source and the git ref (tag, branch or sha) built on the core node.
# `core` records the commit it resolved in /etc/orbit/testbed-versions; a node
# already built at that commit is not built again.
: "${OPEN5GS_REPO:=https://github.com/open5gs/open5gs.git}"
: "${OPEN5GS_REF:=v2.8.0}"

# Where the source is cloned and built, where it is installed (binaries under
# bin/, the NF configuration under etc/open5gs/), and where testbed.sh keeps
# its own scripts and records on the core node.
: "${OPEN5GS_SRC_DIR:=/opt/open5gs-src}"
: "${OPEN5GS_PREFIX:=/opt/open5gs}"
: "${OPEN5GS_TESTBED_DIR:=/opt/open5gs-testbed}"

# MongoDB release series, installed from MongoDB's own apt repository (Ubuntu
# 24.04 ships none). The exact version installed is recorded with the core's.
: "${OPEN5GS_MONGODB_SERIES:=8.0}"
: "${OPEN5GS_DB_URI:=mongodb://127.0.0.1/open5gs}"

# NAS algorithm preference, most preferred first. NIA2 and NEA0 lead so a UE
# is given the same algorithms as on the SD-Core testbed.
: "${OPEN5GS_INTEGRITY_ORDER:=NIA2, NIA1, NIA0}"
: "${OPEN5GS_CIPHERING_ORDER:=NEA0, NEA2, NEA1}"

# MTU the SMF signals to UEs and the UPF's tun device carries.
: "${OPEN5GS_UE_MTU:=1400}"

# UE contexts each NF sizes its pools for (Open5GS's own default is 1024,
# which leaves no room beside TESTBED_SUB_COUNT=1000). Raise it with the
# subscriber count.
: "${OPEN5GS_MAX_UE:=4096}"

# Log level of every NF (fatal|error|warn|info|debug|trace). Logs go to the
# journal: `journalctl -u open5gs-amfd` on the core node.
: "${OPEN5GS_LOG_LEVEL:=info}"

# The UPF's tun device. It carries the first address of TESTBED_UE_POOL, which
# is the gateway the SMF and UPF are configured with.
: "${OPEN5GS_TUN:=ogstun}"

# The 5G SA network functions, in start order: the NRF and SCP first (every
# other NF registers through them), the UPF before the SMF that associates
# with it. No EPC daemon (MME, SGW-C/U, HSS, PCRF) is given a unit.
OPEN5GS_NFS="nrf scp udr udm ausf pcf nssf bsf upf smf amf"

# open5gs_sbi_addr <nf> -> the loopback address the NF serves SBI on (and
# PFCP, for the SMF and UPF). Open5GS's own defaults.
open5gs_sbi_addr() {
    case "$1" in
        nrf) echo 127.0.0.10 ;; scp) echo 127.0.0.200 ;; amf) echo 127.0.0.5 ;;
        smf) echo 127.0.0.4 ;;  upf) echo 127.0.0.7 ;;   ausf) echo 127.0.0.11 ;;
        udm) echo 127.0.0.12 ;; pcf) echo 127.0.0.13 ;;  nssf) echo 127.0.0.14 ;;
        bsf) echo 127.0.0.15 ;; udr) echo 127.0.0.20 ;;
    esac
}

# open5gs_conf_head <nf> -> the part of an NF's configuration every NF shares:
# logging, pool sizing, and its SBI server with the SCP as its client (the NRF
# has no client; the SCP's is the NRF).
open5gs_conf_head() {
    local nf=$1
    case "$nf" in udr|pcf) echo "db_uri: $OPEN5GS_DB_URI" ;; esac
    cat <<EOF
logger:
  level: $OPEN5GS_LOG_LEVEL

global:
  max:
    ue: $OPEN5GS_MAX_UE

$nf:
EOF
    [ "$nf" = upf ] && return 0
    if [ "$nf" = nrf ]; then
        cat <<EOF
  serving:
    - plmn_id:
        mcc: $TESTBED_MCC
        mnc: $TESTBED_MNC
EOF
    fi
    cat <<EOF
  sbi:
    server:
      - address: $(open5gs_sbi_addr "$nf")
        port: 7777
EOF
    case "$nf" in
        nrf) ;;
        scp) printf '    client:\n      nrf:\n        - uri: http://%s:7777\n' "$(open5gs_sbi_addr nrf)" ;;
        *)   printf '    client:\n      scp:\n        - uri: http://%s:7777\n' "$(open5gs_sbi_addr scp)" ;;
    esac
}

# open5gs_conf <nf> -> the NF's whole configuration file.
open5gs_conf() {
    local nf=$1 gw i dns
    gw=$(ip_in "$TESTBED_UE_POOL" 1)
    open5gs_conf_head "$nf"
    case "$nf" in
        amf)
            cat <<EOF
  ngap:
    server:
      - address: $CORE_N2
  metrics:
    server:
      - address: $(open5gs_sbi_addr amf)
        port: 9090
  guami:
    - plmn_id:
        mcc: $TESTBED_MCC
        mnc: $TESTBED_MNC
      amf_id:
        region: 2
        set: 1
  tai:
    - plmn_id:
        mcc: $TESTBED_MCC
        mnc: $TESTBED_MNC
      tac: $TESTBED_TAC
  plmn_support:
    - plmn_id:
        mcc: $TESTBED_MCC
        mnc: $TESTBED_MNC
      s_nssai:
        - sst: $TESTBED_SST
          sd: $TESTBED_SD
  security:
    integrity_order: [ $OPEN5GS_INTEGRITY_ORDER ]
    ciphering_order: [ $OPEN5GS_CIPHERING_ORDER ]
  network_name:
    full: Open5GS
    short: Next
  amf_name: open5gs-amf0
  time:
    t3512:
      value: 540
EOF
            ;;
        smf)
            cat <<EOF
  pfcp:
    server:
      - address: $(open5gs_sbi_addr smf)
    client:
      upf:
        - address: $(open5gs_sbi_addr upf)
  gtpu:
    server:
      - address: $(open5gs_sbi_addr smf)
  metrics:
    server:
      - address: $(open5gs_sbi_addr smf)
        port: 9090
  session:
    - subnet: $TESTBED_UE_POOL
      gateway: $gw
      dnn: $TESTBED_DNN
  dns:
EOF
            IFS=', ' read -r -a dns <<<"$TESTBED_DNS"
            for i in "${dns[@]}"; do [ -n "$i" ] && echo "    - $i"; done
            echo "  mtu: $OPEN5GS_UE_MTU"
            ;;
        upf)
            cat <<EOF
  pfcp:
    server:
      - address: $(open5gs_sbi_addr upf)
  gtpu:
    server:
      - address: $UPF_N3
  session:
    - subnet: $TESTBED_UE_POOL
      gateway: $gw
      dnn: $TESTBED_DNN
      dev: $OPEN5GS_TUN
  metrics:
    server:
      - address: $(open5gs_sbi_addr upf)
        port: 9090
EOF
            ;;
        udm)
            # The home network keys Open5GS installs, for SUCI profiles A and B.
            echo "  hnet:"
            for i in 1 2 3 4 5 6; do
                if [ $((i % 2)) -eq 1 ]; then
                    printf '    - id: %s\n      scheme: 1\n      key: %s/etc/open5gs/hnet/curve25519-%s.key\n' "$i" "$OPEN5GS_PREFIX" "$i"
                else
                    printf '    - id: %s\n      scheme: 2\n      key: %s/etc/open5gs/hnet/secp256r1-%s.key\n' "$i" "$OPEN5GS_PREFIX" "$i"
                fi
            done
            ;;
        nssf)
            cat <<EOF
      nsi:
        - uri: http://$(open5gs_sbi_addr nrf):7777
          s_nssai:
            sst: $TESTBED_SST
            sd: $TESTBED_SD
EOF
            ;;
        pcf)
            printf '  metrics:\n    server:\n      - address: %s\n        port: 9090\n' "$(open5gs_sbi_addr pcf)"
            ;;
    esac
}

# open5gs_unit <nf> -> the NF's systemd unit. Restart=always with no exit
# status exempted: an NF that starts before its address or MongoDB is ready
# (at boot) is started again until it stays up.
open5gs_unit() {
    local nf=$1 after="network-online.target" wants="network-online.target"
    case "$nf" in
        udr|pcf) after+=" mongod.service"; wants+=" mongod.service" ;;
        upf)     after+=" systemd-networkd.service" ;;
    esac
    cat <<EOF
[Unit]
Description=Open5GS ${nf^^} (testbed.sh, core kind open5gs)
After=$after
Wants=$wants

[Service]
Type=simple
User=open5gs
Group=open5gs
ExecStart=$OPEN5GS_PREFIX/bin/open5gs-${nf}d -c $OPEN5GS_PREFIX/etc/open5gs/$nf.yaml
Restart=always
RestartSec=2
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
}

# open5gs_subscribers_js -> a mongosh script that upserts TESTBED_SUB_COUNT
# subscribers from TESTBED_SUB_START into Open5GS's `subscribers` collection
# (schema_version 1, the document `open5gs-dbctl add_ue_with_slice` writes),
# one slice with one IPv4 session of 5QI 9, and prints how many it holds.
open5gs_subscribers_js() {
    cat <<EOF
const start = "$TESTBED_SUB_START", count = $TESTBED_SUB_COUNT;
const ki = "$TESTBED_KI", opc = "$TESTBED_OPC";
const sst = $TESTBED_SST, sd = "$TESTBED_SD", dnn = "$TESTBED_DNN";
EOF
    cat <<'EOF'
const imsiAt = (i) => String(Number(start) + i).padStart(start.length, "0");
const ambr = () => ({
    downlink: { value: NumberInt(1), unit: NumberInt(3) },
    uplink: { value: NumberInt(1), unit: NumberInt(3) },
});
const subscriber = (imsi) => ({
    schema_version: NumberInt(1),
    imsi: imsi,
    msisdn: [],
    imeisv: [],
    mme_host: [],
    mm_realm: [],
    purge_flag: [],
    slice: [{
        sst: NumberInt(sst),
        sd: sd,
        default_indicator: true,
        session: [{
            name: dnn,
            type: NumberInt(1),
            qos: {
                index: NumberInt(9),
                arp: {
                    priority_level: NumberInt(8),
                    pre_emption_capability: NumberInt(1),
                    pre_emption_vulnerability: NumberInt(2),
                },
            },
            ambr: ambr(),
            pcc_rule: [],
            _id: new ObjectId(),
        }],
        _id: new ObjectId(),
    }],
    security: { k: ki, op: null, opc: opc, amf: "8000" },
    ambr: ambr(),
    access_restriction_data: 32,
    network_access_mode: 0,
    subscriber_status: 0,
    operator_determined_barring: 0,
    subscribed_rau_tau_timer: 12,
    __v: 0,
});
db.subscribers.createIndex({ imsi: 1 }, { unique: true });
const ops = [];
for (let i = 0; i < count; i++) {
    const imsi = imsiAt(i);
    ops.push({ updateOne: { filter: { imsi: imsi }, update: { $set: subscriber(imsi) }, upsert: true } });
}
db.subscribers.bulkWrite(ops, { ordered: false });
const have = db.subscribers.countDocuments({ imsi: { $gte: imsiAt(0), $lte: imsiAt(count - 1) } });
print("subscribers " + have + " " + db.subscribers.countDocuments({}));
EOF
}

# open5gs_stage <dir> writes everything `core` puts on the core node into
# <dir>, as it will sit under OPEN5GS_TESTBED_DIR: the settings (env), the
# install and apply scripts, the subscriber script, and under root/ the files
# apply.sh copies into place.
open5gs_stage() {
    local d=$1 nf etc="$1/root$OPEN5GS_PREFIX/etc/open5gs"
    mkdir -p "$etc" "$d/root/etc/systemd/system" "$d/root/etc/systemd/network" \
             "$d/root/etc/sysctl.d" "$d/root/etc/modules-load.d"

    for nf in $OPEN5GS_NFS; do
        open5gs_conf "$nf" > "$etc/$nf.yaml"
        open5gs_unit "$nf" > "$d/root/etc/systemd/system/open5gs-${nf}d.service"
    done

    # The tun device and its address belong to systemd-networkd, so they exist
    # before the UPF starts and after a reboot. The address and the pool route
    # stay in place while no UPF holds the device.
    cat > "$d/root/etc/systemd/network/99-open5gs.netdev" <<EOF
[NetDev]
Name=$OPEN5GS_TUN
Kind=tun
EOF
    cat > "$d/root/etc/systemd/network/99-open5gs.network" <<EOF
[Match]
Name=$OPEN5GS_TUN

[Network]
Address=$(ip_in "$TESTBED_UE_POOL" 1)/${TESTBED_UE_POOL##*/}
ConfigureWithoutCarrier=yes
IgnoreCarrierLoss=yes
LinkLocalAddressing=no
IPv6AcceptRA=no

[Link]
MTUBytes=$OPEN5GS_UE_MTU
RequiredForOnline=false
EOF
    # The UPF is a router between the tun device and N6. Nothing is NATed.
    printf 'net.ipv4.ip_forward = 1\n' > "$d/root/etc/sysctl.d/30-open5gs.conf"
    printf 'sctp\n' > "$d/root/etc/modules-load.d/open5gs.conf"

    cat > "$d/env" <<EOF
OPEN5GS_REPO='$OPEN5GS_REPO'
OPEN5GS_REF='$OPEN5GS_REF'
OPEN5GS_SRC_DIR='$OPEN5GS_SRC_DIR'
OPEN5GS_PREFIX='$OPEN5GS_PREFIX'
OPEN5GS_TESTBED_DIR='$OPEN5GS_TESTBED_DIR'
OPEN5GS_MONGODB_SERIES='$OPEN5GS_MONGODB_SERIES'
OPEN5GS_TUN='$OPEN5GS_TUN'
OPEN5GS_TUN_ADDR='$(ip_in "$TESTBED_UE_POOL" 1)/${TESTBED_UE_POOL##*/}'
OPEN5GS_NFS='$OPEN5GS_NFS'
EOF
    open5gs_subscribers_js > "$d/subscribers.js"

    cat > "$d/install.sh" <<'EOF'
#!/usr/bin/env bash
# Installs MongoDB and builds Open5GS at OPEN5GS_REF. Written by testbed.sh.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck disable=SC1091
. "$here/env"
export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
log=$here/build.log

deps="git curl gnupg ca-certificates python3-pip python3-setuptools python3-wheel
      ninja-build build-essential flex bison cmake meson pkg-config libsctp-dev
      libgnutls28-dev libgcrypt20-dev libssl-dev libidn-dev libmongoc-dev libbson-dev
      libyaml-dev libnghttp2-dev libmicrohttpd-dev libcurl4-gnutls-dev libtins-dev
      libtalloc-dev"
missing=""
for p in $deps; do dpkg -s "$p" >/dev/null 2>&1 || missing+=" $p"; done
if [ -n "$missing" ]; then
    echo "installing build dependencies"
    apt-get update -qq </dev/null
    # shellcheck disable=SC2086
    apt-get install -y -qq $missing </dev/null >/dev/null
fi

# The AMF's NGAP socket needs the kernel's SCTP, which the cloud image's
# kernel package may leave out.
if ! modprobe sctp 2>/dev/null; then
    echo "installing linux-modules-extra-$(uname -r) for SCTP"
    apt-get update -qq </dev/null
    apt-get install -y -qq "linux-modules-extra-$(uname -r)" </dev/null >/dev/null
    modprobe sctp
fi

if ! dpkg -s mongodb-org 2>/dev/null | grep -q "^Version: $OPEN5GS_MONGODB_SERIES\\."; then
    echo "installing MongoDB $OPEN5GS_MONGODB_SERIES"
    rm -f /etc/apt/sources.list.d/mongodb-org-*.list
    key=/usr/share/keyrings/mongodb-server-$OPEN5GS_MONGODB_SERIES.gpg
    curl -fsSL "https://www.mongodb.org/static/pgp/server-$OPEN5GS_MONGODB_SERIES.asc" | gpg --dearmor --yes -o "$key"
    # shellcheck disable=SC1091
    codename=$(. /etc/os-release && echo "$VERSION_CODENAME")
    echo "deb [ arch=amd64,arm64 signed-by=$key ] https://repo.mongodb.org/apt/ubuntu $codename/mongodb-org/$OPEN5GS_MONGODB_SERIES multiverse" \
        > "/etc/apt/sources.list.d/mongodb-org-$OPEN5GS_MONGODB_SERIES.list"
    apt-get update -qq </dev/null
    apt-get install -y -qq mongodb-org </dev/null >/dev/null
fi
systemctl enable -q --now mongod

if [ ! -d "$OPEN5GS_SRC_DIR/.git" ]; then
    echo "cloning $OPEN5GS_REPO"
    rm -rf "$OPEN5GS_SRC_DIR"
    git clone -q "$OPEN5GS_REPO" "$OPEN5GS_SRC_DIR"
fi
cd "$OPEN5GS_SRC_DIR"
git remote set-url origin "$OPEN5GS_REPO"
git fetch -q --tags --force origin '+refs/heads/*:refs/remotes/origin/*'
# A branch or a tag is known after the fetch above. A commit no branch or tag
# reaches is asked for by name.
sha=$(git rev-parse -q --verify "origin/$OPEN5GS_REF^{commit}" || git rev-parse -q --verify "$OPEN5GS_REF^{commit}") \
    || { git fetch -q origin "$OPEN5GS_REF" 2>/dev/null && sha=$(git rev-parse -q --verify 'FETCH_HEAD^{commit}'); } \
    || { echo "no such Open5GS ref: $OPEN5GS_REF" >&2; exit 1; }
git checkout -q --detach "$sha"

if [ "$(cat "$OPEN5GS_PREFIX/.testbed-built" 2>/dev/null)" = "$sha" ]; then
    echo "Open5GS $OPEN5GS_REF ($sha) is already built"
else
    echo "building Open5GS $OPEN5GS_REF ($sha); log in $log"
    # A running NF holds its binary open, and the install writes in place.
    for nf in $OPEN5GS_NFS; do systemctl stop "open5gs-${nf}d" 2>/dev/null || true; done
    rm -rf build "$OPEN5GS_PREFIX/.testbed-built"
    {
        meson setup build --prefix="$OPEN5GS_PREFIX"
        ninja -C build
        ninja -C build install
    } > "$log" 2>&1 </dev/null || { tail -40 "$log" >&2; exit 1; }
    echo "$sha" > "$OPEN5GS_PREFIX/.testbed-built"
fi

# What was deployed, as `<component> <ref> <resolved>` lines. The 3GPP release
# is the one this source tree's own feature list claims.
release=$(grep -m1 -oE 'Release-[0-9]+' docs/_pages/features.md 2>/dev/null || echo unknown)
{
    echo "open5gs $OPEN5GS_REF $sha"
    echo "open5gs-3gpp-release $OPEN5GS_REF $release"
    echo "mongodb $OPEN5GS_MONGODB_SERIES $(mongod --version | sed -n 's/^db version v//p')"
} > "$here/versions"
EOF

    cat > "$d/apply.sh" <<'EOF'
#!/usr/bin/env bash
# Puts the staged configuration in place and (re)starts the 5G SA network
# functions. Written by testbed.sh.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck disable=SC1091
. "$here/env"

id open5gs >/dev/null 2>&1 || adduser --system --group --no-create-home --quiet open5gs
cp -r --no-preserve=ownership "$here/root/." /

sysctl -q --system
modprobe sctp

# The tun device: created by systemd-networkd from the files just written. An
# address left from an earlier TESTBED_UE_POOL is dropped.
networkctl reload
for _ in $(seq 1 20); do ip link show "$OPEN5GS_TUN" >/dev/null 2>&1 && break; sleep 0.5; done
ip link show "$OPEN5GS_TUN" >/dev/null 2>&1 || { echo "systemd-networkd did not create $OPEN5GS_TUN" >&2; exit 1; }
if ! ip -4 -o addr show dev "$OPEN5GS_TUN" | grep -q "inet $OPEN5GS_TUN_ADDR "; then
    ip -4 addr flush dev "$OPEN5GS_TUN"
    networkctl reconfigure "$OPEN5GS_TUN"
    for _ in $(seq 1 20); do
        ip -4 -o addr show dev "$OPEN5GS_TUN" | grep -q "inet $OPEN5GS_TUN_ADDR " && break; sleep 0.5
    done
fi
ip -4 -o addr show dev "$OPEN5GS_TUN" | grep -q "inet $OPEN5GS_TUN_ADDR " \
    || { echo "$OPEN5GS_TUN did not get $OPEN5GS_TUN_ADDR" >&2; exit 1; }

# 5G SA only: an EPC daemon left by a packaged install would listen beside
# the AMF and UPF.
for d in mmed sgwcd sgwud hssd pcrfd seppd; do
    systemctl disable -q --now "open5gs-$d" 2>/dev/null || true
done

systemctl daemon-reload
units=""
for nf in $OPEN5GS_NFS; do units+=" open5gs-${nf}d"; done
# shellcheck disable=SC2086
systemctl enable -q $units
# shellcheck disable=SC2086
systemctl stop $units
for nf in $OPEN5GS_NFS; do
    systemctl start "open5gs-${nf}d"
    # Everything registers through the NRF and SCP, so they get a head start.
    case "$nf" in nrf|scp) sleep 1 ;; esac
done
EOF
    chmod 755 "$d/install.sh" "$d/apply.sh"
}

# open5gs_push <stage-dir> replaces OPEN5GS_TESTBED_DIR on the core node with
# the staged tree, keeping the build log and the version record.
open5gs_push() {
    local d=$1 tarball="$1.tar"
    tar -C "$d" -cf "$tarball" . || die "could not pack $d"
    push_file "$NODE_CORE" "$tarball" "$OPEN5GS_TESTBED_DIR.tar"
    on_core "mkdir -p $OPEN5GS_TESTBED_DIR && rm -rf $OPEN5GS_TESTBED_DIR/root && tar -C $OPEN5GS_TESTBED_DIR --no-same-owner -xf $OPEN5GS_TESTBED_DIR.tar && rm -f $OPEN5GS_TESTBED_DIR.tar" \
        || die "could not unpack the staged files on $NODE_CORE"
    rm -rf "$d" "$tarball"
}

# open5gs_wait_ready waits until every NF has stayed active and the AMF and
# UPF are listening on their testbed addresses.
open5gs_wait_ready() {
    local probe="for nf in $OPEN5GS_NFS; do systemctl is-active -q open5gs-\${nf}d || exit 1; done
        grep -qE ' 38412 .*[[:space:]]$CORE_N2([[:space:]]|\$)' /proc/net/sctp/eps || exit 1
        ss -Hlun | grep -q '$UPF_N3:2152 ' || exit 1
        journalctl -u open5gs-smfd -b --no-pager -o cat | tail -200 | grep -q 'PFCP associated' || exit 1"
    local i
    for i in $(seq 1 30); do
        if on_core "$probe" >/dev/null 2>&1; then
            # Still true a moment later: an NF in a restart loop is briefly active.
            sleep 3
            on_core "$probe" >/dev/null 2>&1 && return 0
        fi
        sleep 2
    done
    return 1
}

core_open5gs_deploy() {
    local stage="$TESTBED_CACHE_DIR/open5gs-stage"
    rm -rf "$stage"
    open5gs_stage "$stage"
    open5gs_push "$stage"

    info "installing MongoDB $OPEN5GS_MONGODB_SERIES and Open5GS at $OPEN5GS_REF on $NODE_CORE (a first build takes several minutes)"
    on_core "$OPEN5GS_TESTBED_DIR/install.sh" \
        || die "could not install Open5GS (does $NODE_CORE reach the Internet? build log: $OPEN5GS_TESTBED_DIR/build.log on $NODE_CORE)"

    info "configuring Open5GS: PLMN $TESTBED_MCC/$TESTBED_MNC, TAC $TESTBED_TAC, slice $TESTBED_SST/$TESTBED_SD, DNN $TESTBED_DNN, UE pool $TESTBED_UE_POOL"
    on_core "$OPEN5GS_TESTBED_DIR/apply.sh" || die "could not configure or start Open5GS"

    info "waiting for the AMF on $AMF_N2 and the UPF on $UPF_N3:2152"
    open5gs_wait_ready \
        || die "Open5GS did not come up — see '$PROG core-status' and 'journalctl -u \"open5gs-*\"' on $NODE_CORE"

    open5gs_provision
}

# open5gs_provision upserts the subscriber population and reports how much of
# it MongoDB holds. The script comes from the current configuration, not from
# the last deploy, so a changed count or key is applied.
open5gs_provision() {
    local f="$TESTBED_CACHE_DIR/open5gs-subscribers.js" out="" have="" total=""
    info "provisioning $TESTBED_SUB_COUNT subscribers from $TESTBED_SUB_START into MongoDB"
    open5gs_subscribers_js > "$f"
    push_file "$NODE_CORE" "$f" "$OPEN5GS_TESTBED_DIR/subscribers.js"
    rm -f "$f"
    out=$(on_core "mongosh --quiet '$OPEN5GS_DB_URI' $OPEN5GS_TESTBED_DIR/subscribers.js" 2>&1 | tr -d '\r') \
        || die "could not provision subscribers: $out"
    read -r have total <<<"$(printf '%s\n' "$out" | sed -n 's/^subscribers //p')" || true
    case "$have" in
        "$TESTBED_SUB_COUNT") info "subscribers provisioned: $have" ;;
        *) die "subscribers provisioned: '${have:-none}', expected $TESTBED_SUB_COUNT: $out" ;;
    esac
    if [ "$total" != "$have" ]; then
        warn "MongoDB holds $total subscribers, $((total - have)) of them outside $TESTBED_SUB_START +$TESTBED_SUB_COUNT; they are left alone"
    fi
}

# core_open5gs_reprovision restores the subscriber population. The UDR reads
# MongoDB on every request, so no NF needs restarting.
core_open5gs_reprovision() {
    exists_instance "$NODE_CORE" && is_running "$NODE_CORE" || die "$NODE_CORE is not running"
    on_core "command -v mongosh >/dev/null && systemctl is-active -q mongod" \
        || die "MongoDB is not running on $NODE_CORE — run '$PROG core' first"
    open5gs_provision
}

core_open5gs_status() {
    local start=$TESTBED_SUB_START
    lxc exec "$NODE_CORE" -- bash -s 2>/dev/null <<EOF || echo "  ($NODE_CORE not answering)"
echo "network functions (systemd units; EPC daemons are not installed)"
for u in mongod \$(for nf in $OPEN5GS_NFS; do echo open5gs-\${nf}d; done); do
    printf '  %-16s %s\n' "\$u" "\$(systemctl is-active "\$u" 2>/dev/null || true)"
done
echo
echo "version"
sed 's/^/  /' $OPEN5GS_TESTBED_DIR/versions 2>/dev/null || echo "  (not deployed)"
echo
echo "host interfaces (the node holds the UPF's N3 and N6 addresses; $OPEN5GS_TUN carries the UE pool)"
ip -br -4 addr show | sed 's/^/  /'
echo
echo "listeners"
printf '  %-28s %s\n' "NGAP  sctp $AMF_N2" "\$(grep -qE ' 38412 .*[[:space:]]$CORE_N2([[:space:]]|\$)' /proc/net/sctp/eps 2>/dev/null && echo yes || echo NO)"
printf '  %-28s %s\n' "GTP-U udp  $UPF_N3:2152" "\$(ss -Hlun 2>/dev/null | grep -q '$UPF_N3:2152 ' && echo yes || echo NO)"
printf '  %-28s %s\n' "PFCP  udp  $(open5gs_sbi_addr upf):8805" "\$(ss -Hlun 2>/dev/null | grep -q '$(open5gs_sbi_addr upf):8805 ' && echo yes || echo NO)"
echo
echo "user plane"
echo "  ip_forward \$(sysctl -n net.ipv4.ip_forward 2>/dev/null)"
ip -4 route show $TESTBED_UE_POOL 2>/dev/null | sed 's/^/  /'
echo "  NAT rules: \$( (nft list ruleset 2>/dev/null; iptables-save -t nat 2>/dev/null) | grep -ciE 'masquerade|snat' || true)"
echo
echo "subscribers"
if command -v mongosh >/dev/null 2>&1; then
    echo "  \$(mongosh --quiet '$OPEN5GS_DB_URI' --eval 'print(db.subscribers.countDocuments({}))' 2>/dev/null || echo '?') provisioned (expected $TESTBED_SUB_COUNT from $start)"
else
    echo "  (MongoDB not installed)"
fi
EOF
}

# core_open5gs_keys reads Ki and OPc back from the first subscriber in MongoDB.
core_open5gs_keys() {
    local ki="" opc=""
    read -r ki opc <<<"$(on_core "mongosh --quiet '$OPEN5GS_DB_URI' --eval 'const s = db.subscribers.findOne({imsi: \"$TESTBED_SUB_START\"}); if (s) print(s.security.k + \" \" + s.security.opc)'" 2>/dev/null | tr -d '\r' | grep -m1 -E '^[0-9a-fA-F]{32} [0-9a-fA-F]{32}$')" || true
    echo "${ki:--} ${opc:--}"
}

# core_open5gs_versions prints the record install.sh wrote on the core node.
core_open5gs_versions() {
    on_core "cat $OPEN5GS_TESTBED_DIR/versions" | tr -d '\r'
}
