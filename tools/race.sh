#!/bin/sh
# Race-detector run of the given packages (default ./...), what "make race"
# does, but internal/auth (PBKDF2 in nearly every test, about half of all
# race time) is split by test name into AUTH_PARTS processes that run next to
# the other packages. AUTH_PARTS=1 (or fewer than 4 CPUs) runs one plain
# go test; hosted runners of private repositories have two cores, where
# parallel race runs oversubscribe the machine and make timing tests flaky.
#
# CI (FULL tier) runs it in two shards: RACE_SHARD=1/2 tests only the packages listed in HEAVY
# (internal/auth), RACE_SHARD=2/2 all the others, so a new package lands in shard 2 on its own.
# "tools/race.sh --check" fails unless every package of ./... is in exactly one shard.
#
#   tools/race.sh [package ...]
#   RACE_SHARD=i/2 tools/race.sh
#   tools/race.sh --check
set -eu
export CGO_ENABLED=1
GO=${GO:-go}
parts=${AUTH_PARTS:-2}
[ "$(nproc)" -ge 4 ] || parts=1
auth=github.com/nofuturekid/tasmofleet/internal/auth
HEAVY=$auth

# shard_pkgs <i>: the packages of ./... in shard i of 2.
shard_pkgs() {
	if [ "$1" = 1 ]; then
		$GO list ./... | grep -Fx "$HEAVY" || true
	else
		$GO list ./... | grep -vFx "$HEAVY" || true
	fi
}

if [ "${1:-}" = --check ]; then
	all=$($GO list ./... | sort)
	got=$( (shard_pkgs 1; shard_pkgs 2) | sort)
	if [ "$all" != "$got" ]; then
		echo "race shards: a package is in no shard or in both (< only in ./..., > only in the shards):" >&2
		tmp=$(mktemp -d)
		echo "$all" >"$tmp/all"
		echo "$got" >"$tmp/got"
		diff "$tmp/all" "$tmp/got" >&2 || true
		rm -rf "$tmp"
		exit 1
	fi
	[ -n "$(shard_pkgs 1)" ] && [ -n "$(shard_pkgs 2)" ] || { echo "race shards: a shard is empty" >&2; exit 1; }
	echo "race shards: all $(echo "$all" | wc -l) packages are in exactly one of 2 shards"
	exit 0
fi

if [ -n "${RACE_SHARD:-}" ] && [ $# -eq 0 ]; then
	pkgs=$(shard_pkgs "${RACE_SHARD%/*}")
	[ -n "$pkgs" ] || { echo "race: shard $RACE_SHARD has no packages" >&2; exit 1; }
else
	[ $# -gt 0 ] || set -- ./...
	pkgs=$($GO list "$@")
fi
if [ "$parts" -le 1 ] || ! echo "$pkgs" | grep -qx "$auth"; then
	exec $GO test -race -count=1 -timeout 40m $pkgs
fi
others=$(echo "$pkgs" | grep -vx "$auth" || true)
names=$($GO test -list '.*' "$auth" | grep '^Test')
pids=""
if [ -n "$others" ]; then
	$GO test -race -count=1 -timeout 30m $others &
	pids="$!"
fi
i=0
while [ $i -lt "$parts" ]; do
	re=$(echo "$names" | awk -v n="$parts" -v i=$i 'NR % n == i' | paste -sd'|')
	$GO test -race -count=1 -timeout 30m "$auth" -run "^($re)\$" &
	pids="$pids $!"
	i=$((i + 1))
done
rc=0
for p in $pids; do wait "$p" || rc=1; done
exit $rc
