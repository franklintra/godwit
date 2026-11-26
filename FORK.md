# franklintra/godwit fork

This branch, `franklin/integration`, is upstream Godwit (`godwit-email/godwit`) plus the fork's feature
branches, merged for one local build. Upstream PRs come from the feature branches, never from here.

## Branches

- Base: upstream v1.40.2 (`a6040c1`), the fork's `main`.
- Merged, in this order: `feat/headers`, `feat/privacy`, `feat/server-drafts`, `feat/identities`,
  `feat/automation`, `feat/openpgp`, `feat/plaintext`.

Each feature branch stays based on upstream, so it can become an upstream PR on its own. To add one:

1. Merge it with `git merge --no-ff feat/<name>`.
2. If it appended a numbered migration to `core/store/schema.go`, move those statements to a new entry in
   `store.ForkMigrations` (`core/store/fork.go`) and remove them from `schema.go`. This step is required,
   even when git merges `schema.go` without a conflict. `TestMigrationsAreUpstreams` fails until it is done.
   The branch's own migration test then uses `store.ForkMigrations` instead of cutting `store.Migrations`
   (see `core/store/draft_headers_migration_test.go`).
3. Renumber its ADR if the number is taken, and fix the references (`git grep 'ADR 00xx'`). The fork
   uses 0045 headers, 0046 network privacy, 0047 server drafts, 0048 identities, 0049 open from outside
   (automation), 0050 OpenPGP and 0051 plain-text drafts.
4. Regenerate `uiclient/client_gen.go` (`go generate ./uiclient`) and `mcpserver/views/draft.html`
   (`cd web && bun run views`) if the merge changed their inputs, then run the checks in "Checks".

## Fork migrations

Upstream records its schema version in `PRAGMA user_version`, which is the number of entries in
`store.Migrations`. A fork migration appended there would take the number of upstream's next migration.
A database that recorded that number would then skip upstream's own migration, and stock Godwit would
refuse to open the database ("schema is newer").

The fork therefore never adds a numbered migration:

- `store.Migrations` is upstream's list only. `store.ForkBase` pins its length. Update it when the fork
  moves to a newer upstream release.
- `store.ForkMigrations` holds the fork's schema as named steps. A step holds only additive statements:
  `ALTER TABLE … ADD COLUMN`, `CREATE TABLE` and `CREATE [UNIQUE] INDEX` (`CheckForkMigrations`).
- After the numbered migrations, every `Open` runs each fork statement whose column, table or index is
  missing. No step is recorded. A second open changes nothing. A column lost to a rollback, a downgrade
  or an upstream table rebuild comes back (empty).
- A fork database keeps upstream's `user_version`. The next upstream release runs all of its own
  migrations on it, and stock 1.40.2 still opens it. Stock Godwit ignores the extra columns and tables.
- If upstream adopts a fork feature with the same names, its migration skips each statement that creates
  an object a fork step declares and the database already has (`skipForkObjects`). It runs its other
  statements as written. Statements for objects that no fork step declares are never skipped.

Rules:

- Never remove a fork step, even after upstream adopts its object. The step is what lets upstream's
  migration skip the object.
- When upstream adopts a feature with a different column type, default or table shape, write the change
  as a new fork step or a fork-only fix in `schema.go`. Do not rely on the skip.
- Before moving to a newer upstream release, read its new migrations. A migration that rebuilds a table
  that holds fork columns (`CREATE TABLE x_new …; INSERT … SELECT …; DROP TABLE x; ALTER TABLE x_new
  RENAME TO x`) drops their data. An `INSERT INTO x_new SELECT * FROM x` fails on the extra columns. Patch
  such a migration in the fork to carry the fork columns over.
- Data changes (backfills) cannot be fork steps. If one is needed, make it idempotent and run it from
  `applyFork` with its own guard.

Tests: `core/store/fork_test.go`.

## The macOS build

`.github/workflows/fork-macos.yml` runs on a push to `franklin/integration` (and by hand once it is on
the default branch). It runs the steps of `ci.yml`'s `package` job for `darwin/arm64` without a DMG,
signing or notarization, and keeps `Godwit.app` as the artifact `Godwit-macOS-arm64-<sha>` for 14 days:

```sh
gh run list -R franklintra/godwit -w fork-macos.yml
gh run download <run-id> -R franklintra/godwit -D <dir>
ditto -x -k <dir>/Godwit-macOS-arm64.zip <dir>
```

The app is signed ad hoc, so macOS treats it as a different app from the release: Keychain items that
the release made can ask for permission again, and privacy permissions (notifications, contacts, full
disk access) can need a new grant. The release's updater still runs in this build. Turn off
**Settings → Network privacy → Check for updates automatically** (or set `"automaticChecks": false` in
`~/Library/Application Support/Godwit/updater.json` before the first launch), or the next upstream
release replaces the fork build.

## Checks

Heavy work runs on the VPS (`.worktrees/vps-test integration '<command>'`), not on the Mac:

```sh
go tool mygo generate && go generate ./uiclient
test -z "$(gofmt -l $(git ls-files '*.go'))"
go tool mygo i18n check
go build ./... && go vet ./core/... ./rpcbind/... ./mcpserver/... ./webui/... ./app/...
go test ./core/store ./core/mail ./rpcbind ./mcpserver ./webui ./uiclient ./internal/gpg/... ./backend/jmap ./cmd/godwit
go test ./core -run 'Draft|Header|Server|Network|Setting|Catalog|Registry|Avatar|BIMI|Migration|Identit|OpenPGP|Open|MessageID|Mailto|Send'
cd web && bun run typecheck && bunx vitest run src/compose src/mcp
```
