# light-tokenmeter

[![Test](https://github.com/ai-process/light-tokenmeter/actions/workflows/test.yml/badge.svg)](https://github.com/ai-process/light-tokenmeter/actions/workflows/test.yml)
[![Docker image](https://img.shields.io/docker/v/coyl/light-tokenmeter?label=docker%20hub&sort=semver)](https://hub.docker.com/r/coyl/light-tokenmeter)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache_2.0-blue.svg)](LICENSE)

A very small service that answers one question: **who is spending your LLM tokens, and
are they worth it?** Your apps (or a gateway such as Keystone,
[KTMS](https://github.com/ai-process/ktms)) push one event per model call; you
query totals by project, user, action, vendor, model or status and see who is greedy and
who is worthy.

- One static Go binary, one Postgres database, a small image (see [Resource footprint](#resource-footprint)). No queue, no cache, no UI.
- **Bearer-token authentication** with `write` and `read` scopes, and Prometheus metrics.
- **Append-only and idempotent.** Every event has a UUID; sending it twice is harmless.
- **Fast writes.** Batches of up to 500 events are inserted with a single `COPY`.
- **Flexible reads.** Time-bucketed (per day) summaries with any combination of filters
  and groupings, plus aggregation over your own metadata (e.g. everything one batch job spent).
- **Two APIs.** HTTP/JSON for curl and dashboards, gRPC for services.
- **Scalable analytics with ClickHouse.** Optional: events are copied from Postgres to ClickHouse in batches, without loss or double counting, and reports are served from ClickHouse with automatic TTL retention.

## Contents

- [Quick start](#quick-start)
- [Resource footprint](#resource-footprint)
- [Installation](#installation)
- [Sending events](#sending-events)
- [Finding the greedy and the worthy](#finding-the-greedy-and-the-worthy)
- [API reference](#api-reference)
- [API stability](#api-stability)
- [Configuration](#configuration)
- [Security](#security)
- [Operations](#operations)
- [Development](#development)

## Quick start

Requires Docker with the Compose plugin.

```bash
git clone https://github.com/ai-process/light-tokenmeter.git
cd light-tokenmeter
docker compose up --build -d
curl -s localhost:8080/readyz        # {"status":"ready"}
```

Record an event and read it back:

```bash
curl -s -X POST localhost:8080/v1/usage/events -H 'Content-Type: application/json' -d '{
  "event_id": "550e8400-e29b-41d4-a716-446655440000",
  "user_id": "user-123", "project_id": "support-bot", "action_id": "answer-ticket",
  "vendor": "openai", "model": "gpt-4o-mini", "status": "success",
  "duration_ms": 1234, "tokens_input": 150, "tokens_output": 300
}'

curl -s "localhost:8080/v1/usage/summary?from=2020-01-01T00:00:00Z&to=2100-01-01T00:00:00Z&group_by_project_id=true&group_by_model=true"
```

The schema is created automatically on startup.

## Resource footprint

The image `coyl/light-tokenmeter:0.1.1` was ~28 MB uncompressed and ~11 MB to download for
linux/amd64 (Docker Hub manifest and `docker image inspect`). It was Alpine-based; from 0.1.2 the
runtime is distroless (static, nonroot). Memory, readiness and throughput have not been measured
yet.

## Installation

### Docker image

Release images are published for `linux/amd64` and `linux/arm64`:

```bash
docker pull coyl/light-tokenmeter:latest     # or pin a release, e.g. coyl/light-tokenmeter:1.0.0
```

It needs only a Postgres database:

```bash
docker run -d --name light-tokenmeter \
  -p 8080:8080 -p 9090:9090 \
  -e DATABASE_URL='postgres://user:pass@db-host:5432/tokenmeter?sslmode=require' \
  coyl/light-tokenmeter:latest
```

Alongside your own stack:

```yaml
services:
  light-tokenmeter:
    image: coyl/light-tokenmeter:latest
    restart: unless-stopped
    environment:
      DATABASE_URL: postgres://tokenmeter:${DB_PASSWORD}@db:5432/tokenmeter?sslmode=disable
    depends_on: [db]
    # no published ports: reach it from other containers at light-tokenmeter:8080 / :9090
```

Kubernetes: liveness probe `GET :8080/healthz`, readiness probe `GET :8080/readyz` (checks the
database). The service is stateless, so run as many replicas as you like. Images are signed; see
[Verifying a release](SECURITY.md#verifying-a-release).

### From source

Go 1.26+ and Postgres:

```bash
make build                                   # → bin/light-tokenmeter
export DATABASE_URL='postgres://tokenmeter:tokenmeter@localhost:5432/tokenmeter?sslmode=disable'
./bin/light-tokenmeter
```

Or `make run`. A `.env` file in the working directory is loaded if present (see `env.example`).

### Postgres

Any reasonably current Postgres works (16 is what the compose file uses). Create an empty
database and a user that may create tables; migrations run on every start and are embedded
in the binary. Table `usage_events` grows by one row per model call, so plan retention for
your volume (for example a periodic `DELETE ... WHERE created_at < now() - interval '13 months'`).

## Sending events

An event describes one model call:

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `event_id` | UUID | yes | Idempotency key; a repeated id is ignored. |
| `user_id` | string ≤256 | yes | Who the call was for. |
| `project_id` | string ≤256 | yes | Which app or tenant. |
| `action_id` | string ≤256 | yes | What it was for, e.g. `summarize-ticket`. |
| `vendor` | string ≤64 | yes | `openai`, `anthropic`, `google`, … (free-form). |
| `model` | string ≤128 | yes | Model name. |
| `status` | string | yes | `success`, `error`, `cancelled` or `timeout`. |
| `occurred_at` | RFC 3339 | no | When it happened, from 1900 to April 2262; the server's clock is used when it is missing or the zero time. |
| `duration_ms` | int | no | Call latency. |
| `tokens_input`, `tokens_output` | int | no | Token counts. |
| `tokens_total` | int | no | Defaults to input + output; if given it must equal that sum. |
| `error_class` | string ≤256 | no | Normalised error class. |
| `vendor_http_status` | int | no | HTTP status returned by the vendor. |
| `meta` | map of string | no | Your own tags, e.g. `{"job_id": "…"}`: non-empty keys, at most 16 KiB of keys and values per event. |
| `cost_nanousd` | int ≥0 | no | Call cost in nano-USD (1e-9 USD), priced by the caller; stored as-is and never recomputed. |
| `cache_read_tokens`, `cache_write_tokens` | int ≥0 | no | Input tokens read from and written to the vendor's prompt cache. They are part of `tokens_input`, not added to it, so together they must not exceed it; an event that breaks this is refused. Anthropic reports them outside its `input_tokens`: send the sum of all three as `tokens_input`. |

Batches (`POST /v1/usage/events:batch`, or `RecordUsageEvents` over gRPC) take up to 500
events and report how many were created and how many were duplicates:

```bash
curl -s -X POST localhost:8080/v1/usage/events:batch -H 'Content-Type: application/json' -d '{
  "events": [
    {"event_id":"550e8400-e29b-41d4-a716-446655440001","user_id":"u1","project_id":"p1",
     "action_id":"a","vendor":"openai","model":"gpt-4o-mini","status":"success",
     "tokens_input":150,"tokens_output":300},
    {"event_id":"550e8400-e29b-41d4-a716-446655440002","user_id":"u2","project_id":"p1",
     "action_id":"a","vendor":"google","model":"gemini-2.5-flash","status":"error",
     "error_class":"rate_limited","vendor_http_status":429}
  ]}'
# {"created_count":2,"duplicate_count":0}
```

The service stores token counts and optional caller-provided costs (`cost_nanousd`), avoiding runtime pricing lookups during ingestion. You can also multiply token totals by your own price list when reading summaries.

## Finding the greedy and the worthy

`GET /v1/usage/summary` returns one row per **day** and per combination of the dimensions
you group by. `from` (inclusive) and `to` (exclusive) are required, RFC 3339.

```bash
Q='from=2026-09-01T00:00:00Z&to=2026-10-01T00:00:00Z'

# Which project burned the most?
curl -s "localhost:8080/v1/usage/summary?$Q&group_by_project_id=true"

# Which models does one project lean on?
curl -s "localhost:8080/v1/usage/summary?$Q&project_id=support-bot&group_by_model=true"

# Which actions are expensive per request, and which fail?
curl -s "localhost:8080/v1/usage/summary?$Q&group_by_action_id=true&group_by_status=true"

# The heaviest users of one model
curl -s "localhost:8080/v1/usage/summary?$Q&model=gpt-4o&group_by_user_id=true"
```

Each row carries `tokens_input`, `tokens_output`, `tokens_total`, `request_count`,
`sum_duration_ms`, `cost_nanousd` (summed spend; divide by 1e9 for USD) and
`cache_read_tokens` / `cache_write_tokens` (the cached part of `tokens_input`), so you can
compute tokens per request, cost per request, average latency and error share yourself. The signals that usually separate greedy from worthy:

- high `tokens_total / request_count` on a trivial `action_id` (bloated prompts),
- a large share of tokens spent on `status=error` or `timeout` (paying for failures),
- an expensive model serving an `action_id` that a cheaper one handles equally well.

Filters: `project_id`, `user_id`, `vendor`, `model`, `action_id`, `status` (must be `success`, `error`, `cancelled`, or `timeout`; invalid values return 400). Groupings:
`group_by_project_id`, `group_by_user_id`, `group_by_vendor`, `group_by_model`,
`group_by_action_id`, `group_by_status` (each `=true`).

Responses group by day based on event time (`occurred_at`, falling back to `created_at`). If query results exceed 10,000 rows, the response returns `truncated: true` and caps results at 10,000.

### Usage by your own metadata

If you tag events with a key such as `job_id` in `meta`, the gRPC call `GetUsageByMeta`
returns everything that job spent (tokens and `cost_nanousd`), split by vendor and model
and optionally by a second meta key (say `task_id`), plus an optional `project_id` filter. Lookups on `meta.job_id`
are indexed via a partial expression index (`meta ? 'job_id'`). Responses include
`truncated: true` if matching aggregate groups exceed 10,000 rows.

```bash
grpcurl -plaintext -d '{"meta_key":"job_id","meta_value":"job-42","group_by_meta_key":"task_id"}' \
  localhost:9090 usage.v1.UsageService/GetUsageByMeta
```

gRPC reflection is on, so `grpcurl` needs no `.proto` files (with auth on, pass a token with
`-H 'authorization: Bearer …'`).

## API reference

### HTTP (`:8080`)

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/usage/events` | Record one event. |
| `POST` | `/v1/usage/events:batch` | Record up to 500 events. |
| `GET` | `/v1/usage/summary` | Aggregated usage, see above. |
| `GET` | `/healthz` | Liveness. |
| `GET` | `/readyz` | Readiness (database ping). |

Errors from `/v1/usage/*` are JSON `{"error": "<message>", "code": "<code>", "details": "<optional>"}`,
with code `invalid_json` or `validation_error` (400), `unauthenticated` (401), `forbidden` (403),
`request_too_large` (413, over 16 MiB) or `internal_error` (500). A refused batch names the first
bad event as `invalid event at index N: …`. `/readyz` answers 503 with
`{"status":"not ready","error":"database unavailable"}`; unknown paths and methods get a plain
404 or 405. Usage routes need a bearer token when auth is on, see
[Authentication](#authentication).

### gRPC (`:9090`)

`usage.v1.UsageService`, defined in [`proto/usage/v1/usage.proto`](proto/usage/v1/usage.proto):
`RecordUsageEvent`, `RecordUsageEvents`, `GetUsageSummary`, `GetUsageByMeta`. Go stubs live in
[`gen/usage/v1`](gen/usage/v1) (`github.com/ai-process/light-tokenmeter/gen/usage/v1`).
Invalid input is `INVALID_ARGUMENT`; a refused batch names the first bad event as
`invalid event at index N: …`, so a client can drop that event and resend the rest. Messages may
be up to 16 MiB.

## API stability

From 1.0, light-tokenmeter follows [semantic versioning](https://semver.org): within 1.x nothing
below changes incompatibly. Breaking changes wait for 2.0. Minor releases add features; patch
releases fix bugs.

### What is public

| Surface | What stays compatible in 1.x |
| --- | --- |
| gRPC `usage.v1` (`UsageService`) | No RPC, field, enum value or message is removed or renamed, and no field number or type changes. New RPCs and fields may appear. `buf breaking` checks every pull request. |
| HTTP `/v1/usage/*` | Paths, query parameters, request and response fields of the [events](#sending-events) and [summary](#finding-the-greedy-and-the-worthy) endpoints. New fields and parameters may appear, so ignore unknown fields. |
| Errors | HTTP status codes and `code` values above; gRPC status codes; the `invalid event at index N` message prefix on both. New codes may appear, so treat an unknown one like its status. |
| Authentication | `Authorization: Bearer`, the `write` and `read` scopes and the `TOKENMETER_TOKENS` entry format, see [Authentication](#authentication). |
| Probes | `/healthz` and `/readyz`, their status codes and the `status` and `clickhouse` fields. |
| Metrics | Names, types and labels of the `tokenmeter_*` metrics in [Metrics](#metrics). New metrics and label values may appear. |
| Environment variables | Names and meaning of every variable in [Configuration](#configuration), and their defaults, except tuning knobs (`CLICKHOUSE_BATCH_SIZE`, `CLICKHOUSE_FLUSH_INTERVAL`, `DB_MAX_CONNS`, `DB_MIN_CONNS`), whose defaults may change in a minor release with a changelog note. |
| Storage | Any 1.x release upgrades the Postgres and ClickHouse schemas of an earlier one by itself at startup, with no manual steps. The tables are not an API, except as the recipes in this README use them. |
| Limits | 500 events per batch, 16 MiB per request, and the field and `meta` limits in [Sending events](#sending-events) never shrink. |

Not public, and free to change in any release: log message text (other than the prefix above),
the `internal/` Go packages, the outbox's lease and retry timings, and resource numbers.

### Compatibility with KTMS and ktms-admin

| light-tokenmeter | Keystone (KTMS) | ktms-admin | Authentication |
| --- | --- | --- | --- |
| 1.x | 1.x | 1.x | On or off. With it on, KTMS needs `USAGE_COLLECTOR_TOKEN` (`write`) and ktms-admin `TOKENMETER_TOKEN` (`read`). |
| 1.x | 0.x | 0.x | Off only: releases before 1.0 send no token. |

`usage.v1` stays compatible within 1.x, so any KTMS 1.x reports to any light-tokenmeter 1.x.

### Versions and upgrades

- **Within 1.x, upgrade in one step** from any earlier 1.x release: Postgres and ClickHouse
  migrations run forward at startup and cover every release in between.
  `scripts/upgrade-test.sh` upgrades from the previous release and from the oldest release of the
  same major on every pull request and before an image is published, with both databases, and
  checks the totals.
- **Rolling upgrades work within 1.x:** migrations only add, so replicas of the previous release
  keep serving on the migrated databases while new ones start; the upgrade test runs the old
  release again, on its own, after the new one migrated. A change that cannot be additive waits
  for 2.0.
- **Downgrades are not supported.** To go back, restore the Postgres backup taken before the
  upgrade (ClickHouse can be rebuilt from it, see [ClickHouse](#clickhouse)).
- **From 0.x**, upgrade straight to 1.0; its release is tested from 0.1.0 and 0.1.2. Later
  releases are tested from 1.x ones only.

Nothing is experimental in 1.0. KTMS keeps a copy of `usage.proto` for its usage reports; its CI
fails when the copy differs from this repository's.

## Configuration

Environment variables (see [`env.example`](env.example)):

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | local dev URL | Postgres connection string. **Always set it.** |
| `GRPC_ADDR` | `:9090` | gRPC listen address. |
| `HTTP_ADDR` | `:8080` | HTTP listen address. |
| `METRICS_ADDR` | `:9091` | Prometheus `/metrics` and `/healthz`, see [Metrics](#metrics). Set it empty to turn them off. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LOG_FORMAT` | `json` | Log format: `json` (structured JSON on stdout) or `console` (colored human-readable). |
| `SHUTDOWN_TIMEOUT` | `30s` | Graceful shutdown window. |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `25` / `5` | Connection pool bounds per replica. |
| `TOKENMETER_TOKENS` | empty | Accepted tokens as `name:scope[+scope]:sha256hex`, comma-separated; see [Authentication](#authentication). Empty, with no file, turns auth off. |
| `TOKENMETER_TOKENS_FILE` | empty | A file of the same entries, one per line, added to `TOKENMETER_TOKENS`; for a mounted secret. |
| `SENTRY_DSN` | empty | Error reporting; empty disables it. |
| `SENTRY_ENVIRONMENT` / `SENTRY_RELEASE` | `development` / empty | Sentry metadata. |
| `SENTRY_ENABLE_TRACING` | `false` | Sentry performance tracing. |
| `SENTRY_DEBUG` | `false` | Enable Sentry SDK debug logging. |
| `SENTRY_WITH_BREADCRUMBS` | `false` | Include breadcrumbs in Sentry error reports. |
| `CLICKHOUSE_ENABLED` | `false` | Copy events to ClickHouse and serve reports from it, see [ClickHouse](#clickhouse). |
| `CLICKHOUSE_ADDR` | `localhost:9000` | ClickHouse native TCP address. |
| `CLICKHOUSE_DATABASE` | `tokenmeter` | ClickHouse database name. |
| `CLICKHOUSE_USER` | `default` | ClickHouse user. |
| `CLICKHOUSE_PASSWORD` | empty | ClickHouse password. |
| `CLICKHOUSE_BATCH_SIZE` | `5000` | Most events copied to ClickHouse in one insert. |
| `CLICKHOUSE_FLUSH_INTERVAL` | `1s` | How often waiting events are copied to ClickHouse; reports lag by about this much. |

## Security

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities, verifying releases and running the
service safely.

### Authentication

Every usage call needs a bearer token once `TOKENMETER_TOKENS` (or `TOKENMETER_TOKENS_FILE`)
lists at least one. A token has one or both scopes:

| Scope | Allows |
| --- | --- |
| `write` | `POST /v1/usage/events`, `POST /v1/usage/events:batch`, `RecordUsageEvent`, `RecordUsageEvents` |
| `read` | `GET /v1/usage/summary`, `GetUsageSummary`, `GetUsageByMeta` |

`/healthz`, `/readyz` and gRPC health (`grpc.health.v1.Health`, a liveness check) never need a
token. gRPC reflection accepts any configured token, so `grpcurl` works with `-H` below.

The service stores only SHA-256 hashes. Each entry is `name:scope[+scope]:sha256hex`; entries
are separated by commas or newlines, and in a file `#` starts a comment:

```bash
TOKEN=$(openssl rand -hex 32)                  # give this to the client
printf %s "$TOKEN" | openssl dgst -sha256 -r | cut -d' ' -f1 # put this in the config
TOKENMETER_TOKENS="ktms:write:<hash>,console:read:<hash>"
```

Clients send `Authorization: Bearer <token>`, as an HTTP header or as gRPC metadata:

```bash
curl -H "Authorization: Bearer $TOKEN" 'localhost:8080/v1/usage/summary?from=…&to=…'
grpcurl -plaintext -H "authorization: Bearer $TOKEN" localhost:9090 usage.v1.UsageService/GetUsageSummary
```

| Problem | HTTP | gRPC |
| --- | --- | --- |
| No token, or an unknown one | `401` `unauthenticated` with `WWW-Authenticate: Bearer` | `UNAUTHENTICATED` |
| The token lacks the scope | `403` `forbidden` | `PERMISSION_DENIED` |

**Rotation without downtime**: add the new token's entry next to the old one and restart (or
roll) the service, switch the clients to the new token, then remove the old entry. Any number of
entries may be active at once, each token in one entry only. With `LOG_LEVEL=debug` every
accepted call logs the token's name, so you can see when the old one stops being used; refused
calls are logged at `warn`.

A `TOKENMETER_TOKENS_FILE` that is missing, malformed or holds no tokens stops the service at
startup rather than turning auth off.

With no tokens configured the service logs a warning at startup and lets every call in. Use that
only on a private network you trust.

### Transport and data

The service speaks plain HTTP and gRPC, so a token crosses the network in the clear. Outside a
private network, terminate TLS in front of it (Envoy, nginx, Caddy, a service mesh), and do not
publish ports 8080/9090 on a public interface. Events can contain whatever you put in `user_id`
and `meta`; avoid personal data you would not want in a database.

## Operations

- **Migrations** run at startup and are embedded in the binary.
- **Upgrades**: deploy a newer image tag; see [Versions and upgrades](#versions-and-upgrades).
- **Backups**: Postgres holds everything (ClickHouse can be rebuilt from it); back it up like any other.
- **Logs**: structured JSON on stdout.

### Metrics

`/metrics` on `METRICS_ADDR` (`:9091`), with the Go and process metrics:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `tokenmeter_events_total` | counter | `result`: `created`, `duplicate`, `invalid` | Events received. A refused batch counts all its events as `invalid`; a body that cannot be read shows only in `tokenmeter_requests_total` (400 or 413). |
| `tokenmeter_requests_total` | counter | `transport` (`http`, `grpc`), `route` (pattern or RPC), `code` | API requests, including those refused by auth; probes are not counted. A client that gives up shows as `Canceled` or `DeadlineExceeded`. |
| `tokenmeter_request_duration_seconds` | histogram | `transport`, `route` | API request duration: writes and report queries. |
| `tokenmeter_storage_errors_total` | counter | `store` (`postgres`, `clickhouse`), `op` | Failed storage operations. |
| `tokenmeter_read_fallbacks_total` | counter | | Reports answered from Postgres because ClickHouse failed. |
| `tokenmeter_clickhouse_enabled` | gauge | | 1 when `CLICKHOUSE_ENABLED` is set. |
| `tokenmeter_clickhouse_serving` | gauge | | 1 while reports come from ClickHouse; 0 while not connected, backfilling, or after a failed ClickHouse query. |
| `tokenmeter_clickhouse_copied_events_total` | counter | | Events copied to ClickHouse. |
| `tokenmeter_clickhouse_insert_failures_total` | counter | `reason`: `unreachable`, `refused` | Failed ClickHouse inserts (retried or postponed, never dropped). |
| `tokenmeter_clickhouse_outbox_events` | gauge | | Events waiting for ClickHouse, sampled every 15 s and counted up to 1 000 000. |
| `tokenmeter_clickhouse_outbox_oldest_seconds` | gauge | | Age of the oldest waiting event: how far ClickHouse reports trail Postgres. |
| `tokenmeter_build_info` | gauge | `version` | 1, labelled with the running release. |

Suggested alerts:

```yaml
groups:
  - name: light-tokenmeter
    rules:
      - alert: TokenmeterDown
        expr: up{job="light-tokenmeter"} == 0
        for: 2m
      - alert: TokenmeterWritesFailing # events are not being recorded
        expr: sum(rate(tokenmeter_requests_total{route=~".*(events|RecordUsageEvent).*", code=~"5..|Internal|Unavailable"}[5m])) > 0
        for: 5m
      - alert: TokenmeterInvalidEvents # a client sends events that are refused
        expr: sum(rate(tokenmeter_events_total{result="invalid"}[15m])) > 0
        for: 15m
      - alert: TokenmeterAuthRefusals # a client lost its token, or someone is probing
        expr: sum(rate(tokenmeter_requests_total{code=~"401|403|Unauthenticated|PermissionDenied"}[10m])) > 0.1
        for: 10m
      - alert: TokenmeterClickHouseBehind # ClickHouse-served reports undercount
        expr: max(tokenmeter_clickhouse_outbox_oldest_seconds) > 600 and on() max(tokenmeter_clickhouse_serving) == 1
        for: 10m
      - alert: TokenmeterClickHouseRefusing # a batch waits for a fix, see "ClickHouse"
        expr: sum(increase(tokenmeter_clickhouse_insert_failures_total{reason="refused"}[30m])) > 0
      - alert: TokenmeterReportsFromPostgres # ClickHouse configured but not serving
        expr: tokenmeter_clickhouse_enabled == 1 and tokenmeter_clickhouse_serving == 0
        for: 15m
```

### ClickHouse

Postgres is always the record. With `CLICKHOUSE_ENABLED=true`:

- **Writes** go to Postgres as usual; each new event is also added, in the same statement, to a
  `clickhouse_outbox` table. A duplicate `event_id` is added to neither.
- **Copying**: every `CLICKHOUSE_FLUSH_INTERVAL` each replica takes up to `CLICKHOUSE_BATCH_SIZE`
  waiting events and inserts them into ClickHouse, then removes them from the outbox. Replicas
  take different batches.
- **Reads**: `/v1/usage/summary`, `GetUsageSummary` and `GetUsageByMeta` are answered from
  ClickHouse, so they trail writes by about one flush interval. If ClickHouse fails a query, or
  is not reachable yet, the answer comes from Postgres.
- **Startup and readiness**: the service starts and is ready without ClickHouse; it connects in
  the background and `/readyz` reports `"clickhouse":"unavailable"` meanwhile (or while it is
  down), without failing.
- **ClickHouse unreachable** (or failing for reasons not about the rows): the replica keeps its
  batch, renews its two-minute lease, and retries with backoff up to 15 s apart. A batch still
  failing after five minutes is postponed like a refused one, in case the trouble is its own.
  Nothing is dropped, and writes keep working; the outbox drains once ClickHouse is back.
- **ClickHouse refusing rows** (a data error such as a type mismatch, often a changed schema): the
  batch is postponed, 30 s and then up to 10 minutes later, while the events behind it flow. It
  keeps its deduplication token and is never split or dropped, so it cannot be counted twice; an
  error with the batch ID and ClickHouse's message is logged on every refusal. Fix the cause and
  the batch goes through at its next retry.
- **Restart or crash**: waiting events stay in the outbox. A batch inserted but not yet removed
  is taken over when its lease ends and inserted again under the same deduplication token;
  ClickHouse ignores the repeat, as it remembers the last 100 000 inserts
  (`non_replicated_deduplication_window`; a `ReplicatedMergeTree` you create yourself needs
  `replicated_deduplication_window`).

Migrations need a ClickHouse user allowed to create and alter `usage_events`; until they apply,
the service records to Postgres and logs the error. Set `CLICKHOUSE_ENABLED` the same on every
replica: an event written by a replica with it off is never queued for ClickHouse.

The first time ClickHouse is turned on for a database, and its `usage_events` is still empty, the
last 90 days of Postgres history (what ClickHouse's TTL keeps) are queued for it, and reports
stay on Postgres until that history is copied. Turn ClickHouse on after an upgrade from 0.x, not
during the roll: old replicas would write to it directly and could be counted twice. Events recorded while it
was off later on are not copied; Postgres reports still include them. Versions before 1.0 kept
events in memory and could drop them; to rebuild ClickHouse from Postgres, empty its
`usage_events` table and run
`INSERT INTO clickhouse_outbox (event_id) SELECT event_id FROM usage_events ON CONFLICT DO NOTHING`
in Postgres.

## Development

```bash
make test        # go test -race ./...
make build       # bin/light-tokenmeter
make proto       # regenerate gen/ (needs buf)
docker compose up --build
```

The storage tests need Postgres on `localhost:5433` (or `DATABASE_URL`) and ClickHouse on
`localhost:9000` (or `CLICKHOUSE_TEST_ADDR`), and skip without them. Each test creates and drops a
database of its own (the Postgres role needs `CREATEDB`), so tests never touch other data; databases
left by an interrupted run are dropped by the next run after an hour. CI runs them with `REQUIRE_DB=1`, which turns a missing database into a
failure:

```bash
docker run -d --rm -p 5433:5432 -e POSTGRES_USER=tokenmeter -e POSTGRES_PASSWORD=tokenmeter postgres:17-alpine
docker run -d --rm -p 9000:9000 -e CLICKHOUSE_USER=tokenmeter -e CLICKHOUSE_PASSWORD=tokenmeter -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 clickhouse/clickhouse-server:24.3-alpine
REQUIRE_DB=1 make test
```

Layout: `cmd/server` (entrypoint), `internal/server` (HTTP + gRPC), `internal/service`
(business logic), `internal/repository` (SQL), `internal/domain` (models and validation),
`internal/database` (pool and embedded migrations).

### Releasing

1. Move the `[Unreleased]` entries of [`CHANGELOG.md`](CHANGELOG.md) under a new version heading
   with today's date, in a pull request.
2. After it is merged, tag main and push the tag:

   ```bash
   git tag v1.0.0 && git push origin v1.0.0
   ```

The [Release](.github/workflows/release.yml) workflow then runs the upgrade test from earlier
releases and `govulncheck`, creates the GitHub Release from the changelog section, and publishes
the signed multi-arch image `coyl/light-tokenmeter:1.0.0`; `1.0` moves to it when it is the newest
release of 1.0.x, and `latest` when it is the newest release of all. Tags are `vX.Y.Z` only, with
no pre-releases; push one release tag at a time. The repository needs the secrets `DOCKER_USERNAME` and `DOCKER_PASSWORD`. Run
the upgrade test locally (needs Docker) with `./scripts/upgrade-test.sh`.

Pull requests are welcome; see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE)
