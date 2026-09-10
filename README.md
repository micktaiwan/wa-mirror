# wa-mirror

A command-line WhatsApp client that links your machine to your own account as a
companion device (exactly like WhatsApp Web), mirrors your conversations into a
local SQLite file, and reads them back from the terminal. It can also send
messages, pull older history from your phone, and tell you when someone was last
online.

Built on [whatsmeow](https://github.com/tulir/whatsmeow). Pure Go, no cgo.

> This is an unofficial client. Using it may break WhatsApp's terms of service,
> and your account is yours to risk.

## Install

Requires Go 1.26 or later.

```sh
git clone https://github.com/micktaiwan/wa-mirror.git
cd wa-mirror
go build -o wa .
mv wa ~/.local/bin/    # or anywhere on your PATH
```

## Get started

```sh
wa --pair              # scan the QR code: WhatsApp > Settings > Linked devices
wa --install-daemon    # macOS: keep mirroring in the background, across reboots
wa --status            # linked? how fresh is the mirror?
```

The background daemon is a launchd agent, so `--install-daemon` is macOS only.
Elsewhere, run `wa serve` under the service manager of your choice.

## Read

```
wa                          the 20 latest messages
wa alex                     the 20 latest with Alex (a bare word is --from)
wa --threads                one line per conversation
wa --search "dinner" --hours 48
wa --since 2026-08-01 --until 2026-08-15 --limit 200
wa --json                   machine-readable output
```

**`--limit` always applies, date windows included, and defaults to 20.** A
window asked without `--limit` returns the 20 most recent messages inside it,
silently. Anything you want to conclude from ("nothing happened that day") needs
a wide `--limit`.

A message you sent shows `(delivered hh:mm)` or `(read hh:mm)` once the other
side says so. No mark does not mean unread: receipts are only pushed live, so a
message sent while the mirror was down never gets one, and people can turn read
receipts off.

## Send

```
wa --send alex "see you at 8"
wa --send alex --image photo.jpg --text "look"
wa --send 120363000000000001@g.us "hello group"
```

A name that matches several people is refused rather than guessed. A group you
have never written to needs its full JID, `@g.us` included.

## Pull older history

At pairing, the phone only pushes recent conversations. To go further back:

```
wa --backfill alex                  up to 500 older messages, 50 per request
wa --backfill alex --count 2000
wa --backfill --all --limit 20      the 20 liveliest conversations
wa --backfill alex --recent         re-read the latest messages, for missed attachments
```

Your phone must be online: WhatsApp keeps no messages on its servers, so the
phone is the only place old history exists. A conversation with no message in
the mirror has nothing to anchor on and cannot be deepened yet.

## Last seen

```
wa --last-seen alex
wa --last-seen --top 10
```

Presence is pushed by WhatsApp, never stored, so there is no history: only what
the server says right now. Asking marks **you** online for the few seconds the
command runs. A hidden "last seen" and no answer at all look alike from here.

## Good to know

- **Everything lives in `~/.wa/`**: `wa.db` holds the session keys and your
  messages **in plain text**, `media/` the pictures, voice notes and documents
  received in the last 30 days, and `wa.log` the daemon log. Only your disk
  encryption protects them.
- **One connection per companion device.** `--send`, `--backfill` and
  `--last-seen` pause the background daemon while they run, then restart it.
- **One person can have two conversations**: their phone number and the `@lid`
  alias WhatsApp now hands out. `--backfill <name>` digs through both.
- **Edits and deletions** rewrite the original row: an edited message shows
  `(edited hh:mm)`, a deleted one becomes `[deleted]` and its text and file are
  dropped.
- **Reactions** arrive live but are not part of history sync, so an imported
  thread can look unanswered when the app shows a heart on it.
- **The link takes one of your "Linked devices" slots**, visible and revocable
  from your phone. `wa --logout` unlinks from this side.

## License

MIT. whatsmeow is licensed under MPL-2.0.
