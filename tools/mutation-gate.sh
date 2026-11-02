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
# Throwaway GOCACHE (#546): gremlins builds the package and its test binary once per mutant, and a mutant's
# build is never used again. In the shared Go cache that is pure garbage (154 mutants of internal/cmdpolicy
# add 261 MB; several parallel runs a day filled a 255 GB disk, and CI would save it into its cache). So
# gremlins runs with GOCACHE pointing at a fresh directory that is removed when the gate ends, also on failure
# or interrupt. It sits next to the real cache (on disk: a whole ota run puts about 1 GB there, too much for a
# TMPDIR on tmpfs). gremlins gives every mutant coefficient x the time of its first (coverage) run as its
# timeout, so that run must not pay for a cold cache: one non-mutant build of the package warms the
# throwaway cache first (ota's coverage run takes 11 s cold, 2.6 s warm; without the warm-up a hanging mutant
# would block a worker for minutes, and slow mutants would stop counting as timed out).
# The gremlins binary itself is built through the real cache (go tool -n). gremlins finds `go` on PATH, so the
# go that GO (the Makefile's $(GO), default go) selects is put first there; NICE (the Makefile's "nice -n 15",
# empty in CI) only wraps the gremlins run.
# MUTATION_GOCACHE_SEED=<dir> copies a directory of earlier non-mutant builds into the throwaway cache before
# the warm-up (scripts/mutation-diff.test.mjs, whose toy modules would otherwise rebuild the standard library
# for every run); unset by default, because the real cache can hold tens of GB.
#
# MUTATION_WORKERS (default nproc/2, at least 2) and MUTATION_TIMEOUT_COEFF (default 20) tune
# gremlins for slow or busy machines; a coefficient that is too low turns every
# mutant into a timeout.
#
# Diff scope (#475): with MUTATION_DIFF=<ref> (make mutation-changed passes BASE) only the mutants on
# lines this branch changed since the merge base with <ref> are run, not the whole package. A mutant
# on such a line that no test reaches (not covered) counts as lived there: a changed line that no
# test runs must not pass; in a whole package, as before, not covered mutants stay out of the
# efficacy. The thresholds judge the changed lines alone, so a handful of mutants is a noisy sample:
# below MUTATION_DIFF_MIN (default 10) decided mutants (killed + lived + not covered) the
# percentage is replaced by "at least one killed, at most one lived or not covered"; the timeout share
# is judged as for a package, but only from MUTATION_DIFF_MIN run mutants (decided + timed out) on.
# No mutant at all on the changed lines passes (a comment, a declaration), loudly.
# The whole package is mutated as before when no non-test Go file of it changed, or when anything else
# in its directory changed too (a _test.go file, testdata, an embedded file: the tests may have got
# weaker while a code line changed), or when an untracked file is in it (git diff cannot see it).
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
	# On the changed lines of a diff a mutant no test reaches counts as lived.
	[ "$scope" = diff ] && lived=$((lived + notcovered))
	decided=$((killed + lived))
	ran=$((decided + timedout))
	small=0 # too few decided mutants for a percentage
	[ "$scope" = diff ] && [ "$decided" -lt "$mindiff" ] && small=1
	judgeto=1 # too few run mutants for a timeout share
	[ "$scope" = diff ] && [ "$ran" -lt "$mindiff" ] && judgeto=0
	if [ "$decided" -eq 0 ]; then
		if [ "$scope" = diff ] && [ "$timedout" -eq 0 ]; then
			echo "mutation gate: $pkg ok (diff scope: no mutant on the changed lines)"
			exit 0
		fi
		echo "mutation gate: no mutant was killed or survived in $pkg (timed out: $timedout); the gate would pass without testing anything" >&2
		exit 1
	fi
	if [ "$judgeto" -eq 1 ] && [ $((timedout * 100)) -gt $((ran * maxto)) ]; then
		echo "mutation gate: $timedout of $ran mutants timed out in $pkg (> $maxto%); raise MUTATION_TIMEOUT_COEFF or lower load" >&2
		exit 1
	fi
	if [ "$small" -eq 1 ]; then
		# Too few mutants for a percentage: one equivalent mutant would fail the pull request.
		if [ "$killed" -eq 0 ] || [ "$lived" -gt 1 ]; then
			echo "mutation gate: $killed killed, $lived lived or not covered of the $decided mutants on the changed lines in $pkg (a diff of fewer than $mindiff mutants may keep one, but needs a kill)" >&2
			exit 1
		fi
		echo "mutation gate: $pkg ok (diff scope: $killed killed, $lived lived or not covered, $timedout timed out)"
		exit 0
	fi
	eff=$((killed * 100 / decided))
	if [ "$eff" -lt "$min" ]; then
		echo "mutation gate: efficacy $eff% < $min% in $pkg ($lived mutants lived or not covered)" >&2
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
nice=${NICE-}
gocmd=${GO:-go}

# Scope: the whole package, or only the mutants on the lines changed since MUTATION_DIFF's merge base.
scope=package
diff=${MUTATION_DIFF:-}
if [ -n "$diff" ]; then
	d=${pkg#./}
	if ! changed=$(CHANGED_BASE="$diff" tools/changed-files.sh); then
		echo "mutation gate: no merge base with $diff, mutating the whole package $pkg"
	else
		inpkg=$(echo "$changed" | grep -E "^$d/" || true)
		code=$(echo "$inpkg" | grep -E "^$d/[^/]+\.go\$" | grep -v '_test\.go$' || true)
		other=$(echo "$inpkg" | grep -vE "^$d/[^/]+\.go\$" || true) # data, subdirectories
		tests=$(echo "$inpkg" | grep -E "_test\.go\$" || true)
		if [ -z "$code" ]; then
			echo "mutation gate: no non-test Go file of $pkg changed (its tests or data did), mutating the whole package"
		elif [ -n "$tests$other" ]; then
			echo "mutation gate: tests or data of $pkg changed too, mutating the whole package"
		elif [ -n "$(git ls-files --others --exclude-standard -- "$d" | grep -E '\.go$' | grep -v '_test\.go$')" ]; then
			echo "mutation gate: an untracked file in $pkg is invisible to git diff, mutating the whole package"
		else
			scope=diff
		fi
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

# The gremlins binary comes out of the real cache (built once), the mutants go to the throwaway one.
gremlins=$($tool -n gremlins) || { echo "mutation gate: could not build gremlins" >&2; exit 1; }
goroot=$($gocmd env GOROOT)
realcache=$($gocmd env GOCACHE)
out=$(mktemp)
cache=$(mktemp -d "$(dirname "$realcache")/mutation-gocache.XXXXXX")
# EXIT runs on every exit; a signal becomes an exit so the trap runs for it too.
trap 'rm -rf "$out" "$cache"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
[ -z "${MUTATION_GOCACHE_SEED:-}" ] || cp -R "$MUTATION_GOCACHE_SEED/." "$cache/"
# Warm-up (see the header): the build gremlins' coverage run does, without running any test.
# shellcheck disable=SC2086 # nice, gocmd: command prefixes
GOCACHE="$cache" $nice $gocmd test -cover -run '^$' "$pkg/..." >"$out" 2>&1 || { cat "$out"; echo "mutation gate: could not build $pkg" >&2; exit 1; }
echo "== mutation $pkg ($scope scope, efficacy >= $min%, workers $workers, timeout coefficient $coeff)"
# shellcheck disable=SC2086 # excludes is a list of flags, nice a command prefix
if [ "$scope" = diff ]; then
	# Inside the package directory and with 0 lines of context, relative paths: see the header.
	(cd "$pkg" && GOCACHE="$cache" PATH="$goroot/bin:$PATH" GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=diff.relative GIT_CONFIG_VALUE_0=true GIT_CONFIG_KEY_1=diff.context GIT_CONFIG_VALUE_1=0 \
		$nice "$gremlins" unleash --workers "$workers" --timeout-coefficient "$coeff" --diff "$diff" $excludes) >"$out" 2>&1 || failed=1
else
	GOCACHE="$cache" PATH="$goroot/bin:$PATH" $nice "$gremlins" unleash --workers "$workers" --timeout-coefficient "$coeff" $excludes "$pkg" >"$out" 2>&1 || failed=1
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
