#!/bin/sh
# Mutation gate for one package. Fails loud instead of passing vacuously.
#
#   tools/mutation-gate.sh <package> <min-efficacy-%>
#
# Fails when
#   - gremlins itself fails,
#   - no mutant was decided (killed + lived == 0): nothing was really tested,
#   - timed-out mutants exceed MUTATION_MAX_TIMEOUT_PCT (default 20) of all run
#     mutants (killed + lived + timed out): the run is not trustworthy,
#   - efficacy killed / (killed + lived) is below the package threshold.
#
# MUTATION_WORKERS (default nproc/2, at least 2) and MUTATION_TIMEOUT_COEFF (default 20) tune
# gremlins for slow or busy machines; a coefficient that is too low turns every
# mutant into a timeout.
set -eu
pkg=$1
min=$2
# Default: half of JOBS (the Makefile's per-job CPU budget) or of the CPUs;
# each worker runs a full go test of the package.
ncpu=${JOBS:-$(nproc 2>/dev/null || echo 4)}
workers=${MUTATION_WORKERS:-$(( ncpu / 2 > 2 ? ncpu / 2 : 2 ))}
coeff=${MUTATION_TIMEOUT_COEFF:-20}
maxto=${MUTATION_MAX_TIMEOUT_PCT:-20}
tool=${TOOL:-go tool -modfile=tools/go.mod}

out=$(mktemp)
trap 'rm -f "$out"' EXIT
echo "== mutation $pkg (efficacy >= $min%, workers $workers, timeout coefficient $coeff)"
if ! $tool gremlins unleash --workers "$workers" --timeout-coefficient "$coeff" "$pkg" >"$out" 2>&1; then
	cat "$out"
	echo "mutation gate: gremlins failed for $pkg" >&2
	exit 1
fi
grep -E '^ *(LIVED|TIMED OUT)' "$out" || true
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
