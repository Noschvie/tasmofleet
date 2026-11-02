#!/bin/sh
# Report of a sharded mutation run, for the job summary and a JSON artifact: per package the scope
# (diff or package), efficacy and counts, and every mutant that lived, timed out or was not covered
# by a test, as file:line and mutator. Written from the counts and findings files that
# tools/mutation-gate.sh leaves in each shard's directory, before the gate decides, so a failing
# gate comes with its report.
#
#   tools/mutation-report.sh <counts-dir> <package>:<min-efficacy-%>...
#
# Writes <counts-dir>/mutation-report.json (MUTATION_REPORT overrides the path) and, when
# GITHUB_STEP_SUMMARY is set, a markdown report appended to it. Packages without counts were not
# run (unchanged) and are left out.
set -eu
dir=$1
shift
json=${MUTATION_REPORT:-$dir/mutation-report.json}
md=$(mktemp)
trap 'rm -f "$md"' EXIT

pkgs_json=""
rows=""
details=""
scopes=""
for e in "$@"; do
	pkg=${e%%:*} min=${e##*:} base=$(basename "$pkg")
	# shellcheck disable=SC2012 # file names are fixed
	counts=$(ls "$dir"/*/"$base" 2>/dev/null) || continue
	# shellcheck disable=SC2086
	sums=$(cat $counts | awk 'NF == 5 { k += $1; l += $2; t += $3; c += $4; s = $5 } END { print k + 0, l + 0, t + 0, c + 0, s == "diff" ? "diff" : "package" }')
	k=${sums%% *}
	rest=${sums#* }
	l=${rest%% *}
	rest=${rest#* }
	t=${rest%% *}
	rest=${rest#* }
	c=${rest%% *}
	scope=${rest#* }
	# the gate's efficacy: on the changed lines of a diff a mutant no test reaches counts as lived
	eff=$(awk -v k="$k" -v l="$l" -v c="$c" -v scope="$scope" 'BEGIN { if (scope == "diff") l += c; if (k + l == 0) print "null"; else printf "%.1f", k * 100 / (k + l) }')
	findings=$(cat "$dir"/*/"$base".findings 2>/dev/null | sort -t'|' -k3,3 -k4,4n -k2,2 || true)
	list=$(echo "$findings" | awk -F'|' 'NF == 4 { if (n++) printf ","; printf "{\"status\":\"%s\",\"mutator\":\"%s\",\"file\":\"%s\",\"line\":%s}", $1, $2, $3, $4 }')
	pkgs_json="$pkgs_json${pkgs_json:+,}{\"package\":\"${pkg#./}\",\"scope\":\"$scope\",\"min_efficacy\":$min,\"efficacy\":$eff,\"killed\":$k,\"lived\":$l,\"timed_out\":$t,\"not_covered\":$c,\"findings\":[$list]}"
	if [ "$eff" = null ]; then effs="n/a"; else effs="$eff %"; fi
	rows="$rows| \`${pkg#./}\` | $scope | $effs | $min % | $k | $l | $t | $c |
"
	scopes="$scopes $scope"
	if [ -n "$findings" ]; then
		details="$details
<details><summary><code>${pkg#./}</code>: $l lived, $t timed out, $c not covered</summary>

| mutant | where | status |
| --- | --- | --- |
$(echo "$findings" | awk -F'|' 'NF == 4 { printf "| %s | `%s:%s` | %s |\n", $2, $3, $4, $1 }')

</details>
"
	fi
done

printf '{"commit":"%s","packages":[%s]}\n' "${GITHUB_SHA:-}" "$pkgs_json" >"$json"
echo "mutation report: $json"

[ -n "${GITHUB_STEP_SUMMARY:-}" ] || exit 0
title="Mutation testing, whole packages"
case "$scopes" in *package*) ;; *diff*) title="Mutation testing, lines changed by this pull request" ;; esac
{
	echo "## $title"
	echo
	if [ -z "$rows" ]; then
		echo "No mutation package was run."
	else
		echo "| package | scope | efficacy | needed | killed | lived | timed out | not covered |"
		echo "| --- | --- | --- | --- | --- | --- | --- | --- |"
		printf '%s' "$rows"
		case "$scopes" in *diff*) echo; echo "A diff scope judges only the mutants on the changed lines, and a mutant no test reaches counts as lived there; fewer than 10 of them are not judged by the percentage (tools/mutation-gate.sh)." ;; esac
		printf '%s\n' "$details"
	fi
} >"$md"
cat "$md" >>"$GITHUB_STEP_SUMMARY"
