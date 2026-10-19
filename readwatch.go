package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// A read watch waits for the blue ticks on one message we sent, and raises a
// desktop alert when they come. It is armed from the command line and fired by
// the daemon, which is the only process that sees receipts: the two meet in the
// read_watches table, so arming needs no connection and no restart.
//
// Like the message alerts, it names the person and never quotes the text.

const readWatchSchema = `
CREATE TABLE IF NOT EXISTS read_watches (
	id         TEXT    NOT NULL,
	chat_jid   TEXT    NOT NULL,
	name       TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (id, chat_jid)
);`

// fireReadWatch is called by the daemon for each message a read receipt names.
// The id alone is matched: one person can hold two conversations (number and
// @lid alias), and the receipt may come back on the other one.
func (m *mirror) fireReadWatch(ctx context.Context, id string, at int64) {
	var name string
	err := m.db.QueryRowContext(ctx, `SELECT name FROM read_watches WHERE id = ?`, id).Scan(&name)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "wa: read watch: %v\n", err)
		return
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM read_watches WHERE id = ?`, id); err != nil {
		fmt.Fprintf(os.Stderr, "wa: read watch: %v\n", err)
	}
	text := fmt.Sprintf("%s read your message (%s)", name, time.Unix(at, 0).Format("15:04"))
	if err := showAlert("WhatsApp", text, "wa-read-"+id); err != nil {
		fmt.Fprintf(os.Stderr, "wa: desktop alert: %v\n", err)
	}
}

// notifyRead arms a watch on the last message sent to someone, lists the
// armed watches, or cancels them.
func notifyRead(ctx context.Context, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}
	if len(args) == 0 {
		return listReadWatches(ctx, db)
	}
	if args[0] == "--cancel" {
		if len(args) < 2 {
			return fmt.Errorf("--cancel takes a name, or all")
		}
		return cancelReadWatches(ctx, db, strings.Join(args[1:], " "))
	}
	return armReadWatch(ctx, db, strings.Join(args, " "))
}

func armReadWatch(ctx context.Context, db *sql.DB, who string) error {
	targets, err := conversationsNamed(ctx, db, who)
	if err != nil {
		return err
	}
	jids := make([]any, 0, len(targets))
	marks := make([]string, 0, len(targets))
	for _, t := range targets {
		jids = append(jids, t.jid)
		marks = append(marks, "?")
	}
	var id, chat, body string
	var ts, readAt int64
	err = db.QueryRowContext(ctx, `
		SELECT id, chat_jid, body, ts, read_at FROM messages
		WHERE from_me = 1 AND deleted_at = 0 AND kind <> 'reaction' AND chat_jid IN (`+strings.Join(marks, ",")+`)
		ORDER BY ts DESC LIMIT 1`, jids...).Scan(&id, &chat, &body, &ts, &readAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no message sent to %s in the mirror", targets[0].name)
	}
	if err != nil {
		return err
	}
	name := targets[0].name
	sent := fmt.Sprintf("%s  %s", stamp(ts), truncate(strings.ReplaceAll(body, "\n", " "), 60))
	if readAt != 0 {
		fmt.Printf("already read at %s: %s\n", stamp(readAt), sent)
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO read_watches (id, chat_jid, name, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id, chat_jid) DO NOTHING`, id, chat, name, time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("watching %s: an alert when they read %s\n", name, sent)
	if getMeta(ctx, db, "last_connected") == "" {
		fmt.Println("note: the mirror has never connected — the alert needs `wa serve` running")
	}
	return nil
}

func listReadWatches(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT w.name, COALESCE(m.ts, 0), COALESCE(m.body, '')
		FROM read_watches w LEFT JOIN messages m ON m.id = w.id AND m.chat_jid = w.chat_jid
		ORDER BY w.created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, body string
		var ts int64
		if err := rows.Scan(&name, &ts, &body); err != nil {
			return err
		}
		fmt.Printf("%s  %s  %s\n", name, stamp(ts), truncate(strings.ReplaceAll(body, "\n", " "), 60))
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("no read watch armed")
	}
	return nil
}

func cancelReadWatches(ctx context.Context, db *sql.DB, who string) error {
	var res sql.Result
	var err error
	if who == "all" {
		res, err = db.ExecContext(ctx, `DELETE FROM read_watches`)
	} else {
		res, err = db.ExecContext(ctx, `DELETE FROM read_watches WHERE name LIKE ?`, "%"+who+"%")
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	fmt.Printf("%d read watch(es) cancelled\n", n)
	return nil
}
