# Core kind `free5gc`: free5GC on the core node's host network.
#
# Sourced by testbed.sh; the contract is described there under "core kinds".
#
# free5GC is built from source on the core node and runs as one systemd unit
# per network function, with no containers: the AMF binds NGAP to CORE_N2 and
# the UPF (go-upf with the gtp5g kernel module) binds GTP-U to UPF_N3, so N2,
# N3 and N6 carry the node's real addresses. The SBI and PFCP stay on loopback.
# MongoDB holds the NRF's registry and the subscribers, which are written
# through the webconsole's API so the documents are the pinned version's own.

CORE_NAME="free5GC"

# Nothing manages this core but testbed.sh: no control node, no `ctrl` or
# `keys` step.
CORE_HAS_CTRL=false

# The UPF runs on the core node's host network, so the core node itself holds
# UPF_N3 on n3 and UPF_N6 on n6.
CORE_UPF_ATTACH=host

# Extra packages cloud-init installs on the core node at first boot. Empty:
# `core` installs what it needs, so it also works on a node built without it.
CORE_NODE_PACKAGES=""

# ── Component versions ───────────────────────────────────────────────────────
# free5GC at a release tag, with every NF at the commit that release pins as a
# submodule. Any git ref works (tag, branch or sha); `core` records what each
# component resolved to in /etc/orbit/testbed-versions on the core node.
#
# v4.3.0 implements the 5GC of 3GPP Release 15 with parts of later releases;
# its NAS and NGAP codecs (free5gc/nas v1.3.0, free5gc/ngap v1.2.0) follow
# TS 24.501 and TS 38.413 at Release 15/16 level.
: "${FREE5GC_REPO:=https://github.com/free5gc/free5gc.git}"
: "${FREE5GC_REF:=v4.3.0}"
#
# The kernel module go-upf forwards with. go-upf refuses to start on a version
# outside the range it was written against (v4.3.0: >= 0.9.3 and < 0.10.3), so
# move this together with FREE5GC_REF.
: "${GTP5G_REPO:=https://github.com/free5gc/gtp5g.git}"
: "${GTP5G_REF:=v0.10.2}"
#
# Go toolchain the NFs are built with, from go.dev. v4.3.0's modules ask for
# go 1.26.2 or later.
: "${FREE5GC_GO_VERSION:=1.26.11}"
#
# MongoDB release series, from MongoDB's own apt repository for this Ubuntu.
: "${FREE5GC_MONGO_SERIES:=8.0}"

# ── Deployment ───────────────────────────────────────────────────────────────
# Where the source tree, the binaries and the helper scripts live on the core
# node. The rendered configuration is in /etc/free5gc.
: "${FREE5GC_DIR:=/opt/free5gc}"

# Control-plane NFs to build and run, in start order after the NRF. The UPF and
# the webconsole backend always run as well. This is free5GC's default set
# without the NEF.
: "${FREE5GC_NFS:=nrf udr udm ausf nssf pcf chf amf smf}"

# NAS algorithm preference of the AMF, most preferred first. NIA2 and NEA0
# lead, as on the SD-Core testbed; NIA0 (null integrity) is not offered.
: "${FREE5GC_NIA_ORDER:=NIA2 NIA1 NIA3}"
: "${FREE5GC_NEA_ORDER:=NEA0 NEA2 NEA1 NEA3}"

# MTU of the UPF's GTP device: the largest UE packet that fits one N3 frame
# once the outer IPv4, UDP and GTP-U headers (with the PDU session container)
# are added.
: "${FREE5GC_GTP_MTU:=$((TESTBED_MTU - 64))}"

# Session and UE AMBR of every subscriber, uplink and downlink. Set high so the
# UPF's rate enforcement is not what a throughput run measures.
: "${FREE5GC_AMBR:=10 Gbps}"

# Next hop for UE traffic leaving N6 for anything beyond the N6 subnet. Empty:
# `core` uses GW_N6 if it answers on N6 and no next hop otherwise (a site with
# no router, where the UEs' peers are on the N6 subnet itself). An address
# forces it; `none` forces no next hop. UE traffic never leaves by the node's
# own default route.
: "${FREE5GC_N6_GATEWAY:=}"

# Log level of every NF (trace, debug, info, warn, error).
: "${FREE5GC_LOG_LEVEL:=info}"

# The webconsole's API, on loopback. Its administrator account is free5GC's
# stock one and is not a secret.
FREE5GC_WEBUI="http://127.0.0.1:5000"

# Routing table and rule priorities that steer UE traffic out of N6.
FREE5GC_N6_TABLE=2152
FREE5GC_N6_PREF=2152

# free5gc_units -> every unit of the core, in start order.
free5gc_units() {
    local nf
    echo free5gc-upf
    for nf in $FREE5GC_NFS; do echo "free5gc-$nf"; done
    echo free5gc-webconsole
}

# free5gc_push <name> <remote> [mode] writes stdin to a file on the core node.
free5gc_push() {
    local tmp="$TESTBED_CACHE_DIR/free5gc.$1"
    cat > "$tmp"
    push_file "$NODE_CORE" "$tmp" "$2" "${3:-644}"
    rm -f "$tmp"
}

# free5gc_modules_loadable makes the core node able to load gtp5g, which is
# built on the node and so carries no signature the firmware trusts. LXD VMs
# boot with Secure Boot on, and the kernel then refuses such a module; the
# only unattended way round it is to turn Secure Boot off for this VM, which
# takes a restart of the node.
# free5gc_secure_boot_on succeeds when the core node booted with Secure Boot
# enforced. It reads the firmware's SecureBoot variable, whose fifth byte is 1
# when enforcement is on; a node without the variable has it off.
free5gc_secure_boot_on() {
    on_core 'f=$(ls /sys/firmware/efi/efivars/SecureBoot-* 2>/dev/null | head -n1)
[ -n "$f" ] && [ "$(od -An -t u1 -j4 -N1 "$f" | tr -d " ")" = 1 ]'
}

free5gc_modules_loadable() {
    free5gc_secure_boot_on || return 0
    info "turning Secure Boot off on $NODE_CORE (gtp5g is built here and unsigned); restarting the node"
    # The setting was renamed: boot.mode on current LXD, security.secureboot
    # before it.
    lxc_do config set "$NODE_CORE" boot.mode=uefi-nosecureboot 2>/dev/null \
        || lxc_do config set "$NODE_CORE" security.secureboot=false 2>/dev/null \
        || die "could not turn Secure Boot off on $NODE_CORE (tried boot.mode=uefi-nosecureboot and security.secureboot=false)"
    lxc_do stop "$NODE_CORE" >/dev/null || die "could not stop $NODE_CORE"
    lxc_do start "$NODE_CORE" >/dev/null || die "could not start $NODE_CORE"
    local i
    for i in $(seq 1 60); do
        lxc exec "$NODE_CORE" -- true >/dev/null 2>&1 && break
        sleep 3
    done
    wait_cloud_init "$NODE_CORE"
    ! free5gc_secure_boot_on \
        || die "Secure Boot is still on in $NODE_CORE; gtp5g cannot be loaded"
}

# free5gc_prereqs installs the build tools, the kernel headers, MongoDB and Go.
free5gc_prereqs() {
    info "installing build prerequisites on $NODE_CORE"
    on_core "export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
apt-get update -qq && apt-get install -y -qq git make gcc dkms curl gnupg ca-certificates python3-yaml linux-headers-\$(uname -r) >/dev/null" \
        || die "could not install prerequisites (does $NODE_CORE reach the Internet? try '$PROG check')"

    if ! on_core "dpkg -s mongodb-org 2>/dev/null | grep -q '^Version: $FREE5GC_MONGO_SERIES\.'"; then
        info "installing MongoDB $FREE5GC_MONGO_SERIES from repo.mongodb.org"
        on_core "set -e; export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
. /etc/os-release
curl -fsSL https://pgp.mongodb.com/server-$FREE5GC_MONGO_SERIES.asc | gpg --dearmor --yes -o /usr/share/keyrings/mongodb-server-$FREE5GC_MONGO_SERIES.gpg
rm -f /etc/apt/sources.list.d/mongodb-org-*.list
echo \"deb [ arch=amd64 signed-by=/usr/share/keyrings/mongodb-server-$FREE5GC_MONGO_SERIES.gpg ] https://repo.mongodb.org/apt/ubuntu \$VERSION_CODENAME/mongodb-org/$FREE5GC_MONGO_SERIES multiverse\" > /etc/apt/sources.list.d/mongodb-org-$FREE5GC_MONGO_SERIES.list
apt-get update -qq && apt-get install -y -qq mongodb-org >/dev/null" \
            || die "could not install MongoDB $FREE5GC_MONGO_SERIES"
    fi
    on_core "systemctl enable -q --now mongod" || die "could not start mongod"

    if ! on_core "/usr/local/go/bin/go version 2>/dev/null | grep -q ' go$FREE5GC_GO_VERSION '"; then
        info "installing Go $FREE5GC_GO_VERSION"
        on_core "set -e
curl -fsSL https://go.dev/dl/go$FREE5GC_GO_VERSION.linux-amd64.tar.gz -o /tmp/go.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz && rm -f /tmp/go.tar.gz" \
            || die "could not install Go $FREE5GC_GO_VERSION"
    fi
}

# free5gc_gtp5g builds gtp5g at GTP5G_REF and registers it with DKMS, so a
# kernel upgrade rebuilds it, and has it loaded at every boot.
free5gc_gtp5g() {
    info "fetching gtp5g at $GTP5G_REF"
    free5gc_fetch "$FREE5GC_DIR/gtp5g" "$GTP5G_REPO" "$GTP5G_REF" || die "could not fetch gtp5g at $GTP5G_REF from $GTP5G_REPO"
    on_core "set -e
src=$FREE5GC_DIR/gtp5g
ver=\$(sed -n 's/^PACKAGE_VERSION=\"\\(.*\\)\"/\\1/p' \$src/dkms.conf)
[ -n \"\$ver\" ] || { echo 'gtp5g: no PACKAGE_VERSION in dkms.conf' >&2; exit 1; }
sha=\$(git -C \$src rev-parse HEAD)
if [ \"\$(cat /usr/src/gtp5g-\$ver/.testbed-commit 2>/dev/null)\" != \"\$sha\" ] || ! dkms status gtp5g/\$ver -k \$(uname -r) | grep -q installed; then
    systemctl stop free5gc-upf 2>/dev/null || true
    for old in \$(dkms status gtp5g | sed -n 's|^gtp5g/\\([^,:]*\\).*|\\1|p' | sort -u); do dkms remove gtp5g/\$old --all >/dev/null 2>&1 || true; done
    rm -rf /usr/src/gtp5g-*
    mkdir -p /usr/src/gtp5g-\$ver
    git -C \$src archive HEAD | tar -x -C /usr/src/gtp5g-\$ver
    echo \$sha > /usr/src/gtp5g-\$ver/.testbed-commit
    dkms add gtp5g/\$ver >/dev/null 2>&1
    dkms build gtp5g/\$ver >/dev/null 2>&1 || { cat /var/lib/dkms/gtp5g/\$ver/build/make.log >&2; exit 1; }
    dkms install --force gtp5g/\$ver >/dev/null 2>&1
    rmmod gtp5g 2>/dev/null || true
fi
echo gtp5g > /etc/modules-load.d/free5gc-gtp5g.conf
modprobe gtp5g" || die "could not build and load gtp5g $GTP5G_REF for the running kernel"
    info "gtp5g loaded: $(on_core "cat /sys/module/gtp5g/version 2>/dev/null || modinfo -F version gtp5g" | tr -d '\r')"
}

# free5gc_fetch <dir> <repo> <ref> checks <ref> out in <dir> on the core node,
# fetching only that commit. A tag keeps its name, which free5GC's build stamps
# into the binaries.
free5gc_fetch() {
    local dir=$1 repo=$2 ref=$3
    on_core "set -e
[ -d $dir/.git ] || git init -q $dir
cd $dir
git remote remove origin 2>/dev/null || true
git remote add origin $repo
git fetch -q --depth 1 origin '+refs/tags/$ref:refs/tags/$ref' 2>/dev/null || git fetch -q --depth 1 origin $ref
git checkout -q -f --detach FETCH_HEAD"
}

# free5gc_build fetches free5GC at FREE5GC_REF with the submodules of the NFs
# this testbed runs, and builds them. Make rebuilds only what changed, so a
# second run at the same ref costs seconds.
free5gc_build() {
    local src="$FREE5GC_DIR/src" nf paths="NFs/upf webconsole" targets="upf webconsole/bin/webconsole"
    for nf in $FREE5GC_NFS; do paths+=" NFs/$nf"; targets+=" $nf"; done

    info "fetching free5GC at $FREE5GC_REF"
    free5gc_fetch "$src" "$FREE5GC_REPO" "$FREE5GC_REF" || die "could not fetch free5GC at $FREE5GC_REF from $FREE5GC_REPO"
    on_core "cd $src && git submodule -q sync && git submodule -q update --init --force --depth 1 $paths" \
        || die "could not fetch the free5GC submodules ($paths)"

    info "building free5GC ($targets) with Go $FREE5GC_GO_VERSION (slow the first time)"
    on_core "cd $src && PATH=/usr/local/go/bin:\$PATH GOTOOLCHAIN=local GOFLAGS=-buildvcs=false make $targets >/tmp/free5gc-build.log 2>&1 || { tail -40 /tmp/free5gc-build.log >&2; exit 1; }" \
        || die "free5GC build failed (full log: /tmp/free5gc-build.log on $NODE_CORE)"
}

# free5gc_n6_gateway -> the next hop UE traffic takes beyond the N6 subnet, or
# nothing. See FREE5GC_N6_GATEWAY.
free5gc_n6_gateway() {
    case "$FREE5GC_N6_GATEWAY" in
        none) return 0 ;;
        '')   if on_core "ping -c2 -W1 -q -I $UPF_N6 $GW_N6 >/dev/null 2>&1"; then echo "$GW_N6"; fi ;;
        *)    echo "$FREE5GC_N6_GATEWAY" ;;
    esac
}

# free5gc_configure writes everything `core` owns outside the source tree: the
# settings the helper scripts read, the scripts themselves, the NF
# configuration rendered from the pinned release's own files, and the units.
free5gc_configure() {
    local n6gw nf units
    n6gw=$(free5gc_n6_gateway)
    if [ -n "$n6gw" ]; then
        info "UE traffic beyond the N6 subnet goes via $n6gw"
    else
        info "no N6 next hop: UEs reach the N6 subnet ($TESTBED_NET_N6) only"
    fi

    info "rendering the free5GC configuration into /etc/free5gc"
    free5gc_push env /etc/free5gc/testbed.env <<EOF
# Written by testbed.sh (core kind free5gc). Read by the helper scripts in
# $FREE5GC_DIR/testbed and by free5gc-upf.service.
FREE5GC_NFS="$FREE5GC_NFS"
CORE_N2=$CORE_N2
UPF_N3=$UPF_N3
UPF_N6=$UPF_N6
N6_GATEWAY=$n6gw
N6_TABLE=$FREE5GC_N6_TABLE
N6_PREF=$FREE5GC_N6_PREF
UE_POOL=$TESTBED_UE_POOL
UE_DNS=${TESTBED_DNS%%,*}
GTP_MTU=$FREE5GC_GTP_MTU
MCC=$TESTBED_MCC
MNC=$TESTBED_MNC
TAC=$(printf '%06x' "$TESTBED_TAC")
SST=$TESTBED_SST
SD=$TESTBED_SD
DNN=$TESTBED_DNN
NIA_ORDER="$FREE5GC_NIA_ORDER"
NEA_ORDER="$FREE5GC_NEA_ORDER"
LOG_LEVEL=$FREE5GC_LOG_LEVEL
WEBUI=$FREE5GC_WEBUI
SUB_START=$TESTBED_SUB_START
SUB_COUNT=$TESTBED_SUB_COUNT
KI=$TESTBED_KI
OPC=$TESTBED_OPC
AMBR="$FREE5GC_AMBR"
EOF
    on_core "chmod 600 /etc/free5gc/testbed.env"
    free5gc_render_script    | free5gc_push render    "$FREE5GC_DIR/testbed/render-config" 755
    free5gc_provision_script | free5gc_push provision "$FREE5GC_DIR/testbed/provision" 755
    free5gc_upf_host_script  | free5gc_push upfhost   "$FREE5GC_DIR/testbed/upf-host-setup" 755
    free5gc_nrf_hook_script  | free5gc_push nrfhook   "$FREE5GC_DIR/testbed/nrf-hook" 755
    on_core "set -a; . /etc/free5gc/testbed.env; set +a; $FREE5GC_DIR/testbed/render-config $FREE5GC_DIR/src/config /etc/free5gc" \
        || die "could not render the free5GC configuration — the layout of $FREE5GC_REF's config files may have changed"

    free5gc_push sysctl /etc/sysctl.d/99-free5gc-upf.conf <<EOF
# The UPF routes between its GTP device and N6.
net.ipv4.ip_forward = 1
EOF

    # One unit per NF. Each runs from the source tree, where the release keeps
    # the certificates its configuration names by relative path. The NRF goes
    # first and the rest follow it through a restart: an NF registers once, at
    # start, and the NRF's registry is emptied whenever the NRF starts.
    free5gc_push unit-nrf /etc/systemd/system/free5gc-nrf.service <<EOF
[Unit]
Description=free5GC NRF
After=network-online.target mongod.service
Wants=network-online.target mongod.service
PartOf=free5gc.target

[Service]
WorkingDirectory=$FREE5GC_DIR/src
Environment=GIN_MODE=release
ExecStartPre=$FREE5GC_DIR/testbed/nrf-hook pre
ExecStart=$FREE5GC_DIR/src/bin/nrf -c /etc/free5gc/nrfcfg.yaml
ExecStartPost=$FREE5GC_DIR/testbed/nrf-hook post
Restart=on-failure
RestartSec=2

[Install]
WantedBy=free5gc.target
EOF
    for nf in $FREE5GC_NFS; do
        [ "$nf" = nrf ] && continue
        free5gc_push "unit-$nf" "/etc/systemd/system/free5gc-$nf.service" <<EOF
[Unit]
Description=free5GC $(echo "$nf" | tr '[:lower:]' '[:upper:]')
After=network-online.target mongod.service free5gc-nrf.service free5gc-upf.service
Wants=network-online.target mongod.service
PartOf=free5gc.target free5gc-nrf.service

[Service]
WorkingDirectory=$FREE5GC_DIR/src
Environment=GIN_MODE=release
ExecStart=$FREE5GC_DIR/src/bin/$nf -c /etc/free5gc/${nf}cfg.yaml
Restart=on-failure
RestartSec=2

[Install]
WantedBy=free5gc.target
EOF
    done
    free5gc_push unit-upf /etc/systemd/system/free5gc-upf.service <<EOF
[Unit]
Description=free5GC UPF (go-upf on gtp5g)
After=network-online.target
Wants=network-online.target
PartOf=free5gc.target

[Service]
WorkingDirectory=$FREE5GC_DIR/src
ExecStartPre=$FREE5GC_DIR/testbed/upf-host-setup
ExecStart=$FREE5GC_DIR/src/bin/upf -c /etc/free5gc/upfcfg.yaml
Restart=on-failure
RestartSec=2

[Install]
WantedBy=free5gc.target
EOF
    free5gc_push unit-webconsole /etc/systemd/system/free5gc-webconsole.service <<EOF
[Unit]
Description=free5GC webconsole backend (subscriber API on $FREE5GC_WEBUI)
After=network-online.target mongod.service free5gc-nrf.service
Wants=network-online.target mongod.service
PartOf=free5gc.target

[Service]
WorkingDirectory=$FREE5GC_DIR/src
Environment=GIN_MODE=release
ExecStart=$FREE5GC_DIR/src/webconsole/bin/webconsole -c /etc/free5gc/webuicfg.yaml
Restart=on-failure
RestartSec=2

[Install]
WantedBy=free5gc.target
EOF
    units=$(free5gc_units | sed 's/$/.service/' | tr '\n' ' ')
    free5gc_push target /etc/systemd/system/free5gc.target <<EOF
[Unit]
Description=free5GC core (every NF)
Wants=$units

[Install]
WantedBy=multi-user.target
EOF
    # A unit of an NF that is no longer in FREE5GC_NFS would keep running.
    on_core "for u in /etc/systemd/system/free5gc-*.service; do n=\$(basename \$u); case ' $units' in *\" \$n \"*) ;; *) systemctl disable -q --now \$n 2>/dev/null; rm -f \$u ;; esac; done
systemctl daemon-reload && systemctl enable -q free5gc.target $units" || die "could not install the free5GC units"
}

# The render script: the pinned release's own config files, with only what
# this testbed decides replaced. Every scalar is read and written as the text
# it is in the file, so values like the SD 010203 keep their leading zero.
free5gc_render_script() {
    cat <<'PY'
#!/usr/bin/env python3
"""Renders free5GC's configuration for the testbed: <release config dir> <out dir>."""
import os
import sys

import yaml
from yaml.emitter import Emitter
from yaml.representer import SafeRepresenter
from yaml.resolver import BaseResolver
from yaml.serializer import Serializer


class Plain(Emitter, Serializer, SafeRepresenter, BaseResolver):
    """Writes every scalar unquoted, as free5GC's own files have them."""

    def __init__(self, stream, **_):
        Emitter.__init__(self, stream, width=1000)
        Serializer.__init__(self)
        SafeRepresenter.__init__(self, default_flow_style=False, sort_keys=False)
        BaseResolver.__init__(self)

    def ignore_aliases(self, data):
        return True


SRC, DST = sys.argv[1], sys.argv[2]
E = os.environ
PLMN = {"mcc": E["MCC"], "mnc": E["MNC"]}
SNSSAI = {"sst": E["SST"]}
if E.get("SD"):
    SNSSAI["sd"] = E["SD"]


def amf(c):
    c["ngapIpList"] = [E["CORE_N2"]]
    c["servedGuamiList"] = [{"plmnId": dict(PLMN), "amfId": c["servedGuamiList"][0]["amfId"]}]
    c["supportTaiList"] = [{"plmnId": dict(PLMN), "tac": E["TAC"]}]
    c["plmnSupportList"] = [{"plmnId": dict(PLMN), "snssaiList": [dict(SNSSAI)]}]
    c["supportDnnList"] = [E["DNN"]]
    c["security"]["integrityOrder"] = E["NIA_ORDER"].split()
    c["security"]["cipheringOrder"] = E["NEA_ORDER"].split()


def smf(c):
    info = c["snssaiInfos"][0]
    info["sNssai"] = dict(SNSSAI)
    dnn = info["dnnInfos"][0]
    dnn["dnn"] = E["DNN"]
    dnn["dns"]["ipv4"] = E["UE_DNS"]
    info["dnnInfos"] = [dnn]
    c["snssaiInfos"] = [info]
    c["plmnList"] = [dict(PLMN)]
    upf = c["userplaneInformation"]["upNodes"]["UPF"]
    slice_ = upf["sNssaiUpfInfos"][0]
    slice_["sNssai"] = dict(SNSSAI)
    slice_["dnnUpfInfoList"] = [{"dnn": E["DNN"], "pools": [{"cidr": E["UE_POOL"]}]}]
    upf["sNssaiUpfInfos"] = [slice_]
    n3 = upf["interfaces"][0]
    n3["endpoints"] = [E["UPF_N3"]]
    n3["networkInstances"] = [E["DNN"]]
    upf["interfaces"] = [n3]


def upf(d):
    n3 = d["gtpu"]["ifList"][0]
    n3["addr"] = E["UPF_N3"]
    n3["mtu"] = E["GTP_MTU"]
    d["gtpu"]["ifList"] = [n3]
    # No NAT interface: UE addresses leave N6 as they are.
    d["dnnList"] = [{"dnn": E["DNN"], "cidr": E["UE_POOL"]}]


def nrf(c):
    c["DefaultPlmnId"] = dict(PLMN)


def ausf(c):
    c["plmnSupportList"] = [dict(PLMN)]


def nssf(c):
    c["supportedPlmnList"] = [dict(PLMN)]
    c["supportedNssaiInPlmnList"] = [{"plmnId": dict(PLMN), "supportedSnssaiList": [dict(SNSSAI)]}]
    c["nsiList"] = [{"snssai": dict(SNSSAI), "nsiInformationList": c["nsiList"][0]["nsiInformationList"]}]
    c["taList"] = [{
        "tai": {"plmnId": dict(PLMN), "tac": E["TAC"]},
        "accessType": "3GPP_ACCESS",
        "supportedSnssaiList": [dict(SNSSAI)],
    }]


def pcf(c):
    # No BSF runs in this testbed.
    if "bsf" in c:
        c["bsf"]["enable"] = "false"


def webui(c):
    c["webServer"]["ipv4Address"] = E["WEBUI"].split("//")[1].split(":")[0]
    c["webServer"]["port"] = E["WEBUI"].rsplit(":", 1)[1]


EDITS = {"amf": amf, "smf": smf, "upf": upf, "nrf": nrf, "ausf": ausf, "nssf": nssf, "pcf": pcf, "webui": webui}
HEADER = "# Rendered by testbed.sh from free5GC's %scfg.yaml; edits here are lost on the next `core`.\n"

for name in E["FREE5GC_NFS"].split() + ["upf", "webui"]:
    with open(f"{SRC}/{name}cfg.yaml") as f:
        doc = yaml.load(f, Loader=yaml.BaseLoader)
    try:
        if name in EDITS:
            # The UPF's file has no `configuration:` level.
            EDITS[name](doc if name == "upf" else doc["configuration"])
        doc["logger"]["level"] = E["LOG_LEVEL"]
    except (KeyError, IndexError, TypeError) as err:
        sys.exit(f"{name}cfg.yaml: expected key missing ({err!r}) -- free5GC's config layout changed")
    with open(f"{DST}/{name}cfg.yaml", "w") as f:
        f.write(HEADER % name)
        yaml.dump(doc, f, Dumper=Plain)
PY
}

# The provisioning script: replaces the whole subscriber population through the
# webconsole's API, which creates SUB_COUNT consecutive IMSIs from one request
# and writes every collection the UDR and PCF read.
free5gc_provision_script() {
    cat <<'PY'
#!/usr/bin/env python3
"""Replaces free5GC's subscribers with the testbed's population (webconsole API)."""
import json
import os
import sys
import time
import urllib.error
import urllib.request

E = os.environ
BASE = E["WEBUI"]
PLMN = E["MCC"] + E["MNC"]
SNSSAI = {"sst": int(E["SST"])}
if E.get("SD"):
    SNSSAI["sd"] = E["SD"]
SLICE = "%02x%s" % (int(E["SST"]), E.get("SD", ""))
DNN = E["DNN"]
AMBR = {"uplink": E["AMBR"], "downlink": E["AMBR"]}


def call(method, path, body=None, token=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method, headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Token", token)
    with urllib.request.urlopen(req, timeout=900) as resp:
        raw = resp.read()
    return json.loads(raw) if raw.strip() else None


def login():
    last = None
    for _ in range(60):
        try:
            return call("POST", "/api/login", {"username": "admin", "password": "free5gc"})["access_token"]
        except (urllib.error.URLError, OSError, KeyError) as err:
            last = err
            time.sleep(1)
    sys.exit(f"webconsole at {BASE} did not accept a login: {last}")


SUBSCRIBER = {
    "plmnID": PLMN,
    "ueId": "imsi-" + E["SUB_START"],
    "AuthenticationSubscription": {
        "authenticationMethod": "5G_AKA",
        "sequenceNumber": "000000000023",
        "authenticationManagementField": "8000",
        "permanentKey": {"permanentKeyValue": E["KI"], "encryptionKey": 0, "encryptionAlgorithm": 0},
        "milenage": {"op": {"opValue": "", "encryptionKey": 0, "encryptionAlgorithm": 0}},
        "opc": {"opcValue": E["OPC"], "encryptionKey": 0, "encryptionAlgorithm": 0},
    },
    "AccessAndMobilitySubscriptionData": {
        "gpsis": ["msisdn-"],
        "subscribedUeAmbr": AMBR,
        "nssai": {"defaultSingleNssais": [SNSSAI], "singleNssais": []},
    },
    "SessionManagementSubscriptionData": [{
        "singleNssai": SNSSAI,
        "dnnConfigurations": {DNN: {
            "pduSessionTypes": {"defaultSessionType": "IPV4", "allowedSessionTypes": ["IPV4"]},
            "sscModes": {"defaultSscMode": "SSC_MODE_1", "allowedSscModes": ["SSC_MODE_1", "SSC_MODE_2", "SSC_MODE_3"]},
            "5gQosProfile": {"5qi": 9, "arp": {"priorityLevel": 8, "preemptCap": "", "preemptVuln": ""}, "priorityLevel": 8},
            "sessionAmbr": AMBR,
            "staticIpAddress": [],
        }},
    }],
    "SmfSelectionSubscriptionData": {"subscribedSnssaiInfos": {SLICE: {"dnnInfos": [{"dnn": DNN}]}}},
    "AmPolicyData": {"subscCats": ["free5gc"]},
    "SmPolicyData": {"smPolicySnssaiData": {SLICE: {"snssai": SNSSAI, "smPolicyDnnData": {DNN: {"dnn": DNN}}}}},
    "FlowRules": [],
    "QosFlows": [],
    "ChargingDatas": [
        {"snssai": SLICE, "dnn": "", "filter": "", "chargingMethod": "Offline", "quota": "100000", "unitCost": "1"},
    ],
}

token = login()
existing = call("GET", "/api/subscriber", token=token) or []
for i in range(0, len(existing), 200):
    call("DELETE", "/api/subscriber", existing[i:i + 200], token)
call("POST", "/api/subscriber/imsi-%s/%s/%s" % (E["SUB_START"], PLMN, E["SUB_COUNT"]), SUBSCRIBER, token)
print(len(call("GET", "/api/subscriber", token=token) or []))
PY
}

# The UPF's host side, run before every UPF start: the kernel module,
# forwarding, and the routing that makes N6 the way out for UE traffic. The N6
# interface is found by the address it holds, not by name.
free5gc_upf_host_script() {
    cat <<'SH'
#!/bin/sh
# Prepares the host for the free5GC UPF. Written by testbed.sh.
set -eu
. /etc/free5gc/testbed.env

modprobe gtp5g
sysctl -q -w net.ipv4.ip_forward=1

dev=$(ip -o -4 addr show | awk -v a="$UPF_N6" '{split($4, x, "/"); if (x[1] == a) print $2}' | head -1)
[ -n "$dev" ] || { echo "no interface holds the UPF's N6 address $UPF_N6" >&2; exit 1; }
net=$(ip -4 route show dev "$dev" proto kernel scope link | awk '{print $1; exit}')

# Traffic from a UE address is routed by a table of its own: the N6 subnet
# on-link, everything else via the N6 next hop or nowhere. Traffic to a UE
# address stays in the main table, where the UPF routes the pool to its GTP
# device.
ip route flush table "$N6_TABLE" 2>/dev/null || true
ip route add "$net" dev "$dev" src "$UPF_N6" table "$N6_TABLE"
if [ -n "$N6_GATEWAY" ]; then
    ip route add default via "$N6_GATEWAY" dev "$dev" table "$N6_TABLE"
else
    ip route add unreachable default table "$N6_TABLE"
fi
while ip rule del pref "$N6_PREF" 2>/dev/null; do :; done
while ip rule del pref "$((N6_PREF - 1))" 2>/dev/null; do :; done
ip rule add pref "$((N6_PREF - 1))" to "$UE_POOL" lookup main
ip rule add pref "$N6_PREF" from "$UE_POOL" lookup "$N6_TABLE"
SH
}

# The NRF unit's hooks. `pre` runs before every NRF start: it waits for MongoDB
# and empties the NRF's registry and the subscription collections, which hold
# only what the previous set of NF processes registered (subscribers are in
# other collections and stay). `post` holds the units ordered after the NRF
# back until it answers.
free5gc_nrf_hook_script() {
    cat <<'SH'
#!/bin/bash
# Hooks of free5gc-nrf.service. Written by testbed.sh.
case "$1" in
    pre)
        for _ in $(seq 1 60); do
            mongosh --quiet free5gc --eval 'db.runCommand({ping: 1}).ok' >/dev/null 2>&1 && break
            sleep 1
        done
        exec mongosh --quiet free5gc --eval '
["NfProfile", "applicationData.influenceData.subsToNotify", "applicationData.subsToNotify",
 "policyData.subsToNotify", "exposureData.subsToNotify"].forEach(c => db.getCollection(c).drop())' >/dev/null ;;
    post)
        for _ in $(seq 1 150); do
            (exec 3<>/dev/tcp/127.0.0.10/8000) 2>/dev/null && exit 0
            sleep 0.2
        done
        exit 1 ;;
esac
SH
}

# free5gc_wait_ready blocks until every unit is active, the AMF listens for
# NGAP on CORE_N2 and the UPF for GTP-U on UPF_N3.
free5gc_wait_ready() {
    local units; units=$(free5gc_units | tr '\n' ' ')
    on_core "for i in \$(seq 1 90); do
    ok=1
    for u in $units; do systemctl is-active -q \$u || ok=0; done
    grep -q '$CORE_N2' /proc/net/sctp/eps 2>/dev/null || ok=0
    ss -Hlun 'sport = :2152' 2>/dev/null | grep -q '$UPF_N3:2152' || ok=0
    [ \$ok = 1 ] && exit 0
    sleep 2
done
for u in $units; do systemctl is-active -q \$u || { echo \"\$u is \$(systemctl is-active \$u):\" >&2; journalctl -u \$u -n 15 --no-pager >&2; }; done
exit 1" || die "free5GC did not come up (AMF on $AMF_N2, UPF on $UPF_N3:2152); see '$PROG core-status' and journalctl -u 'free5gc-*' on $NODE_CORE"
}

core_free5gc_deploy() {
    free5gc_modules_loadable
    free5gc_prereqs
    free5gc_gtp5g
    free5gc_build
    free5gc_configure

    info "starting free5GC"
    on_core "sysctl -q --system; systemctl stop free5gc.target $(free5gc_units | tr '\n' ' ') 2>/dev/null; systemctl start free5gc.target" \
        || die "could not start free5gc.target"
    free5gc_wait_ready
    info "AMF listening on $AMF_N2 (SCTP), UPF on $UPF_N3:2152 (GTP-U)"

    free5gc_provision
}

# free5gc_provision replaces the subscriber population with TESTBED_SUB_COUNT
# subscribers from TESTBED_SUB_START and states how many landed.
free5gc_provision() {
    info "provisioning $TESTBED_SUB_COUNT test subscribers from $TESTBED_SUB_START"
    on_core "set -a; . /etc/free5gc/testbed.env; set +a; $FREE5GC_DIR/testbed/provision >/dev/null" \
        || die "could not provision subscribers through the webconsole API ($FREE5GC_WEBUI on $NODE_CORE; journalctl -u free5gc-webconsole)"
    free5gc_report_subscribers
}

# free5gc_mongo <javascript> evaluates an expression against free5GC's database
# on the core node and prints the result.
free5gc_mongo() {
    lxc exec "$NODE_CORE" -- mongosh --quiet free5gc --eval "$1" 2>/dev/null | tr -d '\r'
}

# free5gc_report_subscribers states how much of the population landed. A
# report, not a gate.
free5gc_report_subscribers() {
    local subs=""
    subs=$(free5gc_mongo 'print(db.getCollection("subscriptionData.authenticationData.authenticationSubscription").countDocuments({}))') || subs=""
    case "$subs" in
        ''|*[!0-9]*) warn "could not read the subscriber count from MongoDB; check '$PROG core-status'" ;;
        "$TESTBED_SUB_COUNT") info "subscribers provisioned: $subs" ;;
        *) warn "subscribers provisioned: $subs, expected $TESTBED_SUB_COUNT" ;;
    esac
}

# core_free5gc_reprovision restores the subscriber population from the settings
# the core was deployed with. MongoDB's data is on the node's disk, so this is
# for a population that was edited or emptied, not for a reboot.
core_free5gc_reprovision() {
    exists_instance "$NODE_CORE" && is_running "$NODE_CORE" || die "$NODE_CORE is not running"
    on_core "[ -x $FREE5GC_DIR/testbed/provision ] && systemctl is-active -q free5gc-webconsole" \
        || die "free5GC is not deployed on $NODE_CORE — run '$PROG core' first"
    free5gc_provision
}

core_free5gc_status() {
    echo "core units"
    lxc exec "$NODE_CORE" -- bash -c "for u in mongod $(free5gc_units | tr '\n' ' '); do printf '%-22s %s\n' \$u \"\$(systemctl is-active \$u 2>/dev/null)\"; done" 2>/dev/null | sed 's/^/  /' || echo "  ($NODE_CORE not answering)"
    echo
    echo "host interfaces (the node holds the AMF's N2 address and the UPF's N3 and N6 addresses)"
    lxc exec "$NODE_CORE" -- ip -br -4 addr show 2>/dev/null | sed 's/^/  /' || true
    echo
    echo "listeners (NGAP $AMF_N2 SCTP, GTP-U $UPF_N3:2152, PFCP on loopback)"
    lxc exec "$NODE_CORE" -- bash -c "awk 'NR>1 {print \"sctp \" \$NF \":\" \$6}' /proc/net/sctp/eps 2>/dev/null; ss -Hlun 2>/dev/null | awk '\$4 ~ /:(2152|8805)\$/ {print \"udp  \" \$4}'" 2>/dev/null | sed 's/^/  /' || true
    echo
    echo "UPF attachment (gtp5g $(lxc exec "$NODE_CORE" -- cat /sys/module/gtp5g/version 2>/dev/null | tr -d '\r' || true))"
    lxc exec "$NODE_CORE" -- bash -c "ip -br link show type gtp5g 2>/dev/null; ip -4 route show $TESTBED_UE_POOL 2>/dev/null; ip rule show 2>/dev/null | grep -E '^($((FREE5GC_N6_PREF - 1))|$FREE5GC_N6_PREF):'; ip route show table $FREE5GC_N6_TABLE 2>/dev/null | sed 's/^/table $FREE5GC_N6_TABLE: /'" 2>/dev/null | sed 's/^/  /' || true
    echo
    local subs; subs=$(free5gc_mongo 'print(db.getCollection("subscriptionData.authenticationData.authenticationSubscription").countDocuments({}))') || subs=""
    echo "subscribers: ${subs:-unknown} (expected $TESTBED_SUB_COUNT)"
}

# core_free5gc_keys reads Ki/OPc of the first test subscriber as the UDR holds
# them.
core_free5gc_keys() {
    local ki="" opc="" out=""
    out=$(free5gc_mongo "const d = db.getCollection('subscriptionData.authenticationData.authenticationSubscription').findOne({ueId: 'imsi-$TESTBED_SUB_START'}); print(d ? d.encPermanentKey + ' ' + d.encOpcKey : '')") || out=""
    read -r ki opc <<<"$out" || true
    case "$ki" in *[!0-9a-fA-F]*) ki="" ;; esac
    case "$opc" in *[!0-9a-fA-F]*) opc="" ;; esac
    [ ${#ki} -eq 32 ] || ki=""
    [ ${#opc} -eq 32 ] || opc=""
    echo "${ki:--} ${opc:--}"
}

# core_free5gc_versions prints what is on the core node: free5GC and each NF
# submodule at its commit, gtp5g, and the toolchain and database versions.
core_free5gc_versions() {
    on_core "src=$FREE5GC_DIR/src
echo \"free5gc $FREE5GC_REF \$(git -C \$src rev-parse HEAD)\"
for nf in $FREE5GC_NFS upf webconsole; do
    p=NFs/\$nf; [ \$nf = webconsole ] && p=webconsole
    echo \"\$nf $FREE5GC_REF \$(git -C \$src/\$p rev-parse HEAD)\"
done
echo \"gtp5g $GTP5G_REF \$(git -C $FREE5GC_DIR/gtp5g rev-parse HEAD)\"
echo \"go $FREE5GC_GO_VERSION \$(/usr/local/go/bin/go version | awk '{print \$3}')\"
echo \"mongodb $FREE5GC_MONGO_SERIES \$(dpkg-query -W -f '\${Version}' mongodb-org)\"
echo \"kernel - \$(uname -r)\"" | tr -d '\r'
}
