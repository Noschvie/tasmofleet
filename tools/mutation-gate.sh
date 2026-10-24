#!/bin/sh
# Mutation gate for one package. Fails loud instead of passing vacuously.
#
#   tools/mutation-gate.sh <package> <min-efficacy-%>
#   tools/mutation-gate.sh --sum <package> <min-efficacy-%> <counts-file>...
#
# Fails when
#   - gremlins itself fails,
#   - no mutant was decided (killed + lived == 0): nothing was really tested,
#   - timed-out mutants exceed MUTATION_MAX_TIMEOUT_PCT (default 20) of all run
#     mutants (killed + lived + timed out): the run is not trustworthy,
#   - efficacy killed / (killed + lived) is below the package threshold.
#
# Sharding (CI runs the big packages on several runners): with
# MUTATION_SHARD=i/n only shard i of n of the package's files is mutated
# (files dealt out by size, largest first, to the lightest shard), the counts
# are appended to MUTATION_COUNTS_OUT as "killed lived timedout", and no
# threshold is applied. --sum then adds up the shards' counts files and
# applies the same checks to the whole package, so a sharded run decides
# exactly like an unsharded one.
#
# MUTATION_WORKERS (default nproc/2, at least 2) and MUTATION_TIMEOUT_COEFF (default 20) tune
# gremlins for slow or busy machines; a coefficient that is too low turns every
# mutant into a timeout.
set -eu
maxto=${MUTATION_MAX_TIMEOUT_PCT:-20}

# decide <pkg> <min> <killed> <lived> <timedout>
decide() {
	pkg=$1 min=$2 killed=$3 lived=$4 timedout=$5
	decided=$((killed + lived))
	ran=$((decided + timedout))
	if [ "$decided" -eq 0 ]; then
		echo "mutation gate: no mutant was killed or survived in $pkg (timed out: $timedout); the gate would pass without testing anything" >&2
		exit 1
	fi
	if [ $((timedout * 100)) -gt $((ran * maxto)) ]; then
		echo "mutation gate: $timedout of $ran mutants timed out in $pkg (> $maxto%); raise MUTATION_TIMEOUT_COEFF or lower load" >&2
		exit 1
	fi
	eff=$((killed * 100 / decided))
	if [ "$eff" -lt "$min" ]; then
		echo "mutation gate: efficacy $eff% < $min% in $pkg ($lived mutants lived)" >&2
		exit 1
	fi
	echo "mutation gate: $pkg ok (efficacy $eff%, $timedout timed out)"
}

if [ "${1:-}" = --sum ]; then
	pkg=$2 min=$3
	shift 3
	[ $# -gt 0 ] || { echo "mutation gate: no counts files for $pkg" >&2; exit 1; }
	set -- $(cat "$@" | awk 'NF == 3 { k += $1; l += $2; t += $3; n++ } END { print k + 0, l + 0, t + 0, n + 0 }')
	echo "== mutation $pkg: $4 shards, Killed: $1, Lived: $2, Timed out: $3"
	decide "$pkg" "$min" "$1" "$2" "$3"
	exit 0
fi

pkg=$1
min=$2
# Default: half of JOBS (the Makefile's per-job CPU budget) or of the CPUs;
# each worker runs a full go test of the package.
ncpu=${JOBS:-$(nproc 2>/dev/null || echo 4)}
workers=${MUTATION_WORKERS:-$(( ncpu / 2 > 2 ? ncpu / 2 : 2 ))}
coeff=${MUTATION_TIMEOUT_COEFF:-20}
tool=${TOOL:-go tool -modfile=tools/go.mod}

excludes=""
shard=${MUTATION_SHARD:-}
if [ -n "$shard" ]; then
	[ -n "${MUTATION_COUNTS_OUT:-}" ] || { echo "mutation gate: MUTATION_SHARD needs MUTATION_COUNTS_OUT" >&2; exit 1; }
	i=${shard%/*} n=${shard#*/}
	dir=$(go list -f '{{.Dir}}' "$pkg")
	files=$(go list -f '{{range .GoFiles}}{{.}} {{end}}' "$pkg")
	# Largest file first to the lightest shard; keep the files of shard i.
	mine=$(cd "$dir" && wc -l $files | grep -v ' total$' | sort -k1,1nr -k2 | awk -v n="$n" '
		{ b = 1; for (s = 2; s <= n; s++) if (load[s] < load[b]) b = s; load[b] += $1; print b, $2 }' |
		awk -v i="$i" '$1 == i { print $2 }')
	for f in $files; do
		echo "$mine" | grep -qx "$f" || excludes="$excludes -E (^|/)$(echo "$f" | sed 's/[.]/[.]/g')\$"
	done
	if [ -z "$mine" ]; then
		echo "== mutation $pkg shard $shard: no files"
		echo "0 0 0" >>"$MUTATION_COUNTS_OUT"
		exit 0
	fi
	echo "== mutation $pkg shard $shard:" $mine
fi

out=$(mktemp)
trap 'rm -f "$out"' EXIT
echo "== mutation $pkg (efficacy >= $min%, workers $workers, timeout coefficient $coeff)"
# shellcheck disable=SC2086 # excludes is a list of flags
if ! $tool gremlins unleash --workers "$workers" --timeout-coefficient "$coeff" $excludes "$pkg" >"$out" 2>&1; then
	cat "$out"
	echo "mutation gate: gremlins failed for $pkg" >&2
	exit 1
fi
grep -E '^ *(LIVED|TIMED OUT)' "$out" || true
# A shard whose files hold no mutant (e.g. only doc.go) has nothing to report;
# its share is zero. Unsharded, "no results" falls through and fails below.
if [ -n "$shard" ] && grep -q '^No results to report' "$out" && ! grep -q 'Killed:' "$out"; then
	echo "no mutants in this shard"
	echo "0 0 0" >>"$MUTATION_COUNTS_OUT"
	exit 0
fi
summary=$(grep -E 'Killed: [0-9]+, Lived: [0-9]+|Timed out: [0-9]+' "$out" | tr '\n' ' ')
echo "$summary"
killed=$(echo "$summary" | sed -n 's/.*Killed: \([0-9]*\).*/\1/p')
lived=$(echo "$summary" | sed -n 's/.*Lived: \([0-9]*\).*/\1/p')
timedout=$(echo "$summary" | sed -n 's/.*Timed out: \([0-9]*\).*/\1/p')
if [ -z "$killed" ] || [ -z "$lived" ] || [ -z "$timedout" ]; then
	cat "$out"
	echo "mutation gate: could not parse the gremlins summary for $pkg" >&2
	exit 1
fi
if [ -n "$shard" ]; then
	echo "$killed $lived $timedout" >>"$MUTATION_COUNTS_OUT"
	exit 0
fi
decide "$pkg" "$min" "$killed" "$lived" "$timedout"
