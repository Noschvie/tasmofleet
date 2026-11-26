# Changelog fragments

Every change adds one file `changes/unreleased/<issue>-<slug>.md` instead of editing `CHANGELOG.md`:

```markdown
---
section: Fixed
---
What changed, for users (#N, thanks @handle)
```

- `section` is one of "Upgrade notes: action may be required" (a note the admin must act on: What's new shows it as a
  warning at the top of the release), Upgrade notes (a migration, nothing to do: Details only), Added, Changed, Fixed, Removed, Breaking, Security.
- An optional `user: <one sentence>` line (no asterisk) is what What's new shows for the change: the assembler writes
  the line as `- **<user line>** <text>`. Without it What's new takes the first clause of the text.
- The body is the line exactly as it should read in the changelog; credits stay in the text. Several fragments per
  issue are fine. The file name only orders the lines (leading number first).
- A PR that changes code and has no fragment fails the `changelog fragment` check. Nothing user-visible (dependency
  bump, CI only)? Put `changelog: skip - <reason>` on a line of the PR body.
- The release commit runs `make changelog-assemble CL_VERSION=X.Y.Z NAME="Name" DATE=YYYY-MM-DD` (NAME optional for a
  patch, DATE defaults to today), which writes the version section and deletes the fragments.
