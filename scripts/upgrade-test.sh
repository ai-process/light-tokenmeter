#!/bin/sh
# Proves README "Versions and upgrades": an older release's Postgres and
# ClickHouse keep working under this build, and under the old release again.
# Env: PREV (tags to upgrade from), CUR (tag being released), NEW_IMAGE.
set -eu

cd "$(dirname "$0")/.."

# Releases before 1.0 were tagged X.Y.Z, later ones vX.Y.Z; the image tag
# has no "v". Only tags reachable from HEAD count, so a backport branch is
# checked against its own line.
stable_tags() {
	git tag --merged HEAD --sort=-v:refname | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$' | grep -vx "${CUR:-none}" || true
}
# has_image fails the run on registry errors: reading a rate limit as "never
# published" would quietly compare against an older release.
has_image() {
	if out=$(docker manifest inspect "coyl/light-tokenmeter:${1#v}" 2>&1); then
		return 0
	fi
	case $out in
	*"no such manifest"* | *"manifest unknown"*) return 1 ;;
	esac
	echo "cannot check coyl/light-tokenmeter:${1#v}: $out" >&2
	exit 1
}
pick_prev() {
	for t in $(stable_tags); do
		if has_image "$t"; then
			echo "$t"
			return
		fi
	done
}
# The oldest release of the same major: "one step from any earlier release"
# is promised within a major. The first release of a major (1.0.0) is
# instead tested from the oldest release of the major before it.
pick_oldest() {
	major=$(echo "${CUR:-$1}" | sed -E 's/^v?([0-9]+)\..*/\1/')
	if ! stable_tags | grep -qE "^v?$major\."; then
		major=$(echo "$1" | sed -E 's/^v?([0-9]+)\..*/\1/')
	fi
	for t in $(stable_tags | grep -E "^v?$major\." | sed 's/^v//' | sort -t. -k2,2n -k3,3n); do
		for tag in "v$t" "$t"; do
			if git rev-parse -q --verify "refs/tags/$tag" >/dev/null && has_image "$tag"; then
				echo "$tag"
				return
			fi
		done
	done
}

if [ -z "${PREV:-}" ]; then
	prev=$(pick_prev)
	oldest=$([ -n "$prev" ] && pick_oldest "$prev" || true)
	PREV=$prev
	[ -n "$oldest" ] && [ "$oldest" != "$prev" ] && PREV="$prev $oldest"
fi
[ -n "$PREV" ] || { echo "no published previous release found" >&2; exit 1; }
if [ "${1:-}" = --print-prev ]; then
	echo "$PREV"
	exit 0
fi

P=tm-upgrade-$$
NET=$P-net
NEW_IMAGE=${NEW_IMAGE:-}
BUILT=""
PULLED=""
STATE=$(mktemp)
export UPGRADE_STATE="$STATE"

cleanup() {
	docker rm -f -v "$P-old" "$P-new" "$P-old-again" "$P-pg" "$P-ch" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker rmi -f $BUILT $PULLED >/dev/null 2>&1 || true
	rm -f "$STATE"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

wait_ready() {
	http=$(docker port "$1" 8080/tcp | head -n 1)
	i=0
	until curl -fsS "http://$http/readyz" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 90 ]; then
			echo "$1 did not become ready" >&2
			docker logs "$1" 2>&1 | tail -n 40 >&2
			exit 1
		fi
		sleep 1
	done
}

# run_meter NAME IMAGE starts a service on the shared databases and points the
# test client at it.
run_meter() {
	docker run -d --name "$1" --network "$NET" -p 127.0.0.1::8080 \
		-e DATABASE_URL="postgres://tokenmeter:tokenmeter@$P-pg:5432/tokenmeter?sslmode=disable" \
		-e CLICKHOUSE_ENABLED=true -e CLICKHOUSE_ADDR="$P-ch:9000" -e CLICKHOUSE_DATABASE=tokenmeter \
		-e CLICKHOUSE_USER=tokenmeter -e CLICKHOUSE_PASSWORD=tokenmeter \
		"$2" >/dev/null
	wait_ready "$1"
	TM_HTTP=$(docker port "$1" 8080/tcp | head -n 1)
	export TM_HTTP
}

phase() { go test -count=1 -tags upgrade -run "^$1\$" ./test/upgrade/; }

if [ -z "$NEW_IMAGE" ]; then
	NEW_IMAGE="$P-new-image"
	BUILT=$NEW_IMAGE
	docker build -q -t "$NEW_IMAGE" . >/dev/null
fi

for from in $PREV; do
	echo "upgrade test: $from -> this checkout"
	docker rm -f -v "$P-old" "$P-new" "$P-old-again" "$P-pg" "$P-ch" >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker network create "$NET" >/dev/null
	docker run -d --name "$P-pg" --network "$NET" \
		-e POSTGRES_USER=tokenmeter -e POSTGRES_PASSWORD=tokenmeter -e POSTGRES_DB=tokenmeter postgres:17-alpine >/dev/null
	docker run -d --name "$P-ch" --network "$NET" -e CLICKHOUSE_USER=tokenmeter -e CLICKHOUSE_PASSWORD=tokenmeter \
		-e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 clickhouse/clickhouse-server:24.3-alpine >/dev/null
	# Over TCP: the socket answers while the image's init server still runs.
	until docker exec "$P-pg" pg_isready -h 127.0.0.1 -U tokenmeter -d tokenmeter >/dev/null 2>&1; do sleep 1; done
	until docker exec "$P-ch" wget -q --spider http://127.0.0.1:8123/ping >/dev/null 2>&1; do sleep 1; done

	old="coyl/light-tokenmeter:${from#v}"
	if ! docker image inspect "$old" >/dev/null 2>&1; then
		PULLED="$PULLED $old"
	fi
	docker pull -q "$old" >/dev/null
	run_meter "$P-old" "$old"
	phase TestSeed
	docker stop -t 30 "$P-old" >/dev/null

	run_meter "$P-new" "$NEW_IMAGE"
	phase TestVerify
	docker stop -t 30 "$P-new" >/dev/null

	run_meter "$P-old-again" "$old"
	phase TestServe
	echo "upgrade test passed: $from -> this checkout"
done
