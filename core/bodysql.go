package core

import "github.com/godwit-email/godwit/core/store"

// bodyText and bodyHTML are the SQL expressions of the bodies of the
// message whose alias in the statement is a, in message_bodies or still
// inline (ADR 0053). Statements read bodies only through them, and Go
// reads their values with store.BodyString: TestBodiesReadThroughHelper
// fails on any other read of messages.body_text or messages.body_html.
func bodyText(a string) string { return store.BodyTextSQL(a) }

func bodyHTML(a string) string { return store.BodyHTMLSQL(a) }
