# Pitfalls: one rule per mistake that already happened

Every entry below is something that went wrong in this project. Each says what
happened, the rule that prevents it, and where the rule is enforced. "Convention"
means nothing stops you: you have to know it. The full working rules are in
[CLAUDE.md](../CLAUDE.md); this page is the short list of why they exist.

## Git and merging

### Merged with the GitHub rebase button (PR #57)

- **Happened:** PR #57 was merged with the GitHub button. GitHub's rebase makes
  unsigned commits, so `main` got two of them and had to be repaired by hand.
- **Rule:** merge only by a fast-forward push of the green PR head
  (`git push origin <branch>:main`). Never use a merge button.
- **Enforced:** ruleset `main` (signed commits, linear history, `required`). The
  button still exists, so the rule is also a convention.

### Local gates ignored uncommitted work (#481)

- **Happened:** `make safety` and `make mutation-changed` diffed `BASE..HEAD`. With
  the changes still uncommitted they printed "no safety package changed" and exited 0. Once `main` had moved, they selected too many packages.
- **Rule:** gates diff against the merge base and include the working tree and
  untracked files.
- **Enforced:** `tools/pr-diff.sh` (shared by the diff-based targets); covered by
  `scripts/*.test.mjs`.

### Tag set on a commit that was not green yet

- **Happened:** a docs-only push gets no `main.yml` run, and the run of a pushed
  head takes minutes. A tag on a commit without a finished `main.yml` run is refused
  by the release guards.
- **Rule:** tag the head of the push that carried the release commit, and only after
  `main.yml` has finished on it.
- **Enforced:** job `guards` in `release.yml` (`tools/release-green.sh`).

## Working with agents and long-running commands

### `pkill -f` killed other agents' dev environments (2026-10-31)

- **Happened:** an agent stopped its mockup server with `pkill -f` on a process name
  and also killed the `tasmofleet-devenv` processes of parallel agents.
- **Rule:** never `pkill -f` or `killall` on `tasmofleet`, devenv or `http.server`
  processes. Stop only the PIDs you started.
- **Enforced:** not enforced, convention (briefs repeat it).

### Waits that never ended (2026-10-29)

- **Happened:** agents left `tail -f` and `while pgrep -f ...` loops running for
  hours. The `pgrep -f` pattern matched its own command line, so the loop never
  saw the process end. One agent started 11 identical waiters.
- **Rule:** one wait per thing, always with a timeout. No `tail -f`. Never `pgrep -f`
  with a pattern that appears in its own command line; wait on a PID with `kill -0`.
- **Enforced:** not enforced, convention.

### Waiting on the wrong CI result

- **Happened:** `gh pr checks <n> --required --watch` returns at once with "no
  required checks" while the `required` job is not registered yet, which reads like
  success.
- **Rule:** use `timeout 2400 gh pr checks <n> --watch --interval 60`, then read the
  `required` line.
- **Enforced:** not enforced, convention.

### A milestone cut as a strict chain (0.20.0)

- **Happened:** work packages waited for the previous merge, review and FULL run.
  The milestone took about two days although each package touched only a small
  interface of its predecessor.
- **Rule:** fix shared contracts (types, signatures, migration numbers) first, split
  each package into an independent part and a thin hook-up part, give every package
  its own files.
- **Enforced:** not enforced, convention.

## CI

### Mutation runs filled the disk with build output (#546)

- **Happened:** gremlins builds every mutant and each build lands in the Go cache.
  One run on one package added 261 MB; several parallel worktrees filled a 255 GB
  cache.
- **Rule:** mutation runs use a throwaway `GOCACHE` that is removed when the run ends.
  No cleanup timer.
- **Enforced:** `tools/mutation-gate.sh`; `scripts/mutation-gocache.test.mjs`.

### Script tests left temp directories in `/tmp` (#552)

- **Happened:** 266 directories after one afternoon of runs. On a tmpfs `/tmp`
  that is RAM.
- **Rule:** a test that creates a temp dir removes it in `after()`/`finally`.
- **Enforced:** convention; the `scripts/*.test.mjs` files clean up after themselves.

### PR runs evicted the caches of `main` (#476)

- **Happened:** every PR run saved about 1.5 GB of Go cache that only its own PR
  could restore. The repository went over the 10 GB cache limit and `main`'s warm
  caches were evicted.
- **Rule:** only runs on `main` save caches.
- **Enforced:** `.github/actions/go/action.yml` and the `save:` inputs of the
  workflows.

### Checks that did not gate the merge (#482, #522)

- **Happened:** the docs link check ran as a separate workflow, and the PR image
  build check was not in `needs` of `required`. A broken link, or a Dockerfile
  change that broke the image build, could merge with `required` green.
- **Rule:** a check that must stop a merge is a job that the `required` aggregator
  waits for. A skip counts only when the PR did not ask for the job.
- **Enforced:** job `required` in `pr.yml`.

### Actions pinned by tag, secrets passed with `inherit` (#484, #483)

- **Happened:** workflows with `packages: write` or `contents: write` used actions by
  tag, and `publish` called `image.yml` with `secrets: inherit`, which passes every
  secret, including later ones.
- **Rule:** pin third-party actions to a full commit SHA with the version in a
  comment. Reusable workflows take secrets by name, never `inherit`.
- **Enforced:** Renovate keeps the pins current; otherwise convention and review.

### Environment secrets did not reach a called workflow (#694, #696, #693)

- **Happened:** #694 moved the Docker Hub secrets into an environment that the
  reusable workflows set themselves. The secrets arrived empty and `main` went red.
  It was reverted in #696 and done right in #693.
- **Rule:** an environment's secrets do not reach a called workflow on their own:
  the job in the called workflow sets `environment:` AND the caller still passes
  each secret by name (the callee declares it under `workflow_call.secrets`). Delete
  same-named repository secrets, a value passed by name wins over the environment's.
  Test that in a probe repository before the change reaches `main`. Pattern: `docs/dev/ci-tiers.md`.
- **Enforced:** not enforced, convention. The fail-loud check on missing credentials
  on `main` and `v*` tags is what made the failure visible.

### The nightly stayed red for days (#485, #428)

- **Happened:** 5 of 9 nightly FULL runs failed, and #428 was closed while the
  failures went on.
- **Rule:** one issue per failing nightly test, quarantine it, fix it within about
  two weeks. Do not close the failure issue before the next nightly is green.
- **Enforced:** job `nightly-failure-issue` opens the issue; the quarantine
  mechanism is in the Tests section.

### Specs outside the PR tier broke unnoticed (#346)

- **Happened:** three e2e specs that only the nightly FULL tier runs were red on
  `main` from 0.22.0 on, and the release shipped.
- **Rule:** a red FULL tier is fixed through an issue, with the cause decided per
  spec (spec or product). A change that rewrites a page also runs the specs that
  look at that page locally.
- **Enforced:** `nightly-failure-issue`; the local part is convention.

### A `make` target that no workflow ran (#477)

- **Happened:** `make scripts-test` failed on Node 22 and nobody noticed, because no
  workflow ran it.
- **Rule:** every check target runs in a workflow. A new target is wired into CI
  in the same change.
- **Enforced:** the `lint` job runs the check targets; adding a target to it is
  convention.

## Tests

### A test that depended on the time of day (#340)

- **Happened:** the test seeded data at `now - 1h` and expected "Today". In the first
  hour after midnight UTC it failed, on a PR.
- **Rule:** use `internal/clock` or a fixed time, never `time.Now()` arithmetic that
  can cross midnight.
- **Enforced:** convention.

### A test case that could do nothing (#141)

- **Happened:** a "tampered MAC" case replaced the last two characters with `AA`. If
  the random MAC already ended in `AA`, nothing was tampered and the (correct) login
  broke the assertion.
- **Rule:** make the change under test certain (flip a character to one that differs).
  A test that cannot fail when the rule breaks is wrong.
- **Enforced:** `make revert-check` catches tests that pass without the fix; the rest
  is review.

### A freed port was reused (#39)

- **Happened:** the "unreachable device" got a port by listening on `127.0.0.1:0` and
  closing it. A simulator started next could be handed the same port, so the device
  answered. The v0.12.0 tag run failed.
- **Rule:** for "nothing listens here" use a privileged loopback port that no
  unprivileged process can bind.
- **Enforced:** convention.

### E2E waits on a short window instead of a state (#20, #12, #273)

- **Happened:** specs asserted a transient state ("Updating") inside 5 s, or a menu
  that races with a column refit. They failed on loaded runners without a code
  cause.
- **Rule:** wait on the state the code reaches (job state, a settled layout), not on
  a window. A test that fails without a code cause gets a `flaky` issue and is
  quarantined (`@quarantine`, or `quarantine.Test(t, "<issue URL>")`), then fixed
  or deleted within about two weeks.
- **Enforced:** `internal/testutil/quarantine`; the `quarantine` job of `nightly.yml`.

## UI review

### A UI that differed from the approved mockup (#338)

- **Happened:** 0.22.0 shipped a settings redesign whose tabs did not match the
  approved mockup (stacked options, duplicate labels, orphan reset link). Related:
  #333, #336, #337.
- **Rule:** before coding, list what the mockup shows per screen. Make paired
  screenshots (mockup and build, 1280 and 375 px, light and dark) outside the repo.
  Every visible difference is fixed or listed with the reason. "Tests pass" is not
  acceptance for UI.
- **Enforced:** not enforced, convention (the reviewer compares before merging).

### Cards wider than their content, and row gaps (#419, #391)

- **Happened:** cards spanned the full width for two short columns and pushed other
  cards down; card grids with row-aligned gaps came back after redesigns.
- **Rule:** cards stack per column with no row gaps. A full-width card must really fill
  its width at 1280 px.
- **Enforced:** `e2e/tests/card-widths.spec.ts` and `card-stacks.spec.ts`.

### A timestamp without a date (#422)

- **Happened:** "Recent restarts" showed only a clock time, so a restart three days
  ago read like one from today.
- **Rule:** if a timestamp can be older than today it carries the date. Tests pin
  both cases.
- **Enforced:** the test for that card; elsewhere convention (UI review checks every
  timestamp).

### Layouts that broke on phones and tablets (#510, #511, #633)

- **Happened:** badges overlapped lists, long dotted names broke mid-label, and whole
  pages broke between 720 and 1199 px, none seen on a desktop check.
- **Rule:** check 375 px and tablet widths, not only 1280.
- **Enforced:** layout guards in `e2e/tests/` (`tablet-guards.spec.ts`,
  `layout-guard-words.spec.ts`, `whats-new-layout.spec.ts`).

### A visible browser opened on the maintainer's desktop (2026-11-01)

- **Happened:** an agent used the Playwright MCP browser tools, which open a headed
  window on the desktop.
- **Rule:** headless scripts (Playwright, curl) by default. If a visible browser is
  needed, use Claude in Chrome in your own tab and close it afterwards.
- **Enforced:** not enforced, convention.

## Releases

### The migration guard covered two files (#595)

- **Happened:** "migrations are append-only" was only pinned for `0001` and `0002`
  (a list that needed a manual line per release). A minor release could edit a
  shipped migration and nothing failed; existing installs would silently keep the
  old schema.
- **Rule:** never edit, rename or delete a migration that exists at the newest
  `vX.Y.Z` tag; add a new file.
- **Enforced:** `make migration-append-only` (in `make check`, job `migrations` in
  `pr.yml`, tag job `guards`). It needs the tags (`git fetch --tags`).

### Screenshots deleted while a template still linked them (#423)

- **Happened:** the 0.25.0 media refresh removed two images that the 0.24.0 Unraid
  template, still carried by the Community Applications feed, linked. The listing
  showed broken images for days.
- **Rule:** media linked from any published template revision is never deleted from
  `main`; templates link tag-pinned URLs.
- **Enforced:** not enforced, convention.

### An empty variable stopped the container (#148)

- **Happened:** the Unraid template ships `TASMOFLEET_LOG_LEVEL` empty; Unraid passes
  it as `KEY=`, and the config loader treated it as a value and refused to start. Every
  install that kept the default looped.
- **Rule:** an empty environment value means "not set" for every key. Test each lockable
  key with `KEY=`.
- **Enforced:** `internal/config` tests.

## Repository administration

### Moving the repository to an organization (#690)

- **Happened:** the move to the `hellwand` organization reset CodeQL default setup,
  turned off secret scanning and push protection, and emptied the ruleset's bypass
  list. GitHub Pages was not redirected, and the new container package was private
  until the organization allowed public packages.
- **Rule:** after a transfer, re-check every security setting, the ruleset bypass,
  Pages and package visibility, and update URLs (#690) in one change. Secrets,
  variables, environments and releases survive; do not assume the rest does.
- **Enforced:** not enforced, convention.
