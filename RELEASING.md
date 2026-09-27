# Releasing

One GitHub release serves both the Terraform registry and the OpenTofu
registry. `.goreleaser.yml` produces what both expect:
- a zip per platform;
- `_manifest.json`;
- `_SHA256SUMS` and its binary GPG signature `_SHA256SUMS.sig`.

Each zip also gets an SPDX SBOM. The zips, the manifest and the SBOMs get build
provenance attestations. SHA256SUMS leaves out the SBOMs, because every line
of it ends up in users' lock files.

## Setup, once

- **Signing key.** "turfbuild release signing <security@turf.build>", RSA
  4096, fingerprint `0A5B 3535 5F7C 6D01 286B  9E11 C319 99AE 1374 A19C`, no
  expiry. It has to be RSA: the Terraform registry rejects ECC keys. The
  private key and its passphrase are the `GPG_PRIVATE_KEY` and `PASSPHRASE`
  secrets of the `release` environment, which admits only `v*` tags. The
  ASCII-armored public key is registered with both registries.
- **Terraform registry.** The provider is published from
  `turfbuild/terraform-provider-kubewait`. The registry's webhook ingests each
  published release.
- **OpenTofu registry.** The provider and the key were submitted through the
  issue forms on `opentofu/registry`. It picks up new releases within about
  30 minutes.
- **Repository.** Immutable releases are on, and only admins can create `v*`
  tags.

## Cutting a release

1. `main` is green: CI and govulncheck.
2. Tag and push. Versions are semver with a `v` prefix. Before 1.0, a schema or
   semantics change is a minor release, and a fix is a patch.

   ```sh
   git tag -a v0.1.0 -m v0.1.0
   git push origin v0.1.0
   ```

3. The Release workflow builds, signs and attests a **draft** release.
4. Verify the draft:

   ```sh
   v=0.1.0
   gh release download v$v --repo turfbuild/terraform-provider-kubewait --dir /tmp/kubewait-$v
   cd /tmp/kubewait-$v
   gpg --verify terraform-provider-kubewait_${v}_SHA256SUMS.sig terraform-provider-kubewait_${v}_SHA256SUMS
   shasum -a 256 -c terraform-provider-kubewait_${v}_SHA256SUMS
   gh attestation verify terraform-provider-kubewait_${v}_linux_amd64.zip --repo turfbuild/terraform-provider-kubewait
   gh attestation verify terraform-provider-kubewait_${v}_linux_amd64.zip.spdx.json --repo turfbuild/terraform-provider-kubewait
   unzip -o terraform-provider-kubewait_${v}_linux_amd64.zip terraform-provider-kubewait_v$v
   govulncheck -mode=binary terraform-provider-kubewait_v$v
   ```

   If anything is wrong, delete the draft and the tag, fix, and tag again.
   Nothing reaches a registry before the release is published.
5. Edit the notes if needed, then publish. Check that both registries list the
   version:

   ```sh
   curl -s https://registry.terraform.io/v1/providers/turfbuild/kubewait/versions
   curl -s https://registry.opentofu.org/v1/providers/turfbuild/kubewait/versions
   ```

6. Set `VERSION` in `GNUmakefile` to the next patch, so a `make mirror` build
   never carries a published version number.

A published version is never deleted or replaced; publish a new one.

## Security fixes

govulncheck runs on every push and pull request, and daily. It scans the source
with the Go that builds releases, `go.mod`'s `toolchain`. It also scans the
latest release's binary, which records the Go and modules it was built with.

| Finding | Fix |
| --- | --- |
| A vulnerable module | Bump it (Renovate usually opens the PR), then cut a patch release. |
| A Go security release | Renovate bumps `toolchain` in `go.mod`. Merge it, then cut a patch release. |
| Only the latest release is affected | `main` is already fixed; cut a patch release. |
| A vulnerability in kubewait itself | It arrives by private vulnerability reporting. Open a repository security advisory (a CVE can be requested there), fix it, cut a patch release, then publish the advisory with the fixed version. |
