# Core kind `sdcore`: SD-Core deployed by Aether OnRamp onto Kubernetes (RKE2)
# on the core node, managed from a control node running aether-ops.
#
# Sourced by testbed.sh; the contract is described there under "core kinds".
# The core_sdcore_* functions are defined in testbed.sh itself.

CORE_NAME="SD-Core"

# aether-ops manages SD-Core, so the testbed has a control node and the
# `ctrl` and `keys` steps.
CORE_HAS_CTRL=true

# The UPF is a pod attached to the core node's n3 and n6 by macvlan: those
# interfaces stay up with no address, and the UPF holds UPF_N3 and UPF_N6.
CORE_UPF_ATTACH=macvlan

# OnRamp installs what it needs.
CORE_NODE_PACKAGES=""
