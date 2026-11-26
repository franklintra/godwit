---
status: proposed
---

# A draft carries its own header fields, never one Godwit writes

Programs that write drafts for the user want to recognize the replies later. The usual way is a header field of their own on the message they draft, such as `X-Hermes-Session: <id>`: the reply's `In-Reply-To` points at a Message-ID, and the program finds its tag on the message that Message-ID names. Other mail clients accept arbitrary fields for this (MailMate's `emate mailto --header`, RFC 6068 `mailto:` links). Godwit's draft had a fixed set of fields, so a program could not tag what it drafted.

## Decision

- **`Draft.Headers` is an ordered list of name and value.** It is stored with the draft (`drafts.headers`, JSON), read back by `GetDraft`, and written by `mail.Build` after Godwit's own fields, in order, when the draft is sent. The outbox item is the built message, so a scheduled or undone send keeps them.
- **`SaveDraft` keeps them when the field is nil and replaces them otherwise.** Over the Local API a JSON `null` or an absent `Headers` keeps the stored ones and `[]` removes them. The compose window saves without the field, so its autosave never drops a program's tag; a change to the headers is a new revision like any other save, so an approval of the old revision fails ([0043](0043-program-drafts-approve-and-send.md)).
- **`mail.CheckHeaders` decides which fields a draft may carry**, at save and again when the message is built, so a stored row cannot bypass it:
  - a name is RFC 5322's `field-name` (printable ASCII but the colon), stored in canonical case so what is shown is what is sent;
  - a value is UTF-8 text without line breaks or control characters, so no value can end its field and start another (header injection); a value that is not ASCII is RFC 2047 encoded, and long values are folded;
  - at most 32 fields, a name up to 76 bytes and a value up to 900, so every line fits RFC 5322's 998 limit even unfolded;
  - refused: the fields Godwit writes (From, To, Cc, Bcc, Subject, Date, Message-ID, In-Reply-To, References, MIME-Version, `Content-*`, `X-Godwit-*`), the ones a relay or signer adds (Received, Return-Path, `Resent-*`, DKIM-Signature, `ARC-*`, Authentication-Results), Autocrypt and OpenPGP (the key a reply is encrypted to), and every field that sends mail somewhere the draft's recipients do not name: Sender, Reply-To, Mail-Reply-To, Mail-Followup-To, Disposition-Notification-To, Return-Receipt-To, Errors-To.
  - A refused field fails the save with a typed `*mail.HeaderError` (wire code `invalid_header`, detail `{Name, Problem}`), which never echoes the value.
- **Reply-To stays the draft's `ReplyTo`.** It is an address the compose window shows and edits; a second way to set it would let a program set one the window does not show as such.
- **A `mailto:` link keeps only `X-*`, Keywords and Comments.** RFC 6068 allows any field, but a link on a web page or in a list's List-Unsubscribe is a stranger's, its query often carries tracking parameters that are no header (`utm_source=…`), and RFC 6068 section 6 warns against taking fields blindly. A program that tags its drafts uses `X-`. The same check applies after this filter, and a link reference planted in a value is defused as the subject's is.
- **What the refused fields would do for good reasons is a draft field instead.** `Draft.ReadReceipt` asks for a read receipt (Disposition-Notification-To) to the From address only, so no caller names where a receipt goes. Bcc and Reply-To are the draft's `Bcc` and `ReplyTo`, which godwit mcp's draft tools now set as `bcc` and `reply_to`. Every approval view lists Bcc, Reply-To and the read receipt.
- **Header values follow the draft's other text on send:** link references expand and 脱敏 placeholders are restored ([0025](0025-mask-at-cloud-egress.md)); one that cannot be restored stops the send.
- **The user sees them wherever a send is decided:** a folded "Extra headers (n)" row under the compose window's fields (read-only), the drafts folder's 批准并发送 pane, the MCP send confirmation, and godwit mcp's `show_draft` card.

## Egress

No new egress. A header field travels only with a send the user makes, approves (批准并发送, the MCP send confirmation) or allowed under 通过 MCP 发送 ([0023](0023-mcp-send-setting.md)), to the recipients that send names. The refused list removes every field that would make the message, or a reply, receipt or bounce to it, reach an address the approval does not show. `SaveDraft` was already in `full` and `mcp`; godwit mcp's `create_draft` and `update_draft` gain an optional `headers` object, and their descriptions say so.

## Considered Options

- **Accept any field but the ones Godwit writes**: would let a program route read receipts or replies to a third party (Disposition-Notification-To, Mail-Followup-To) through a send whose approval names other recipients.
- **Keep every `mailto:` field that passes the check**: turns tracking parameters into header fields on the user's mail, and lets a stranger's link add fields the user never asked for.
- **A separate `SetDraftHeaders` method**: avoids the nil-versus-empty rule, but a program then needs two calls and two revisions to write one draft; the nil rule matches how the compose window already saves only what it edits.
- **Allow editing in the compose window**: possible later; showing them read-only is enough for the user to decide before sending.
