# Pull requests, the protected main, branches and worktrees

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

## Pull requests and the protected main

`main` is protected on GitHub by the ruleset `main`: no force pushes, no deletion, linear
history, signed commits, PR and the status check `required` from `pr.yml`. Every change,
docs-only ones included, goes through a pull request: branch, push, open a PR against `main`,
wait until CI is green on the PR, then the maintainer merges it **fast-forward only** by
pushing the PR head to `main`, so the commits keep their own signatures:

```sh
git fetch origin && git rebase origin/main   # if main moved: rebase, re-sign, push, wait for CI again
git push origin <branch>:main                # ff push of the green PR head; GitHub marks the PR merged
```

Only repository admins may push to `main` (ruleset bypass), and only a PR head whose `required`
check is green. If `main` moved, the branch is rebased (and re-signed), pushed and must be green
again before the fast-forward push; there is no other merge path. Never merge through the GitHub
button: the only method enabled there is rebase, and GitHub's rebase commits are unsigned, so
the ruleset rejects them.
`pr.yml` runs the FAST tier on every PR: a `changes` job detects docs-only PRs (heavy jobs
skipped), PRs that touch a `SAFETY` package (race and diff coverage added) or Go code (revert check added), and the final `required`
job passes only when every job that had to run passed; a skip counts only when the PR did
not ask for that job; the link check, the migration guard and the release section guard must always succeed, and so must the image build check when the PR touches what the image reads (Dockerfile, module files, the HA app folder); the HA app image is built next to it (`_ha-app.yml`) and never gates the merge. No approvals are required (single maintainer). Release tags are still pushed
directly, on a green commit of `main`.
## Branches and worktrees

After a pull request is merged into `main`, its branch is deleted on origin and the
local worktree and branch are removed. Before deleting, check containment:
`git cherry origin/main <branch>` shows only `-` lines, or the branch's diff against
`main` is empty (rebased or squashed work). Branches with uncontained changes are never
deleted silently.
