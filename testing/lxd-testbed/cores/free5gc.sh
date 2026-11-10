# Core kind `free5gc`: free5GC on the core node's host network.
#
# Sourced by testbed.sh; the contract is described there under "core kinds".

CORE_NAME="free5GC"

# Nothing manages this core but testbed.sh: no control node, no `ctrl` or
# `keys` step.
CORE_HAS_CTRL=false

# The UPF runs on the core node's host network, so the core node itself holds
# UPF_N3 on n3 and UPF_N6 on n6.
CORE_UPF_ATTACH=host

# Extra packages cloud-init installs on the core node at first boot.
CORE_NODE_PACKAGES=""

core_free5gc_deploy()      { die "core kind 'free5gc': deploy is not implemented yet"; }
core_free5gc_status()      { die "core kind 'free5gc': status is not implemented yet"; }
core_free5gc_reprovision() { die "core kind 'free5gc': reprovision is not implemented yet"; }
core_free5gc_versions()    { die "core kind 'free5gc': versions is not implemented yet"; }

# Reports no deployed keys, so ORBIT is handed TESTBED_KI and TESTBED_OPC.
core_free5gc_keys() { echo "- -"; }
