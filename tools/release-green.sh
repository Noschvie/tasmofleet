#!/usr/bin/env bash
# Release guard (#554): the commit to release has already been checked as a pull request and on main,
# so the tag run does not check it again. Fails, with the reason, unless SHA has
#   1. a successful check run "required" of pr.yml (the one status branch protection demands), and
#   2. a successful main.yml run (push to main) whose jobs all succeeded; only ghcr-cleanup (registry
#      housekeeping after the image) and ha-app (the Home Assistant app image, #522) may fail without holding a release.
# Main moves only by a fast-forward push of a PR head (CLAUDE.md "Pull requests and the protected main"),
# so the SHA of the tagged commit is the PR head and both are found under it.
#
# usage: tools/release-green.sh SHA        env: GITHUB_REPOSITORY, GH_TOKEN (checks: read, actions: read)
# release.yml runs it for a tag push and, as a dry run that publishes nothing, for workflow_dispatch.
set -euo pipefail
sha=${1:?usage: release-green.sh SHA}
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY not set}
fail() {
	echo "release refused: $*" >&2
	exit 1
}

# --- 1. the required check run of pr.yml ---------------------------------------------------------
# Newest first. A job called "required" of another workflow does not count: the workflow file is checked.
checks=$(gh api --paginate "repos/$repo/commits/$sha/check-runs?check_name=required&per_page=100" --jq '.check_runs[]' | jq -s 'sort_by(.started_at) | reverse')
found=
for row in $(jq -r '.[] | select(.html_url | test("/actions/runs/[0-9]+")) | "\(.html_url | capture("/actions/runs/(?<id>[0-9]+)").id):\(.status):\(.conclusion // "none")"' <<<"$checks"); do
	id=${row%%:*}
	rest=${row#*:}
	status=${rest%%:*}
	conclusion=${rest#*:}
	[ "$(gh api "repos/$repo/actions/runs/$id" --jq .path)" = ".github/workflows/pr.yml" ] || continue
	found=$id
	[ "$status" = completed ] || fail "$sha: the check \"required\" of pr.yml (run $id) is $status, not finished"
	[ "$conclusion" = success ] || fail "$sha: the check \"required\" of pr.yml (run $id) concluded $conclusion"
	echo "required: success (pr.yml run $id)"
	break
done
[ -n "$found" ] || fail "$sha has no check \"required\" from pr.yml: it was never green as a pull request"

# --- 2. the main.yml run --------------------------------------------------------------------------
run=$(gh api --paginate "repos/$repo/actions/runs?head_sha=$sha&event=push&per_page=100" --jq '.workflow_runs[]' | jq -s '[.[] | select(.path == ".github/workflows/main.yml")] | sort_by(.created_at) | reverse | .[0] // empty')
[ -n "$run" ] || fail "$sha has no main.yml run: wait for it after the push to main. Only the head of a push gets one, and a push that changes only paths main.yml ignores gets none: tag the head of the push that carried the release commit"
id=$(jq -r .id <<<"$run")
status=$(jq -r .status <<<"$run")
[ "$status" = completed ] || fail "$sha: main.yml run $id is $status, not finished"
jobs=$(gh api --paginate "repos/$repo/actions/runs/$id/jobs?per_page=100" --jq '.jobs[]' | jq -s '[.[] | select((.name | startswith("ghcr-cleanup") or startswith("ha-app")) | not)]')
bad=$(jq -r '[.[] | select(.conclusion != "success" and .conclusion != "skipped") | "\(.name): \(.conclusion)"] | join(", ")' <<<"$jobs")
[ -z "$bad" ] || fail "$sha: main.yml run $id has failed jobs: $bad"
# A run that never started its jobs (a workflow file error ends it as startup_failure) has none that failed either.
[ "$(jq '[.[] | select(.conclusion == "success")] | length' <<<"$jobs")" -gt 0 ] || fail "$sha: main.yml run $id ran no jobs (it concluded $(jq -r '.conclusion // "nothing"' <<<"$run"))"
echo "main.yml: success (run $id)"
echo "$sha is green: required (pr.yml) and main.yml"
