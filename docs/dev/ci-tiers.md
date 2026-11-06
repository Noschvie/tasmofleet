# Test tiers in CI

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

## Test tiers in CI (`.github/workflows/`)

One workflow per trigger. The file a run comes from is its tier; only `pr.yml` looks at the diff (docs-only,
SAFETY package, catalogue, image paths). Every gate runs through the Makefile.

| File                                                        | Trigger                                    | Runs                                                                                                                                                                                                                 |
| ----------------------------------------------------------- | ------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pr.yml`                                                    | `pull_request`                             | FAST: `changes`, `checks` (with diff coverage on SAFETY PRs and the revert check), link check, migration and release section guards, image build check (gates the merge) and HA app image (does not), **`required`** |
| `main.yml`                                                  | push to `main`                             | lint, unit and integration tests, build (no e2e; saves the Go caches), `:edge` image and (apart from it) HA app image, then `ghcr-cleanup`                                                                           |
| `release.yml`                                               | tag `v*`                                   | `guards` (incl. green `required` + `main.yml` of the SHA), then `publish` (image), `release`, `pages`; no checks. Dispatch = dry run                                                                                 |
| `nightly.yml`                                               | cron 22:37 UTC, `workflow_dispatch`        | FULL: `checks` (e2e all, `-count=1`, catalogue), `race`, `quarantine`, `ha-app`, `ghcr-cleanup`                                                                                                                      |
| `weekly.yml`                                                | cron Sunday 21:13 UTC, `workflow_dispatch` | the full `mutation` run (all MUTATION packages) and its gate, the only place mutation runs                                                                                                                           |
| `_checks.yml`, `_mutation.yml`, `_image.yml`, `_ha-app.yml` | `workflow_call` only                       | the shared jobs; the caller's `with:` says which (inputs `e2e`, `e2e_shards`, `testflags`, `safety_race`, `diff_cover`, `revert_check`, `catalogue`, `scope`, `mode`)                                                |
| `_failure-issue.yml`                                        | `workflow_call` only                       | the issue for a failed scheduled run: failed jobs, failed tests, commits since the last green run                                                                                                                    |
| `ghcr-cleanup.yml`, `pages.yml`                             | `workflow_call`, `workflow_dispatch`       | registry pruning (manual run: dry-run preview), the website and the demo                                                                                                                                                        |

- **FAST** (`pr.yml`; `required` must be green to merge, target ≤ 5 min):
  lint, unit and integration tests, build, and the `@core` e2e tests in two shards plus the MQTT specs in a job of their own.
  When a PR changes a package listed in `SAFETY` (Makefile: fwselect, ota, tasmota, secretbox, auth, httpx,
  policy, cmdpolicy, mqttcmd, transport, restore), the race tests of the changed ones and diff coverage (`make diff-cover`) run too, and are then
  required. Every PR that changes Go code also runs the revert check (`make revert-check`). There is no mutation testing in the PR path (#555);
  the weekly run shards it in four (`MUTATION_SHARD`, files dealt out by estimated cost, `tools/mutation-weights.txt`) and gates on their sum
  (`make mutation-sum`). CI restores the Go build/test cache (`.github/actions/go`, keyed on Go
  version and `go.sum`) and sets file mtimes from git (`tools/git-mtimes.sh`), so unchanged packages
  are not retested: their results come from the cache that `main.yml` saves. Only runs on `main` save caches (`main.yml`,
  `nightly.yml`, `weekly.yml`; the Go caches, the npm and Tasmota-source caches alike): a cache saved by a PR or a tag run
  can be restored by nothing else and only evicts main's (#476).
- **Push to `main`** (`main.yml`): lint, unit and integration tests and the build, but no e2e: `main` moves only by a
  fast-forward push of a PR head that was green, so the e2e shards would repeat the PR (#478). The tests run, cacheable, so
  that main's go test cache holds their results for the PRs and tags (the nightly runs `-count=1` and stores none); the
  e2e caches are refreshed by the nightly. The `:edge` image and the HA app image follow.
- **RELEASE** (`release.yml`, tags `vX.Y.Z`): no tests of its own (#554). `main` moves only by a fast-forward push of a
  green PR head, so the tagged commit has already passed the FAST tier on its PR (`required`) and lint, unit and integration
  tests and the build on `main` (`main.yml`); running them again on identical code only delayed the image. Job `guards` fails at once, with the reason, when the
  tag is not on `main`, has no `CHANGELOG.md` section, is a patch tag that changes a migration, edits a migration of the previous tag,
  or when its commit lacks a successful `required` check run of `pr.yml` or a successful `main.yml` run (`tools/release-green.sh`;
  only `ghcr-cleanup` and `ha-app` may fail there). Only the head of a push to `main` gets a `main.yml` run, and a push that changes only
  paths `main.yml` ignores gets none: tag the head of the push that carried the release commit, only after `main.yml` has
  finished on it. The race tests of every `SAFETY` package run in the nightly FULL on the same
  `main`. Then the image is published (job `publish` calling `_image.yml` with `mode: release`, the only way to push a tag
  image), then the GitHub release is made, then the demo site is deployed, each only after the one before succeeded.
  `workflow_dispatch` with a `sha` is a dry run of the green-commit check alone: it never publishes, releases or deploys.
- **FULL** (`nightly.yml`: cron on `main`, manual dispatch): additionally every e2e test in three shards plus the MQTT job, every
  unit test with the race detector (two shards: the slowest packages listed in `tools/race.sh`, all others; `make shards-check` fails when a package is in no shard), the command catalogue check, uncached test runs, and the Home Assistant app in a real Supervisor (`tools/ha-app-devcontainer-test.sh`). It blocks no release; when it fails on the schedule, the job `nightly-failure-issue` opens an issue "Nightly FULL failed on main (date)" (labels `ci`, `flaky`) with the failed jobs, the tests their logs name and the commits on `main` since the last green night, or comments on the one that is still open. GitHub starts the scheduled run 5 to 8 hours after its cron time, so the cron is set to land in the early morning. A manual run never opens an issue and only previews the registry cleanup. Layout and visual regressions and race outside the safety packages are caught here, so the nightly FULL is the place to look for them; a
  failure is fixed through an issue, not by holding a release.
- **WEEKLY** (`weekly.yml`: cron on `main`, Sunday night, manual dispatch): the full mutation run of every MUTATION package with its gate (`make mutation`
  in four shards, `_mutation.yml` with scope all). The nightly does not run it (#480), and no PR waits for it (#555), so this is where a surviving mutant shows up, once a week. It blocks nothing; when it fails on the schedule, the job `weekly-failure-issue` opens an
  issue "Weekly mutation run failed on main (date)" (label `ci`) or comments on the open one. The run also saves the mutation Go cache.
- **Which e2e tests are `@core`**: smoke, sign-in and authorization (header, local, OIDC, standalone,
  operators refused), first-run setup, base path, the device list, the OTA update flow (batch, push upload),
  the chip-family and OTA-exclusion refusals in the UI, device writes (console, settings, restore, make
  compliant). Tag with `test("title", { tag: "@core" }, ...)`. Layout, visual and component checks stay in
  FULL. Add a test to core when it guards a safety rule or the main path; keep the core run short.
- **Quarantine**: a test that fails without a code cause gets a GitHub issue (label `flaky`, failure evidence)
  and is quarantined: Playwright tag `@quarantine`, or `quarantine.Test(t, "<issue URL>")` from
  `internal/testutil/quarantine` for Go tests. Quarantined tests run only in the `quarantine` job of `nightly.yml`,
  with retries, and never block a PR or a release. Fix or delete each one within about two weeks; the fixing
  change removes the tag and closes the issue.

Docs-only PRs skip the checks; the link check (`tools/check-doc-links.py`), the migration guard (`make migration-append-only`)
and the release section guard (`make release-pr-check`) run on every PR and gate the merge. The list of docs
that are safe to ignore (and the ones a test reads, which are not) is in the header of `.github/workflows/pr.yml`.
Third-party actions are pinned to a full commit SHA with the version in a comment (`uses: owner/repo@<sha> # vX.Y.Z`);
Renovate keeps both current. Reusable workflows take their secrets by name (`secrets:`), never `secrets: inherit`.
The Docker Hub credentials (`DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`) live only in the `dockerhub` environment (deployment policy: `main` and `v*` tags); repository secrets of that name must not exist, because a value passed by name wins over the environment secret. The callee jobs (`image` in `_image.yml`, `cleanup` in `ghcr-cleanup.yml`) set `environment:` themselves and declare the secrets under `workflow_call.secrets`; the callers (`main.yml`, `nightly.yml`, `release.yml`) still pass both by name, which is what unlocks the environment secret in the callee. On `main` and `v*` tags of `hellwand/tasmofleet` missing credentials fail the job.
