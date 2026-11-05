#!/bin/sh
# The revert check (#555, make revert-check): the tests this branch adds or changes must FAIL when its
# production Go changes are reverted. Works in a scratch copy of the tree, never in the checkout: production
# files are put back to the merge base (files the branch added are removed, deleted ones restored), test files
# stay, then the changed tests run (go test -json, tags "integration" included). The rule and the opt-out are
# in scripts/revert-check.mjs.
#
# Opt-out for a pure refactor or feature where nothing should fail without the change: the label
# "no-revert-check" or a line "revert-check: skip - <reason>" in the PR body. In CI the job passes PR_NUMBER and
# GH_TOKEN and the script reads label and body from the API when it runs, so a re-run after editing the body
# sees the edit. Locally set REVERT_CHECK_SKIP="<reason>".
#
#   BASE=<ref or sha> tools/revert-check.sh
set -eu
GO=${GO:-go}
NICE=${NICE-}
orig=$(pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

tools/pr-diff.sh >"$tmp/diff"
node scripts/revert-check.mjs plan --diff "$tmp/diff" --out "$tmp/plan.json"
node scripts/revert-check.mjs prod "$tmp/plan.json" >"$tmp/prod"
node scripts/revert-check.mjs runs "$tmp/plan.json" >"$tmp/runs"

summary() { # summary <line>: also into the job summary on GitHub
	echo "$1"
	[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "$1" >>"$GITHUB_STEP_SUMMARY"
}

if [ ! -s "$tmp/prod" ]; then
	summary "revert check: no production Go file changed, nothing to revert"
	exit 0
fi
if [ ! -s "$tmp/runs" ]; then
	summary "revert check: production code changed, but no Go test was added or changed; nothing to check here (diff coverage covers the SAFETY packages)"
	exit 0
fi

skip=""
if [ -n "${REVERT_CHECK_SKIP:-}" ]; then
	skip="REVERT_CHECK_SKIP	$REVERT_CHECK_SKIP"
elif [ -n "${PR_NUMBER:-}" ]; then
	if gh api "repos/${GITHUB_REPOSITORY:?}/pulls/$PR_NUMBER" >"$tmp/pr.json"; then
		skip=$(node scripts/revert-check.mjs optout "$tmp/pr.json")
	else
		echo "::warning::revert check: could not read pull request $PR_NUMBER for the opt-out; running the check"
	fi
fi
if [ -n "$skip" ]; then
	summary "revert check SKIPPED (opt-out: $(echo "$skip" | cut -f1)): $(echo "$skip" | cut -f2-)"
	exit 0
fi

echo "revert check: reverting $(wc -l <"$tmp/prod" | tr -d ' ') production file(s) in a scratch copy:"
sed 's/^/  /' "$tmp/prod"
mb=$(git merge-base "${BASE:-${CHANGED_BASE:-origin/main}}" HEAD)
copy="$tmp/tree"
mkdir "$copy"
git ls-files --cached --others --exclude-standard | tar --ignore-failed-read -T - -cf - 2>/dev/null | tar -xf - -C "$copy"
while IFS="$(printf '\t')" read -r status path; do
	if [ "$status" = added ]; then
		rm -f "$copy/$path"
	else
		mkdir -p "$copy/$(dirname "$path")"
		git show "$mb:$path" >"$copy/$path"
	fi
done <"$tmp/prod"

: >"$tmp/results.json"
cd "$copy"
while IFS="$(printf '\t')" read -r dir names; do
	echo "revert check: go test ./$dir ${names:+-run $names}"
	# A failing run is the expected outcome; the verdict reads the events.
	if [ -n "$names" ]; then
		$NICE $GO test -json -count=1 -tags=integration -timeout 15m -run "$names" "./$dir" >>"$tmp/results.json" || true
	else
		$NICE $GO test -json -count=1 -tags=integration -timeout 15m "./$dir" >>"$tmp/results.json" || true
	fi
done <"$tmp/runs"
cd "$orig"
rc=0
node scripts/revert-check.mjs verdict "$tmp/plan.json" "$tmp/results.json" --module "$($GO list -m)" >"$tmp/verdict" || rc=$?
cat "$tmp/verdict"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || sed 's/^/    /' "$tmp/verdict" >>"$GITHUB_STEP_SUMMARY"
exit $rc
