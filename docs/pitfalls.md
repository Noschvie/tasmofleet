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
- **Enforced:** ruleset `main: CI must be green (no bypass)` (signed commits, linear history, `required`;
  no admin bypass since #787, so the button's unsigned commits are refused for the maintainer too). The
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
- **Rule:** use `tools/ci-wait.sh <n>` (see [pull-requests](dev/pull-requests.md)); it waits
  until `required` is registered and prints `required=<result> sha=<head>`.
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

### A weekly mutation shard timed out and nobody was told (#729)

- **Happened:** on 2026-11-09 shard 4/4 of the weekly mutation run hit the 30 minute
  job limit. A job cut off by its timeout ends "cancelled", not "failed", so the
  `failure()` condition of `weekly-failure-issue` was false and no issue was opened.
  The shard was too long because the files added since the last refresh of
  `tools/mutation-weights.txt` (about 200 mutants) had no entry, counted as one mutant
  each and were all dealt to the shard that already held the slowest package.
- **Rule:** a job that reports a scheduled run's failure runs when the job it watches
  did not succeed (`!cancelled() && needs.<job>.result != 'success'`), not only on
  `failure()`. A file without a weights entry is dealt by its size. After a new file
  or a slow week, refresh the counts (`tools/mutation-weights.sh`) and compare each
  shard's duration with `make mutation-plan`.
- **Enforced:** the condition in `.github/workflows/weekly.yml`;
  `scripts/mutation-shard.test.mjs` for the estimate.

### The weekly mutation shards grew past the job limit again (#886)

- **Happened:** on 2026-11-16 shard 4/5 hit the 30 minute limit again, and the other four took 21 to 28 minutes. The
  mutant counts had barely changed; the time per mutant had: every mutant reruns its package's tests, and the MQTT
  transport work added large test files (restore 4.9 to 6.6 s per mutant, mqttcmd channel +17 %, policy 0.5-0.9 to
  1.2-2.0 s). The cost lines of `tools/mutation-weights.txt` were still those of 2026-11-12. Two more causes: the
  deal did not know that every shard pays a fixed setup (test build and coverage run, 6 to 37 s, 22 s on average) for
  each package it touches, so the estimate was about 4 minutes low, and the total work (about 127 min) no longer fitted
  five shards under 20 min.
- **Rule:** the weekly mutation run has 8 shards (`MUTATION_CI_SHARDS` in the Makefile; `make shards-check` fails
  when the matrix or the gate count of `_mutation.yml` differs). The job timeout is not raised: it only moves
  the cliff. After the test time of a MUTATION package grows, set the `cost` lines from the last weekly run
  (measured over estimated seconds per package) and compare each shard's duration with `make mutation-plan`;
  the `setup` line is the per-package fixed cost.
- **Enforced:** `make shards-check`; `scripts/mutation-shard.test.mjs` for the setup in the deal.

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

### Superseded Go caches filled the 10 GB limit (#692)

- **Happened:** the key of a Go cache carries the commit, so every run on `main` saved a new entry and nothing removed
  the old ones. Four runs in 25 minutes put the repository at 13.9 GB of 10. The floating `go 1.27` then moved from
  1.27.3 to 1.27.4, the Go version is part of the key, and a whole series (about 5 GB) that nothing could restore stayed
  next to the live one until GitHub's eviction took it, together with caches still in use.
- **Rule:** a cache that gets a new key per run needs a prune at the cause. It runs in a job after the jobs that save
  (`actions/cache` saves in its post step, so a cleanup step inside the saving job runs before the new entry exists) and
  groups by series without the Go version and the `go.sum` hash. Keep the newest entry of `refs/heads/main` per series,
  list first and delete by id (`gh cache list --key` is a prefix match, the delete call an exact key), and give the
  job `actions: write` in the caller as well as in the called workflow.
- **Enforced:** `scripts/prune-go-caches.test.mjs` (the selection); `.github/workflows/cache-prune.yml` runs after
  `main.yml`, `nightly.yml` and `weekly.yml`. Only entries of `refs/heads/main` are listed and deleted; a manual dispatch of the nightly or weekly run only previews.

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
- **Enforced:** the repository setting "require actions to be pinned to a full-length commit SHA" (#787):
  a workflow using an unpinned third-party action fails; Renovate keeps the pins current. `secrets: inherit` is
  convention and review.

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

### The previous edge was deleted together with a child the new edge uses (#728)

- **Happened:** after a re-run of the `main.yml` run of one commit, `ghcr.io/.../tasmofleet:edge` was gone. The step
  "delete the previous edge" deleted the old edge index and the platform manifests it listed. The re-run had built a
  bit-identical arm64 image, so the new edge index listed the same arm64 manifest; the nightly `ghcr-cleanup` then
  found the new edge incomplete (a "partial image") and deleted it too.
- **Rule:** a platform manifest of a multi-arch index is just a digest, and two indexes can share it. Before deleting
  a manifest of the old edge, read from the registry (after the push) which digests the tagged indexes use and
  which children they list, and keep all of those. `scripts/edge-cleanup.mjs` decides, with tests; do not infer what
  is unused from the order of events or from comparing two indexes.
- **Enforced:** `make scripts-test` (the decision). The registry calls around it only run on `main` (`_image.yml`, mode edge).

### Base images pulled straight from Docker Hub hit the pull limit (#1212)

- **Happened:** the release commit's `main.yml` run failed twice in `image / build image (edge)`: Docker Hub answered
  429 Too Many Requests while BuildKit pulled `golang` and `debian`, although the Docker Hub login had succeeded. Several
  image builds had run in the hour before; the login only exists on `main` and tags and only raises the quota. A rerun
  after about 25 minutes passed.
- **Rule:** no CI build depends on the Docker Hub quota alone. Every `docker/setup-buildx-action` step carries
  `buildkitd-config-inline` with `[registry."docker.io"] mirrors = ["mirror.gcr.io"]`; BuildKit asks the mirror first and falls
  back to Docker Hub. Keep the `docker.io/library/...` names in the Dockerfile (Renovate, local builds) and do not answer
  a 429 with a retry or a longer timeout. A new buildx step gets the config in the same change (`docs/dev/ci-tiers.md`).
- **Enforced:** convention and review; the image build check of the PR (`pr.yml`) and the nightly run the config.

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

## Devices and transport

### A second MQTT path that skipped the router's checks (#605)

- **Happened:** the power toggle published `Power<n>` over MQTT by itself whenever the device was on our
  broker and the bus was up. It never asked whether the topic was the device's own (exclusive, its GroupTopic
  read), which every other change over MQTT has to pass, so a switch could reach the devices of a shared topic.
- **Rule:** a call that talks to the command channel itself asks the router's write rule (`transport.Choose`'s
  writes path, injected by the app), never a copy of it; a write MQTT published and nobody answered is never
  resent, and only a write MQTT provably did not publish goes over HTTP.
- **Enforced:** `TestPowerAsksTheRoutersWriteRule`, `TestPowerFollowsTheStrictWriteRule`,
  `TestRouterWritesFollowChoose` (claims T2, T3 and T11 of `docs/security/mqtt-transport.md`); otherwise convention.

### "HTTP not known to fail" read as "HTTP works" (#605 review)

- **Happened:** a first version of #605 published a change of the MQTT connection (MqttHost, Topic, ...) without
  the lockout question whenever HTTP was not failing. The HTTP state of a device live on MQTT is normally unknown:
  nothing polls its HTTP, a failure is forgotten after 10 minutes and a timeout is never recorded as one, so a
  firewalled device would have been taken off the broker with no HTTP to reach it again. The next version trusted
  a fresh answer on record (15 minutes) instead: the person's own `WebServer 0` sent over MQTT a minute later
  closes HTTP, and a following `MqttHost` would have gone out without a question all the same.
- **Rule:** a decision that relies on HTTP as the way back asks HTTP itself right before (the router's
  `Status 11`); neither unknown nor an answer on record counts. A fresh answer on record (`HTTPOK`) is only the
  console's hint whether to ask up front; when the router refuses after all, the console asks then. Up-front
  refusals that would block the whole fleet (bulk preview, Make compliant's plan) use "known to fail" instead and
  leave the rest to the router. One exception, `Status 0` (#1001): the router has no HTTP check of its own to leave
  it to, and an unknown HTTP state ends in a timeout per device, so the bulk preview lists a device whose writes go
  MQTT first and whose HTTP has no fresh answer as "Not sent"; the device's console asks for an HTTP check.
- **Enforced:** `TestACutOffWriteGoesWithoutAQuestionOnlyWhileHTTPIsProvenUsable`,
  `TestACutOffWriteAfterHTTPClosedIsRefusedDespiteAnAnswerOnRecord`, `TestConsoleAsksWhenTheRouterRefusesACutOffChange`,
  `TestReachedOnlyOverMQTTMeansHTTPIsNotKnownToWork`, `TestKnownOnlyOverMQTTMeansHTTPIsFailing` (claims T8 and T10 of
  `docs/security/mqtt-transport.md`); the `Status 0` exception: `TestBulkStatus0IsNotSentWhereHTTPIsNotKnownToWork`
  (claim T13).

### A written value compared in another form than the device reports it (#947)

- **Happened:** Make compliant compared what it read back with the rule's text after trimming and ON/OFF only.
  Tasmota answers some values in a form of its own: `Timezone 1` reads back as `+01:00` (`CmndTimezone`), so a
  Timezone rule never matched a device that had the value, and a read-back on the Settings tab would have shown
  every Timezone write as "differs".
- **Rule:** written and reported values are compared in one place, `settings.Matches`, which brings both sides to
  the firmware's form (ON/OFF, coordinates at four decimals, Timezone as `+HH:MM`); the read-back and the policy's
  live check both call it. A new form goes there, with the firmware source line, never into one caller. Only a
  form the firmware reports for the value as written belongs there: a zero-padded `08` or `09` Timezone hour is
  octal 0 to the firmware (`strtol` base 0), so it sets `+00:00` and must show as differing, not be matched.
- **Enforced:** `TestMatchesKnowsTheFirmwaresOwnForms`, `TestAWrittenTimezoneIsVerifiedInTheFirmwaresForm`;
  otherwise convention.

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

### Layouts that broke on phones and tablets (#510, #511, #633, #934)

- **Happened:** badges overlapped lists, long dotted names broke mid-label, and whole
  pages broke between 720 and 1199 px, none seen on a desktop check.
- **Rule:** check 375 px and tablet widths, not only 1280.
- **Enforced:** layout guards in `e2e/tests/` (`tablet-guards.spec.ts`,
  `layout-guard-words.spec.ts`, `whats-new-layout.spec.ts`). Every page with a table runs the table guards at
  1280 and 375 px (#821): `e2e/support/table-pages.ts` reads the tables from the templates, and
  `table-pages.spec.ts` fails for a table that no page of its list shows, so a new table needs an entry in
  `tablePages` (the page, and the steps that bring the table up). The Devices list also runs with a saved
  column layout wider than the frame at 1290 px, the owner's window (`e2e/support/saved-layout.ts`, used by
  `tablet-guards`, `pinned-overlap`, `columns` and `live-cell-swaps`): the default layout is the one a person
  never has, and the three table defects of 0.36.0 (#805, #806, #811) showed only in a saved one.
  Guard o also judges the status bar (`footer.sb`, #934): its items are one nowrap row on every page, and
  when they are wider than the window flex shrinks their boxes below their text, which then runs under the
  next item. The wide row clips every item (`app.css`, "status bar"); `devices-backups.spec.ts` seeds a
  running backup and checks the bar from 1280 down to 720 px and on a phone.

### A tooltip over the next row, and a bubble for a name that fits (#811)

- **Happened:** the Network scan table's name cell had `title` = its own text. The tooltip script showed it
  for every name, below the cell, and a cell fills its whole row, so the bubble lay over the next row's name.
  The Devices list name cell had the same title and the same placement.
- **Rule:** a title that only repeats the element's text shows only while that text is cut off, and the
  bubble of anything inside a table cell sits beside it, never below it (`static/tooltip.js`). A new table
  needs no special case for this. The bubble is measured from the left edge (a fixed box left where the last
  bubble stood is only as wide as the room right of it, so it would be measured too tall and centred too high).
- **Enforced:** `e2e/tests/scan-table.spec.ts` for the scan table (the markup it loads is
  `testdata/scan/*.html`, kept equal to the templates by `TestScanResultFixtures`; regenerate with
  `-update-scan-fixtures`) and `e2e/tests/pinned-overlap.spec.ts` for the Devices list (every titled
  element of two rows, the pinned Power, Status and Actions columns included). Both use `hoverTip` in
  `e2e/support/layout.ts`. Every other table gets the same check from guards s (a cell is not cut off while
  another column has room to spare, titled or not) and r (no element of one row over the text of another, as
  loaded and with the pointer on a row and its titled elements) in `e2e/support/layout.ts` (#821); a very long
  explanation (about 100 characters or more) in a dense row is not guarded.
  A table guard must survive a live region swap (#1015): `hoverRowProblems` names its table by its place in the
  page (the path from the nearest ancestor with an id, such as a live region's card), never by an attribute set
  at runtime, because the swapped-in table has no such attribute; every call it makes is bounded.

### A visible browser opened on the maintainer's desktop (2026-11-01)

- **Happened:** an agent used the Playwright MCP browser tools, which open a headed
  window on the desktop.
- **Rule:** headless scripts (Playwright, curl) by default. If a visible browser is
  needed, use Claude in Chrome in your own tab and close it afterwards.
- **Enforced:** not enforced, convention.

### A document-wide script on every live cell swap (#726)

- **Happened:** the Devices list swaps one cell per fragment, about 90 times a second with 60 devices,
  changed or not. A `syncAll` that walks the whole document was added to `htmx:afterSwap` (#594), and the
  open page fell 10 s and more behind its server; the nightly e2e job failed on it (#703).
- **Rule:** a listener on `htmx:afterSwap` or `htmx:afterSettle` runs for every live cell: check the swap's
  target first and keep per-swap work off the whole document. Since #1059 every such listener in
  `internal/web/static` reads the swapped element (`e.target`) and its subtree and returns at once when what it
  works on cannot be in it; `syncAll` (`select.js`) runs document-wide only for a swap that is or holds a
  selection control, box or output (the list reload), and a health cell only touches its own row. The runs at
  `DOMContentLoaded` and `pageshow` stay document-wide (#594). A new listener follows the same pattern. Live
  cells whose fragment did not change are not swapped (`live.js`).
- **Enforced:** `e2e/tests/live-cell-swaps.spec.ts` counts swaps per live cell and every
  `document.querySelector` and `document.querySelectorAll` call during the `htmx:afterSwap` and
  `htmx:afterSettle` of a live cell (expected 0), and that a list reload still runs `syncAll`.

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
- **Rule:** media linked from any published template revision is never deleted or
  renamed; templates link tag-pinned URLs or the Pages pictures (since #859 the
  pictures are build output of `make pages`, see `docs/dev/releasing.md`).
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
