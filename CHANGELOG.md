# Changelog

All notable changes to light-tokenmeter are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org); see [API stability](README.md#api-stability) for what
1.x keeps compatible. From 1.0 releases are tagged `vX.Y.Z`; the image tag has no `v`
(`coyl/light-tokenmeter:1.0.0`).

## [Unreleased]

## [1.0.0] - 2026-10-23

The first stable release, together with Keystone. The Model Switchboard. (KTMS) 1.0 and
ktms-admin 1.0. From here on, what [API stability](README.md#api-stability) lists changes
incompatibly only in a new major version. Upgrade from 0.1.x in one step.

### Added
- Authentication: bearer tokens with `write` and `read` scopes, configured as SHA-256 hashes in
  `TOKENMETER_TOKENS` or `TOKENMETER_TOKENS_FILE`, several at once for rotation. Refusals are
  `401`/`403` over HTTP and `UNAUTHENTICATED`/`PERMISSION_DENIED` over gRPC. Without tokens the
  service runs open and warns at startup. gRPC health is registered; reflection needs a token.
- Prometheus metrics on `METRICS_ADDR` (`:9091`): events, requests and their duration, storage
  errors, ClickHouse copying, outbox size and age, and `tokenmeter_build_info`. The README lists
  them with suggested alerts.
- README "API stability" and "Versions and upgrades": what 1.x keeps compatible, one-step and
  rolling upgrades, no downgrades.
- An upgrade test (`scripts/upgrade-test.sh`) from the previous release and the oldest of the
  same major, with Postgres and ClickHouse, on every pull request and before each release.
- `buf lint`, `buf breaking` and a generated-code check on every pull request; `govulncheck` on
  every pull request and release.
- Releases: `vX.Y.Z` tags, a GitHub Release with this changelog's section, and multi-arch images
  signed with cosign, with SBOM and provenance (see [SECURITY.md](SECURITY.md)).

### Changed
- ClickHouse no longer loses events. Events reach it through a Postgres outbox written in the same
  statement as the event, copied in leased batches under a deduplication token, so a full buffer,
  a ClickHouse outage or a crash neither drops nor double-counts them. The service starts and
  stays ready without ClickHouse, and reads fall back to Postgres when it fails.
- The first start with an empty ClickHouse copies the last 90 days of Postgres history to it;
  reports stay on Postgres until that is done.
- Validation before the freeze: `error_class` up to 256 characters (longer used to fail with a
  500), `meta` up to 16 KiB of keys and values with no empty keys, requests up to 16 MiB (`413
  request_too_large` over HTTP), and `occurred_at` between 1900 and 2262-04-10; the zero time means
  unset.
- HTTP batch errors name the bad event as `invalid event at index N`, like gRPC.
- Summary rows leave out `status` unless grouped by it, like the other grouping fields.
- Relicensed under Apache 2.0.

### Security
- clickhouse-go v2.49.0 with ch-go v0.74.0, fixing GO-2025-3603 in the ClickHouse protocol reader.

## [0.1.2] - 2026-10-21

### Added
- Optional ClickHouse storage for writes and analytics at high volume.
- `cost_nanousd` on usage events, aggregated in summaries.

### Changed
- The runtime image is distroless (static, nonroot).
- Go 1.27.3 and Alpine 3.23 with current CA certificates.

### Fixed
- A retried batch no longer reaches ClickHouse twice.

## [0.1.1] - 2026-10-14

### Fixed
- The `job_id` index is used for meta lookups, summaries bucket by event time, and `truncated`
  is reported.
- Go and dependency vulnerabilities (Go 1.27, `x/net`, gRPC).

## [0.1.0] - 2026-10-07

First release: append-only, idempotent usage events over HTTP and gRPC, daily summaries with
filters and groupings, and usage by meta.

[Unreleased]: https://github.com/ai-process/light-tokenmeter/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/ai-process/light-tokenmeter/compare/0.1.2...v1.0.0
[0.1.2]: https://github.com/ai-process/light-tokenmeter/compare/0.1.1...0.1.2
[0.1.1]: https://github.com/ai-process/light-tokenmeter/compare/0.1.0...0.1.1
[0.1.0]: https://github.com/ai-process/light-tokenmeter/releases/tag/0.1.0
