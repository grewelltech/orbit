#!/usr/bin/env bash
# Apply a WAN impairment profile on the testbed's wanem node. Installed by
# testbed.sh as /opt/wanemd/bin/profile.sh with this testbed's far-site subnets
# filled in. Run as root on wanem.
#
#   profile.sh <name>     apply a profile (replaces any profile applied before)
#   profile.sh clear      remove the profile; the WAN is back to the bare line rate
#   profile.sh show       list wanemd's rules
#
# Impairment is applied ONLY to far-site cellular traffic: a wanemd rule whose
# filter matches the far N2 and far N3 subnets in both directions. UE traffic
# rides inside GTP-U on N3, so it is covered. Everything else crossing the WAN,
# notably far-site management (OAM), falls through to wanemd's default rule,
# which is never impaired. wanemd has no exclusion list in its config: scoping
# the impairment is how management stays unshaped. Do not impair the default
# rule (id 1); that would hit management too.
#
# Values are per direction (wanemd-cli's --latency/--jitter/--loss are
# round-trip totals split in half; the explicit --ul*/--dl* flags avoid that
# ambiguity).
set -euo pipefail
CLI=${CLI:-/opt/wanemd/bin/wanemd-cli}
RULE=far-cellular
FILTER='cli.ip == __FAR_N2__ or cli.ip == __FAR_N3__'

# name: one-way delay (ms), one-way jitter (ms), one-way loss (%)
profile_values() {
  case $1 in
    wan-30ms) echo "15 2.5 0.25" ;;   # ~30 ms RTT, ~±5 ms, ~0.5% round-trip loss: a regional WAN/metro link
    wan-80ms) echo "40 5 0.5" ;;      # ~80 ms RTT, ~±10 ms, ~1% round-trip loss: a long-haul or poor WAN
    sat-600ms) echo "300 10 0.5" ;;   # GEO-satellite-like backhaul
    *) return 1 ;;
  esac
}

rule_id() { "$CLI" --list --json | python3 -c '
import json,sys
for r in json.load(sys.stdin).get("command_result") or []:
    if r.get("name") == sys.argv[1]: print(r["id"])' "$RULE"; }

clear_rule() { local id; id=$(rule_id); [ -z "$id" ] || "$CLI" --id "$id" --delete >/dev/null; }

case ${1:?usage: profile.sh <wan-30ms|wan-80ms|sat-600ms|clear|show>} in
  show)  "$CLI" --list ;;
  clear) clear_rule; echo "cleared: WAN at line rate only" ;;
  *)
    read -r d j l < <(profile_values "$1") || { echo "unknown profile: $1" >&2; exit 2; }
    clear_rule
    "$CLI" --create "$RULE" --filter "$FILTER" \
      --ullatency "$d" --dllatency "$d" --uljitter "$j" --dljitter "$j" --ulloss "$l" --dlloss "$l" >/dev/null
    echo "applied $1 to far-site cellular traffic: one-way delay ${d} ms, jitter ${j} ms, loss ${l}% (rule $(rule_id))"
    ;;
esac
