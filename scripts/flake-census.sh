#!/usr/bin/env bash
#
# flake-census.sh: runs the tests many times and counts the failures per
# test, with each failing run's error line. It is the baseline every flake
# fix is measured against: run it before and after a fix, with the same
# flags, and put both tables in the PR.
#
# Usage:
#   scripts/flake-census.sh -m l1|l2 [-n RUNS] [-i INSTANCES] [-l LOADERS] [-o DIR] [-r REGEX] [PKG...]
#
#   -m l1       "CI-like": INSTANCES copies at once, each pinned to its own 4
#               CPUs (GitHub's ubuntu-latest for a public repo) with
#               GOMAXPROCS=4, each running `go test -race ./...` (default -p)
#               as CI's gotestsum does, one fresh run after another. No extra
#               load. PKG defaults to ./... .
#   -m l2       "harsh": INSTANCES shards per package at once, each with
#               GOMAXPROCS=2 and -race, running -count=RUNS/INSTANCES, plus
#               LOADERS busy loops. PKG defaults to the suspect list below.
#   -n RUNS     runs of each test (default 600: a 1-in-200 flake shows at
#               least once with about 95% probability, 1 - (199/200)^600)
#   -i INSTANCES  copies at once (default: l1 a quarter of the CPUs minus
#               two, so no two copies share a CPU; l2 30 per package)
#   -l LOADERS  l2 only: busy-loop processes (default: a quarter of the CPUs)
#   -o DIR      where to keep the raw go test -json output (default: a new
#               temp dir)
#   -r REGEX    passed to go test -run (default: every test)
#
# Every copy runs in its own network namespace (loopback up) as the invoking
# user, so copies never collide on a fixed address. CI runs in one
# namespace, and l1 keeps that where it matters: the packages of one l1 copy
# share its namespace, as in CI, so a collision between packages still
# shows. Needs go, jq, ip, unshare with unprivileged user namespaces, and
# taskset (l1). Test-only: it changes nothing in the tree.
#
# Output: a Markdown table on stdout, one row per test that failed at least
# once, sorted by count; DIR/failures.jsonl keeps every failing run's output.
set -euo pipefail

mode=""
runs=600
inst=""
loaders=$(( $(nproc) / 4 ))
out=""
run_re=""
while getopts "m:n:i:l:o:r:h" opt; do
	case "$opt" in
	m) mode=$OPTARG ;;
	n) runs=$OPTARG ;;
	i) inst=$OPTARG ;;
	l) loaders=$OPTARG ;;
	o) out=$OPTARG ;;
	r) run_re=$OPTARG ;;
	*) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
	esac
done
shift $((OPTIND - 1))
pkgs=("$@")
case "$mode" in
l1)
	[ ${#pkgs[@]} -gt 0 ] || pkgs=(./...)
	[ -n "$inst" ] || inst=$(( $(nproc) / 4 - 2 ))
	[ $(( inst * 4 )) -le "$(nproc)" ] || { echo "flake-census: $inst copies of 4 CPUs need more than $(nproc)" >&2; exit 2; }
	command -v taskset >/dev/null || { echo "flake-census: needs taskset" >&2; exit 2; }
	;;
l2)
	[ ${#pkgs[@]} -gt 0 ] || pkgs=(./internal/engine ./internal/conformance ./internal/cli ./internal/server ./internal/sctp ./internal/mockamf/...)
	[ -n "$inst" ] || inst=30
	;;
*) echo "flake-census: -m l1 or -m l2" >&2; exit 2 ;;
esac
[ -n "$out" ] || out=$(mktemp -d -t flake-census.XXXXXX)
mkdir -p "$out"
for tool in go jq unshare ip; do command -v "$tool" >/dev/null || { echo "flake-census: needs $tool" >&2; exit 2; }; done
[ "$inst" -ge 1 ] && [ "$runs" -ge "$inst" ] || { echo "flake-census: need RUNS >= INSTANCES >= 1" >&2; exit 2; }

# The outer user namespace maps the invoking user to root only to bring
# loopback up in the new network namespace; the inner one maps it back, so
# the tests run as that user.
uid=$(id -u)
gid=$(id -g)
netns_sh='u=$0; g=$1; shift; ip link set lo up && exec unshare --user --map-user="$u" --map-group="$g" "$@"'

# Each copy runs ceil(runs/inst) times, so every test runs at least RUNS times.
per=$(( (runs + inst - 1) / inst ))

load_pids=()
stop_load() { [ ${#load_pids[@]} -eq 0 ] || kill "${load_pids[@]}" 2>/dev/null || true; }
trap stop_load EXIT

go test -race -count=1 -run '^$' "${pkgs[@]}" >/dev/null # build once, so the copies share the cache

test_pids=()
if [ "$mode" = l1 ]; then
	echo "flake-census l1: $inst copies x $per runs = $((inst * per)) CI-like runs of ${pkgs[*]}; 4 CPUs and GOMAXPROCS=4 each, -race, default -p; raw output in $out" >&2
	for c in $(seq 0 $((inst - 1))); do
		cpus="$((c * 4))-$((c * 4 + 3))"
		(
			for r in $(seq 1 "$per"); do
				GOMAXPROCS=4 taskset -c "$cpus" unshare -rn sh -c "$netns_sh" "$uid" "$gid" \
					go test -race -json -count=1 ${run_re:+-run "$run_re"} "${pkgs[@]}" >"$out/l1.$c.$r.json" 2>"$out/l1.$c.$r.err" || true
			done
		) &
		test_pids+=($!)
	done
else
	for _ in $(seq 1 "$loaders"); do
		sh -c 'while :; do :; done' &
		load_pids+=($!)
	done
	echo "flake-census l2: ${#pkgs[@]} package(s), $((per * inst)) runs each ($inst shards x $per), $loaders loaders, -race, GOMAXPROCS=2; raw output in $out" >&2
	for p in "${pkgs[@]}"; do
		tag=$(echo "$p" | tr '/.' '__')
		for s in $(seq 1 "$inst"); do
			GOMAXPROCS=2 unshare -rn sh -c "$netns_sh" "$uid" "$gid" \
				go test -race -json -count="$per" -timeout=0 ${run_re:+-run "$run_re"} "$p" >"$out/l2.$tag.$s.json" 2>"$out/l2.$tag.$s.err" &
			test_pids+=($!)
		done
	done
fi
wait "${test_pids[@]}" || true
stop_load

# A failing run's error line, from what it printed between its "run" and
# "fail" events (framework lines such as === RUN and --- FAIL dropped; lines
# from passing runs never count). t.Log and t.Error print alike, so it is the
# first line that reads as an error (fail, error, want/got, timeout, panic,
# EOF, refused, broken pipe, mismatch, unexpected, deadline, in use), else
# the run's last line. A parent that failed only through a subtest has none
# of its own and says so.
for f in "$out"/*.json; do
	jq -c -n '
		reduce (inputs | select(.Test != null)) as $e ({buf: {}, fails: []};
			($e.Package + " " + $e.Test) as $k
			| if $e.Action == "run" then .buf[$k] = []
			elif $e.Action == "output" then
				if ($e.Output | test("^\\s*(=== |--- |PASS|FAIL|ok\\s)") | not) and ($e.Output | test("\\S"))
				then .buf[$k] += [$e.Output | gsub("^\\s+|\\s+$"; "")] else . end
			elif $e.Action == "fail" then
				(.buf[$k] // []) as $l
				| .fails += [{pkg: $e.Package, test: $e.Test, lines: $l,
					first: ((first($l[] | select(test("fail|error|want|got|timeout|timed out|panic|EOF|refused|broken pipe|mismatch|unexpected|deadline|in use"; "i"))) // $l[-1]) // "(a subtest failed)")}]
			else . end)
		| .fails[]' "$f" 2>/dev/null || echo "flake-census: could not parse $f" >&2
done >"$out/failures.jsonl"
jq -rs '
	group_by(.pkg + " " + .test)
	| map({pkg: .[0].pkg, test: .[0].test, n: length,
		firsts: (group_by(.first) | map({first: .[0].first, n: length}) | sort_by(-.n))})
	| sort_by(-.n, .test)
	| (["| Package | Test | Failures | Most common error line (its count) | Distinct error lines |", "|---|---|---|---|---|"]
		+ map("| \(.pkg | sub("^.*/internal/"; "")) | \(.test) | \(.n) | `\(.firsts[0].first | .[0:160] | gsub("\\|"; "\\\\|"))` (\(.firsts[0].n)) | \(.firsts | length) |"))
	| .[]' "$out/failures.jsonl"

# Runs that failed outside any test (build error, panic, a timeout) show here.
for f in "$out"/*.json; do
	if jq -e 'select(.Action == "fail" and .Test == null)' "$f" >/dev/null 2>&1 &&
		! jq -e 'select(.Action == "fail" and .Test != null)' "$f" >/dev/null 2>&1; then
		echo "package-level failure, no test named: $(basename "$f") (see ${f%.json}.err)"
	fi
done
echo "flake-census: done; raw output in $out" >&2
