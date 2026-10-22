# light-tokenmeter

[![Test](https://github.com/ai-process/light-tokenmeter/actions/workflows/test.yml/badge.svg)](https://github.com/ai-process/light-tokenmeter/actions/workflows/test.yml)
[![Docker image](https://img.shields.io/docker/v/coyl/light-tokenmeter?label=docker%20hub&sort=semver)](https://hub.docker.com/r/coyl/light-tokenmeter)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache_2.0-blue.svg)](LICENSE)

A very small service that answers one question: **who is spending your LLM tokens, and
are they worth it?** Your apps (or a gateway such as
[KTMS](https://github.com/ai-process/ktms)) push one event per model call; you
query totals by project, user, action, vendor, model or status and see who is greedy and
who is worthy.

- One static Go binary, one Postgres table, a small image (see [Resource footprint](#resource-footprint)). No queue, no cache, no UI.
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

The published image `coyl/light-tokenmeter:0.1.1` (`latest`) is ~28 MB uncompressed and ~11 MB to
download for linux/amd64 (Docker Hub manifest and `docker image inspect`). That image is Alpine-based;
the distroless runtime in the current [`Dockerfile`](Dockerfile) is not released yet. Memory,
readiness and throughput have not been measured.

## Installation

### Docker image

Release images are published for `linux/amd64` and `linux/arm64`:

```bash
docker pull coyl/light-tokenmeter:latest     # or pin a tag, e.g. coyl/light-tokenmeter:0.1.0
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
database). The service is stateless, so run as many replicas as you like.

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
| `occurred_at` | RFC 3339 | no | When it happened, between 1900 and 2299; the server's clock is used otherwise. |
| `duration_ms` | int | no | Call latency. |
| `tokens_input`, `tokens_output` | int | no | Token counts. |
| `tokens_total` | int | no | Defaults to input + output; if given it must equal that sum. |
| `error_class` | string | no | Normalised error class. |
| `vendor_http_status` | int | no | HTTP status returned by the vendor. |
| `meta` | map of string | no | Your own tags (keep it small), e.g. `{"job_id": "…"}`. |
| `cost_nanousd` | int ≥0 | no | Call cost in nano-USD (1e-9 USD), priced by the caller; stored as-is and never recomputed. |

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
`sum_duration_ms` and `cost_nanousd` (summed spend; divide by 1e9 for USD), so you can
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

Errors are JSON with a `code` (`validation_error`, `internal_error`, `unauthenticated`,
`forbidden`) and a message. Usage routes need a bearer token when auth is on, see
[Authentication](#authentication).

### gRPC (`:9090`)

`usage.v1.UsageService`, defined in [`proto/usage/v1/usage.proto`](proto/usage/v1/usage.proto):
`RecordUsageEvent`, `RecordUsageEvents`, `GetUsageSummary`, `GetUsageByMeta`. Go stubs live in
[`gen/usage/v1`](gen/usage/v1) (`github.com/ai-process/light-tokenmeter/gen/usage/v1`).

## Configuration

Environment variables (see [`env.example`](env.example)):

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | local dev URL | Postgres connection string. **Always set it.** |
| `GRPC_ADDR` | `:9090` | gRPC listen address. |
| `HTTP_ADDR` | `:8080` | HTTP listen address. |
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
- **Upgrades**: deploy a newer image tag.
- **Backups**: Postgres holds everything (ClickHouse can be rebuilt from it); back it up like any other.
- **Logs**: structured JSON on stdout.

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
  the background and `/readyz` reports `"clickhouse":"unavailable"` meanwhile, without failing.
- **ClickHouse unreachable** (or failing for reasons not about the rows): the replica keeps its
  batch, renews its two-minute lease, and retries with backoff up to 15 s apart. Nothing is
  dropped, and writes keep working; the outbox drains once ClickHouse is back.
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

Events recorded before ClickHouse was turned on, or while it was off, are not copied; Postgres
reports still include them. Versions before 1.0 kept events in memory and could drop them; to rebuild ClickHouse from
Postgres, empty its `usage_events` table and run
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

Pushing a git tag publishes a multi-arch image to Docker Hub as `coyl/light-tokenmeter:<tag>`
(and `latest` for semver tags). The repository needs the secrets `DOCKER_USERNAME` and
`DOCKER_PASSWORD`.

```bash
git tag 0.1.0 && git push origin 0.1.0
```

Pull requests are welcome; see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE)
