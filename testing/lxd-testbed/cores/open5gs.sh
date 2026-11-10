# Core kind `open5gs`: Open5GS on the core node's host network.
#
# Sourced by testbed.sh; the contract is described there under "core kinds".

CORE_NAME="Open5GS"

# Nothing manages this core but testbed.sh: no control node, no `ctrl` or
# `keys` step.
CORE_HAS_CTRL=false

# The UPF runs on the core node's host network, so the core node itself holds
# UPF_N3 on n3 and UPF_N6 on n6.
CORE_UPF_ATTACH=host

# Extra packages cloud-init installs on the core node at first boot.
CORE_NODE_PACKAGES=""

core_open5gs_deploy()      { die "core kind 'open5gs': deploy is not implemented yet"; }
core_open5gs_status()      { die "core kind 'open5gs': status is not implemented yet"; }
core_open5gs_reprovision() { die "core kind 'open5gs': reprovision is not implemented yet"; }
core_open5gs_versions()    { die "core kind 'open5gs': versions is not implemented yet"; }

# Reports no deployed keys, so ORBIT is handed TESTBED_KI and TESTBED_OPC.
core_open5gs_keys() { echo "- -"; }
