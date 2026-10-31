#!/bin/sh
# Regenerates tools/mutation-weights.txt: per file of every MUTATION package the number of
# runnable (covered) mutants, from a gremlins dry run. tools/mutation-gate.sh multiplies it by the
# package's cost line to deal files out to the CI shards (make mutation-shards-check shows the
# result). The "cost" lines are seconds per mutant x 100, measured from the five FULL runs of
# 2026-10-31 (ci.yml, mutation jobs); keep them when regenerating, update them by hand when the
# shards drift apart again.
#
#   tools/mutation-weights.sh <package>...
set -eu
out=tools/mutation-weights.txt
tool=${TOOL:-go tool -modfile=tools/go.mod}
keep=$(mktemp)
trap 'rm -f "$keep"' EXIT
grep -E '^(#|cost )' "$out" >"$keep" || true
{
	cat "$keep"
	for p in "$@"; do
		rel=${p#./}
		# shellcheck disable=SC2086
		$tool gremlins unleash --dry-run "$p" 2>&1 | awk -v rel="$rel" '/^ *RUNNABLE / { split($NF, a, ":"); n[a[1]]++ } END { for (f in n) print rel, f, n[f] }' | sort
	done
} >"$out.new"
mv "$out.new" "$out"
