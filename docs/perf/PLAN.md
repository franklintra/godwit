# Godwit performance architecture: plan

Status: decided plan, 2026-11-28. Branch `perf/arch` (from `origin/perf/realbench` 2ccf1a1 = `perf/audit`
362cc3f with the v3 round, plus the real-DB bench). Inputs: `arch/realbench.md` (real-DB baseline after
v3), `arch/storage.md`, `arch/concurrency.md`, `arch/app-events.md`, `store-audit.md`, `real-db-trace.md`.
No mail content appears here: counts, statement prefixes and timings only.

## 1. Verdict

Three changes together reach the target. None of them is enough alone.

1. **Move the mail bodies to their own table** (`message_bodies`). Today 2.4 GB of bodies sit inline in
   `messages`, so every scan, every flag update and every cold point read walks body overflow pages.
   After the move, the metadata table is ~170 MB and stays in the OS page cache.
2. **One writer, three readers, lanes by caller.** The UI's reads never queue behind sync, badges or
   scans. A checkpoint coordinator and short background statements keep the WAL bounded. This is the
   fix for "everything goes through one lock".
3. **Remove work the UI does not need.** Partial indexes for the starred list and counts, one RPC to
   open a mail, batched avatars, a coalesced backend tray, then a change journal so a refresh reads only
   what changed.

Central caveat: the real-DB baseline after v3 is far from the target (message open p95 3.3 s, view
switch p95 29 s, target 150 ms). The two largest holders on the real DB are the starred list scan
(150 s in 5 min) and the sync's own writes (81 s). Lanes stop them blocking the UI. The body move and
the starred indexes make them small. The plan measures each wave on the real DB copy, so a gap shows
early.

## 2. What the measurements say (real DB copy, after v3)

Baseline files on the VPS, used by every acceptance line below:

- real DB: `/home/dev/godwit-bench/results/realbench_base_realdb-20261128-100829.json`
- fixture: `/home/dev/godwit-bench/results/realbench_base_fixture-20261128-102015.json`

Window during a 10k-message sync, ms (p50 / p95):

| metric | real DB | fixture |
|---|---|---|
| message_open | 108 / 3,323 | 58 / 703 |
| view_switch | 626 / 29,179 | 118 / 1,294 |
| view.starred | 19,385 / 33,405 | 142 / 604 |
| view.saved_reply | 3,514 / 6,066 | 632 / 1,472 |
| new_message | 513 / 2,539 | 158 / 512 |
| thread_deadline | 44 / 245 | 64 / 391 |
| avatars (12) | 422 / 2,351 | 159 / 293 |
| scroll (10 mails) | 4,689 / 17,488 | 4,082 / 6,416 |
| palette_find | 12,039 (1 run) | 4,897 |

Other: sync 18 msg/s (fixture 108); connection busy 98 % of each round; open.cold.first_list 9,094 ms;
RSS 67 / 89 / 101 MB (first list / after views / after idle), peak 117 MB; idle CPU 150 ms/min.

Connection holders on the real DB, window part (5 min 11 s):

1. Starred thread list (`SELECT t.id ... FROM threads t WHERE EXISTS (SELECT 1 FROM messages m ...`):
   150.5 s in 24 runs. It walks all threads by date and loads message rows to test `\Flagged`.
2. Backend: 80.9 s. Per-message sync transaction (logged as `INSERT INTO threads`) 35.8 s at 9 ms each;
   security `UPDATE messages SET security_status = 'neutral'` 18.2 s in 8 runs (it rewrites rows that
   carry inline bodies).
3. StarredCounts: 33.1 s, 2.2 s per run (walks unread rows, loads each to test `\Flagged`).
4. Palette Find: 10.7 s. ViewContents reply-status scan: 4 s in 8 runs. Tray ListFolders: 6,240 runs.
   ContactAvatar: short, but 78 s queued in all.

The single-connection trace from the Mac (gw-trace2) adds: writes are only 1 % of DB time; reads hold the
connection; GetThread own SQL p50 0.8 ms but p95 seconds from queueing and cold body-laden rows.

## 3. Target design

### 3.1 Storage: `message_bodies` (storage.md option a)

```sql
CREATE TABLE message_bodies (
  message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  body_text TEXT,   -- first: SQL reads it; a read stops before body_html
  body_html TEXT)   -- later (wave 2): plain TEXT or a zstd BLOB
```

- `messages.body_text` and `messages.body_html` stay in the schema and are always NULL after the move.
- Every SQL read of a body uses one helper in core (`COALESCE(<message_bodies value>, <inline value>)`),
  so a row in either place reads the same. The COALESCE stays forever (old rows, stock Godwit writes, an
  unfinished move).
- Every Go read of a body value goes through `store.BodyString(v any) string`. In wave 1 it is the
  identity. In wave 2 it decompresses a zstd BLOB. So the reader sites change once.
- The two sync writers (`core/mailsync/messages.go`, `core/mailsync/local.go`) write NULL inline and
  insert into `message_bodies` with `last_insert_rowid()` in the same transaction.
- FTS: `messages_fts` does not change, so no reindex. Fork-owned triggers: `messages_au` fires only on
  subject and from_addr; `message_bodies_ai` and `message_bodies_au` keep the FTS body correct; two
  safety triggers move bodies that any other writer puts inline (tests, a future upstream path).
- A fork test fails when non-test code names `body_text` or `body_html` of `messages` outside an
  allowlist (the helper, drafts, the two writers).
- Rejected: moving more columns (option b), bodies as files, a contentless FTS now, VACUUM at open.

### 3.2 Concurrency: one writer, lanes of readers (concurrency.md)

| connection | opens as | runs |
|---|---|---|
| W | read-write | every Execute, Transaction, Durable*, PRAGMA, WITH, connection-bound function |
| I0, I1 | ReadOnly | plain SELECTs of UI calls the user waits on |
| B | ReadOnly | every other plain SELECT: badges, avatars, sync, labeler, scanner, Wit, `full`, `mcp` |

- The lane comes from the caller, never from the statement's duration. `rpc` gets a `CallContext` hook
  at the place each call's ctx is made. `rpcbind` sets Interactive for role `ui` and a method in
  `rpcbind/testdata/interactive_methods.txt`, UIBackground for other `ui` methods, Background for
  `full` and `mcp`. No mark means Background (sync and every core goroutine).
- A client may send a lane hint with a request. The server may only lower the class with it, never
  raise it. Wave 3 uses it for refresh-driven reads.
- Admission: I is FIFO and may borrow an idle B. B and W are priority queues
  (Interactive > UIBackground > Background). A waiter leaves the queue when its ctx ends; mygo
  interrupts a running statement.
- Yield: a Background statement on B or W waits while an interactive call is in flight, at most 50 ms.
- Budgets: Interactive statements target < 50 ms and are logged once per statement text above 250 ms
  (the work list for wave 3). Background statements stay at or under ~100 ms on the real DB through
  keyset chunks (`store.Chunks`).
- Checkpoints: W keeps `wal_autocheckpoint = 1000` and gets `journal_size_limit = 64 MiB`. A coordinator
  runs a PASSIVE checkpoint after commits (at most every 1 s). Above 16 MB of WAL with an incomplete
  checkpoint, it holds B for one chunk and retries. Above 256 MB it also holds Background writes and runs
  RESTART (a safety net that should not fire). TRUNCATE at 30 s idle and at close.
- Readers: `cache_size = -1024`, `mmap_size = 0`, lazy open after migrations, fall back to W if a reader
  cannot open or the journal mode is not WAL (devsync).
- Read-your-writes holds with no extra code: every write has committed when its call returns.

Why this does not repeat `perf/store-readers{,2}`: the WAL grew there because long reader statements held
snapshots back to back (fixed by chunks plus the coordinator); ThreadDeadline queued in a "long" lane
(fixed by lanes by caller; it reads one thread since perf/thread-deadline); RSS grew from per-connection
caches (fixed by 1 MiB reader caches); sync took CPU from the UI (fixed by one B connection plus yield).

### 3.3 App events and refresh (app-events.md, re-ranked after v3)

v3 already throttles count reads and made SavedViews cheap (real DB idle p50 145 ms). What still
costs, in order of value:

1. **Starred list and StarredCounts** (P4): partial indexes, the starred list drives from the index.
2. **Opening a mail** (P3): one `OpenThread` RPC before paint, side calls after paint, prefetch of the
   next and previous mail.
3. **Avatars and tray** (P7, P5): one `ContactAvatars` call; the backend tray coalesces its reloads and
   stops recounting every folder of every account on every event.
4. **Palette Find with no text** (P8): cached until new mail.
5. **Change journal and targeted refresh** (P2): core keeps a lossless journal of what changed; the UI
   pulls `Changes(since)` and re-reads only dirty accounts, rows and the open thread.
6. **Reply-status cache** (P1): only if the wave 2 real-DB run shows reply-status reads still above
   budget (gate in wave 3).

Left out: P6 (`latest.Run` already cancels the run it supersedes on perf/audit), batching several sync
messages per transaction (later, only if reader cache loss shows), mmap (measure after the move), a
VACUUM action and a contentless FTS (optional, need 2x disk or a full rebuild), option (b).

## 4. Migration strategy

### 4.1 Rule change in FORK.md: fork rewrites

The fork keeps its rule "never a numbered migration". It adds one new kind of step:

- `store.ForkRewrites` are the only non-additive fork steps. Each has a name, an `Ensure` part and a
  `Backfill` part.
- `Ensure` runs on every Open, after upstream's numbered migrations and after `applyFork`, in one
  transaction. It creates its tables with IF NOT EXISTS and compares each fork trigger's SQL in
  `sqlite_master` with the expected text; it drops and re-creates a trigger that differs (this also
  restores a trigger an upstream migration redefined). On an up-to-date DB it costs a few
  `sqlite_master` reads.
- `Backfill` moves data in the background, one short transaction per batch, at Background priority. It
  records its state in `meta` under `fork.<name>` (`moving`, `done`) and a cursor under
  `fork.<name>.cursor`.
- Each rewrite is idempotent and safe to interrupt at any point. Readers work during the backfill.
- Each rewrite has a reverse step (hidden CLI `godwit fork-undo <name> --data-dir <dir>`) so stock
  Godwit can read the data again.
- New merge checks before moving to a newer upstream release: a migration that rebuilds `messages`
  (it would cascade-delete `message_bodies`: patch it), a rebuild or reindex of `messages_fts` from
  `messages.body_text` (it would index NULL bodies: patch it to read the body table), new triggers on
  `messages` (check them against the fork triggers), new code that reads bodies from `messages` (the
  allowlist test fails in CI).
- The promise "stock 1.40.2 still opens it" becomes "stock Godwit opens it; run `fork-undo bodies`
  first to read the mail bodies".

### 4.2 Crash safety

One batch, measured on the real copy:

```sql
-- Go first reads ids: SELECT id FROM messages WHERE id > :cursor
--   AND (body_text IS NOT NULL OR body_html IS NOT NULL) ORDER BY id LIMIT :n
BEGIN IMMEDIATE;
INSERT OR IGNORE INTO message_bodies(message_id, body_text, body_html)
  SELECT id, body_text, body_html FROM messages WHERE id IN (:ids);
UPDATE messages SET body_text = NULL, body_html = NULL WHERE id IN (:ids);
UPDATE meta SET value = :max_id WHERE key = 'fork.bodies.cursor';
COMMIT;
```

- After a crash, every message has its body in exactly one place: the batch committed or it did not.
- The selection is "inline IS NOT NULL", so the cursor only skips work; it is never trusted for
  correctness. `INSERT OR IGNORE` keeps a body a writer already stored.
- Done: a header-only count of inline bodies is 0, then `fork.bodies = done`.
- The `messages_au` trigger no longer fires on `body_text`, and `message_bodies_ai` skips rows whose
  inline body is still set, so the FTS is not touched. Measured: FTS content and index SHA3-identical
  before and after; bodies SHA3-identical.
- Tested on the real copy by the storage study: `kill -9` in the middle, then 0 rows in both places,
  0 in neither, `quick_check` ok, the resume finished. Wave 1 repeats this with the app itself.
- No full-file backup copy: it would need 2x disk. Safety comes from the transactional batches.

### 4.3 Disk

- The move needs one batch plus the WAL, not 2x. Each batch's UPDATE frees overflow pages that the
  next batch's INSERT reuses. Measured: +116 MB (3 %), WAL peak 37 MB.
- Guard: before each batch, if the volume has less than 512 MB free, the backfill pauses, logs once,
  and tries again later. Reads stay correct in the meantime.
- Wave 2 compresses `body_html` (zstd level 3 in Go) and gives pages back with the existing `Reclaim`
  batches. Measured: the file goes from 3.90 GB to 2.15 GB.

### 4.4 Time on the real copy

- Measured outside the app: 106 s for 63,320 mails in batches of 200 (p50 276 ms per batch).
- In the app, a batch has a budget of ~50 ms or 2 MB of body (about 25-50 mails), then it yields.
  Expect 2-5 min of background time on the VPS volume, less on the Mac's NVMe.
- Compression (wave 2): ~93 s to compress, then `Reclaim` gives 1.75 GB back in 64-page batches.
- Every real-DB bench run starts from a fresh copy of the unmigrated DB, so every run re-measures the
  migration. Wave 1 adds a `-premove` phase to perfbench `-db` (default on): it opens the copy with
  the build, waits until every rewrite is done, and reports the time, WAL peak and file growth, before
  the measured parts start. A `migrate` part measures UI probes while the move runs.

## 5. Waves

Rules for every package:

- Branch `perf/arch-<slug>` from `perf/arch`, worktree `~/Developer/godwit/.worktrees/perf-arch-<slug>`.
- Read `BRIEF.md`, `FORK.md`, `AGENTS.md`, `docs/agents/`, this plan and the study note of the package.
- Build, vet and test only on the VPS through `vps-test`, every command starting with
  `(cd web && bun install --frozen-lockfile >/dev/null 2>&1); go tool mygo generate >/dev/null 2>&1; `.
  Focused tests only (`go test ./app/ -run '<patterns>'`, never the whole app suite).
- Real-DB runs: `scripts/perfbench/run.sh -db /srv/devdata/godwit-realdb/godwit-real.db -runs 3 -label
  <slug>-<n>` (~11 min plus the premove phase). Use `-only` while you iterate. At most 3 full real-DB
  runs per package: the VPS lock is global. Never print mail content.
- Compare with the baselines in section 2 using `run.sh compare`.
- Commits: Conventional Commits, subject <= 50 chars, no Claude attribution. Push the branch to origin.
  No PR, no upstream push.
- Write `PR.md` (not committed) with results, tests, and what could not be checked on Linux.
- Stay inside your files. Shared registry files are named per wave; the merge resolves them.

Merging: after each wave, merge the packages into `perf/arch` with `git merge --no-ff` in the order
given, regenerate `uiclient/client_gen.go`, run gofmt, `go vet` and the focused suites, run one fixture
and one real-DB bench on `perf/arch`, and record the numbers in `docs/perf/RESULTS.md`. The next wave
starts only from that merged `perf/arch`.

### Wave 1: foundations (3 packages, parallel)

Merge order: 1A, 1B, 1C. Shared registry files: `rpcbind/bind.go` (1A changes `ListenWith` only; 1C adds
entries to the `uiOnly` map only). After the merge, add `OpenThread` to
`rpcbind/testdata/interactive_methods.txt`.

#### 1A `store-lanes`: reader pool, lanes, checkpoint policy

Brief: build concurrency.md sections 5.1-5.8 as designed. W plus ReadOnly I0, I1, B; routing of plain
SELECTs only (no `last_insert_rowid()`, `changes()`, `total_changes()`; PRAGMA, WITH, RETURNING go to
W); our own admission in front of mygo's gate; priority queues on B and W; I borrows an idle B; cancel
of waiters; bounded yield (50 ms); `CallContext` hook in `rpc` and the lane hint (lower only); the
interactive method list (from concurrency.md 5.2) pinned in testdata with a test that each name is in
`ui_methods.txt`; `runCaller` sets Background; checkpoint coordinator, `journal_size_limit`, idle and
close TRUNCATE (close order: readers, checkpoint, W); per-connection settings (5.7); fallback to W when a
reader cannot open or the journal mode is not WAL; `SQLITE_BUSY` or `SQLITE_READONLY` on a reader runs
the statement once on W and logs it; log interactive statements over 250 ms once per text with the
method; count busy-handler waits in perftrace (the Mac trace shows single statements stalling 0.35,
0.65, 1.37 s); the lane in the SQL trace kind (`q@I0`, `q@B`, `x@W`). `store.Chunks` keyset helper with
tests (used in wave 2). Port readers2's perfbench WAL sampling (`wal_max_mb`, `wal_end_mb`) and per-lane
busy shares. ADR 0052 "Lanes by caller, one writer" (the old readers2 ADR number is free on this line).
Pitfalls: readers2's lessons in concurrency.md section 4; do not classify by duration; do not add marks
in core; keep `fullSync`'s PRAGMA on W; the trace's per-caller tag must still work.

Files: `core/store/store.go`, new `core/store/{lanes.go,checkpoint.go,chunks.go}` and their tests,
`rpc/server.go`, `rpc/jsonrpc.go`, `rpc/client.go` (hint), `rpcbind/bind.go` (`ListenWith` only), new
`rpcbind/lanes.go`, new `rpcbind/testdata/interactive_methods.txt`, an rpcbind test, `core/ui_state.go`
(one line), `internal/perftrace/*`, `scripts/perfbench/{wal.go (new),profile.go,sqltrace.go,window.go,
contention.go,run.go,report.go}`, `docs/adr/0052-*.md`. Stay out of: `core/store/fork.go`, every SQL
string in `core/*.go` and `core/mailsync/*`, `app/`, `cmd/godwit/`, `scripts/perfbench/{fixture.go,
open.go,realdb.go}`.

Acceptance:
- Tests: SQL routing table; a misrouted write fails with SQLITE_READONLY; read-after-write across W, I0,
  I1, B from other goroutines; SetFlags then GetThread; priority order and borrowing; a cancelled waiter
  leaves the queue; yield never exceeds 50 ms; WAL stays under 64 MB with a reader looping 50 ms
  statements and a writer committing; TRUNCATE at idle and close; `PRAGMA synchronous` never reaches a
  reader; devsync keeps every read on W. Run `go test ./core/store ./rpc ./rpcbind ./mcpserver` and
  `go test ./core -run 'Sync|Search|Draft|Outbox|Export|Thread|Flag|Reply|Deadline|View'`.
- Fixture (`PERF_CGROUP=1`, `-only contention,open,hot,mem -runs 5`): window message_open p95 <= 300 ms
  (703), new_message p95 <= 150 (512), thread_deadline p95 <= 150 (391), view_switch p95 <= 700 (1,294);
  `wal_max_mb` <= 128 and `wal_end_mb` <= 64 in every part; sync >= 97 msg/s (108); RSS after_views
  median <= 98 MB (93) and peak <= 117 MB (112 + 5); open cold/warm and contention.open within +10 %.
- Real DB (`-runs 3`): new_message p95 <= 300 ms (2,539); thread_deadline p95 <= 150 (245); avatars p95
  <= 1,000 (2,351); message_open p95 <= 1,500 (3,323; cold body rows remain until 1B); in the profile, no
  interactive statement queues behind a Background statement (I-lane holders are I or a borrowed B only);
  WAL bounds as above; RSS peak <= 122 MB; sync >= 16 msg/s.

#### 1B `bodies-table`: move bodies, fork rewrites

Brief: build storage.md sections 4(a), 5.1-5.3, 8 and 9. `ForkRewrite` machinery (`Ensure` in
`applyFork`'s tail, `Backfill` run by a core background loop started next to `startReclaim`);
`fork.bodies` Ensure (table, fork triggers, safety triggers, meta state); Backfill with a 50 ms / 2 MB
budget, the free-disk guard (512 MB), done check; the SQL helper and `store.BodyString` (identity
now); the 16 reader statements in 12 files and the 2 writers; tests that read bodies from `messages`
move to the helper. Allowlist test for body column references. Reverse step `godwit fork-undo bodies
--data-dir <dir>` (hidden; same batches backwards; drops the fork body triggers and restores upstream's
`messages_au`; a later fork open moves the bodies again). FORK.md section "Fork rewrites" (text in 4.1).
ADR 0053 "Mail bodies in their own table". perfbench: the fixture builder and big/typical message
selection read bodies through the helper; `-db` gets the `-premove` phase (default on) and a `migrate`
part (UI probes during the move, time to done, WAL peak, file growth). Measure the safety-trigger write
cost once (storage.md: `writecost.py` never ran) and keep both sync writers off that path.
Pitfalls: `last_insert_rowid()` must still be the message id for `message_locations` (it is, because
`message_id` is the rowid); `images_test` drops a column a trigger names; INSERT OR IGNORE plus
inline-IS-NOT-NULL selection is what makes resume safe; do not run `PRAGMA optimize` here (wave 3
decides it with plan tests).

Files: `core/store/fork.go` (`applyFork` tail only), new `core/store/{rewrite.go,bodies.go}` and tests;
new `core/bodysql.go` (helper) and `core/forkrewrite.go` (loop); `core/service.go` (one line: start the
loop); readers in `core/{messages.go,messagetext.go,mailcontext.go,images.go,compose.go,builtins.go,
wit.go,aiservice.go,signincodes.go,rules.go,rules_preview.go,replystatus.go (dropMassMail only)}`;
writers in `core/mailsync/{messages.go (the INSERT only),local.go}`; the body tests named in storage.md
(sync_test, wit_actions_test, gmail_merge_test, openpgp_test, images_test, store_test, aiservice,
bodytranslation, security_dkim, translators); new `cmd/godwit/forkundo.go` plus its registration in
`cmd/godwit/main.go`; `FORK.md`; `docs/adr/0053-*.md`; `scripts/perfbench/{fixture.go,open.go,realdb.go,
rewrite.go (new)}`. Stay out of: `core/store/store.go`, `rpc/`, `rpcbind/`, `app/`,
`cmd/godwit/headless.go`, `core/avatars.go`.

Acceptance:
- Equivalence tests (old layout vs new layout, same results): sync from the fake IMAP server then
  GetMessage, MessageText, stored HTML, attachments and inline images for text-only, HTML-only, both,
  empty and very large bodies; local archive import and DeleteLocal; FTS search (>= 3 runes) on subject,
  from and body; short search (< 3 runes); `RebuildSearchIndex` then the same searches; rules body
  conditions and rule preview (FTS, instr, "is"); dropMassMail and ThreadDeadline; reply and forward
  quoting; drafts, outbox, export, MCP `get_message`; Gmail merge keeps bodies; deletes cascade.
- Migration tests: partial move (stop after k batches) then reads equal, resume, done; cancelled ctx
  inside a batch; "exactly one place" after every batch; FTS `integrity-check`; trigger re-assert after
  an upstream-like redefinition; undo then stock-schema reads equal.
- Real copy, premove phase: time to done <= 5 min with the app idle; WAL peak <= 64 MB; file growth
  <= 5 % (<= 4,172 MB); `migrate` part: message_open p95 during the move <= idle p95 + 100 ms.
- Real copy, kill test: SIGKILL of the backend at ~30 s and ~60 s into the move; after restart 0 rows in
  both places, 0 in neither, `quick_check` ok, FTS integrity ok, move completes, SHA3 of
  (id, body_text, body_html) and of the FTS shadow tables equal to the pre-move values.
- Real copy after the move, hot idle p50: SavedViews <= 60 ms (145), view "À traiter" <= 60 ms (143),
  big message <= 48 ms (43), typical message <= 3 ms (2.4), starred p95 <= 300 ms (1,202). Window
  profile: backend exec down >= 30 % (80.9 s); security UPDATE total <= 2 s (18.2 s); message_open and
  view_switch p95 not worse than baseline.
- Fixture: sync >= 97 msg/s; hot and window metrics within +10 %. RSS within +3 MB. Disk within +5 %.

#### 1C `reader-open`: one RPC to open a mail, avatars, tray

Brief: app-events.md P3, P5, P7. New `OpenThread(threadID, messageID)` returns the thread, its labels,
the routed message with its body (lazy images) and ImagesAllowed, built from the existing core
functions (no new SQL). The reader paints from the list row at once, then the body; side calls
(ThreadDeadline, ViewWhy, UnsubscribeMethod, PluginCards, PluginTags, ListSkills, CachedAttachments,
SenderAvatar) start after BodyPainted. Prefetch the next and previous mail's bundle when the reader is
idle (at most 2, cancelled on navigation, dropped on any MessagesChanged for that thread). New
`ContactAvatars(addrs)` reads the role folders once and resolves each address as `ContactAvatar` does;
the list asks for visible rows after paint. The backend tray coalesces reloads to at most one per 2 s,
only for mail events and its own settings, and reads RecentSignInCodes only after MessageReceived or on
its timer. Register both methods as `ui`-only. Pitfalls: keep the reader's existing timing tests green;
do not change GetThread or GetMessage semantics; ImagesAllowed is called 3 times per open today, keep
one.

Files: new `core/openthread.go` and test, `core/avatars.go` (+ test), `app/service.go` (interface),
`app/reader.go`, `app/mail_list.go` (avatar calls), `app/contacts.go` (avatar calls), app test fakes and
new reader tests, `cmd/godwit/headless.go` (+ `headless_test.go`), `rpcbind/bind.go` (`uiOnly` entries
only), `rpcbind/testdata/{ui,full}_methods.txt`, `uiclient/client_gen.go` (generated). Stay out of:
`core/messages.go`, `core/images.go`, `core/store/`, `rpc/`, `scripts/perfbench/`.

Acceptance:
- Tests: opening a mail sends exactly one RPC before BodyPainted; side calls only after it; a prefetched
  next mail paints with no RPC before paint; `OpenThread` equals GetThread + ThreadLabels +
  GetMessageLazyImages + ImagesAllowed on the fixture for every thread of a sample; `ContactAvatars`
  equals N `ContactAvatar` calls, also with remote avatars off; the tray's unread total equals the old
  sum. `go test ./app/ -run 'Reader|Avatar|Contact|MailList|Tray'`, `go test ./cmd/godwit -run Tray`,
  `go test ./rpcbind ./core -run 'OpenThread|Avatar'`.
- Fixture: window message_open p95 <= 500 ms (703), avatars p95 <= 200 (293), scroll p50 <= 2,500
  (4,082). Real DB: avatars p95 <= 1,000 (2,351), scroll p50 <= 3,000 (4,689), message_open not worse;
  tray statements in the window profile <= 10 % of baseline (6,240 ListFolders runs). RSS within +2 MB.

### Wave 2: remove the remaining work (4 packages, parallel)

Start: `perf/arch` after wave 1. Merge order: 2A, 2B, 2C, 2D. Shared registry files: none except
`rpcbind/testdata/*` and `uiclient/client_gen.go` for 2D.

#### 2A `counts-indexes`: starred list, starred counts, palette

Brief: a new `ForkMigrations` step `starred-indexes`: `messages_flagged ON messages(thread_id, ...)
WHERE instr(flags, '\Flagged') > 0` and a covering partial index for StarredCounts with exactly the
query's terms (`instr(flags, '\Seen') = 0 AND instr(flags, '\Flagged') > 0`) and the columns
`notPurgedCond` reads. The starred thread list (unified and per account) drives from the flagged index
(`t.id IN (SELECT m.thread_id ...)`) and keeps the same order, paging and purge rules. Palette Find with
no text caches its result until new mail or 5 min. Pitfalls: SQLite uses a partial index only when the
WHERE has its terms as written; prove the plan with EXPLAIN QUERY PLAN in a test; StarredCounts' SQL
must not change (it is in `core/service.go`, owned by 2D in this wave): if the planner does not pick the
covering index, stop and report.

Files: `core/store/fork.go` (append a step), `core/threads.go`, `core/find.go`, tests
(`core/threads_starred_test.go` new, `core/find_test.go`, `core/store/fork_test.go`). Stay out of:
`core/service.go`, `core/messages.go`, `core/mailsync/`, `core/store/{bodies.go,rewrite.go}`.

Acceptance:
- Tests: plan tests for both queries; the starred lists and pages, StarredCounts and Find results equal
  the old queries on the fixture with random flags, purges and server-draft copies.
- Real DB: index build at first open <= 10 s, size <= 5 MB; hot starred p95 <= 50 ms (1,202); window
  view.starred p95 <= 150 ms (33,405); StarredCounts <= 20 ms per run during sync (2.2 s); palette_find
  (no text) <= 1,000 ms (12,039).

#### 2B `background-chunks`: short background statements

Brief: concurrency.md 5.4 and 5.9. Chunk with `store.Chunks` the sync per-folder listing
(`applyFlags` in `core/mailsync/messages.go`), the security listing (`core/mailsync/security.go`) and the
security scanner's scans and its excluded-folder UPDATE (`core/security_scanner.go`); split message
deletes into batches of <= 100 ids; the scanner's MessagesChanged event carries its thread ids (for 2D).
Use the 1A slow-statement data from the merged wave-1 real-DB run to find any other Background
statement above 100 ms and chunk it if it lives in these files. Pitfalls: a chunked listing sees rows
change between chunks; prove the result is the same for a stable snapshot and still correct with
concurrent writes (fake IMAP tests).

Files: `core/mailsync/messages.go` (listings and deletes, not the INSERT), `core/mailsync/security.go`,
`core/mailsync/{fetch.go,merge.go}` (delete batching), `core/security_scanner.go`,
`core/store/chunks.go` (fixes only), tests in `core/mailsync/` and `core/`. Stay out of:
`core/store/fork.go`, `core/threads.go`, `core/service.go`, `core/events/`.

Acceptance:
- Tests: equivalence of the chunked listings and the scanner against the old single statement on the
  fixture and with the fake IMAP server; delete batching keeps cascades; `go test ./core/mailsync` and
  `go test ./core -run 'Security|Sync|Delete|Purge'`.
- Real DB window profile: every Background statement p99 <= 100 ms, max <= 250 ms; longest W transaction
  <= 100 ms; sync >= 18 msg/s (baseline), fixture >= 97; WAL bounds of 1A hold.

#### 2C `bodies-zstd`: compress body_html, give the space back

Brief: storage.md 4(c) and 5.4. Rewrite `fork.bodies_zstd`: batches select `typeof(body_html) = 'text'`
rows of `message_bodies`, compress in Go (klauspost zstd, level 3, one shared encoder and decoder)
outside the transaction, write in a short transaction guarded by the old value; new mail stays plain
and is compressed by later passes when idle (writers do not change). `store.BodyString` decompresses
BLOBs (magic 28 B5 2F FD). Give pages back with `Reclaim` batches. `fork-undo bodies` decompresses
first. Keep `body_text` plain. Pitfalls: the allowlist test must also forbid SQL predicates on
`body_html`; concurrent body changes must not be overwritten.

Files: `core/store/{bodies.go,rewrite.go}` and tests, `core/store/store.go` (`Reclaim` only, if
needed), `cmd/godwit/forkundo.go`, `scripts/perfbench/rewrite.go`, `go.mod` (klauspost becomes direct).
Stay out of: `core/store/fork.go`, reader sites in `core/*.go`, `core/mailsync/`.

Acceptance:
- Tests: for every fixture message the body read through the helper is byte-identical before and after;
  a kill inside the compress phase leaves readable bodies; undo restores plain inline bodies
  byte-identical (SHA3) on a fixture copy.
- Real copy: file <= 2,400 MB after compress and reclaim (3,973); compress and reclaim times reported;
  WAL peak <= 64 MB; UI probes during the phase within idle p95 + 100 ms; hot big message p50 <= 48 ms,
  typical <= 3 ms. RSS within +2 MB.

#### 2D `change-journal`: exact, lossless change sets in core

Brief: app-events.md P2, core part only. Core keeps an in-memory journal fed synchronously at
`Bus.Publish`: a generation and per entry the dirty account ids, folder ids, thread ids and flags
(settings, labels, signatures, address books, sender lists, views, outbox, models). New `ui`-only
method `Changes(since) (Delta, error)` returns the union since `since`, or `Reset` after a rollover
(4,096 entries). Close the gaps so the journal is exact: account status and features writes publish
`AccountsChanged`; label writes carry thread ids; the bus marks a dropped event so a subscriber knows to
pull. The UI does not change in this package. ADR 0054 "Change journal". Pitfalls: no DB access in the
journal; publish order equals commit order; Reset must be safe for any client.

Files: `core/events/bus.go` (+ test), new `core/journal.go` (+ test), `core/service.go` (account status
and features writes), `core/credentials.go`, `core/accounts.go`, `core/messages.go` (features write
only), label write sites in `core/labels*.go`, `rpcbind/bind.go` (`uiOnly` entry),
`rpcbind/testdata/{ui,full}_methods.txt`, `uiclient/client_gen.go`, `docs/adr/0054-*.md`. Stay out of:
`core/security_scanner.go` (2B fills its ids), `core/threads.go`, `core/find.go`, `app/`.

Acceptance:
- Tests: a fuzz test over random publish sequences (including overflow) shows `Changes(since)` is a
  superset of what changed; Reset on rollover; the drop marker; a list of publish sites each carries ids
  (test); journal memory <= 1 MB at 4,096 entries; Publish overhead <= 5 µs.
- Fixture and real DB: no metric worse than +5 % (the UI does not use it yet).

### Wave 3: refresh only what changed, finish the budget (3 packages, 3C gated)

Start: `perf/arch` after wave 2. Merge order: 3B, 3A, 3C. Shared registry files: `rpcbind/testdata/*`
and `uiclient/client_gen.go` (3A only).

#### 3A `targeted-refresh`: the UI pulls change sets

Brief: app-events.md P2, UI part, plus P4's own-write counts. `App.refresh` asks `Changes(since)` and
runs only the parts whose flags are set; folder counts for dirty accounts only; the list re-reads only
dirty threads in the loaded range through a new core call `ThreadRows(view, ids)` (same SQL as
ListThreads with `t.id IN (...)`), plus a "newer first row" check; the open thread is re-read only when
dirty; full reload on Reset or a view change. Own writes patch folder, starred and view unread counts in
the UI at once; the journal corrects them. Refresh-driven reads send the lane hint UIBackground.

Files: `app/{app.go,shell.go,mail_list.go,mail_sidebar.go,reader.go,deadline.go,writeq.go,service.go}`
and their tests, new `core/threadrows.go` (+ test), `rpcbind/bind.go` (`uiOnly` entry),
`rpcbind/testdata/*`, `uiclient/client_gen.go`. Stay out of: `core/threads.go`, `core/replystatus.go`,
`core/savedview.go`, `core/store/`.

Acceptance:
- Tests: a scripted event mix where the model after each refresh (folders, counts, list rows, reader
  thread, view badges) equals a full reload; Reset path; `ThreadRows` equals the matching ListThreads
  rows; existing recount and boot-cascade tests. `go test ./app/ -run 'Refresh|Recount|Boot|MailList|
  Reader|Sidebar'`.
- Real DB window: full App.load runs <= 1 per minute; window ListThreads statements down >= 70 %;
  GetThread re-reads only for a dirty open thread.

#### 3B `reply-status-cache`: gated

Gate: start this package only if the merged wave-2 real-DB run shows view.saved_reply p95 > 150 ms during
sync, or SavedViews plus ViewContents above 20 % of the B lane's busy time. Otherwise skip it and say so
in RESULTS.md.

Brief: app-events.md P1. Per-thread reply facts cached in core, updated from the journal's dirty threads
with the existing per-thread query; full background rebuild on role, account, alias, time zone changes
and on Reset; idle rebuild every 10 min as a safety net. Readers: SavedViews, ViewContents,
ViewGroupPage, ViewWhy, MarkViewRead, PreviewView, ThreadDeadline. A debug env var runs the full
recompute after each cached answer and logs a mismatch count only.

Files: `core/replystatus.go`, `core/savedview.go`, new `core/replycache.go` and tests. Stay out of:
`app/`, `core/threads.go`, `core/threadrows.go`, `core/store/`.

Acceptance: a property test (random receive, flag, move, archive, delete, purge, undo, reply
queued/sent/failed, thread merge, role change, alias added, day and 30-day edges) where cached results
equal a full recompute after each step; existing replystatus and savedview tests on both paths; real DB
window view.saved_reply p95 <= 150 ms; SavedViews <= 10 ms; RSS within +5 MB.

#### 3C `interactive-budget`: the last slow interactive statements

Brief: take the 1A slow-statement log and the per-method SQL from the merged wave-2 real-DB run. Fix each
interactive statement with p95 above 100 ms (index as a new fork step, query shape, or chunked
prefetch). Decide `PRAGMA optimize`: run it on a real copy, diff EXPLAIN QUERY PLAN of every hot
statement before and after, and turn it on (once after the body move and at close) only if no plan gets
worse; pin the hot plans in a test either way. Measure `mmap_size` 64-128 MB on readers with the
anon/file RSS split and keep 0 unless RSS stays within budget and p95 improves.

Files: query files the log names, except `core/replystatus.go`, `core/savedview.go` (3B),
`core/threadrows.go` and `app/` (3A); `core/store/fork.go` (append a step); `core/store/store.go`
(optimize, mmap); new plan-pin test. Coordinate through the merge order (3C last).

Acceptance: the final acceptance in section 6.

## 6. Final acceptance (perf/arch after wave 3)

Real DB copy, window during the 10k sync, `-runs 3`:

| metric | baseline | target |
|---|---|---|
| message_open p95 | 3,323 ms | < 150 ms |
| view_switch p95 | 29,179 ms | < 150 ms |
| new_message p95 | 2,539 ms | < 150 ms |
| thread_deadline p95 | 245 ms | < 150 ms |
| WAL peak / end | not measured | <= 128 MB / <= 64 MB |
| RSS after_views / peak | 89 / 117 MB | <= 94 / <= 122 MB |
| DB file | 3,973 MB | <= 2,400 MB |
| sync during window | 18 msg/s | >= 18 msg/s |
| idle CPU | 150 ms/min | <= 150 ms/min |
| open.cold.first_list | 9,094 ms | not worse |

Fixture: no window, hot, open or mem metric worse than baseline +10 %; sync >= 97 msg/s.

Correctness: the focused suites in FORK.md "Checks" plus `go test ./core -run
'Sync|Search|Draft|Outbox|Export|Thread|Flag|Reply|Deadline|View|Rule|Body|Rewrite|Journal'`,
`go test ./mcpserver ./rpcbind ./core/store ./core/mailsync`, and the equivalence tests of each package.

After that: open the PR `perf/arch` into `franklin/integration`, let `fork-macos.yml` build the app, and
install it on the Mac by the steps in FORK.md. The first launch moves the bodies in the background
(a few minutes); the app stays usable during it.

## 7. Risks

| risk | answer |
|---|---|
| New upstream code reads bodies from `messages` and shows empty bodies | allowlist test fails in CI; FORK.md merge checks |
| An upstream migration rebuilds `messages` or `messages_fts` | FORK.md rule: patch it; Ensure re-asserts the fork triggers on every open |
| Downgrade to stock Godwit shows empty bodies | `godwit fork-undo bodies`; FORK.md says it |
| WAL grows with readers (readers2 again) | chunks, coordinator, hard limits; WAL metrics in every bench part |
| An interactive statement is slow and holds one I connection | two I connections plus borrowing B; the 250 ms log feeds 3C |
| B saturates and badges or sync lag | 2A, 2B and 1C remove most B work; per-lane busy shares in the bench |
| RSS grows (readers, caches, journal) | 1 MiB reader caches, budgets per package, mem part in every run |
| A missed invalidation shows a stale row or badge (3A, 3B) | journal fed at publish, Reset, model-equals-full-reload tests, shadow check |
| Real-DB runs queue on the global VPS lock | at most 3 full runs per package, `-only` while iterating |
| The VPS container (3 GB) is smaller than the DB | cold numbers are pessimistic; the Mac has more cache; compare runs only on the same setup |
| The migration on the Mac meets a full disk | 512 MB guard, pause and retry, reads correct during the pause |
