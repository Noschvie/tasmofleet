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
- **What's new reads a headline from every line** (#1108): the page shows one headline per change and keeps the full
  line behind a "Details" fold per release (the last 5 releases; the full changelog on GitHub has the rest). The
  headline is the bold lead of the line, else its first clause (to the first `: `, `; ` or `. ` outside code, quotes
  and parentheses), with issue numbers, credits and asides that hold code left out. Say it for users with an optional
  front matter line `user: <one sentence, no asterisk>`: the assembler writes the line as `- **<user line>** <text>`,
  so the sentence is the headline and the text stays the detail. Write a `user:` line, or start the line with a
  sentence a user understands, whenever the first clause would be a migration, a column or a function name: a
  migration belongs in "Upgrade notes" (Details only), and a test fails when the page shows
  `Migration 00` outside the Details folds. A change the admin must act on goes in "Breaking": What's new shows it as a warn
  banner at the top of its release ("Upgrade notes: action may be required" does the same for a release that has it;
  the assembler's sections do not include it).
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

## Screenshots and the update animation

The website's pictures are build output, never committed (#859): a committed copy only changes when somebody
remembers to renew it, and the project page showed the UI of 0.28.0 next to a demo of 0.37.0.

- `make pages` (`pages.yml`, run by `release.yml` after the GitHub release and by hand) runs `make demo` (the
  published demo, documentation addresses) and `make demo-media` (the Playwright run of
  `e2e/tests/demo-site.spec.ts` with `DEMO_MEDIA=1`: the cleaned-up fleet, four screenshots and two videos, which
  `ffmpeg` converts to the animations) from the tagged commit. The pictures land in `dist/pages/assets/`:
  `dashboard.png`, `devices.png`, `device-settings.png`, `policy-editor.png`, `demo.webp`, `tour.webp`. So the website, the demo
  and the pictures always show the UI of the release that was published, and `site/assets` keeps only fonts, the
  wordmarks and the favicon. `scripts/pages-check.mjs` fails `make pages` when `site/index.html` links a file the
  artifact lacks, and `make scripts-test` fails when a picture is committed under `site/`.
- **The README links those published files** (`https://hellwand.github.io/tasmofleet/assets/<name>`) instead of
  keeping copies in a media folder. Why not a release step that refreshes such a folder behind a guard: a guard cannot
  tell whether a change was visible, so it would either refuse every release or pass a stale copy, and the refresh
  had to ride in the release's push. The price: the README's pictures show the latest release, not `main`, and they
  need Pages to be deployed (the first deployment of a new picture name is the release that adds it).
- Preview: `make demo-media` writes the pictures to `dist/media` (needs `ffmpeg` and the e2e dependencies); `make
  pages` is the whole artifact in `dist/pages`. `site/index.html` opened straight from the source tree shows no
  pictures.

## Release process

- **The release commit rides in the merge train** (#556): it is the last commit of the release's train (or of
  the single PR when there is no train), so one `required` run on its tip, one fast-forward and one `main.yml` run cover
  the release. A separate release PR only when the release has nothing else to merge. When a PR's tip adds a new
  `## X.Y.Z` section, its CI runs the tag's guards (`make release-pr-check BASE=origin/main`: `changelog-check` and
  `migration-guard`); run it locally before pushing a train. There is no media refresh step: the screenshots are
  build output (see "Screenshots and the update animation" above). Then wait for `main.yml` on the pushed head and
  set the tag on that head.
- **Release names**: every minor release is named after a lighthouse. The release commit takes the next
  free name from `docs/release-names.md` and marks it assigned there; the heading is
  `## 0.21.0 "Name" (YYYY-MM-DD)`. Patch releases repeat their minor's name. Tags stay `vX.Y.Z`. The
  GitHub release title is `TasmoFleet X.Y.Z "Name"` (derived from the heading by
  `make changelog-title`), and announcements and forum posts use the same form. Old sections without a
  name stay valid.
- **Patch releases (X.Y.Z with Z>0) never add or change a database migration; a schema change
  always means a new minor version.** The tag build enforces it (`make migration-guard`).
- Tags are signed (`git tag -s vX.Y.Z`) and only set on a green commit of `main`. A pushed `v*` tag cannot be
  moved or deleted, by anyone (ruleset `tags: release tags cannot be moved or deleted`, no bypass: deletion, update,
  non-fast-forward; creating a new `v*` tag stays allowed). A wrong tag is fixed with a new version, not by moving it;
  only in an emergency may a repository admin disable that ruleset for the one repair and set it back to active right
  after. Tag signing itself is convention. The tag
  run (`release.yml`) checks that the commit is green already (`required` and `main.yml`, see RELEASE tier; no tests are repeated), and the
  image, the GitHub release and the demo site follow only when that passed.
- **Native archives** (#472): the tag run also builds static linux/amd64 and linux/arm64 binaries (`make dist
  VERSION=X.Y.Z`, job `binaries` in `release.yml`: tar.gz with the binary, `LICENSE`, `README.md` and the systemd unit
  `deploy/systemd/tasmofleet.service`, plus `SHA256SUMS`), tests the amd64 archive under systemd on the runner and
  attaches them to the GitHub release. `publish` waits for it: a failed archive step publishes no image and no release.
  The archives do not contain decode-config; the [native install](../installation.md#backup-decoder-for-a-native-install)
  points users to the Dockerfile's `DECODER_*` pins, so a decoder bump needs no second edit. A change to the unit or to
  `tools/dist.sh` can be tried before the tag with a dry run (`gh workflow run release.yml --ref <branch>`), which
  publishes nothing.
- **Announce every release** in the one pinned Announcements discussion "TasmoFleet releases" (#315),
  after the tag run has published the image and the GitHub release. Edit its body (`updateDiscussion`):
  the "Current release" section shows `TasmoFleet X.Y.Z "Name"`, the image tags
  (`nofuturekid/tasmofleet:X.Y.Z`, also on ghcr.io), upgrade notes (migrations or "No database
  migration"), a short bullet list of the changes with issue refs and `thanks @handle` credits, and links
  to the GitHub release and `CHANGELOG.md`. A minor release also adds a short comment (3-5 bullets) so
  watchers get a notification; a patch release only edits the body. No per-release discussions. English,
  no fluff.
- **Smoke check after the deploy** (the live instance runs the new image): the numbers are not enough (version,
  schema version, devices online, every page answers 200), because a page can answer 200 and still be broken (0.36.0:
  layout defects on pages that answered 200 were found by the owner, not by the release: #805, #806, #811). The
  person or agent releasing also opens these pages in a browser that is signed in, at the owner's viewport (1280 px wide, as in [ui-review](ui-review.md)), takes a
  screenshot of each and looks at every one:
  - Devices list: `/devices`
  - one device: `/devices/<id>` (a device that is online)
  - Updates: `/transactions`
  - Add devices: `/devices/new`
  - System: `/settings/system`

  `make smoke-shots SMOKE_URL=<instance address with its base path> SMOKE_USER=<local admin>` takes the five
  screenshots (headless, signed in through the local sign-in form, 1280 px wide, one PNG per page in
  `~/.cache/tasmofleet-smoke/<UTC time>/`, outside the repo; `make e2e-deps` once for Playwright). The password is
  never an argument: set `TASMOFLEET_SMOKE_PASSWORD` in the environment or pipe it on stdin (the first line). It
  picks the first online device through the list's own online filter (`SMOKE_DEVICE=<id>` names one), loads pages
  only, and signs out again. It takes the screenshots; it does not judge them: look at every one. A page that
  does not answer 200, a bounce to the sign-in page or an instance without the local sign-in stops it with the
  page's name, and a refused sign-in is not retried (the sign-in limit counts attempts per account). It renders
  the account's stored theme: set it to Auto (light) before the run.

  "Looks at" means: nothing overlaps or is cut off, no empty card or raw template text, the version in the footer is
  the released one. A defect found here is a defect of the release (an issue, a fix release), not a note for later.
  The screenshots stay outside the repo ([ui-review](ui-review.md)); the report names the pages that were checked.
- **The headline feature is exercised end to end before the release counts as done.** The release's headline
  feature (the one the CHANGELOG section leads with) is run once on the live instance, from the UI to the visible
  result, not only checked for "the new card is shown" (0.36.0: an update planned for a later time had only run
  against simulated devices). When that needs a real device, use a test device and say so. A release whose headline
  feature cannot be exercised that way is not done: the release report says so, names what was not exercised, and
  the exercise is done on a test device before the next release builds on that feature.
- **The release is done** when the tag run has published the image and the GitHub release, the deploy and both
  checks above passed, and the announcement is edited. The report to the maintainer records the result: the
  version, the pages looked at, the feature that was exercised (what was done, what came out) and anything not
  exercised.
- **Close the release's milestone** (`X.Y` for a minor, the minor's milestone for its patches) once the announcement
  is out and nothing in it is open: move leftover issues to the next milestone, then
  `gh api -X PATCH repos/hellwand/tasmofleet/milestones/<number> -f state=closed` (0.38.0 stayed open for a day because
  this step was not written down). Closing it is part of "the release is done".
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
  - **The template's screenshots link the Pages pictures** (`https://hellwand.github.io/tasmofleet/assets/<name>`,
    the names in "Screenshots and the update animation"), never a tag: until 0.37.0 each release pinned them to
    `v<X.Y.Z>/docs/media/`, which no longer exists from 0.38.0 on. The 0.38.0 release switches the four
    `<Screenshot>` lines once (dashboard, devices, device-settings, policy-editor). They are the same files the README shows, so they always show the latest release. Never rename or remove
    one of those names: a published template revision links it (#423: a media refresh removed two images that a
    published revision still linked, and the listing showed broken images for days). The older revisions link
    tag-pinned `raw.githubusercontent.com/hellwand/tasmofleet/v<X.Y.Z>/docs/media/...` URLs, which tags keep
    forever, so removing that folder from `main` (#859) breaks none of them.
