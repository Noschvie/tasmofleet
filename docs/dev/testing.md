# Running the gates

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

| Gate                                                                      | Command                                                                       |
| ------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| FAST tier, everything but e2e (run before every commit)                   | `make check`                                                                  |
| inner loop between two edits (not a gate)                                 | `make quick`                                                                  |
| FAST tier, the `@core` e2e tests                                          | `make e2e-core`                                                               |
| FAST tier, a safety package changed (race + diff coverage + revert check) | `make safety BASE=origin/main`                                                |
| every changed SAFETY line is executed by a test                           | `make diff-cover BASE=origin/main`                                            |
| the tests you changed fail without your production change                 | `make revert-check BASE=origin/main`                                          |
| FULL tier, everything CI runs nightly and weekly                          | `make ci TESTFLAGS=-count=1`                                                  |
| format                                                                    | `make fmt` / `make fmt-check`                                                 |
| go vet                                                                    | `make vet`                                                                    |
| staticcheck (also inside `make lint`)                                     | `make staticcheck`                                                            |
| golangci-lint (incl. depguard boundaries)                                 | `make lint`                                                                   |
| unit tests                                                                | `make test`                                                                   |
| integration tests                                                         | `make integration`                                                            |
| race detector                                                             | `make race`                                                                   |
| mutation tests (MUTATION in the Makefile), optional                       | `make mutation` / `make mutation-changed BASE=<ref>`                          |
| e2e (Playwright), all but quarantined                                     | `make e2e-deps` once, `npx playwright install chromium` once, then `make e2e` |
| quarantined tests (nightly only)                                          | `make test-quarantine` / `make e2e-quarantine`                                |
| vulnerabilities                                                           | `make vuln`                                                                   |

Tools are pinned in `tools/go.mod` and run via `go tool -modfile=tools/go.mod <tool>`.
Run a single package's tests with `go test ./internal/fwselect/ -run TestGuard -v`.
`make quick` runs fmt-check, vet, golangci-lint and the tests (the integration tests only when their package is affected) on the packages
changed since `origin/main`, uncommitted and untracked files included, and on every package that imports them; a changed file outside every package
(docs, assets, deploy, tools, a `testdata/`) makes it run everything, because tests read such files. It skips templ-check, scripts-test,
shards-check, migration-append-only, vuln and build: `make check` stays the gate before every commit.
Parallelism follows the machine: `JOBS` defaults to `nproc` (`GOFLAGS=-p=$(JOBS)`),
mutation workers to `nproc/2`, Playwright workers to `E2E_WORKERS` (default 6). Locally the Makefile runs tools under `nice -n 15`; CI sets `NICE=` (empty). Override
with e.g. `make check JOBS=4`. `make test` and `make integration` use the go test cache
(unchanged packages report `(cached)`); `TESTFLAGS=-count=1` forces a fresh run. `make integration` runs only
the packages that have `//go:build integration` test files (found at run time, anywhere in the module); only `_test.go` files may carry the tag.
`make check` runs `shards-check` only after a change to the Makefile, `tools/`, a `go.mod` or the package list (CI runs it every time).
**Before you push** (builders): `make check`; when you changed a `SAFETY` package, also `make safety BASE=origin/main`; for every fix
`make revert-check BASE=origin/main` (it takes seconds and is what the PR's `revert check` job runs).
`make diff-cover` runs the tests of the changed SAFETY packages once with a coverprofile (integration-tagged tests included) and fails
for every line you added or changed in a non-test Go file of a SAFETY package that no test of that package executes; comments,
declarations and braces carry no code and are not checked. `make revert-check` copies the tree to a scratch directory, puts the production
`.go` files you changed back to the merge base (files you added are removed, deleted ones restored), keeps the tests, and runs the tests you added or
changed (the whole package when you changed a helper, a table or an import). At least one of them must fail, or the package must stop building
(a feature whose tests use new API); a selected test that never ran or skipped (no decoder, a quarantine) is an error. The output lists every test that fails and every one that passes
either way. A PR that changes production code without any test is not stopped by it (diff coverage covers the SAFETY packages). **Opt-out** for a pure
refactor or a feature where nothing should fail: a line `revert-check: skip - <reason>` in the PR body, or the label `no-revert-check`;
the job reads both from the API when it runs, so after adding it re-run the job. The gate prints "SKIPPED" and the reason; locally
`REVERT_CHECK_SKIP="<reason>"`. Both gates diff against the merge base with `BASE` (default `origin/main`), uncommitted and untracked files included.
Mutation testing is not part of any PR (#555, the maintainer finds it makes PRs wait and rarely catches anything):
`make mutation-changed BASE=<ref>` mutates only the lines this branch changed since the merge base in the MUTATION packages it touched, and
`make mutation` every package whole (the weekly full run, a report until the keep-or-drop decision at the end of November 2026).
Any changed test or data file of a package (or an untracked source file in it) mutates that package whole, so weakened tests
cannot hide behind a small code change. On the changed lines a mutant no test reaches counts as lived. Fewer than 10 decided mutants
on the changed lines (`MUTATION_DIFF_MIN`) are too few for a percentage: such a diff passes with at most one lived mutant but needs a
kill, and one without any decided mutant (a comment, a declaration) passes and says so. Details and the
gremlins `--diff` traps they work around: header of `tools/mutation-gate.sh`. A sharded run leaves `mutation-report.json` and a job
summary (lived, timed-out and not-covered mutants per package) next to the counts (`tools/mutation-report.sh`).
gremlins builds every mutant, and each build is garbage afterwards (+261 MB per `internal/cmdpolicy` run), so the gate runs it with a throwaway `GOCACHE`
next to your Go cache that it removes when it ends (#546): neither your Go cache nor the one CI saves ever holds a mutant build (`MUTATION_GOCACHE_SEED` warms it, header of `tools/mutation-gate.sh`).
