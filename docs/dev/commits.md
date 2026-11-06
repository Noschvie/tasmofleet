# Commits and migrations

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).


- Commits are signed; `main` requires signed commits (branch protection). **Never** disable signing
  (`--no-gpg-sign`, `-c commit.gpgsign=false`). If signing fails, stop and report it.
- Write the message with a quoted heredoc, never `-m "..."` with backticks or `$(...)`:

  ```sh
  git commit -F - <<'EOF'
  fwselect: refuse safeboot images in Guard

  Body in English: what and why.

  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  EOF
  ```

- Trailer `Co-Authored-By: Claude <Model> <noreply@anthropic.com>` on AI-authored commits, naming the
  model that actually wrote the change (e.g. `Claude Sonnet 5.5` for a Sonnet subagent).
  No session links or other URLs to chat sessions.
- Commit only your own files. `make check` must pass before each commit.
- Repository: `https://github.com/hellwand/tasmofleet` (HTTPS remotes).
- No secrets in the repository, not even in tests: use obviously fake values.
- **Migrations are append-only since tag v0.0.1.** Never edit a merged file in
  `internal/store/migrations/`; add `NNNN_name.sql`. `make migration-append-only` (part of `make check`, job
  `migrations` in `pr.yml`, tag job `guards`) fails when a file that exists at the newest `vX.Y.Z` tag in the branch's
  history is modified, renamed or deleted, uncommitted edits included. It needs the tags and fails without them (CI:
  `fetch-depth: 0`; locally `git fetch --tags`). (0001 was amended before the first tag;
  dev/test databases created before that must be recreated.)
