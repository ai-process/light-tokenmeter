# Releasing light-tokenmeter

For maintainers. Versions follow [API stability](README.md#api-stability): a fix is a patch, a
feature is a minor, a break of what is public waits for a new major.

## Steps

1. In a pull request, move the `[Unreleased]` entries of [`CHANGELOG.md`](CHANGELOG.md) under a
   new `## [X.Y.Z] - YYYY-MM-DD` heading and add its compare link at the bottom.
2. After it is merged, tag main and push the tag:

   ```bash
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```

Tags are `vX.Y.Z` only, with no pre-releases. Push one release tag at a time.

## What the workflow does

[`release.yml`](.github/workflows/release.yml), in order:

1. **Checks**: the whole [test workflow](.github/workflows/test.yml), including `govulncheck` and
   the upgrade test from the previous release and the oldest release of the same major.
2. **Draft**: a draft GitHub Release whose notes are the tag's `CHANGELOG.md` section; a tag
   without one stops here.
3. **Binaries** (GoReleaser, [`.goreleaser.yaml`](.goreleaser.yaml)): the `tokenmeter` archives
   for darwin and linux, amd64 and arm64, a cosign-signed `checksums.txt` and an SBOM per archive.
4. **Images** ([`docker-publish.yml`](.github/workflows/docker-publish.yml)): signed multi-arch
   `coyl/light-tokenmeter` and `coyl/tokenmeter-cli` tagged `X.Y.Z`; `X.Y` moves when it is the
   newest release of its line, `latest` when it is the newest of all.
5. **Publish**: the draft becomes the release, marked Latest only for the newest tag, so a
   backport is not.

Re-running a failed run picks up where it stopped: the draft is edited, not duplicated, and
GoReleaser replaces the archives on it.

## Secrets

| Secret | For |
| --- | --- |
| `DOCKER_USERNAME`, `DOCKER_PASSWORD` (an access token) | Pushing the images |
| `HOMEBREW_TAP_TOKEN` (optional) | The `tokenmeter` formula in `ai-process/homebrew-tap`; the repository must exist. Without it the formula is skipped |

Signing needs no secret: cosign signs keylessly with the workflow's OIDC identity, which
[Verifying a release](SECURITY.md#verifying-a-release) pins.

## Locally

```bash
./scripts/upgrade-test.sh   # the upgrade test (needs Docker)
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish,sign,sbom   # sbom needs syft
```
