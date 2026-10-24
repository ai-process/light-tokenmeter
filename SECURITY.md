# Security Policy

light-tokenmeter stores who spends tokens on what. We appreciate the responsible disclosure of
any potential vulnerability.

## Supported Versions

| Version | Supported |
| ------- | --------- |
| `1.x`, latest minor release | :white_check_mark: |
| `0.1.x` | :x: upgrade to 1.0 (one step, see [Versions and upgrades](README.md#versions-and-upgrades)) |

## Verifying a Release

From 1.0.0 on, images are signed with [Sigstore cosign](https://docs.sigstore.dev/) using GitHub
Actions' OIDC identity (keyless: there is no key to leak), and carry an SBOM and SLSA provenance.
Each platform manifest is signed too. Use cosign 3 and pin the identity to the exact tag:

```bash
V=v1.0.0
cosign verify coyl/light-tokenmeter:${V#v} \
  --certificate-identity "https://github.com/ai-process/light-tokenmeter/.github/workflows/docker-publish.yml@refs/tags/$V" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect coyl/light-tokenmeter:${V#v} --format '{{ json .SBOM }}'
docker buildx imagetools inspect coyl/light-tokenmeter:${V#v} --format '{{ json .Provenance }}'
```

The CLI image `coyl/tokenmeter-cli` is signed the same way. The CLI's archives on the GitHub
release (`checksums.txt` is signed; each archive has an `.sbom.json`), from 1.1.0 on:

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/ai-process/light-tokenmeter/.github/workflows/release.yml@refs/tags/$V" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

Every pull request and every release runs [`govulncheck`](https://go.dev/doc/security/vuln/): a
known vulnerability that light-tokenmeter's code can reach fails the build, and a release does not
go out.

## Running it safely

- **Turn authentication on** (`TOKENMETER_TOKENS`): give each client its own token with the scope
  it needs, `write` for KTMS and other senders, `read` for ktms-admin and dashboards. Only hashes
  are configured; see [Authentication](README.md#authentication) for rotation.
- **Terminate TLS in front of it** outside a private network: the service speaks plain HTTP and
  gRPC, so a token crosses the network in the clear otherwise.
- **Keep `METRICS_ADDR` internal**: `/metrics` has no authentication.
- **Mind what goes into events**: `user_id` and `meta` hold whatever you send; avoid personal data
  you would not want in a database.

## Reporting a Vulnerability

**Please do not report security vulnerabilities via public GitHub issues, discussions or pull
requests.** Use one of these private channels:

1. **GitHub Private Vulnerability Reporting:** open the
   [Security Advisories tab](https://github.com/ai-process/light-tokenmeter/security/advisories/new)
   and click **Report a vulnerability**.
2. **Email:** [security@vasiliy.pro](mailto:security@vasiliy.pro) with the subject prefix
   `[SECURITY] light-tokenmeter: <brief description>`.

Please include the affected version, steps to reproduce, and the impact you see. We acknowledge
reports within three working days and coordinate disclosure with you.
