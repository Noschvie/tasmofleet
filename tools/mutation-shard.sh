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
# weights entry counts as one mutant (run tools/mutation-weights.sh to refresh the list).
set -eu
weights=${MUTATION_WEIGHTS:-tools/mutation-weights.txt}
: "${MUTATION_PKGS:?MUTATION_PKGS is not set}"

items=$(for p in $MUTATION_PKGS; do
	rel=${p#./}
	for f in $(go list -f '{{range .GoFiles}}{{.}} {{end}}' "$p"); do
		echo "$rel $f"
	done
done | awk -v wf="$weights" '
	BEGIN {
		while ((getline line < wf) > 0) {
			split(line, a, " ")
			if (a[1] == "#") continue
			if (a[1] == "cost") cost[a[2]] = a[3]; else n[a[1] " " a[2]] = a[3]
		}
	}
	{ c = ($1 in cost) ? cost[$1] : 100; m = (($1 " " $2) in n) ? n[$1 " " $2] : 1; print m * c, $1, $2 }' | sort -k1,1nr -k2,2 -k3,3)

if [ "$1" = plan ]; then
	echo "$items" | awk -v n="$2" '
		{ b = 1; for (s = 2; s <= n; s++) if (load[s] < load[b]) b = s; load[b] += $1; files[b] = files[b] " " $2 "/" $3 }
		END { for (s = 1; s <= n; s++) printf "shard %d/%d: load %.0f s (estimate):%s\n", s, n, load[s] / 100, files[s] }'
	exit 0
fi
i=${1%/*} n=${1#*/}
echo "$items" | awk -v n="$n" -v i="$i" -v pkg="${2:-}" '
	{ b = 1; for (s = 2; s <= n; s++) if (load[s] < load[b]) b = s; load[b] += $1
	  if (b == i && (pkg == "" || pkg == $2)) print $2, $3 }'
