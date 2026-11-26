---
status: proposed
---

# Plain-text drafts: a draft's format, sent as format=flowed

Godwit sends every message as `multipart/alternative`: the HTML and a text version made from it (`mail.Build`). Some people write only plain text: mailing lists that refuse HTML, OpenPGP users, people coming from Mutt, MailMate or Thunderbird. For them the HTML part is noise, the text part loses blank lines and indentation, and a reply has no `> ` quoting. They need a way to send `text/plain` only.

## Decision

- **A draft has a format: `html` or `plain`** (`Draft.Format`, column `drafts.format`, `'html'` for drafts stored before). It decides how the draft is sent and how the compose window edits it. It is a property of the draft, not a send option: what the window shows is what goes out.
- **New drafts take a default.** `Account.ComposeFormat` (`html`, `plain`, or `""` to follow the settings), then `Settings.ComposeFormat` (default `html`). With `Settings.ReplyInKind` (off by default), a reply or reply-all takes the format of the mail it answers: plain to a mail with no HTML, HTML to one with HTML. A forward keeps the default.
- **A plain draft keeps its text in the same body.** The editable part is stored as compose HTML in a fixed form (`mail.PlainHTML`): lines a blank line apart are paragraphs, a line break inside one is a `<br>`, and each extra blank line is one more `<br>`. That form is valid compose HTML, so everything that reads or writes `Draft.HTML` keeps working: the signature and quote markers (`SplitDraft`, `JoinDraft`), Wit, plugins, masking, link references, the drafts folder. `SaveDraft` turns the editable part of a plain draft into this form (`mail.PlainText`, then `mail.PlainHTML`), whoever writes it, so a program that sends HTML for a plain draft loses its formatting, not its text. Wit's edit proposals are made the same way, and the prompt tells the model the draft is plain.
- **Switching format converts the body.** Saving a draft as plain turns its editable part into text: bold and fonts go, a link becomes "text (address)", a list item gets "- ". Saving it as HTML keeps the paragraphs and line breaks as they are. The quote and the signature blocks stay HTML in both formats.
- **A plain draft is sent as one `text/plain; charset=utf-8; format=flowed` part** (RFC 3676). The body is the editable text, the signature, then the quote, in `JoinDraft`'s order, a blank line apart (`mail.PlainBody`). The quote is the attribution line and the quoted mail with one more `>` per level; a plain reply quotes the mail's own text part when it has one, not its HTML made into text. The signature follows `Settings.SignatureDelimiter`: on, it starts with the `-- ` line. Lines longer than 72 characters break after a space, which stays at the end of the line (a soft break); trailing spaces of a line are dropped; lines that start with a space, `>` or `From ` are space-stuffed. The part is `7bit` when every line is short ASCII with no trailing space, else `quoted-printable`, so servers cannot strip the soft breaks. With attachments the part is the first of a `multipart/mixed`. Signature images are not sent.
- **The compose window edits a plain draft as text.** A toolbar button switches the format (`ComposePage.SetFormat`, saved first, against the revision). In plain mode the body is a text area in a fixed-width font with no format controls, the quote shows with its `>` lines and the signature as text. ⌘K rewriting is not offered in plain mode, since its selection contract is the rich editor's; the Wit panel works as before.
- **Programs choose the format the same way.** `SaveDraft` takes `Format` (`""` keeps the stored one, or takes the default for a new draft). `godwit mcp`'s `create_draft` and `update_draft` take an optional `format`; without it a new draft takes the user's default, so a draft for an account set to plain stays plain, its body kept as given.

## Considered options

- **A send option instead of a draft property** ("send as plain text" in the send menu). The window would show rich text that is not what goes out, and Wit, rules and programs could not say which format they write.
- **A separate text column for plain drafts.** Two bodies for one draft, and every reader and writer of `Draft.HTML` (Wit, plugins, masking, the drafts folder, the approval view) would need a second path.
- **Only a per-message toggle, no defaults.** People who write plain text want it for every message, often per account (a mailing-list address); AGENTS.md's "no setting for basic behaviour" was weighed, but the HTML default stays as it is and nothing changes for anyone who does not opt in.
- **`text/plain` without format=flowed.** Hard-wrapped lines read badly on phones, and unwrapped lines break the 78-character advice of RFC 5322. Flowed gives both.

## Consequences

- No new egress: the same recipients get the same message in another MIME shape. No new `core.Service` method; `ComposePage.SetFormat` is the compose page's own.
- A schema migration adds `drafts.format` and `accounts.compose_format`.
- Godwit does not yet unwrap format=flowed mail it receives; it shows the lines as sent.
