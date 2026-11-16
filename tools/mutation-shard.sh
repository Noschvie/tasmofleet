#!/bin/sh
# Deals the files of every MUTATION package out to the CI mutation shards and prints the files of
# one shard as "<package> <file>" lines, or with "plan" the whole plan with the estimated load.
#
#   MUTATION_PKGS="./internal/a ./internal/b" tools/mutation-shard.sh <i>/<n> [package]
#   MUTATION_PKGS=... tools/mutation-shard.sh plan <n>
#
# Weight of a file = runnable mutants (tools/mutation-weights.txt) x the package's seconds per
# mutant, so the shards take about equally long: one shard per package and file would not, the
# time per mutant differs 20-fold between packages. All packages are dealt out together (largest
# file first to the lightest shard), so every shard job computes the same plan. A file without a
# weights entry (added since the last refresh of the list, tools/mutation-weights.sh) counts as 17
# runnable mutants per 100 lines of code, the average of the weighted files: counting it as one mutant
# dealt the new 85- and 115-mutant files of 2026-11 to the shard that was already the slowest, and
# that shard ran into the job timeout (#729). A file the list gives 0 mutants stays free.
#
# Every shard that mutates a package first builds the package's tests and runs gremlins' coverage run,
# about 22 s per package and shard (6 to 37 s measured), whatever the package's size. The line
# "setup <seconds x 100>" of the weights file is that fixed cost: a file that goes to a shard which does
# not hold its package yet costs the setup on top of its weight, so the deal keeps a package's files
# together unless another shard is lighter by more than the setup, and the estimate matches the wall
# time of the shard (#886: without it a shard was estimated at 17.6 min and ran 21 to 30 min). No setup
# line counts as 0.
set -eu
weights=${MUTATION_WEIGHTS:-tools/mutation-weights.txt}
: "${MUTATION_PKGS:?MUTATION_PKGS is not set}"

items=$(for p in $MUTATION_PKGS; do
	rel=${p#./}
	for f in $(go list -f '{{range .GoFiles}}{{.}} {{end}}' "$p"); do
		echo "$rel $f $(grep -Evc '^[[:space:]]*(//|$)' "$p/$f" || true)"
	done
done | awk -v wf="$weights" '
	BEGIN {
		while ((getline line < wf) > 0) {
			split(line, a, " ")
			if (a[1] == "#") continue
			if (a[1] == "setup") continue
			if (a[1] == "cost") cost[a[2]] = a[3]; else { n[a[1] " " a[2]] = a[3]; if (a[4] != "") fc[a[1] " " a[2]] = a[4] }
		}
	}
	{ c = ($1 in cost) ? cost[$1] : 100; if (($1 " " $2) in fc) c = fc[$1 " " $2]; m = (($1 " " $2) in n) ? n[$1 " " $2] : $3 * 0.17; print m * c, $1, $2 }' | sort -k1,1nr -k2,2 -k3,3)

# The deal, one awk program for the plan and for the shard files, so that they cannot drift apart:
# every file (heaviest first) goes to the shard where load + setup (if it does not hold the package
# yet) is lowest, the lowest shard number on a tie. "plan" prints every shard with its estimate, else
# the files of shard i (of the package pkg, if given) are printed.
setup=$(awk '$1 == "setup" { print $2 }' "$weights" 2>/dev/null || true)
if [ "$1" = plan ]; then n=$2 i=plan pkg=; else n=${1#*/} i=${1%/*} pkg=${2:-}; fi
echo "$items" | awk -v n="$n" -v i="$i" -v pkg="$pkg" -v setup="${setup:-0}" '
	{ b = 0
	  for (s = 1; s <= n; s++) {
		x = (s SUBSEP $2) in has ? 0 : setup
		if (b == 0 || load[s] + x < best) { b = s; best = load[s] + x; extra = x }
	  }
	  load[b] += $1 + extra; has[b, $2] = 1; files[b] = files[b] " " $2 "/" $3
	  if (b == i && (pkg == "" || pkg == $2)) print $2, $3 }
	END { if (i == "plan") for (s = 1; s <= n; s++) printf "shard %d/%d: load %.0f s (estimate):%s\n", s, n, load[s] / 100, files[s] }'
