# wa-mirror

A Go CLI that links a machine to a WhatsApp account **as a companion device**
(exactly what WhatsApp Web does), copies the conversations into a local SQLite
file, and reads them back from the command line. Built on
[whatsmeow](https://github.com/tulir/whatsmeow). User-facing documentation is in
`README.md`; this file is what to know before touching the code.

## Build and test

```
go build -o wa .
go test ./...
```

## Rules for this repo

- **This repo is public. No real data, ever** — not in tests, not in comments,
  not in commit messages: no contact name, phone number, group id, message text
  or anecdote taken from a real account. Tests use obviously fake values
  (`33612345678`, `120363000000000001@g.us`, "Alex").
- Code, comments, logs and every string the CLI prints are in English.

## Architecture

| File | Role |
|---|---|
| `main.go` | Argument parsing, dispatch, `--status` |
| `client.go` | whatsmeow connection, QR pairing, event handlers, receipts |
| `message.go` | Reduces a WhatsApp message to a `kind` and a readable body |
| `store.go` | SQLite schema and writes |
| `read.go` | Read queries, filters, terminal rendering |
| `send.go` | `--send` and recipient resolution |
| `backfill.go` | Asks the phone for messages older than the oldest held, and with `--recent` for the latest window |
| `media.go` | Attachment downloads into `~/.wa/media`, on a background worker |
| `presence.go` | `--last-seen`: subscribes to someone's presence for the length of one question |
| `daemon.go` | launchd agent `local.wa-mirror` (macOS only) |

Everything lives in **`~/.wa/wa.db`**: the `whatsmeow_*` tables (session,
encryption keys) and the mirror tables (`messages`, `chats`, `contacts`, `meta`).
The daemon log is `~/.wa/wa.log`.

## What to know before touching the code

- **One connection per companion device.** `--send`, `--backfill` and
  `--last-seen` stand the launchd daemon down for their duration
  (`pauseDaemon`) and bring it back after. Anything new that connects must do
  the same, or the two connections kick each other off.
- **History is incomplete at pairing, and deepened on demand.** The phone pushes
  `HistorySync` blobs covering recent conversations only, often a single message
  for dormant threads. Everything received after linking is captured, as long as
  `wa serve` runs.
- **There is no server-side search.** WhatsApp relays and forgets; the phone
  holds the only full copy. "No result" always means "not in the mirror".
- **`--backfill` works by anchor and by chunk.** It takes the oldest message of a
  conversation and asks the phone for the N before it
  (`BuildHistorySyncRequest` + `SendPeerMessage`, answered by an `ON_DEMAND`
  `events.HistorySync`), then repeats from the new anchor. It stops when a round
  adds nothing. A conversation with no message held has no anchor.
- **A silent phone is not the end of history.** A burst of requests can go
  unanswered for a minute; rerunning shortly after works. The real end is a
  round that answers with nothing new.
- **One person can hold two conversations**: their phone number
  (`@s.whatsapp.net`) and their `@lid` alias, and old history may hang off only
  one of them. `--backfill <name>` digs every address of that person
  (`conversationsNamed`), while `--send <name>` targets one
  (`resolveRecipient`). A bare id is looked up in `chats` first
  (`knownJIDFor`), so a group id is not mistaken for a phone number; a phone
  number keeps its `@s.whatsapp.net` address even when a newer `@lid` alias exists.
- **`--recent` does not work on `@lid` conversations.** The phone answers zero
  messages, even when the request is readdressed to the phone-number JID via
  `Store.LIDs.GetPNForLID` (kept in `pullRecent` because it costs nothing).
- **One SQLite writer.** `db.SetMaxOpenConns(1)`: the modernc driver and
  whatsmeow's concurrent writes do not get along otherwise. Drain a result set
  before querying again inside a loop, or it deadlocks on itself.
- **The dialect passed to `sqlstore` is `"sqlite"`**, not `"sqlite3"`: the name
  `modernc.org/sqlite` registers under. No cgo.
- **Edits and deletions are corrections, not messages.** Both arrive as a
  `ProtocolMessage` carrying the key of the target (`MESSAGE_EDIT` with the new
  content, `REVOKE` with none). `describe` returns no `kind` for them;
  `editTarget` reads them and `applyEdit` / `applyRevoke` rewrite the original
  row, keeping its id and timestamp and setting `edited_at` or `deleted_at`. A
  revoke drops the text and the downloaded file. An edit targeting a message the
  mirror never saw creates nothing and says so on stderr.
- **An edit missed once is lost.** `--backfill` only asks for messages older
  than the oldest held, so it never revisits a gap in the middle of a thread.
- **An unknown type is named, not folded into `other`.** `nameOfSetField` uses
  protobuf reflection to find which field the message carries and makes it the
  `kind` (`ptv`, `event`, `album`…).
- **Reactions are not part of history sync.** They arrive live as a `reaction`
  message only.
- **Media**: downloaded to `~/.wa/media/<message-id>.<ext>`, only for messages
  younger than 30 days (WhatsApp drops the blob after that), on a single
  background worker so a slow download never blocks the handler. Only the file
  name is stored, so moving `~/.wa` does not orphan rows.
- **The link uses one of the account's "Linked devices" slots**, visible and
  revocable from the phone.

## Receipts

`delivered_at` and `read_at` on `messages` (Unix time, `0` when unknown). Rules
in `markDelivered` / `markRead` (`store.go`): the first receipt wins and is never
overwritten, a read receipt also fills an empty delivery, and in a group the
mark means "at least one person". Type sorting is in `storeReceipt`
(`client.go`): `delivered` and `read`/`played` count, `IsFromMe` receipts are
dropped (the account owner reading on another device), and `sender`, `retry`,
`server-error`, `inactive` are about routing.

Receipts exist only in `events.Receipt`, pushed live and never replayed:
`--backfill` brings messages back, never their receipts. No mark is not "unread".

## Presence

WhatsApp never stores presence where it can be read: the server pushes it only
to a client that is itself online, and only for people that client subscribed
to (`SendPresence(available)` then `SubscribePresence(jid)`, answered by
`events.Presence`). Hence:

- no history, and nothing stored in the database on purpose;
- asking marks the account online for the length of the command, set back to
  `unavailable` before hanging up;
- a hidden last seen comes back as `last="deny"`, which whatsmeow turns into a
  zero time: indistinguishable from silence, and the output says so.

Both addresses of a person are subscribed, since there is no telling which one
the server answers on. `--top` folds the two conversations of one person only
after connecting, because the `@lid` ↔ number mapping is only known then. A
group is refused: presence belongs to a person.

## Privacy

`~/.wa/wa.db` and `~/.wa/media/` hold messages and files **in plain text**,
outside any repo. Only disk encryption protects them.

@CLAUDE.local.md
