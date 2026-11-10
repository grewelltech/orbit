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
  --first-supi 001010000000001
```

## 4. Run a test

```sh
# a conformance suite
orbit conformance -t lab1

# an attach storm
orbit load -t lab1 --count 100 --rate 20
```

Conformance results print as a colored table on a terminal (plain output when piped, `--json` for
machines). Watch any run from the terminal or the dashboard:

```sh
orbit runs watch
orbit runs list
```

## Next

- Start a test from the dashboard's **New test** chooser instead of the CLI — they are peers.
- Read [USAGE.md](https://github.com/grewelltech/orbit/blob/main/docs/USAGE.md) for targets, run
  files, gates, and the conformance suite in depth.
