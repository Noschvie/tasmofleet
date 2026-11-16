#!/bin/sh
# Regenerates tools/mutation-weights.txt: per file of every MUTATION package the number of
# runnable (covered) mutants, from a gremlins dry run; a file with none gets a 0 line, so that it is
# told apart from a file nobody measured yet (tools/mutation-shard.sh estimates that one by its size).
# tools/mutation-shard.sh multiplies the count by the package's cost line to deal files out to the CI
# shards (make mutation-plan shows the result). The "cost" lines are seconds per mutant x 100, measured
# from the weekly runs (weekly.yml, mutation jobs), and the "setup" line (seconds x 100 that a shard pays once per package
# it mutates); keep them when regenerating, update them by hand
# when the shards drift apart again: compare each shard's duration with the estimate of the plan.
#
#   tools/mutation-weights.sh <package>...
set -eu
out=tools/mutation-weights.txt
tool=${TOOL:-go tool -modfile=tools/go.mod}
keep=$(mktemp)
trap 'rm -f "$keep"' EXIT
grep -E '^(#|cost |setup )' "$out" >"$keep" || true # per-file cost overrides (4th field) are lost: re-add by hand
{
	cat "$keep"
	for p in "$@"; do
		rel=${p#./}
		files=$(go list -f '{{range .GoFiles}}{{.}} {{end}}' "$p")
		# shellcheck disable=SC2086
		$tool gremlins unleash --dry-run "$p" 2>&1 | awk -v rel="$rel" -v files="$files" '/^ *RUNNABLE / { split($NF, a, ":"); n[a[1]]++ } END { m = split(files, fl, " "); for (i = 1; i <= m; i++) if (!(fl[i] in n)) n[fl[i]] = 0; for (f in n) print rel, f, n[f] }' | sort
	done
} >"$out.new"
mv "$out.new" "$out"
