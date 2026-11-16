# UI changes

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

**UI changes:** the agent makes screenshot pairs per screen (mockup and build where a mockup exists,
otherwise before and after; 1280 and 375 px, light and dark) and keeps them outside the repo. During design
phases these local before/after comparisons are what the maintainer looks at; they never hold work back.
Before merging, the coordinator compares them; every visible difference is fixed or listed in the PR with
the reason. Screenshots are never committed to any branch or embedded in a PR. "Tests pass" is not
acceptance for UI work.

**A new table** (a `<table>` in a `.templ` file, or a `ui.TableData` in Go) needs a page in `tablePages`
(`e2e/support/table-pages.ts`) that shows it, with the steps that bring it up: `make e2e` fails in
`table-pages.spec.ts` until one does. The page then gets the layout guards and the table guards at 1280 and
375 px (`tablet-guards.spec.ts`, `policy-templates.spec.ts`). Guard s judges every cell, the longest of its
row included; a column whose text is as long as the data makes it (the audit summary) is named in the page's
`freeText`, with the reason in `note`. A finding that is open on main is listed in `knownLayoutIssues`
(`e2e/support/layout.ts`) with its issue and removed when the issue is fixed; the test reports it as an
annotation, and reports the entry when the guard no longer fires.
