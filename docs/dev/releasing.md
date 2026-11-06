# Releases

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

## CHANGELOG and versions

- **SemVer** from v0.1.0: new features or visible behaviour -> minor (`0.x.0`); bug fixes
  and small changes only -> patch (`0.x.y`). Breaking changes are allowed before 1.0 but
  must be listed in `CHANGELOG.md`.
- `CHANGELOG.md` has one section per version with the subsections Upgrade notes, Added, Changed, Fixed,
  Removed, Breaking and Security (in that order, empty ones omitted). **A change never edits `CHANGELOG.md`**
  (#527): it adds a fragment `changes/unreleased/<issue>-<slug>.md`, so parallel PRs do not conflict there.
  Format (see `changes/README.md`): front matter `section: Fixed` (one of the sections above), then the line
  exactly as it should read, credits inside it. Several fragments per issue are fine; the file name only orders
  the lines. A line must not start with `- `.
- **The changelog fragment gate** (`pr.yml`, part of `required`): every PR validates its fragments; a PR that
  changes more than docs must add a fragment, or be the release commit, or carry `changelog: skip - <reason>`
  on a line of its PR body (nothing user-visible; the placeholder `<reason>` does not count). Renovate PRs
  (`renovate[bot]`) are exempt. The gate also fails on a `## Unreleased` section in `CHANGELOG.md`.
- **The release commit assembles**: `make changelog-assemble CL_VERSION=X.Y.Z [NAME="Name"] [DATE=YYYY-MM-DD]`
  writes `## X.Y.Z "Name" (date)` above the newest section and deletes the fragments, BEFORE the tag is set;
  `changes/unreleased/` must be empty in the release commit (the gate fails otherwise). NAME is required for a
  new minor (next free name in `docs/release-names.md`) and defaults to the minor's name for a patch; DATE
  defaults to today (Europe/Berlin). It is `CL_VERSION` because `VERSION` is the build variable. The tag build
  fails when the section is missing (`make changelog-check TAG=vX.Y.Z`). The What's new page reads only the
  assembled file; pending fragments are not shown.
- **Credit outside contributors**: every merged PR by someone else, and every issue or
  report by someone else that led to a change, is credited at the end of that change's
  CHANGELOG line (inside its fragment) as `(#N, thanks @handle)`. Do not credit the maintainer's own work.

## Release process

- **The release commit rides in the merge train** (#556): it is the last commit of the release's train (or of
  the single PR when there is no train), followed only by the release's demo-media refresh (`make demo-media`) when there
  is one, so one `required` run on its tip, one fast-forward and one `main.yml` run cover the release. A separate release
  PR only when the release has nothing else to merge. When a PR's tip adds a new
  `## X.Y.Z` section, its CI runs the tag's guards (`make release-pr-check BASE=origin/main`: `changelog-check` and
  `migration-guard`); run it locally before pushing a train. The demo-media refresh must be in that same push: the
  template's tag-pinned media URLs need it in the tagged tree, and a docs-only push of its own gets no `main.yml` run, so
  a tag on it is refused. Then wait for `main.yml` on the pushed head and set the tag on that head.
- **Release names**: every minor release is named after a lighthouse. The release commit takes the next
  free name from `docs/release-names.md` and marks it assigned there; the heading is
  `## 0.21.0 "Name" (YYYY-MM-DD)`. Patch releases repeat their minor's name. Tags stay `vX.Y.Z`. The
  GitHub release title is `TasmoFleet X.Y.Z "Name"` (derived from the heading by
  `make changelog-title`), and announcements and forum posts use the same form. Old sections without a
  name stay valid.
- **Patch releases (X.Y.Z with Z>0) never add or change a database migration; a schema change
  always means a new minor version.** The tag build enforces it (`make migration-guard`).
- Tags are signed (`git tag -s vX.Y.Z`) and only set on a green commit of `main`. The tag
  run (`release.yml`) checks that the commit is green already (`required` and `main.yml`, see RELEASE tier; no tests are repeated), and the
  image, the GitHub release and the demo site follow only when that passed.
- **Announce every release** in the one pinned Announcements discussion "TasmoFleet releases" (#315),
  after the tag run has published the image and the GitHub release. Edit its body (`updateDiscussion`):
  the "Current release" section shows `TasmoFleet X.Y.Z "Name"`, the image tags
  (`nofuturekid/tasmofleet:X.Y.Z`, also on ghcr.io), upgrade notes (migrations or "No database
  migration"), a short bullet list of the changes with issue refs and `thanks @handle` credits, and links
  to the GitHub release and `CHANGELOG.md`. A minor release also adds a short comment (3-5 bullets) so
  watchers get a notification; a patch release only edits the body. No per-release discussions. English,
  no fluff.
- **Unraid users run `:latest` from the Community Applications template**
  ([nofuturekid/unraid-templates](https://github.com/nofuturekid/unraid-templates), `templates/tasmofleet.xml`).
  With every release:
  - add the release to the template's `<Changes>` block;
  - edit the first post of the [Unraid support thread](https://forums.unraid.net/topic/201022-support-tasmofleet/)
    (current release line, links, setup notes that are still true). No reply post per release; only a
    release users have to act on (template, port or variable change, a manual upgrade step) also gets a
    reply post, upgrade notes first, because only replies notify the thread's followers;
  - a release that changes ports, variables or anything else in the template ships the template change
    in the same moment the image becomes `:latest`. An existing install keeps its saved template, so such a
    release must also keep existing installs working without edits, or say exactly what to change, at the
    top of the CHANGELOG section, the announcement, the template `<Changes>` and the forum post.
  - **Media linked from any published CA template revision is never deleted from `main`.** The CA feed
    is regenerated on Squid's schedule and can carry an old template revision for days (#423: the 0.24.0
    template still linked two screenshots that the 0.25.0 media refresh had removed). From the next
    release on, the template's screenshots link tag-pinned URLs
    (`https://raw.githubusercontent.com/hellwand/tasmofleet/v<X.Y.Z>/docs/media/...`), so later
    media changes cannot break an old feed entry. `docs/media/transaction.png` and
    `docs/media/device-dark.png` stay on `main` for the 0.24.0 revision even though no doc links them.
