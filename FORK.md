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
   (automation), 0050 OpenPGP, 0051 plain-text drafts, 0052 lanes by caller (perf/arch) and 0053 mail
   bodies in their own table.
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
  migrations on it, and stock 1.40.2 still opens it. Stock Godwit ignores the extra columns and tables. It
  reads moved mail bodies only after `godwit fork-undo bodies` (see "Fork rewrites").
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
- Data changes (backfills) cannot be fork steps. If one is needed, make it a fork rewrite (below), or make
  it idempotent and run it from `applyFork` with its own guard.

Tests: `core/store/fork_test.go`.

## Fork rewrites

Fork rewrites (`store.ForkRewrites`, `core/store/rewrite.go`) are the only non-additive fork steps. The
rule "never a numbered fork migration" still holds: a rewrite has a name, not a number.

- `Ensure` runs on every `Open`, after upstream's numbered migrations and the `ForkMigrations`, in one
  transaction. It creates its tables with their full shape and compares the SQL of each of its triggers in
  `sqlite_master` with its own. It drops and re-creates a trigger that differs or is missing, so a trigger
  that an upstream migration redefined comes back. On a database that is up to date it only reads
  `sqlite_master` and `meta`.
- `Backfill` moves data in the background (`core/forkrewrite.go`, started next to `startReclaim`), one
  short transaction per batch (about 50 ms or 2 MB). It pauses while the volume has less than 512 MB free.
  It records its state in `meta` under `fork.<name>` (`moving`, then `done`) and its position under
  `fork.<name>.cursor`. Each batch leaves the database correct: a crash or a quit between two batches loses
  nothing, and the next start goes on. Readers work during the backfill.
- `Undo` is the reverse step: `godwit fork-undo <name> [--data-dir <dir>]` (hidden; quit Godwit first).
  It brings the data back to stock Godwit's layout in the same kind of batches. The fork's next start
  does the rewrite again.

`fork.bodies` (ADR 0053): mail bodies live in `message_bodies`. `messages.body_text` and
`messages.body_html` stay in the schema and are NULL once a message's body has moved. Read a body only with
`bodyText`/`bodyHTML` (`core/bodysql.go`) or `store.BodyTextSQL`/`BodyHTMLSQL`, and its value with
`store.BodyString`. `TestBodiesReadThroughHelper` fails on any other read of those columns. The fork owns
the triggers `messages_au` (it no longer fires on `body_text`), `message_bodies_ai`, `message_bodies_au`,
`messages_body_move` and `messages_body_move_au` (the last two move a body that some other writer stores
inline). The sync writers (`core/mailsync/messages.go`, `local.go`) store the body in `message_bodies`
directly.

Before moving to a newer upstream release, also read its migrations and code for:

- a migration that rebuilds `messages` (`CREATE TABLE messages_new …; DROP TABLE messages`): the drop
  cascades to `message_bodies`. Patch it to keep the bodies.
- a rebuild or reindex of `messages_fts` from `messages.body_text`: it would index NULL bodies. Patch it to
  read `store.BodyTextSQL`.
- new triggers on `messages`, or a new definition of `messages_au`: check them against the fork's
  (`core/store/bodies.go`). `Ensure` puts the fork's `messages_au` back after the migration.
- new code that reads `body_text` or `body_html` from `messages`: `TestBodiesReadThroughHelper` names it.

Stock Godwit still opens a fork database, and its writes stay correct (the safety triggers move what it
stores inline), but it shows empty bodies for moved mail. To go back to stock Godwit, quit the fork and run
`godwit fork-undo bodies` first.

## The macOS build

`.github/workflows/fork-macos.yml` runs on a push to `franklin/integration` (and by hand once it is on
the default branch). It runs the steps of `ci.yml`'s `package` job for `darwin/arm64` without a DMG,
signing or notarization, on `macos-15-intel` with the newest Xcode (it cross-compiles; the hosted arm64
pool kept acquiring no runner), and keeps `Godwit.app` as the artifact `Godwit-macOS-arm64-<sha>` for 14 days:

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
