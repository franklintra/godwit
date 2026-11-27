---
status: proposed
---

# The interface's reads on read-only connections beside the writer

`store.Open` opened one SQLite connection, and the interface, the sync workers, the scanner, the labeler, rules and `godwit mcp` all shared it. mygo's `sqlite.DB` runs one operation at a time behind a one-slot gate, so every call waited for every call queued before it. On a 2.8 GB mailbox during a sync the connection was busy 53 of 61 s: a sub-millisecond `ListAccounts` waited up to 23 s, a new message 2 to 3 s, and opening a message while scrolling waited behind the sync's batches, the window's saved-view recounts, the avatars' sender scans and the previous message's `ThreadDeadline`. The database already runs in WAL mode, where readers on other connections run beside the writer.

## Decision

- **`store.DB` keeps one writer and adds two read-only connections for the interface** (`sqlite.OpenOptions{ReadOnly: true}`, 5 s busy timeout, `PRAGMA cache_size = -1024`), opened on the first read that goes to them, after `Open` has migrated. Callers do not change: routing happens inside `store.DB.Query`.
- **Only the interface's plain SELECTs go to a reader.** A context marked with `store.WithUIRead` (the core marks every `ui`-role connection in `core.WithUISession`, so every Service method the window and the tray call carries it) sends a statement to a reader when its first keyword, after blanks and comments, is `SELECT` and it calls none of `last_insert_rowid()`, `changes()` and `total_changes()`, which read the writer's connection state. Everything else stays on the writer: PRAGMAs, `WITH` (a write can follow it), `INSERT … RETURNING`, anything not recognized, `Execute`, `Transaction`, `DurableTransaction`, `DurableExecute`, and every read without the mark (sync, rules, labeler, scanner, `full` and `mcp` clients). The read-only open is the safety net: a write that reaches a reader fails with `SQLITE_READONLY`.
- **The backend's own work keeps its one connection.** An earlier version sent background reads to a shared reader too. The sync then ran 8 times faster and took the CPU from the interface: an opening during a sync got slower (14 s to 28 s). Kept on the writer, background work runs exactly as before, minus the interface's reads it used to queue behind.
- **Reads see every committed write.** mygo has no interactive transaction: `Transaction` is one batch under `BEGIN IMMEDIATE` inside one gate hold, and `Query` refuses `BEGIN`. So a write has committed when its call returns, and a read started after it, on any connection, sees it. A read that runs while another goroutine's transaction is open sees the state before that transaction, which the single connection also allowed, since nothing ordered the two calls.
- **Two lanes keep quick reads from queueing behind the interface's own scans.** Reader 0 is the quick lane, reader 1 the long lane. A long read takes the long lane only. A quick read takes the quick lane, else the long lane when it is free, else whichever frees first, so it waits at most for one statement. A read is long when its context is marked with `store.WithLongRead` or when its statement's last run took 250 ms or more. The core marks the scans it knows: reply statuses (`ThreadDeadline`, every saved view), a saved view's evaluation and placement, the contacts (`Find`'s recipients, the contacts list) and the sender scans of `ContactAvatar`. The marks matter at a cold start: a new process has no run times, and in an earlier version, where a statement not run yet could take any lane, the boot's saved-view scans held every reader on a cold page cache and a 2 ms `ListAccounts` waited 4.2 s. The run-time table catches scans nobody marked after their first run; it is keyed by statement text and bounded (256 entries, cleared when full).
- **The SQL trace names the connection.** `GODWIT_SQL_TRACE` lines keep their five fields; the kind is `q`, `x` or `t<n>` on the writer and `q@r<i>` on reader `i`. perfbench computes queue wait and run time per connection.

## Considered options

- **A priority gate on the one connection** (interface calls first). It reorders the queue, but a call still waits for the statement that holds the connection, and one saved-views or sync statement runs for seconds.
- **Background reads on a reader as well.** It frees the writer for the sync, but the faster sync competes with the interface for CPU (measured above), and the interface's reads then wait for that reader's scans too.
- **Three readers, the third shared with the backend.** About 7 MB of RSS per reader (measured); two cover the interface once the backend keeps the writer.
- **Lanes by run time only, no marks.** It needs no caller to change, but every statement is new in a fresh process, which is when the window and its scans start together.
- **Read-write connections with `PRAGMA query_only`.** A misrouted write would still be refused, but a read-write connection can also run checkpoints and create the WAL. Read-only is the narrower promise.
- **`PRAGMA mmap_size` on the readers.** Not measured; mapped pages count as file RSS.

## Consequences

- No new egress, no new `core.Service` method, no schema change.
- The interface's writes (`SetFlags`, `NewDraft`) still wait for the writer, behind the backend's work, as before.
- A long reader statement delays the end of a WAL checkpoint, never the writer; readers hold their snapshot only for one statement.
- A scan the core adds later should take `store.WithLongRead`; until it does, its first run in each process may hold the quick lane.
- Something that needs no other connection open (`journal_mode = DELETE` in the dev database copy, `core/devsync.go`) must not read through `store.DB.Query` with an interface context first, or must close the store; the readers open only on the first routed read for that reason.
- A temporary table or an attached database exists only on the connection that made it, so a reader would not see it. Godwit has none; adding one needs a way to read on the writer first.
