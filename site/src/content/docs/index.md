---
title: ORBIT
description: Load, fleet and conformance testing for 5G SA cores — one CLI, one dashboard.
template: splash
hero:
  tagline: Load, fleet and conformance testing for 5G SA cores — one CLI, one dashboard.
  actions:
    - text: Install
      link: /install/
      icon: right-arrow
    - text: View on GitHub
      link: https://github.com/grewelltech/orbit
      icon: external
      variant: minimal
---

## Install in one line

```sh
curl -fsSL https://orbit.grewelltech.com/install.sh | sh
```

Already installed? The same command upgrades in place. See [Install & upgrade](/install/)
for pinned versions, a custom install directory, and how the download is verified.

## What it does

- **Load** — attach storms: subscribers attached and released at a controlled rate, gated on attach success.
- **Fleet** — a standing population attached for the run's duration, with user-plane probing.
- **Conformance** — the conformance suite against a core, gated on no unexpected FAIL or ERROR.

Both a CLI and a web dashboard drive the same server-owned runs, so anything one can do the other can too. Start from the [Quickstart](/quickstart/).

## Verified downloads

Every release ships a `checksums.txt` signed with [cosign](https://docs.sigstore.dev/) (keyless,
via the release workflow's GitHub identity) and [SLSA build provenance](https://slsa.dev/). The
installer always checks the SHA-256, and verifies the signature when `cosign` is present — see
[Install & upgrade](/install/#verifying-the-download).
