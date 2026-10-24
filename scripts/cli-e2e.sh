#!/bin/sh
# The tokenmeter CLI against a built light-tokenmeter, once on Postgres alone
# and once with ClickHouse: tokens, events, summary, top, by-meta, export.
# Env: IMAGE (a built service image; default builds this checkout).
set -eu

cd "$(dirname "$0")/.."

P=tm-cli-e2e-$$
NET=$P-net
IMAGE=${IMAGE:-}
BUILT=""
TMP=$(mktemp -d)

cleanup() {
	docker rm -f -v "$P-svc" "$P-pg" "$P-ch" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	[ -z "$BUILT" ] || docker rmi -f "$BUILT" >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT

fail() {
	echo "cli e2e: $*" >&2
	docker logs "$P-svc" 2>&1 | tail -n 40 >&2 || true
	exit 1
}

# retry N CMD... runs CMD until it succeeds, once a second.
retry() {
	n=$1
	shift
	i=0
	until "$@"; do
		i=$((i + 1))
		[ "$i" -lt "$n" ] || return 1
		sleep 1
	done
}

if [ -z "$IMAGE" ]; then
	IMAGE="$P-image"
	BUILT=$IMAGE
	docker build -q -t "$IMAGE" . >/dev/null
fi
go build -o "$TMP/tokenmeter" ./cmd/tokenmeter
PATH="$TMP:$PATH"
export TOKENMETER_CONFIG="$TMP/config.yaml"

tokenmeter token new --name writer --scope write >"$TMP/w" 2>/dev/null
tokenmeter token new --name reader --scope read >"$TMP/r" 2>/dev/null
WRITE=$(sed -n 1p "$TMP/w")
READ=$(sed -n 1p "$TMP/r")
TOKENS="$(sed -n 2p "$TMP/w"),$(sed -n 2p "$TMP/r")"

# check MODE runs the scenario against a fresh service; MODE is pg or clickhouse.
check() {
	echo "=== $1"
	docker rm -f -v "$P-svc" "$P-pg" "$P-ch" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker network create "$NET" >/dev/null
	docker run -d --name "$P-pg" --network "$NET" \
		-e POSTGRES_USER=tokenmeter -e POSTGRES_PASSWORD=tokenmeter -e POSTGRES_DB=tokenmeter postgres:17-alpine >/dev/null
	ch=""
	if [ "$1" = clickhouse ]; then
		docker run -d --name "$P-ch" --network "$NET" -e CLICKHOUSE_USER=tokenmeter -e CLICKHOUSE_PASSWORD=tokenmeter \
			-e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 clickhouse/clickhouse-server:24.3-alpine >/dev/null
		retry 90 docker exec "$P-ch" wget -q --spider http://127.0.0.1:8123/ping >/dev/null 2>&1 || fail "clickhouse did not start"
		ch="-e CLICKHOUSE_ENABLED=true -e CLICKHOUSE_ADDR=$P-ch:9000 -e CLICKHOUSE_DATABASE=tokenmeter -e CLICKHOUSE_USER=tokenmeter -e CLICKHOUSE_PASSWORD=tokenmeter"
	fi
	# Over TCP: the socket answers while the image's init server still runs.
	retry 60 docker exec "$P-pg" pg_isready -h 127.0.0.1 -U tokenmeter -d tokenmeter >/dev/null 2>&1 || fail "postgres did not start"
	# shellcheck disable=SC2086 # $ch is a list of flags
	docker run -d --name "$P-svc" --network "$NET" -p 127.0.0.1::8080 -p 127.0.0.1::9090 \
		-e DATABASE_URL="postgres://tokenmeter:tokenmeter@$P-pg:5432/tokenmeter?sslmode=disable" \
		-e TOKENMETER_TOKENS="$TOKENS" $ch "$IMAGE" >/dev/null
	TOKENMETER_ADDR=$(docker port "$P-svc" 9090/tcp | head -n 1)
	TOKENMETER_HTTP="http://$(docker port "$P-svc" 8080/tcp | head -n 1)"
	export TOKENMETER_ADDR TOKENMETER_HTTP
	retry 90 tokenmeter health >/dev/null 2>&1 || fail "the service did not become ready"
	tokenmeter health

	echo "--- event send"
	export TOKENMETER_TOKEN=$WRITE
	at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	send() {
		tokenmeter event send --project "$1" --user u1 --action a --vendor openai --model "$2" \
			--tokens-in 100 --tokens-out 50 --cost-usd "$3" --at "$at" --meta job_id=j1 >/dev/null
	}
	send p1 m1 0.01
	send p1 m2 0.02
	send p2 m1 0.5
	if tokenmeter usage summary >/dev/null 2>&1; then fail "a write token read a summary"; fi

	echo "--- usage summary"
	export TOKENMETER_TOKEN=$READ
	# With ClickHouse, reads follow the outbox replication.
	three() { tokenmeter usage summary --last 1d -o json | grep -q '"request_count": "3"'; }
	retry 30 three || { tokenmeter usage summary --last 1d -o json >&2; fail "the summary misses events"; }
	tokenmeter usage summary --last 1d --group-by project
	# USD on the CLI is cost_nanousd / 1e9 from the API.
	tokenmeter usage summary --last 1d --group-by project -o json | grep -q '"cost_nanousd": "30000000"' || fail "p1 cost_nanousd"
	tokenmeter usage export --last 1d --group-by project >"$TMP/export.csv"
	cat "$TMP/export.csv"
	grep -q ',p1,2,200,100,300,0,0,0,0.03,30000000$' "$TMP/export.csv" || fail "export row for p1"
	grep -q ',p2,1,100,50,150,0,0,0,0.5,500000000$' "$TMP/export.csv" || fail "export row for p2"

	echo "--- usage top and by-meta"
	tokenmeter usage top --last 1d --by cost --dimension project | sed -n 2p | grep -q 'p2' || fail "top is not p2"
	tokenmeter usage by-meta --key job_id --value j1 -o json | grep -q '"request_count": "2"' || fail "by-meta for m1"

	echo "--- mcp over stdio"
	TOKENMETER_E2E_BIN="$TMP/tokenmeter" go test -count=1 -tags e2e -run '^TestMCPStdio$' ./cmd/tokenmeter/ || fail "mcp disagrees with the API"
	if TOKENMETER_TOKEN=$WRITE tokenmeter mcp </dev/null 2>/dev/null; then fail "mcp ran with a write token"; fi

	code=0
	TOKENMETER_TOKEN=wrong tokenmeter usage summary 2>/dev/null || code=$?
	[ "$code" = 4 ] || fail "a wrong token exited $code, want 4"
}

check pg
check clickhouse
echo "cli e2e: ok"
