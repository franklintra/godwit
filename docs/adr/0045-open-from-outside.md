---
status: proposed
---

# Programs and links show Godwit's windows; they never send

People already script their mail around Godwit: an agent drafts a reply through `godwit mcp`, a shell script tracks a message by its Message-ID, a link in a note points at a conversation. Each of them ends at the same step: the user has to see the draft or the message in Godwit, read it and decide. Until now a program could write a draft (`NewDraft`, `SaveDraft`) but not show it, and Godwit opened `mailto:` links only from inside its own windows.

So Godwit lets a program or a link put a draft or a message on screen, through one path:

- **One target string, one entry point.** What to show is a string that `godwit --open` already took (`window`, `compose`, a notification ID) and now also takes `draft:<id>`, a `mailto:` link or a `godwit:` link. `app.App.Open` is the only place that acts on it. A launch, a second launch (`OnSecondInstance`), a link the system hands the interface or the backend (`mygo.App.OnOpenURL`), `godwit open TARGET`, and the Local API all end there.
- **The backend asks the interface on its events.** `core.OpenDraft(draftID)` and `core.OpenMessage(messageID)` check that the draft or message exists, then call `Options.ShowUI`, which the backend sets when it runs in a desktop session. `ShowUI` is the tray's `showUI` ([0038](0038-backend-process-owns-the-engine.md)): a ready interface gets an `open.requested` event whose new `Open` field carries the target; with no interface, the backend starts one with `--open=<target>`. A backend with no desktop session (`godwit --headless` over SSH, a Linux server) has no `ShowUI`, and both methods return `ErrNoWindow` (`no_window`).
- **Links.** `godwit://draft/<id>`, `godwit://message/<id>`, `godwit://message?message-id=<Message-ID>`, `godwit://compose?mailto=…` (or `to`, `cc`, `bcc`, `subject`, `body`), and `godwit://reply?message-id=…&body=…&all=1`, each with an optional `account` (address or ID). The macOS bundle declares `godwit` and `mailto` (`CFBundleURLTypes`), so the user can choose Godwit as the mail app; Windows and Linux register `godwit` only, because mygo's Windows installer would take `HKCU\Software\Classes\mailto` and its uninstaller would delete that key. `godwit://link/…` stays what core uses for link references shown to a model, and the interface refuses to open it.
- **Both processes listen.** The interface and the backend are the same bundle, and macOS may hand a link to either, as it does with notification clicks. Both handle it, the backend through `showUI`. The interface drops the same target given twice within 5 seconds, the window it already uses for notification clicks, because mygo also finds a link among a second launch's arguments.

## Opening a window is not egress

Showing a draft or a message sends nothing and requests nothing from any host. It writes no persistent state that egresses later: the draft keeps its revision and its program mark ([0043](0043-program-drafts-approve-and-send.md)), and only the user's click on Send in that window sends it. So `OpenDraft` and `OpenMessage` write no audit row, need no approval, and are in the `full` role. They are also on the `mcp` allowlist, and `godwit mcp` has `open_draft`, `open_message` and `create_draft`'s `open`. The worst a program can do with them is bring Godwit to the front, which interrupts the user but gives the program nothing. A compose window that is open makes `RequestMCPSend` refuse the draft (`mcp_send_composing`), so opening a draft cannot become a way to send it.

A link from outside creates at most a draft, with the account the link names or the default one. A body from a link is plain text, and its link references are defused (`core.TextToDraftHTML`), so that sending the draft cannot expand a planted `godwit://link/` into a link from the user's mail. Drafts made from links do not get the program mark: the user is the one who clicked the link, as with a `mailto:` link inside a message.

## Message-ID lookup

`FindMessagesByMessageID` finds a message by its RFC 5322 Message-ID, with or without the angle brackets or as a `mid:` URL. It searches every account, because the same message is stored once in each account that has it. Search has no operator syntax, and this change adds none: `SearchQuery.MessageID` is a filter, and a query that is exactly one Message-ID in angle brackets (pasted from a header) finds that message instead of mail that quotes it.

## Considered Options

- **A UI-only method that the backend proxies**: the backend has no way to call its interface except events, and the tray already uses events plus a fallback launch. A second channel would be a second lifecycle to get right.
- **Refuse when no interface is connected**, as `ErrNoUI` does for approvals: approvals wait for the user, so they must not start unseen. A window that opens is the request itself, and notification clicks already start the interface, so OpenDraft does the same.
- **Mark link drafts as program drafts**: a `mailto:` link clicked in a browser is the user's action, the same as one clicked inside a message. A script that writes drafts should use the Local API, which marks them.
- **A `msgid:` search operator**: Godwit's search box has no operators, and one operator would make users expect more. A filter and the pasted-ID rule cover the lookup.

## Consequences

- `godwit open TARGET` starts an interface process for TARGET, which hands it to the running interface (`OnSecondInstance`) or becomes the interface. Only the default data directory has a single interface; with `--data-dir`, each call starts another interface on the same backend.
- The backend starts the interface with `--open=<target>` as one argument: mygo treats a separate argument that is a link of Godwit's schemes as one more link to open.
- Setting Godwit as the default mail app is done in the system on macOS (Mail › Settings › General › Default email reader). A Settings row that calls `mygo.App.RegisterURLScheme("mailto")` (`LSSetDefaultHandlerForURLScheme`) can come later, together with the Windows Default Apps registration (`RegisteredApplications`, `Capabilities\URLAssociations`) and the MSIX protocol declaration.
