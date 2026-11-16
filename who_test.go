package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestWhoIsResolvesLIDPushName(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender_jid TEXT, sender_name TEXT, from_me INTEGER, ts INTEGER)`,
		`CREATE TABLE contacts (jid TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`,
		`INSERT INTO whatsmeow_lid_map VALUES ('111', '33612345678')`,
		`INSERT INTO contacts VALUES ('33612345678@s.whatsapp.net', 'Alex Doe')`,
		`INSERT INTO messages VALUES ('a', 'status@broadcast', '111@lid', 'Lex', 0, 100)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{"Lex", "Alex", "33612345678"} {
		got, err := whoIs(context.Background(), db, text)
		if err != nil || len(got) != 1 {
			t.Fatalf("%q: %v %v", text, got, err)
		}
		p := got[0]
		if p.Number != "33612345678" || p.Contact != "Alex Doe" || p.Pushname != "Lex" || p.Statuses != 1 {
			t.Fatalf("%q: %+v", text, p)
		}
	}
}
