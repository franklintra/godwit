---
status: proposed
---

# Reads on read-only connections beside the writer

`store.Open` opened one SQLite connection, and the interface, the sync workers, the scanner, the labeler, rules and `godwit mcp` all shared it. mygo's `sqlite.DB` runs one operation at a time behind a one-slot gate, so every call waited for every call queued before it. On a 45,000-message mailbox the interface's boot `ListAccounts`, a sub-millisecond query, took 4.2 s (median of the warm openings) because it waited behind an 11 s saved-views scan; under a running sync a probe's `ListAccounts` reached 4.9 s at p95. The database already runs in WAL mode, where readers on other connections run beside the writer.

## Decision

- **`store.DB` keeps one writer and adds three read-only connections** (`sqlite.OpenOptions{ReadOnly: true}`, 5 s busy timeout), opened on the first read that goes to them, after `Open` has migrated. Callers do not change: routing happens inside `store.DB.Query`.
- **Only a plain SELECT goes to a reader.** `Query` sends a statement to a reader when its first keyword, after blanks and comments, is `SELECT` and it calls none of `last_insert_rowid()`, `changes()` and `total_changes()`, which read the writer's connection state. Everything else stays on the writer: PRAGMAs (they set or read the writer's state), `WITH` (a write can follow it), `INSERT … RETURNING`, and anything not recognized. `Execute`, `Transaction`, `DurableTransaction` and `DurableExecute` always use the writer. The read-only open is the safety net: a write that reaches a reader fails with `SQLITE_READONLY` instead of running beside the writer.
- **Reads see every committed write.** mygo has no interactive transaction: `Transaction` is one batch under `BEGIN IMMEDIATE` inside one gate hold, and `Query` refuses `BEGIN`. So a write has committed when its call returns, and a read started after it, on any connection, sees it. A read that runs while another goroutine's transaction is open sees the state before that transaction, which the single connection also allowed, since nothing ordered the two calls.
- **Lanes keep the interface from queueing behind background reads and behind its own long reads.** A context marked with `store.WithUIRead` (the core marks every `ui`-role connection in `core.WithUISession`, so every Service method the interface calls carries it) reads on the interface's readers; every other read (sync, rules, labeler, `full` and `mcp` clients) waits for the third reader, which the interface also takes when it is free. Reader 0 is the interface's quick lane: a statement whose last run took 100 ms or more (a saved view's count) never takes it, and a statement not run yet takes it only when the other two are busy. The interface starts overlapping long reads itself (each event at launch restarts `SavedViews`, whose statements run for seconds), and on a cold page cache they kept every lane busy, so a 2 ms `ListAccounts` still waited 4.2 s for a reader before this rule. The table of last run times is keyed by statement text and bounded (256 entries, cleared when full).
- **The SQL trace names the connection.** `GODWIT_SQL_TRACE` lines keep their five fields; the kind is `q`, `x` or `t<n>` on the writer and `q@r<i>` on reader `i`. perfbench computes queue wait and run time per connection.

## Considered options

- **A priority gate on the one connection** (interface calls first). It reorders the queue, but a call still waits for the statement that holds the connection, and one saved-views statement runs for seconds.
- **Routing per Service method** (`db.Read()` called by the read-only methods). It needs every read path in `core` to choose a connection, and a later method that forgets keeps the old behavior. Routing by statement in `store` covers every caller, including the sync engine, without touching them.
- **Read-write connections with `PRAGMA query_only`.** A misrouted write would still be refused, but a read-write connection can also run checkpoints and create the WAL. Read-only is the narrower promise.
- **More readers.** Each costs its own page cache, temporary b-trees and schema (about 7 MB of RSS each, measured); three with the quick lane cover the measured cases.
- **A lane per caller** (saved views marked as bulk reads by `core`). It is exact, but every slow path in `core` would have to opt in; the run-time table needs no caller to change.

## Consequences

- No new egress, no new `core.Service` method, no schema change.
- A long reader statement delays the end of a WAL checkpoint, never the writer; readers hold their snapshot only for one statement.
- Something that needs no other connection open (`journal_mode = DELETE` in the dev database copy, `core/devsync.go`) must not read through `store.DB.Query` first, or must close the store; the readers open only on the first routed read for that reason.
- A temporary table or an attached database exists only on the connection that made it, so a reader would not see it. Godwit has none; adding one needs a way to read on the writer first.
