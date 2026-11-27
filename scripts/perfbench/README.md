# perfbench

perfbench measures how fast Godwit opens and what it costs to keep open. It uses a synthetic mailbox shaped like Franklin's: 8 IMAP accounts, about 45,000 messages in about 34,000 conversations, his three saved views, and a full FTS index. Every number is a median of repeated runs, with min, max and p95, in JSON, so two runs compare metric by metric.

It runs on Linux only, because it reads `/proc` and cgroups. For this fork, that is the personal VPS, through `.worktrees/vps-test`. It cannot measure the macOS app (see "Limits").

## Run it

From the Mac, with the worktree to measure:

```sh
# Full run, memory-pressure parts included (about 10 minutes; the fixture first, once)
PERF_CGROUP=1 ~/Developer/godwit/.worktrees/vps-test <worktree> \
  "scripts/perfbench/run.sh -commit $(git -C ~/Developer/godwit/.worktrees/<worktree> rev-parse --short HEAD)"

# Quick run (3 runs, 15 s idle, 4,000-message sync)
PERF_CGROUP=1 ~/Developer/godwit/.worktrees/vps-test <worktree> 'scripts/perfbench/run.sh -quick'

# Some parts only: open, mem, hot, idle, pressure, contention, window, sync, web (contention includes window)
PERF_CGROUP=1 ~/Developer/godwit/.worktrees/vps-test <worktree> 'scripts/perfbench/run.sh -only open,pressure -runs 7'

# The web pages and bundle sizes (builds web/dist, about 1 more minute)
~/Developer/godwit/.worktrees/vps-test <worktree> 'scripts/perfbench/run.sh --web -only web'

# List, show and compare results (B against A)
~/Developer/godwit/.worktrees/vps-test <worktree> 'scripts/perfbench/run.sh list'
~/Developer/godwit/.worktrees/vps-test <worktree> 'scripts/perfbench/run.sh show ~/godwit-bench/results/A.json'
~/Developer/godwit/.worktrees/vps-test <worktree> 'scripts/perfbench/run.sh compare ~/godwit-bench/results/A.json ~/godwit-bench/results/B.json'
```

`run.sh` builds `godwit` and `perfbench` for Linux into `~/godwit-bench/bin/<worktree>/`, makes the fixture if it is missing, runs, and prints a table. The JSON goes to `~/godwit-bench/results/<worktree>-<time>.json`. Other flags: `-mem 350M` (the pressured limit), `-pressure auto|on|off`, `-sync-messages N`, `-idle 60s`, `-label`, `-out`.

`PERF_CGROUP=1` makes `vps-test` give the job a delegated cgroup (`Delegate=yes`): the job runs in its `main/` child, and an empty `bench/` child that dev owns is in `PERFBENCH_CGROUP`. perfbench puts the pressured backend in a child of `bench/` with `memory.max` (page cache included) and `memory.swap.max 0`. Without `PERF_CGROUP`, perfbench skips the pressured parts and logs it, and `vps-test` runs exactly as before.

`compare` marks a change `better` or `worse` only when the median of one run is outside the other's min..max. Otherwise it marks `~` (noise). The VPS is shared and its noise moves with the load, so measure the baseline and your branch one after the other, on the same day.

## The fixture

The fixture is made once, outside the build copies, in `~/godwit-bench/fixture-v<len(store.Migrations)>-g<generator version>/`: `data/` is the data directory, `keyring.bin` and `keyring.key` are the file keyring that `GODWIT_KEYRING_FILE` reads, and `meta.json` says what is in it (table sizes from dbstat, views, the hot paths' targets). Each part that writes starts from a copy in `~/godwit-bench/scratch-<pid>/`, with a fresh WAL and with its pages dropped from the page cache. The copy is deleted at the end. Keep one fixture only: when the schema or the generator changes, delete the old one (`rm -rf ~/godwit-bench/fixture-v<old>*`).

The generator uses Godwit's own code from end to end. An in-process IMAP server on loopback (go-imap's memory server) holds each account's mail as it is generated, `core.Service.AddAccount` adds the account, and `SyncNow` syncs it. So the rows, the bodies stored inline in `messages`, the threads, the locations, the FTS index and the attachments metadata are what a sync writes. Then the generator labels about one message in ten by hand (`SetLabel`), saves 12 drafts and Franklin's saved views, points every account at `127.0.0.1:1` (connection refused, so no run reaches a server or the internet), checkpoints the WAL, and opens the result once with the build to measure. The network privacy switches start off (`GODWIT_OFFLINE_DEFAULTS=1`).

| Account | Messages | Extra folders |
|---|---|---|
| 1 (Gmail-sized) | 15,000 | 8 |
| 2 | 9,000 | 5 |
| 3 | 6,500 | 4 |
| 4 | 5,000 | 3 |
| 5 | 4,000 | 3 |
| 6 | 3,000 | 2 |
| 7 | 1,800 | 1 |
| 8 | 700 | 0 |

Each account has INBOX, Sent, Archive, Trash, Junk and Drafts. The mail is 40% newsletters (30 to 110 KB of table HTML with a text part and List-Unsubscribe), 47% notifications (10 to 40 KB of HTML, a quarter with a PDF, ICS, XLSX or JPEG attachment) and 13% personal threads (1 to 14 messages, a few of 40 to 80, replies in Sent, quoted history). That mix gives about 0.75 conversations per message, as on the Mac. The last years hold most of the mail, 15% of recent INBOX mail and 40% of newsletters are unread, and 2 to 3% are starred.

The saved views are Franklin's documents of 2026-11-26, with his folder ids mapped onto the fixture's:

1. "Non lus": all of folder is inbox, read is unread, label not_contains otp, notice, newsletter.
2. "Pro": any of 5 folders across 4 accounts, with a group "Non lus" (read is unread).
3. "Perso": any of 3 folders across 2 accounts, with the same group.

The seeded view 待处理 stays too. `-scale 0.05` makes a small fixture for work on the harness itself.

## What a run measures

Every backend is the real `godwit --headless --data-dir <copy> --ui-started`, started as the interface starts it, with `GODWIT_STARTUP_TRACE`, `GODWIT_SQL_TRACE` and `GODEBUG=gctrace=1`. Godwit's own interface (`app.App`) runs in mygo's headless `ui.Tester` over a real `ui` connection, so it sends exactly the RPCs that it sends at launch.

**Opening** (`open.*`; `-runs` pairs, cold then warm). The clock starts at the backend's exec and stops at `App.MailReady()`, the first thread list. A cold run first drops the page cache of the database copy and the binary (`posix_fadvise(DONTNEED)`, no root), as after a reboot. The warm run follows it.

- `first_list_ms`: exec to the first thread list. This is the opening time.
- `process_ready_ms`: exec to the Local API accepting a `ui` connection.
- `ui_boot_ms`: accept to the first list.
- `boot_rpcs`, `boot_rpc_kb`, `rpc_saved_views_ms`, `rpc_list_accounts_ms`: the interface's boot calls.
- `boot_sql_wait_ms`, `boot_sql_exec_ms`: the interface's SQL statements, split into time queued for the SQLite connection and time running.

The report's `breakdown` cuts the opening at the backend's trace marks (`main`, lock, each step of `store.Open` and `core.New`, `listen`), then at the socket's accept and at the first list. `boot_rpc` lists the last warm run's calls with start, duration and size.

**Memory pressure** (`pressured.*`, needs `PERF_CGROUP=1`). The same openings with the backend in a cgroup of `-mem` (350 MiB), page cache included. A 2.8 GB database cannot stay cached there, as on a Mac whose memory and swap are full. After the openings: `pressured.hot.*`, the calls that were slow on the Mac (ListAccounts, the Gmail-sized account's folder counts, SavedViews, each view's contents, the unified inbox), and the cgroup's peak, page cache and anonymous memory.

**Contention** (`contention.*`, and `pressured.contention.*` with `PERF_CGROUP=1`). A new account's initial sync writes 10,000 messages from loopback IMAP. The tray's loads (its `readSnapshot`) repeat every second on their own connection, a probe calls ListAccounts every 250 ms, and the interface opens 5 times (quick 3) against the busy backend.

- `open_ms`, `boot_list_accounts_ms`, `boot_saved_views_ms`, `probe_list_accounts_ms`: the latencies.
- `probe_list_accounts_sql_wait_ms` and `_sql_exec_ms`, `boot_sql_wait_ms` and `_exec_ms`, `ui_query_wait_ms` and `_exec_ms` (per statement): queue wait apart from execution.
- `boot_conn_busy_background_pct`: the share of a boot during which the connection runs the backend's own work.
- The notes list the 5 longest background statements during the boots, and the connection time per client.

**Window during sync** (`contention.window.*`, and `pressured.contention.window.*`; runs with `contention`, or alone with `-only window`). What Franklin feels with his window open while a sync writes: a fresh copy, a new account's initial sync from loopback IMAP, the App attached (it reloads on every mail event, SavedViews recount included), the tray's loads every second. Before the sync, the copy gets what the fixture lacks: a signature with two images per account, and with `-window-views franklin` (default) only the reply-based view (his "À traiter"; `fixture` keeps all four). Then `-window-rounds` rounds (8, quick 4; they stop when the sync ends), each on a user connection, with 0.5 s pauses:

- view switch: ListThreads of the unified inbox, the largest INBOX and starred, ViewContents of the reply-based view (`view.*_ms`, together `view_switch_ms`);
- new message: NewDraft then the compose page's Load, as `webui.ComposePage` calls it (`new_message_ms`, split in `new_draft_ms` and `compose_load_ms`);
- avatars: ContactAvatar for 12 list senders at once (`avatars_ms`, `avatar_call_ms`);
- message open: the next 10 mails of the unified inbox, one after the other, each shown 300 ms, with the reader's calls in its order and concurrency (`openMail` in `window.go`): `open.thread_ms` (GetThread, ThreadLabels), `message_open_ms` (until the newest body is read, when the reader switches), `open.all_ms` (until the mail's last call answered: ThreadDeadline, cards, tags, SetFlags, skills, avatar, unsubscribe, OpenPGP), `open.thread_deadline_ms`, and `scroll_ms` for the 10;
- first round only: the palette's Find with nothing typed (`palette_find_ms`; its contacts scan is the one a recipient completion runs).

Per probe group, `*_sql_wait_ms` and `*_sql_exec_ms` split its statements' time into queued and running. Per round, `conn_busy_pct` (the connection busy at all), `conn_busy_background_pct` (the backend's own work), `conn_busy_window_pct` and `conn_busy_user_pct`. `window_saved_views_per_min` and `window_list_threads_per_min` count the App's reloads (on the Mac, SavedViews ran about every 3.5 s during sync). The notes give the connection time per client, the window's calls, and the 12 statements that held the connection longest.

**Memory** (`mem.*`, `gc.*`). The backend's RSS split by mapping, from `/proc/<pid>/smaps`: `go_heap_rss` is the Go arenas, `c_heap` is other anonymous memory (malloc: SQLite's page cache and allocations, libc), and `file` is file-backed pages. `go_live` and `go_total` come from `runtime/metrics`, which the backend writes on SIGUSR1 when the startup trace is on. Samples are taken at the first list, after one round of the hot paths, and after the idle window. Peaks are VmHWM. `gc.*` come from gctrace.

**Idle** (`idle.*`). For 60 s (quick 15 s) in five slices, with the interface attached and idle: CPU ms per minute and context switches per minute. The accounts are offline (connection refused), so this includes the workers' retry timers, not IMAP IDLE.

**Hot paths** (`hot.*`; 20 rounds, quick 8; after a warm-up round; the calls interleaved). Each is the call that the interface makes, with its arguments and page size (50, search 200), on its own connection: the unified inbox, the Gmail-sized account's INBOX, Archive and Sent, starred, the top label, the third page, the sidebar's counts, the longest thread, the largest and a typical message, five searches, ListAccounts, SavedViews and each saved view. Read the p95 as well as the median.

**Sync** (`sync.*`; 3 runs, quick 1). A fresh backend on an empty data directory adds an account on the loopback IMAP INBOX (10,000 messages, quick 4,000). The clock runs from `AddAccount` until every message is stored: messages and MB per second, peak RSS, CPU seconds, the largest GC heap, database size.

**Web** (`web.*`, `bundle.*`, with `--web`). `web/webboot.ts` (Bun, no npm dependency) serves the build's `web/dist` and loads `reader.html` and `wit.html` in Playwright's Chromium headless shell over the DevTools protocol, with the HTTP cache off. A stand-in for MyGo's bridge (`globalThis.mygo`) answers the pages' calls, and the reader gets the fixture's largest body. Per page: DOMContentLoaded, load, first contentful paint, ready, V8 script time, task time, layout and style, JS heap, DOM nodes. `bundle.*` gives what each page loads before it runs, raw and gzip, and what only dynamic imports load.

## Traces in the godwit binary

These traces stay in the binary that ships. Each costs one check of a package variable unless its environment variable is set.

- `GODWIT_STARTUP_TRACE=<file>`: one line `<step> <unix ns>` per mark (`internal/perftrace`). It also makes SIGUSR1 write the runtime's memory metrics to `<file>.mem` (Unix).
- `GODWIT_SQL_TRACE=<file>`: one line per database call (start, end, kind, the `client/method#n` of the Local API call it serves, the statement's start), from `core/store.DB`. All calls share one SQLite connection, so perfbench gets each call's queue wait from the order of the ends.

## Limits

- The macOS interface process is not measured: AppKit, the window's first Metal frame, WKWebView's own start (the Wit page loads at launch), the WebContent process. Chromium on Linux stands in for WebKit only to compare builds of the web pages.
- Linux on a shared 4-core VPS with a LUKS disk, not APFS on Apple Silicon. The pressured mode reproduces the missing page cache, not macOS memory compression or swap. Absolute numbers differ from the Mac; deltas between two builds carry over better.
- All accounts are IMAP. Franklin's Gmail accounts use the Gmail API and his Exchange account uses DavMail. The largest account has Gmail's size, not Gmail's labels-as-folders.

## Files

- `main.go`: the commands `fixture`, `run`, `compare`, `show`.
- `fixture.go`, `mailgen.go`, `mailserver.go`, `keyring.go`: the fixture.
- `run.go`, `open.go`, `backend.go`, `boot.go`, `platform.go`, `rpctrace.go`, `proc.go`: openings, memory, idle.
- `cgroup.go`, `contention.go`, `window.go`, `sqltrace.go`: memory pressure, contention, the window during sync, the SQL trace's analysis.
- `hot.go`, `syncbench.go`, `web.go`, `web/webboot.ts`: the other parts.
- `report.go`: the JSON, the table and the comparison.
- `run.sh`: build and run on the VPS.
