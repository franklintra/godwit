---
status: proposed
---

# Mail bodies in their own table

Godwit stores a message's text and HTML bodies inline in `messages` (`body_text`, `body_html`). On a real mailbox (63,320 messages, 3.97 GB file) the bodies are 2.4 GB of the table's 2.48 GB, and 83 % of the rows are larger than a page. SQLite keeps the start of such a row on its leaf page and the rest on overflow pages, and it reaches a column after the bodies by walking that chain. So a scan of message metadata, a flag update or a cold point read walks gigabytes of body pages: the thread lists, the counts and the reply-status reads take seconds cold, and they hold the one connection the interface waits on.

## Decision

- **Bodies live in `message_bodies`** (`message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE, body_text, body_html`). The key is the rowid: a lookup is one b-tree descent, a delete cascades, and `last_insert_rowid()` after the body insert is still the message id. `body_text` comes first, so a read of the text never walks the HTML.
- **`messages.body_text` and `messages.body_html` stay**, NULL once a body has moved. Upstream's migrations and stock Godwit still find the columns they know. A body is in exactly one place: in `message_bodies`, or inline (a row stock Godwit wrote, or one the move has not reached).
- **Readers take the body table's value, else the inline one**: `COALESCE((SELECT body_text FROM message_bodies WHERE message_id = m.id), m.body_text)`, through one helper (`bodyText`, `bodyHTML` in `core/bodysql.go`; `store.BodyTextSQL`, `BodyHTMLSQL`). Go reads the value with `store.BodyString`, so a later change of the stored form (compression) changes no reader. A test fails on any other read of the inline columns.
- **The sync writers store the body in `message_bodies`** in the message's own transaction, and leave the inline columns NULL. Every message gets a body row, an empty one too.
- **The full-text index keeps its rows and its content.** The fork owns five triggers: `messages_au` fires on the subject and the sender only (upstream's also fires on `body_text`, so the move would empty the index's body); `message_bodies_ai` indexes a body stored for a message inserted without one; `message_bodies_au` re-indexes a changed text; `messages_body_move` and `messages_body_move_au` move a body that any other writer stores inline (stock code, tests, a future upstream path) and keep the index right.
- **The move runs in the background** as the fork's first rewrite (FORK.md, "Fork rewrites"): batches of about 50 ms or 2 MB, each one transaction that copies the bodies and NULLs them inline, a cursor in `meta`, a pause below 512 MB free. The batch selects the rows that still hold a body inline, so the cursor never decides what is correct. A row already in `message_bodies` takes the inline values (they are newer), column by column. The move ends when a count of inline rows, read from the row headers only, is 0.
- **`godwit fork-undo bodies`** puts every body back inline, restores upstream's `messages_au` and drops the table, so stock Godwit reads the mail again.

## Considered options

- **Also move other large or rarely read columns** (`auth_results`, `list_*`, `snippet`). They are about 32 MB together, and the hottest queries read them: moving them adds a lookup to those queries for a table that already fits in memory.
- **Bodies as files, as MailMate keeps them.** A primary-key read of a 40 KB value costs about what an open and read of a file costs. Files need their own fsync ordering, an orphan sweep after a crash, and export and backup would no longer find the mail in the database.
- **Compress in place now.** Compression (zstd of `body_html`) is a second, separate rewrite on top of this one; it needs the body table first.
- **A numbered migration.** It would take upstream's next number (FORK.md). The rewrite has a name and runs after upstream's migrations.
- **One transaction for the whole move.** It holds the writer for minutes and needs twice the disk. Batches need one batch plus the WAL: each batch's UPDATE frees overflow pages that the next batch's INSERT reuses.

## Consequences

- A metadata scan no longer walks body pages: the `messages` table of the real mailbox goes from 2.48 GB to about 170 MB, which stays in the page cache.
- No change to the full-text index's data: the move leaves its shadow tables byte for byte the same. The sync path indexes the body with one more FTS update per message (the message row is indexed first without its body).
- A writer that stores a body inline pays a second write of it (measured: 0.60 ms and 195 KB of WAL per 43 KB message against 0.59 ms and 175 KB on the sync writers' path, and 0.50 ms and 161 KB for stock Godwit's inline insert). No hot path uses it.
- Stock Godwit opens a fork database and writes correctly, but shows empty bodies for moved mail until `godwit fork-undo bodies` runs.
- Before moving to a newer upstream release, its migrations must be read for a rebuild of `messages` (the drop would cascade to the bodies) and a rebuild of `messages_fts` from `messages.body_text` (FORK.md).
- No egress, no new `core.Service` method, no Local API or MCP change.
