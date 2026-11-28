package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/egoist/mygo/plugins/sqlite"
)

// The fork keeps mail bodies in their own table, message_bodies (ADR 0053).
// Inline bodies made every scan of messages walk the bodies' overflow
// pages; without them the messages table is small enough to stay in the
// page cache. messages.body_text and messages.body_html stay in the schema
// and are NULL once a message's body has moved: stock Godwit and upstream
// migrations still see the columns they know.
//
// A body is in exactly one place: in message_bodies, or inline in messages
// (a row stock Godwit wrote, or one the move has not reached yet). Readers
// take the body table's value, else the inline one (BodyTextSQL,
// BodyHTMLSQL), so a row in either place reads the same.

// BodyTextSQL is the SQL expression of the text body of the message whose
// alias (or table name) in the statement is a, wherever it is stored.
func BodyTextSQL(a string) string {
	return "COALESCE((SELECT bdy.body_text FROM message_bodies bdy WHERE bdy.message_id = " + a + ".id), " + a + ".body_text)"
}

// BodyHTMLSQL is BodyTextSQL for the HTML body.
func BodyHTMLSQL(a string) string {
	return "COALESCE((SELECT bdy.body_html FROM message_bodies bdy WHERE bdy.message_id = " + a + ".id), " + a + ".body_html)"
}

// BodyString is a body value as SQL returned it (BodyTextSQL, BodyHTMLSQL):
// "" for NULL. Every Go read of a body goes through it, so a later change
// of the stored form (compression) changes no reader.
func BodyString(v any) string { return Str(v) }

const bodiesTable = `CREATE TABLE message_bodies (
	message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
	body_text TEXT,
	body_html TEXT)`

// bodyTriggers keep the full-text index and the one-place rule with the
// bodies in message_bodies. upstreamMessagesAU is what messages_au was.
var bodyTriggers = []forkTrigger{{
	// Upstream's also fires on body_text, so the move, which NULLs the
	// inline body, would empty the index's body. This one fires on the
	// subject and the sender only and keeps the stored body.
	name: "messages_au",
	sql: `CREATE TRIGGER messages_au AFTER UPDATE OF subject, from_addr ON messages BEGIN
		UPDATE messages_fts SET subject = new.subject, from_addr = new.from_addr,
			body = COALESCE((SELECT body_text FROM message_bodies WHERE message_id = new.id), new.body_text)
		WHERE rowid = old.id;
	END`,
}, {
	// A body stored here for a message whose inline text is NULL: a writer
	// that inserted the message without a body (core/mailsync), so
	// messages_ai indexed none. The move and the safety triggers below
	// insert rows whose inline body is still set, which the index already
	// has: skipped.
	name: "message_bodies_ai",
	sql: `CREATE TRIGGER message_bodies_ai AFTER INSERT ON message_bodies
		WHEN new.body_text <> '' AND (SELECT body_text IS NULL FROM messages WHERE id = new.message_id) BEGIN
		UPDATE messages_fts SET body = new.body_text WHERE rowid = new.message_id;
	END`,
}, {
	name: "message_bodies_au",
	sql: `CREATE TRIGGER message_bodies_au AFTER UPDATE OF body_text ON message_bodies
		WHEN old.body_text IS NOT new.body_text BEGIN
		UPDATE messages_fts SET body = new.body_text WHERE rowid = new.message_id;
	END`,
}, {
	// Safety: a writer that still stores a body inline (stock Godwit, a
	// test, an upstream path the fork has not changed) ends with it in
	// message_bodies. A column it leaves NULL keeps the stored value. It
	// costs a second write of the body, so the sync writers never use it.
	name: "messages_body_move",
	sql: `CREATE TRIGGER messages_body_move AFTER INSERT ON messages
		WHEN new.body_text IS NOT NULL OR new.body_html IS NOT NULL BEGIN
		INSERT INTO message_bodies(message_id, body_text, body_html) VALUES (new.id, new.body_text, new.body_html)
			ON CONFLICT(message_id) DO UPDATE SET body_text = COALESCE(excluded.body_text, body_text), body_html = COALESCE(excluded.body_html, body_html);
		UPDATE messages SET body_text = NULL, body_html = NULL WHERE id = new.id;
	END`,
}, {
	// The same for an inline UPDATE, which also re-indexes a changed text
	// (messages_au no longer does).
	name: "messages_body_move_au",
	sql: `CREATE TRIGGER messages_body_move_au AFTER UPDATE OF body_text, body_html ON messages
		WHEN new.body_text IS NOT NULL OR new.body_html IS NOT NULL BEGIN
		INSERT INTO message_bodies(message_id, body_text, body_html) VALUES (new.id, new.body_text, new.body_html)
			ON CONFLICT(message_id) DO UPDATE SET body_text = COALESCE(excluded.body_text, body_text), body_html = COALESCE(excluded.body_html, body_html);
		UPDATE messages SET body_text = NULL, body_html = NULL WHERE id = new.id;
		UPDATE messages_fts SET body = (SELECT body_text FROM message_bodies WHERE message_id = new.id)
			WHERE rowid = new.id AND old.body_text IS NOT new.body_text;
	END`,
}}

// upstreamMessagesAU is upstream's messages_au, as its first migration
// made it: fork-undo puts it back. TestUpstreamMessagesAU pins it.
const upstreamMessagesAU = `CREATE TRIGGER messages_au AFTER UPDATE OF subject, from_addr, body_text ON messages BEGIN
		UPDATE messages_fts SET subject = new.subject, from_addr = new.from_addr, body = new.body_text
		WHERE rowid = old.id;
	END`

const (
	bodiesState  = "fork.bodies"
	bodiesCursor = "fork.bodies.cursor"
	bodiesUndo   = "undoing" // fork-undo started; a fork Open moves the bodies again
)

var bodiesRewrite = ForkRewrite{Name: "bodies", Ensure: ensureBodies, Backfill: moveBodies, Undo: undoBodies}

// inlineCond is true for a message row that still holds a body inline. It
// reads only the row's header, not the values.
const inlineCond = "(body_text IS NOT NULL OR body_html IS NOT NULL)"

// ensureBodies creates message_bodies and puts back each body trigger that
// is missing or differs. A trigger put back means some writer may have
// stored bodies inline meanwhile: the move starts over from the first row.
func ensureBodies(ctx context.Context, db *DB) error {
	names := []string{"message_bodies"}
	for _, t := range bodyTriggers {
		names = append(names, t.name)
	}
	have, err := db.schemaSQL(ctx, names)
	if err != nil {
		return err
	}
	var stmts []sqlite.Statement
	if _, ok := have["message_bodies"]; !ok {
		stmts = append(stmts, sqlite.Statement{SQL: bodiesTable})
	}
	stmts = append(stmts, triggerStmts(bodyTriggers, have)...)
	if len(stmts) > 0 {
		stmts = append(stmts, setMeta(bodiesState, RewriteMoving), setMeta(bodiesCursor, 0))
	} else {
		state, err := db.RewriteState(ctx, "bodies")
		if err != nil {
			return err
		}
		if state == RewriteMoving || state == RewriteDone {
			return nil
		}
		stmts = append(stmts, setMeta(bodiesState, RewriteMoving), setMeta(bodiesCursor, 0))
	}
	_, err = db.Transaction(ctx, stmts)
	return err
}

// moveBodies moves the next inline bodies, in id order, to message_bodies
// in one transaction: b.Rows rows at most, and no more than b.Bytes of
// body past the first row. The selection is the rows that still hold a
// body inline; the cursor in meta only lets it skip the rows before.
// When none is left past the cursor, one header-only count of all inline
// rows decides: none means done; some (a writer stored them while a
// trigger was missing) start the move over.
func moveBodies(ctx context.Context, db *DB, b Batch) (BatchResult, error) {
	cursor, err := db.metaInt(ctx, bodiesCursor)
	if err != nil {
		return BatchResult{}, err
	}
	rows, err := db.Query(ctx, `SELECT id, COALESCE(octet_length(body_text), 0) + COALESCE(octet_length(body_html), 0)
		FROM messages WHERE id > ? AND `+inlineCond+` ORDER BY id LIMIT ?`, cursor, max(b.Rows, 1))
	if err != nil {
		return BatchResult{}, err
	}
	if len(rows.Values) == 0 {
		left, err := db.Query(ctx, "SELECT count(*) FROM messages WHERE "+inlineCond)
		if err != nil {
			return BatchResult{}, err
		}
		if Int(left.Values[0][0]) > 0 {
			reset := setMeta(bodiesCursor, 0)
			_, err := db.Execute(ctx, reset.SQL, reset.Args...)
			return BatchResult{More: true}, err
		}
		_, err = db.Transaction(ctx, []sqlite.Statement{setMeta(bodiesState, RewriteDone), {SQL: "DELETE FROM meta WHERE key = ?", Args: []any{bodiesCursor}}})
		return BatchResult{}, err
	}
	var ids []int64
	var bytes int64
	for _, r := range rows.Values {
		if len(ids) > 0 && bytes+Int(r[1]) > b.Bytes {
			break
		}
		ids = append(ids, Int(r[0]))
		bytes += Int(r[1])
	}
	list, _ := json.Marshal(ids)
	in := "id IN (SELECT value FROM json_each(?))"
	// A row in message_bodies already (a writer stored inline over a moved
	// body while a trigger was missing): the inline value is the newer, as
	// the safety triggers would have stored it.
	_, err = db.Transaction(ctx, []sqlite.Statement{{
		SQL: `INSERT INTO message_bodies(message_id, body_text, body_html)
			SELECT id, body_text, body_html FROM messages WHERE ` + in + ` AND ` + inlineCond + ` ORDER BY id
			ON CONFLICT(message_id) DO UPDATE SET body_text = COALESCE(excluded.body_text, body_text), body_html = COALESCE(excluded.body_html, body_html)`,
		Args: []any{string(list)},
	}, {
		SQL:  "UPDATE messages SET body_text = NULL, body_html = NULL WHERE " + in + " AND " + inlineCond,
		Args: []any{string(list)},
	}, setMeta(bodiesCursor, ids[len(ids)-1])})
	if err != nil {
		return BatchResult{}, err
	}
	return BatchResult{Rows: len(ids), Bytes: bytes, More: true}, nil
}

// undoBodies puts every body back inline and the schema back to stock
// Godwit's (upstream's messages_au, no message_bodies), in batches that
// each leave every body in one place. A crash leaves state "undoing": the
// next fork-undo goes on, the next fork Open moves the bodies again.
func undoBodies(ctx context.Context, db *DB, progress func(moved int)) error {
	have, err := db.schemaSQL(ctx, []string{"message_bodies"})
	if err != nil {
		return err
	}
	restore := []sqlite.Statement{
		{SQL: "DROP TRIGGER IF EXISTS messages_au"}, {SQL: upstreamMessagesAU},
		{SQL: "DELETE FROM meta WHERE key IN (?, ?)", Args: []any{bodiesState, bodiesCursor}},
	}
	if _, ok := have["message_bodies"]; !ok {
		_, err := db.Transaction(ctx, restore)
		return err
	}
	// Without the safety triggers the inline writes stay inline; the fork's
	// messages_au keeps the index as it is while they do.
	stmts := []sqlite.Statement{}
	for _, t := range bodyTriggers {
		stmts = append(stmts, sqlite.Statement{SQL: "DROP TRIGGER IF EXISTS " + t.name})
	}
	stmts = append(stmts, sqlite.Statement{SQL: bodyTriggers[0].sql}, setMeta(bodiesState, bodiesUndo))
	if _, err := db.Transaction(ctx, stmts); err != nil {
		return err
	}
	moved := 0
	for {
		rows, err := db.Query(ctx, `SELECT message_id, COALESCE(octet_length(body_text), 0) + COALESCE(octet_length(body_html), 0)
			FROM message_bodies ORDER BY message_id LIMIT 500`)
		if err != nil {
			return err
		}
		if len(rows.Values) == 0 {
			break
		}
		var ids []int64
		var bytes int64
		for _, r := range rows.Values {
			if len(ids) > 0 && bytes+Int(r[1]) > 8<<20 {
				break
			}
			ids = append(ids, Int(r[0]))
			bytes += Int(r[1])
		}
		list, _ := json.Marshal(ids)
		_, err = db.Transaction(ctx, []sqlite.Statement{{
			SQL: `UPDATE messages SET body_text = COALESCE(b.body_text, messages.body_text), body_html = COALESCE(b.body_html, messages.body_html)
				FROM message_bodies b WHERE b.message_id = messages.id AND messages.id IN (SELECT value FROM json_each(?))`,
			Args: []any{string(list)},
		}, {
			SQL:  "DELETE FROM message_bodies WHERE message_id IN (SELECT value FROM json_each(?))",
			Args: []any{string(list)},
		}})
		if err != nil {
			return err
		}
		moved += len(ids)
		if progress != nil {
			progress(moved)
		}
	}
	_, err = db.Transaction(ctx, append(restore, sqlite.Statement{SQL: "DROP TABLE message_bodies"}))
	if err != nil {
		return fmt.Errorf("restore the stock schema: %w", err)
	}
	return nil
}
