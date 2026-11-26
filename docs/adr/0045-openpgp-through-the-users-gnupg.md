---
status: proposed
---

# OpenPGP through the user's own GnuPG

People who sign or encrypt mail with OpenPGP already have keys, a trust history (TOFU or the web of trust) and a pinentry they know, all in GnuPG. A mail client that brings its own OpenPGP library (Thunderbird's RNP) makes them import secret keys into a second keyring and keep two trust databases in step. Godwit has no OpenPGP code yet, and ADR 0001 keeps the build pure Go, so the choice is between a Go OpenPGP library with its own keyring and running the user's `gpg`.

## Decision

- **Godwit runs the user's `gpg` (`internal/gpg`) and keeps no keys.** Keys, passphrases and trust stay in GnuPG: gpg-agent and its pinentry ask for the passphrase, and nothing secret passes through Godwit or a command line. Data goes to gpg on stdin and comes back on stdout; Godwit reads gpg's machine-readable status lines (`--status-fd`), never its translated messages. Each run has a timeout: 30 s, or 5 min when gpg-agent may be waiting for the user.
- **`gpg` is found, not bundled.** A path the user chose in Settings (`SetGPGPath`, `meta.gpg_path`), else `gpg` on `PATH`, else the usual install places (Homebrew, MacPorts, GPG Suite, Gpg4win). A macOS app starts with a bare `PATH`, so the list matters there. Only Godwit's interface sets the path (`uiOnly`): it names a program Godwit will run.
- **No network from gpg, ever.** Every run passes `--disable-dirmngr --no-auto-key-retrieve --auto-key-locate clear,local`, whatever the user's `gpg.conf` says. Verifying a mail on display must not tell a keyserver that it was read, and encrypting must not look a recipient up by WKD. Looking a key up is a later, explicit user action, and it will be declared as egress when it comes.
- **Sending is PGP/MIME (RFC 3156).** `mail.Build` writes the message as before; the `Content-*` fields and the body become the entity that is signed (`multipart/signed`, `micalg` from the hash gpg used) or signed and encrypted in one OpenPGP message (`multipart/encrypted`, RFC 3156 6.2). The entity is CRLF, 7-bit (text is quoted-printable, everything else base64) and has no line starting with `From `, so no hop can change what was signed. The outbox stores and sends the sealed bytes, so the Sent copy is the signed or encrypted message as sent. Protected headers (an encrypted Subject) are not written yet.
- **Settings.** An account has a signing key (a secret key's fingerprint), *sign by default* and *encrypt when possible* (`account_openpgp`). A draft has Sign and Encrypt (`off`, `auto`, `on`); while the draft has never been set (`NULL` columns) it follows its account, so changing the account's defaults changes its open drafts too. `auto` encrypts when every recipient has a valid key and sends signed-only otherwise; `on` refuses to send (`OpenPGPKeysMissingError`) until every recipient has one. A message is encrypted to its account's own key too, so the Sent copy stays readable; Bcc recipients are `--hidden-recipient`.
- **"Valid" is what gpg's trust model says.** A recipient's key is used when its user ID with that address has marginal validity or better in a `--with-colons` listing; a signature is *verified* when gpg reports it good with marginal trust or better and one of the key's user IDs has the From address. Godwit does not reimplement TOFU or the web of trust; it reads gpg's `TRUST_*` lines and shows the model's name.
- **A rule's forward is never signed or encrypted.** It is sent unattended, and there is nobody to answer pinentry.
- **Programs may raise protection, never lower it.** `SetDraftOpenPGP` is open to `full` and `mcp`. A program other than the user's own (`mcp`, a plugin, Wit) can turn signing or encryption on or up, and gets `ErrOpenPGPDowngrade` for off or down, so an approved send is never less protected than the draft the user saw.
- **Receiving decrypts on display and stores nothing decrypted.** `OpenPGPMessage` (`uiOnly`) downloads the original (as "view source" does), extracts the signed entity byte for byte or the encrypted part, and runs gpg. The decrypted text, HTML and attachment list go to the reader only: they are never written to the database, the search index, the snippet or a cache file, so encrypted mail cannot be found by its content. The decrypted HTML is sanitized like any body, and remote images stay blocked whatever the sender (EFAIL's exfiltration channel). A signed or encrypted part that is not the whole message (a list footer around it) is reported `Partial`. Inline PGP (armor in a text part) is verified and decrypted too.

## Considered options

- **A Go OpenPGP library (ProtonMail's go-crypto) with a Godwit keyring.** Pure Go and no external binary, but the user would export secret keys out of GnuPG, unlock them in Godwit, and keep a second trust database. It also makes Godwit responsible for key storage and passphrase handling, which gpg-agent already does well.
- **GPGME.** The supported C API to GnuPG, but cgo (ADR 0001).
- **Store decrypted mail for search (opt-in).** Searchable encrypted mail is useful, and Thunderbird offers it. It puts plaintext of mail the sender encrypted on disk next to the ciphertext, so it needs its own decision and setting; it is left out here.
- **Sign then encrypt as two layers (RFC 3156 6.1).** More widely understood by very old clients, but every current OpenPGP client reads the combined form, and it is one gpg run with one pinentry prompt.

## Consequences

- New: `internal/gpg` (runner, status and key-listing parsers, `gpgtest` throwaway keyrings), `core/mail/openpgp.go` (PGP/MIME build and extraction), `core/openpgp.go` and `core/openpgp_read.go`, a migration adding `account_openpgp` and `drafts.openpgp_sign` / `openpgp_encrypt`.
- Local API: `full` gains `OpenPGPStatus`, `OpenPGPKeys`, `AccountOpenPGP`, `SetAccountOpenPGP`, `SetDraftOpenPGP`, `OpenPGPRecipientKeys`; `ui` gains `SetGPGPath`, `OpenPGPMessage`; `mcp` gains `SetDraftOpenPGP` (tool `set_draft_openpgp`). None sends or reaches a host.
- Signing happens when the draft is queued, while the outbox holds the draft: pinentry may keep the queueing waiting for the user, up to 5 minutes.
- Senders that rewrite MIME (Microsoft Graph and EWS send the bytes to Exchange, which may re-encode them) may break a signature; SMTP, JMAP and the Gmail API send the bytes as built.
- Not yet: protected headers, saving the attachments of a decrypted message, key lookup (WKD, keyserver) on explicit request, Autocrypt, S/MIME, OpenPGP settings in the account export.
