---
title: Quickstart
description: Install ORBIT, start the server, and run your first test.
---

A short path from nothing to a run. The full walkthrough lives in
[QUICKSTART.md](https://github.com/grewelltech/orbit/blob/main/docs/QUICKSTART.md); the complete
command and config reference is in
[USAGE.md](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md).

## 1. Install

```sh
curl -fsSL https://orbit.grewelltech.com/install.sh | sh
orbit --help
```

## 2. Serve

The server owns run execution; the CLI and the dashboard are both clients of it.

```sh
orbit serve --state-dir ~/.local/state/orbit
```

Open the dashboard at `http://localhost:8412/` — the home page lists every run, live and finished.

## 3. Define a target

A target is where a run points: a core's AMF, PLMN and (optionally) a subscriber pool.

```sh
orbit target init lab1 --amf 127.0.0.1:38412 --mcc 001 --mnc 01 --sd 010203 \
  --first-supi 001010000000001 --count 100
```

`--count` sizes the subscriber pool; it defaults to 1, so set it to at least
the `--count` your load test uses (100 below) or the run is rejected for a pool
that is too small.

Subscriber keys stay out of the target file — `target init` writes `${ORBIT_KI}`
and `${ORBIT_OPC}` in their place, expanded from your shell when the target
loads. Export them before a run, or loading the target will fail on the unset
references:

```sh
export ORBIT_KI=<hex> ORBIT_OPC=<hex>
```

Keeping a testbed's settings in a file instead? Put the `KEY=VALUE` lines in a
dotenv file and pass `--env-file`, which loads them before the run — in place
of `set -a; . file; set +a`. It feeds both the `${ORBIT_KI}`/`${ORBIT_OPC}`
expansion above and the `ORBIT_*` config layer (`ORBIT_MCC`, `ORBIT_TAC`,
`ORBIT_SD`, …); an already-exported variable still wins. See
[Config from an env file](https://github.com/bgrewell/orbit/blob/main/docs/USAGE.md#config-from-an-env-file---env-file).

```sh
orbit conformance -t lab1 --env-file testbed.env
```

## 4. Run a test

```sh
# a conformance suite
orbit conformance -t lab1

# an attach storm
orbit load -t lab1 --count 100 --rate 20
```

Conformance results print as a colored table on a terminal (plain output when piped, `--json` for
machines). `orbit conformance` and `orbit load` each wait for the run to finish, so review the
results afterward:

```sh
orbit runs list            # every run, newest first
orbit runs get <run-id>    # the full result of a finished run
```

To follow a run live, run `orbit runs watch` in a second terminal *while* a test is in flight — with
no id it follows the active run, or pass a `<run-id>`. It fails with "no active run" if nothing is
running, which is expected once the commands above have completed.

## Next

- Start a test from the dashboard's **New test** chooser instead of the CLI — they are peers.
- Read [USAGE.md](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md) for targets, run
  files, gates, and the conformance suite in depth.
