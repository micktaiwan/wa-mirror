package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dbPath is where both the whatsmeow session and the mirrored messages live.
func dbPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	dir := filepath.Join(home, ".wa")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	return filepath.Join(dir, "wa.db")
}

func dbAddress() string {
	return fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", dbPath())
}

func openDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbAddress())
	if err != nil {
		return nil, err
	}
	// modernc's driver is not safe for unlimited parallel writers on one file.
	db.SetMaxOpenConns(1)
	return db, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id          TEXT    NOT NULL,
	chat_jid    TEXT    NOT NULL,
	sender_jid  TEXT    NOT NULL DEFAULT '',
	sender_name TEXT    NOT NULL DEFAULT '',
	from_me     INTEGER NOT NULL DEFAULT 0,
	is_group    INTEGER NOT NULL DEFAULT 0,
	ts          INTEGER NOT NULL,
	kind        TEXT    NOT NULL DEFAULT 'text',
	body        TEXT    NOT NULL DEFAULT '',
	edited_at   INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER NOT NULL DEFAULT 0,
	media_path  TEXT    NOT NULL DEFAULT '',
	delivered_at INTEGER NOT NULL DEFAULT 0,
	read_at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (id, chat_jid)
);
CREATE INDEX IF NOT EXISTS messages_ts       ON messages (ts);
CREATE INDEX IF NOT EXISTS messages_chat_ts  ON messages (chat_jid, ts);

CREATE TABLE IF NOT EXISTS chats (
	jid      TEXT PRIMARY KEY,
	name     TEXT    NOT NULL DEFAULT '',
	is_group INTEGER NOT NULL DEFAULT 0,
	last_ts  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS contacts (
	jid  TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Columns added after the first databases were created. SQLite has no
	// ADD COLUMN IF NOT EXISTS, and a duplicate column is the normal answer on
	// every run but the first, so it is the one error worth swallowing.
	for _, stmt := range []string{
		`ALTER TABLE messages ADD COLUMN edited_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN media_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN delivered_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN read_at INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// applyEdit rewrites a message that its sender edited afterwards.
//
// The row keeps its original id and timestamp: what the reader needs is the
// text as it stands now, plus the fact that it was changed. Storing the edit as
// a message of its own would leave the stale version in place, which is how a
// corrected typo stayed visible for a whole conversation.
func applyEdit(ctx context.Context, db *sql.DB, chatJID, targetID, kind, body string, at int64) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE messages SET body = ?, kind = ?, edited_at = ?
		WHERE id = ? AND chat_jid = ?`, body, kind, at, targetID, chatJID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// applyRevoke marks a message deleted for everyone. The body is dropped, since
// keeping a copy of what someone withdrew is not ours to do.
func applyRevoke(ctx context.Context, db *sql.DB, chatJID, targetID string, at int64) (bool, error) {
	var file string
	_ = db.QueryRowContext(ctx,
		`SELECT media_path FROM messages WHERE id = ? AND chat_jid = ?`, targetID, chatJID).Scan(&file)
	res, err := db.ExecContext(ctx, `
		UPDATE messages SET body = '', kind = 'deleted', deleted_at = ?, media_path = ''
		WHERE id = ? AND chat_jid = ?`, at, targetID, chatJID)
	if file != "" {
		// Same reason the body is dropped: keeping the picture someone withdrew
		// is not ours to do.
		_ = os.Remove(mediaFullPath(file))
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// markDelivered and markRead stamp what the recipients told us about a message
// we sent. Both keep the first stamp they got: what matters is when it reached
// them, not the last device to confirm it, and in a group the first answer is
// the only one that means anything simple ("someone read it").
//
// Reading implies delivery, so a read receipt fills an empty delivered_at too:
// WhatsApp does not always send both, and a message shown as read but never
// delivered would read as a bug.
func markDelivered(ctx context.Context, db *sql.DB, chatJID, id string, at int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE messages SET delivered_at = ?
		WHERE id = ? AND chat_jid = ? AND from_me = 1 AND delivered_at = 0`, at, id, chatJID)
	return err
}

func markRead(ctx context.Context, db *sql.DB, chatJID, id string, at int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE messages
		SET read_at = ?, delivered_at = CASE WHEN delivered_at = 0 THEN ? ELSE delivered_at END
		WHERE id = ? AND chat_jid = ? AND from_me = 1 AND read_at = 0`, at, at, id, chatJID)
	return err
}

type storedMessage struct {
	ID         string
	ChatJID    string
	SenderJID  string
	SenderName string
	FromMe     bool
	IsGroup    bool
	TS         int64
	Kind       string
	Body       string
}

// putMessage inserts a message, keeping the first non-empty body we saw for it
// (history sync and live delivery can both carry the same message). media_path
// is deliberately absent: the file lands later, on the download worker, and a
// re-delivery of the same message must not wipe the path it already earned.
func putMessage(ctx context.Context, db *sql.DB, m storedMessage) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO messages (id, chat_jid, sender_jid, sender_name, from_me, is_group, ts, kind, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id, chat_jid) DO UPDATE SET
			body        = CASE WHEN excluded.body <> '' THEN excluded.body ELSE messages.body END,
			sender_name = CASE WHEN excluded.sender_name <> '' THEN excluded.sender_name ELSE messages.sender_name END,
			kind        = excluded.kind`,
		m.ID, m.ChatJID, m.SenderJID, m.SenderName, boolInt(m.FromMe), boolInt(m.IsGroup), m.TS, m.Kind, m.Body)
	return err
}

func putChat(ctx context.Context, db *sql.DB, jid, name string, isGroup bool, ts int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO chats (jid, name, is_group, last_ts)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (jid) DO UPDATE SET
			name     = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
			is_group = excluded.is_group,
			last_ts  = MAX(chats.last_ts, excluded.last_ts)`,
		jid, name, boolInt(isGroup), ts)
	return err
}

func putContact(ctx context.Context, db *sql.DB, jid, name string) error {
	if name == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO contacts (jid, name) VALUES (?, ?)
		ON CONFLICT (jid) DO UPDATE SET name = excluded.name`, jid, name)
	return err
}

func setMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func getMeta(ctx context.Context, db *sql.DB, key string) string {
	var v string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
