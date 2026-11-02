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
# (files dealt out by estimated cost, see tools/mutation-shard.sh), the counts
# are appended to MUTATION_COUNTS_OUT as "killed lived timedout notcovered scope" (and the mutants nobody
# killed to MUTATION_COUNTS_OUT.findings, see tools/mutation-report.sh), and no
# threshold is applied. --sum then adds up the shards' counts files and
# applies the same checks to the whole package, so a sharded run decides
# exactly like an unsharded one.
#
# MUTATION_WORKERS (default nproc/2, at least 2) and MUTATION_TIMEOUT_COEFF (default 20) tune
# gremlins for slow or busy machines; a coefficient that is too low turns every
# mutant into a timeout.
#
# Diff scope (#475): with MUTATION_DIFF=<ref> (make mutation-changed passes BASE) only the mutants on
# lines this branch changed since the merge base with <ref> are run, not the whole package. The
# thresholds judge those mutants alone, so a handful of them is a noisy sample: below
# MUTATION_DIFF_MIN (default 10) decided mutants the percentage is replaced by "at least one killed,
# at most one lived", and the timeout share is not judged. No decided mutant on the changed lines
# passes (a comment, a declaration, code no test reaches), loudly. The whole package is mutated as
# before when no non-test file of it changed (the tests themselves changed, so every mutant counts)
# or when an untracked file is in it (git diff cannot see it).
#
# gremlins --diff is only correct under two conditions, both set up here and pinned by
# scripts/mutation-diff.test.mjs: it must run INSIDE the package directory (with a package path
# argument its positions are package-relative and match no path of the repo-relative diff, so every
# mutant is silently skipped), and git must diff with 0 lines of context and relative paths
# (gremlins assumes the added lines of a hunk are contiguous, so with the default 3 lines of
# context it drops the second changed line of any hunk that holds two).
set -eu
maxto=${MUTATION_MAX_TIMEOUT_PCT:-20}
mindiff=${MUTATION_DIFF_MIN:-10}

# decide <pkg> <min> <killed> <lived> <timedout> <notcovered> <scope: package|diff>
decide() {
	pkg=$1 min=$2 killed=$3 lived=$4 timedout=$5 notcovered=$6 scope=$7
	decided=$((killed + lived))
	ran=$((decided + timedout))
	small=0
	[ "$scope" = diff ] && [ "$ran" -lt "$mindiff" ] && small=1
	if [ "$decided" -eq 0 ]; then
		if [ "$scope" = diff ] && [ "$timedout" -eq 0 ]; then
			echo "mutation gate: $pkg ok (diff scope: no mutant was decided on the changed lines; $notcovered not covered by any test)"
			exit 0
		fi
		echo "mutation gate: no mutant was killed or survived in $pkg (timed out: $timedout); the gate would pass without testing anything" >&2
		exit 1
	fi
	if [ "$small" -eq 0 ] && [ $((timedout * 100)) -gt $((ran * maxto)) ]; then
		echo "mutation gate: $timedout of $ran mutants timed out in $pkg (> $maxto%); raise MUTATION_TIMEOUT_COEFF or lower load" >&2
		exit 1
	fi
	if [ "$small" -eq 1 ]; then
		# Too few mutants for a percentage: one equivalent mutant would fail the pull request.
		if [ "$killed" -eq 0 ] || [ "$lived" -gt 1 ]; then
			echo "mutation gate: $killed killed, $lived lived of the $decided mutants on the changed lines in $pkg (a diff of fewer than $mindiff mutants may keep one lived mutant, but needs a kill)" >&2
			exit 1
		fi
		echo "mutation gate: $pkg ok (diff scope: $killed killed, $lived lived, $timedout timed out, $notcovered not covered)"
		exit 0
	fi
	eff=$((killed * 100 / decided))
	if [ "$eff" -lt "$min" ]; then
		echo "mutation gate: efficacy $eff% < $min% in $pkg ($lived mutants lived)" >&2
		exit 1
	fi
	echo "mutation gate: $pkg ok ($scope scope, efficacy $eff%, $timedout timed out)"
}

if [ "${1:-}" = --sum ]; then
	pkg=$2 min=$3
	shift 3
	[ $# -gt 0 ] || { echo "mutation gate: no counts files for $pkg" >&2; exit 1; }
	# one line per shard: killed lived timedout notcovered scope (all shards of a package agree on the scope)
	set -- $(cat "$@" | awk 'NF == 5 { k += $1; l += $2; t += $3; c += $4; s = $5; n++ } END { print k + 0, l + 0, t + 0, c + 0, s == "diff" ? "diff" : "package", n + 0 }')
	echo "== mutation $pkg: $6 shards, $5 scope, Killed: $1, Lived: $2, Timed out: $3, Not covered: $4"
	decide "$pkg" "$min" "$1" "$2" "$3" "$4" "$5"
	exit 0
fi

pkg=$1
min=$2
# Default: half of JOBS (the Makefile's per-job CPU budget) or of the CPUs;
# each worker runs a full go test of the package.
ncpu=${JOBS:-$(nproc 2>/dev/null || echo 4)}
workers=${MUTATION_WORKERS:-$(( ncpu / 2 > 2 ? ncpu / 2 : 2 ))}
coeff=${MUTATION_TIMEOUT_COEFF:-20}
# Absolute: the diff scope runs gremlins from inside the package directory.
tool=${TOOL:-go tool -modfile=$PWD/tools/go.mod}

# Scope: the whole package, or only the mutants on the lines changed since MUTATION_DIFF's merge base.
scope=package
diff=${MUTATION_DIFF:-}
if [ -n "$diff" ]; then
	d=${pkg#./}
	if ! changed=$(CHANGED_BASE="$diff" tools/changed-files.sh); then
		echo "mutation gate: no merge base with $diff, mutating the whole package $pkg"
	elif [ -z "$(echo "$changed" | grep -E "^$d/[^/]+\.go\$" | grep -v '_test\.go$')" ]; then
		echo "mutation gate: no non-test file of $pkg changed (its tests or data did), mutating the whole package"
	elif [ -n "$(git ls-files --others --exclude-standard -- "$d" | grep -E '\.go$' | grep -v '_test\.go$')" ]; then
		echo "mutation gate: an untracked file in $pkg is invisible to git diff, mutating the whole package"
	else
		scope=diff
	fi
fi

excludes=""
shard=${MUTATION_SHARD:-}
if [ -n "$shard" ]; then
	[ -n "${MUTATION_COUNTS_OUT:-}" ] || { echo "mutation gate: MUTATION_SHARD needs MUTATION_COUNTS_OUT" >&2; exit 1; }
	i=${shard%/*} n=${shard#*/}
	files=$(go list -f '{{range .GoFiles}}{{.}} {{end}}' "$pkg")
	# The files of this package that tools/mutation-shard.sh dealt to shard i (weighted by
	# mutant count and test cost, all packages together).
	mine=$(tools/mutation-shard.sh "$shard" "${pkg#./}" | awk '{ print $2 }')
	for f in $files; do
		echo "$mine" | grep -qx "$f" || excludes="$excludes -E (^|/)$(echo "$f" | sed 's/[.]/[.]/g')\$"
	done
	if [ -z "$mine" ]; then
		echo "== mutation $pkg shard $shard: no files"
		echo "0 0 0 0 $scope" >>"$MUTATION_COUNTS_OUT"
		exit 0
	fi
	echo "== mutation $pkg shard $shard:" $mine
fi

out=$(mktemp)
trap 'rm -f "$out"' EXIT
echo "== mutation $pkg ($scope scope, efficacy >= $min%, workers $workers, timeout coefficient $coeff)"
# shellcheck disable=SC2086 # excludes is a list of flags, tool a command
if [ "$scope" = diff ]; then
	# Inside the package directory and with 0 lines of context, relative paths: see the header.
	(cd "$pkg" && GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=diff.relative GIT_CONFIG_VALUE_0=true GIT_CONFIG_KEY_1=diff.context GIT_CONFIG_VALUE_1=0 \
		$tool gremlins unleash --workers "$workers" --timeout-coefficient "$coeff" --diff "$diff" $excludes) >"$out" 2>&1 || failed=1
else
	$tool gremlins unleash --workers "$workers" --timeout-coefficient "$coeff" $excludes "$pkg" >"$out" 2>&1 || failed=1
fi
if [ -n "${failed:-}" ]; then
	cat "$out"
	echo "mutation gate: gremlins failed for $pkg" >&2
	exit 1
fi
grep -E '^ *(LIVED|TIMED OUT)' "$out" || true
# Nothing to report: a shard whose files hold no mutant (e.g. only doc.go), or a diff with no mutant on its
# lines; the share is zero. Otherwise "no results" falls through and fails below.
if { [ -n "$shard" ] || [ "$scope" = diff ]; } && grep -q '^No results to report' "$out" && ! grep -q 'Killed:' "$out"; then
	echo "no mutants in this shard or on the changed lines"
	killed=0 lived=0 timedout=0 notcovered=0
else
	summary=$(grep -E 'Killed: [0-9]+, Lived: [0-9]+|Timed out: [0-9]+' "$out" | tr '\n' ' ')
	echo "$summary"
	killed=$(echo "$summary" | sed -n 's/.*Killed: \([0-9]*\).*/\1/p')
	lived=$(echo "$summary" | sed -n 's/.*Lived: \([0-9]*\).*/\1/p')
	timedout=$(echo "$summary" | sed -n 's/.*Timed out: \([0-9]*\).*/\1/p')
	notcovered=$(echo "$summary" | sed -n 's/.*Not covered: \([0-9]*\).*/\1/p')
	if [ -z "$killed" ] || [ -z "$lived" ] || [ -z "$timedout" ] || [ -z "$notcovered" ]; then
		cat "$out"
		echo "mutation gate: could not parse the gremlins summary for $pkg" >&2
		exit 1
	fi
fi
if [ -n "$shard" ]; then
	echo "$killed $lived $timedout $notcovered $scope" >>"$MUTATION_COUNTS_OUT"
	# The mutants nobody killed, for tools/mutation-report.sh: "<status>|<mutator>|<file>|<line>", the
	# file relative to the repository (gremlins prints it relative to the package).
	awk -v d="${pkg#./}" '/^ *(LIVED|NOT COVERED|TIMED OUT) +[A-Z_]+ at / {
		sub(/^ */, ""); split($0, a, " at "); split(a[2], loc, ":")
		head = a[1]; mut = head; sub(/.* /, "", mut); sub(/ +[A-Z_]+$/, "", head)
		f = loc[1]; if (index(f, d "/") != 1) f = d "/" f
		print head "|" mut "|" f "|" loc[2] }' "$out" >>"$MUTATION_COUNTS_OUT.findings"
	exit 0
fi
decide "$pkg" "$min" "$killed" "$lived" "$timedout" "$notcovered" "$scope"
