---
status: proposed
---

# Drafts on the server, per account

Godwit keeps drafts on this computer only: the `drafts` table and `<datadir>/drafts/` (ADR [0043](0043-program-drafts-approve-and-send.md) says so in passing). Users who also read mail on a phone, in webmail or in other tools want a draft started in one place to continue in another, and tools that prepare replies for the user (an archive that writes reply drafts by IMAP APPEND, a phone's mail app) put their drafts in the account's Drafts folder. Today Godwit shows those as read-only messages, and its own drafts are invisible everywhere else.

We add **server drafts** (GLOSSARY: 服务器草稿), a per-account setting, off by default. This extends ADR 0043: the local draft row stays what the compose window, Wit, the outbox and `ApproveDraft` work on, and the program marker stays local; with the setting on, the account's Drafts folder also holds a copy of each draft.

## Decision

- **The local draft is the source; the server copy follows it.** A sync round, after its fetch, stores a draft whose newest revision has no copy yet once it has been left unchanged for `ServerDraftIdle` (30 s), or at once when its compose window closes (`FlushServerDraft`, `ui` role only). A draft in the outbox waits: it is read-only until taken back.
- **A copy is the message the draft would send**, built by the same code as the outbox, with `Bcc` kept in the header (other apps read it back), flags `\Draft \Seen`, a new Message-ID per copy, and three fields of our own: `X-Godwit-Draft-ID` (the same on every copy of one draft), `X-Godwit-Draft-Mode` and `X-Godwit-Draft-Source` (the replied or forwarded message's Message-ID). Exchange (also through DavMail) drops `In-Reply-To` from saved drafts; the last two keep the reply's link.
- **Replace = APPEND the new copy, then remove the old one.** A failure in between leaves two copies, never none. The copy's row is written as `appending` before the APPEND, so a crash during it is settled by looking the Message-ID up (`Find`) at the next round, not by appending again. Without UIDPLUS's APPENDUID the new copy is found the same way.
- **Removing a copy is a permanent delete**: UID EXPUNGE with UIDPLUS, the backend's own delete on JMAP, Graph, EWS and the Gmail API. An IMAP server without UIDPLUS gets the copy flagged `\Deleted` and no EXPUNGE: a plain EXPUNGE would also remove every message other apps flagged in that folder. Mail apps hide it, and the next EXPUNGE by any app removes it.
- **A copy goes when its draft goes**: sent (the outbox deletes the draft as it does today), deleted, moved to another account, or replaced by a newer copy. `server_drafts.draft_id` is `ON DELETE SET NULL`, so every path that deletes a draft queues the removal without knowing about server drafts.
- **Godwit's copies never show as mail.** Lists, threads, searches and counts already leave out messages waiting for a purge; they now also leave out a message whose Message-ID, or local row, is a draft's copy. The draft shows instead (the drafts folder, the reply bar of ADR 0043).
- **Another app's draft becomes a Godwit draft on demand**: 编辑草稿 on a message in a Drafts folder, or `OpenServerDraft(messageID) → draftID` in the Local API's `full` role. Its To, Cc, Bcc, Reply-To, subject, body (HTML, else the text as paragraphs) and attachments are copied; a reply whose original is here (by `X-Godwit-Draft-Source`, `In-Reply-To` or the last `References` entry) becomes a reply to it, a reply-all or forward when `X-Godwit-Draft-Mode` says so. The message then is the draft's copy: hidden, replaced when the draft is stored, removed when the draft is sent or deleted. Opening it twice gives the same draft. A program that takes a draft over through the Local API marks it as a program's (ADR 0043); the user's click does not.
- **Changed elsewhere: the last writer wins, and no text is lost.** A copy the sync once stored here that is gone from the Drafts folder was changed or removed by another app. Godwit reads `X-Godwit-Draft-ID` of the other drafts in that folder (the newest 200). If one carries the draft's ID, the version saved last wins, compared by that message's `Date` and the draft's `updated_at`; the other version is kept as a new draft of its own, and the interface shows a notice (`drafts.conflict`). An open compose window then finds its draft changed elsewhere, as it does today for a program's edit. If none does, the draft was sent or deleted there: the local draft stays, with a notice, and is stored again only once it changes.
- **Off** stores nothing new. Copies Godwit already holds stay until their draft is sent or deleted, and go then.

## Considered Options

- **The Drafts folder as the only store** (as Thunderbird does): every autosave a server round trip, no drafts offline, and the revision checks, Wit, the outbox and `ApproveDraft` rebuilt on server messages. The copy keeps all of that unchanged.
- **Taking over every draft in the Drafts folder at each sync**: the first sync would turn years of old drafts into Godwit drafts, and Godwit would rewrite them on the server at the first edit. On demand keeps the user in charge of which drafts Godwit owns.
- **Moving the old copy to Trash** where the server lacks UIDPLUS: every 30 s of typing would leave a revision in Trash.
- **A plain EXPUNGE without UIDPLUS**: removes what other apps flagged `\Deleted` in the folder.
- **An IMAP keyword to recognize Godwit's copies**: any app can set or strip it, and some servers do not keep custom keywords. The Message-ID is ours, and the draft ID header travels with a message another app saves again, when that app keeps unknown header fields.
- **Merging both versions on a conflict**: drafts are rich text with attachments; a merge would guess. Keeping the losing version as its own draft shows both to the user.

## Consequences

- Turning the setting on is not egress to a new host: the copies go to the account's own receiving server, over the connection that already syncs the mail and files sent copies. It does put draft text on that server unattended from then on, including what Wit, plugins and the Local API write into drafts; the setting's help says so.
- A server that drops unknown header fields when another app saves a draft breaks the link: Godwit sees its copy removed (the notice) and the other app's version as another app's draft, which 编辑草稿 opens. Nothing is lost, but the user sees two drafts.
- Inline images in another app's draft, other than the account's signature images, are dropped: the compose window cannot show them.
- Changes Wit and plugins make reach the server at the account's next sync round, not after `ServerDraftIdle`: only the compose window's saves, attachments and account changes set the timer.
- Gmail keeps drafts under a label; removing a copy expunges it from `[Gmail]/Drafts`. Not yet checked against Gmail's own handling of draft expunges.
