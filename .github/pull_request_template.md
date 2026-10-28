## What and why

<!-- What does this change, and what problem does it solve? Link the issue: Fixes #123 -->

## Checklist

- [ ] Tests added or updated (bug fixes include a test that fails without the fix)
- [ ] `go test -race ./...` passes with `DATABASE_URL` set
- [ ] No new dependencies in the core module; `signature` is still stdlib-only
- [ ] No edits to released migrations (new schema changes go in a new file)
- [ ] Exported identifiers have doc comments
- [ ] `CHANGELOG.md` updated under `## [Unreleased]`
