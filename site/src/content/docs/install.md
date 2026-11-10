---
title: Install & upgrade
description: Install or upgrade ORBIT from the official release channel, and verify the download.
---

## One-liner

```sh
curl -fsSL https://orbit.grewelltech.com/install.sh | sh
```

This downloads the release archive for your OS/architecture from GitHub Releases, verifies its
SHA-256 against the release's signed `checksums.txt`, and installs the `orbit` binary. Running it
again **upgrades in place**. Linux `amd64` and `arm64` are published today.

Prefer to read before you run? The script is served at a stable URL — inspect it first:

```sh
curl -fsSL https://orbit.grewelltech.com/install.sh -o install.sh
less install.sh
sh install.sh
```

## Options

The installer takes flags (or the matching environment variables):

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `--version vX.Y.Z` | `ORBIT_VERSION` | latest | Install a specific release. |
| `--bin-dir <dir>` | `ORBIT_BIN_DIR` | `/usr/local/bin`, else `~/.local/bin` | Where to install. |
| `--require-signature` | — | off | Fail unless the cosign signature verifies. |
| `--no-verify-signature` | — | off | Skip cosign even if installed (SHA-256 still enforced). |

```sh
# a pinned version into ~/.local/bin, requiring a verified signature
curl -fsSL https://orbit.grewelltech.com/install.sh | \
  sh -s -- --version v0.1.0 --bin-dir "$HOME/.local/bin" --require-signature
```

## Verifying the download

Verification is layered; the installer always enforces at least the checksum, and uses stronger
checks when the tools are present.

1. **SHA-256 (always).** Every artifact's hash is listed in `checksums.txt`; the downloaded
   archive is checked against it.
2. **cosign signature.** `checksums.txt` is signed keyless with [cosign](https://docs.sigstore.dev/),
   using the release workflow's GitHub OIDC identity and logged in the Rekor transparency log. The
   installer verifies it automatically when `cosign` is installed, pinning the expected identity;
   pass `--require-signature` to make it mandatory. To check by hand:

   ```sh
   cosign verify-blob \
     --bundle checksums.txt.cosign.bundle \
     --certificate-identity-regexp '^https://github.com/grewelltech/orbit/.github/workflows/release.yml@refs/tags/' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     checksums.txt
   ```

3. **SLSA build provenance.** Each archive carries signed [SLSA](https://slsa.dev/) provenance,
   verifiable with the GitHub CLI:

   ```sh
   gh attestation verify orbit-vX.Y.Z-linux-amd64.tar.gz --repo grewelltech/orbit
   ```

## Manual install

Download the archive and `checksums.txt` from the
[releases page](https://github.com/grewelltech/orbit/releases), verify, then extract:

```sh
sha256sum --check --ignore-missing checksums.txt
tar -xzf orbit-vX.Y.Z-linux-amd64.tar.gz
sudo install -m 0755 orbit-vX.Y.Z-linux-amd64/orbit /usr/local/bin/orbit
orbit version
```

## From source

```sh
go install github.com/grewelltech/orbit/cmd/orbit@latest
```
