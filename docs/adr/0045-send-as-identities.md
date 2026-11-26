# Send-as identities (发件身份) belong to an account

Until now an account sent only as `Name <Email>`, its own address. People
often send from one mailbox under several names and addresses: a Gmail
send-as address (`contact@company.example` from `me@company.example`), a
shared mailbox in Microsoft 365, a catch-all domain, or a second name on the
same address. MailMate, Thunderbird and Apple Mail call these identities or
aliases. Godwit adds them as **发件身份** (`Identity`), records of an
account, and not as a new kind of account.

**An identity is a From header, not a mailbox.** It has a display name
(empty: the account's), an address, an optional Reply-To, an optional
signature (none: the account's) and a default flag. It has no servers and
no credentials: mail sent as an identity leaves through its account's
sending server, signed in as the account, and the envelope sender (`MAIL
FROM`) stays the account's address. The server decides whether the account
may send as the address. Gmail sends a verified send-as address as is and
rewrites any other From to the account; Microsoft 365 needs the Send As (or
Send on Behalf) permission and refuses the message otherwise; most other
SMTP servers accept any From and leave alignment to SPF and DMARC. Godwit
does not check this when an identity is added: the check would need a test
send, and the server's answer at send time is shown in the outbox as any
other refusal. JMAP is the exception that needs the protocol: a JMAP server
checks the From header against the `Identity` a submission names, so the
JMAP sender picks the server identity of the From address (exact, then
`*@domain`, then `*`) and uses that address as the submission's envelope
sender, and falls back to the account's own when the server has none.

**Drafts store the identity.** `drafts.identity_id` (NULL: the account's own
address) is set by `NewDraft` and changed by `SetDraftFrom(draft, account,
identity, revision)`, which the compose window's From menu calls with one
entry per account address and per identity. Like `SetDraftAccount`, a switch
is a revision of its own and replaces the body's signature block
(`data-godwit="signature"`) with the identity's signature, or the account's
when the identity has none. The identity's Reply-To replaces the draft's
only when the draft had none or the previous identity's, so an address the
user typed stays. `SetDraftAccount` keeps working: it picks the account's
default identity. `GetDraft` returns `Draft.From`, the header the draft is
built with, so every reader (the outbox, the MCP send confirmation, Wit's
sender block, programs) uses one value.

**New mail starts from the default identity; replies from the address the
mail came to.** As MailMate and Thunderbird do it, a reply or forward picks
the identity whose address is the original's From (the user's own mail
keeps the identity it went out as), else the first of its To and then Cc.
Among identities on one address the default wins, then the account's own
address, then the first added. Mail sent to none of the user's addresses (a
list, a Bcc) is answered from the default identity. `Delivered-To` is not
stored, so it is not consulted.

**Identity addresses are the user's.** Wherever Godwit asks "is this mail
from me", the account's identities count with its address: reply
recipients (the user's addresses are dropped from reply-all, and a reply to
one's own mail follows up with its recipients), a conversation's listed
sender, subject threading's shared participants, contacts and avatars (own
addresses are skipped), 回复状态, the security scanner's own-mail check,
quick replies, and the MCP send confirmation's "first time" recipients.

**Local API.** `ListIdentities`, `AddIdentity`, `UpdateIdentity`,
`RemoveIdentity` and `SetDraftFrom` are in `full`. The `mcp` role gets
`ListIdentities` (stored data) and `SetDraftFrom` (a draft write, audited as
`NewDraft` and `SaveDraft` are): `create_draft` takes an optional `from`.
Adding or changing an identity causes no egress: nothing is sent until the
user, or a program allowed to send, sends a draft, and the identity only
changes that draft's From, Reply-To and signature, as an account's name and
signature already do through `UpdateAccount`.

## Considered Options

- **An identity as an account of its own**, sharing the other account's
  credentials: every list of accounts (sidebar, sync, unified inbox) would
  have to skip it, and removing the real account would leave it behind.
- **Free-text From in the draft** (`Draft.From` writable): simple, but the
  signature and Reply-To cannot follow it, replies cannot pick it, and
  nothing tells the user's addresses apart for "is this mail from me".
- **Import Gmail send-as addresses automatically** through the Gmail API's
  `users.settings.sendAs.list`: only Gmail API accounts reach that endpoint
  without new egress (IMAP accounts would need a new host), so it is left
  for later; the identities it would make are the same records.
- **Envelope sender = identity address for SMTP**: Gmail and Microsoft 365
  judge the authenticated user, not `MAIL FROM`, and a server that checks
  `MAIL FROM` against the login would refuse it, so the account's address
  stays the envelope sender.

## Consequences

- Schema: `identities(account_id, name, email, reply_to, signature_id,
  is_default)`, unique on `(account_id, email, name)`; `drafts.identity_id`
  with `ON DELETE SET NULL`. Removing an identity sends its drafts as the
  account; removing the account removes its identities.
- A local archive has none (`ErrLocalArchive`); a receive-only account may
  have some, but sends nothing until SMTP works.
- `events.IdentitiesChanged` tells the interface to reload them.
- Settings › account shows a 发件身份 section to add, edit, remove and make
  one the default; the compose window's From menu lists each account's
  address followed by its identities.
