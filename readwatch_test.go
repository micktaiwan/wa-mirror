package main

import (
	"context"
	"testing"
)

func TestArmReadWatchTakesLastSentMessage(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	chat := "33612345678@s.whatsapp.net"
	if err := putChat(ctx, db, chat, "Alex", false, 300); err != nil {
		t.Fatal(err)
	}
	for _, m := range []storedMessage{
		{ID: "old", ChatJID: chat, FromMe: true, TS: 100, Kind: "text", Body: "first"},
		{ID: "theirs", ChatJID: chat, TS: 200, Kind: "text", Body: "reply"},
		{ID: "last", ChatJID: chat, FromMe: true, TS: 300, Kind: "text", Body: "second"},
	} {
		if err := putMessage(ctx, db, m); err != nil {
			t.Fatal(err)
		}
	}

	if err := armReadWatch(ctx, db, chat); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM read_watches`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != "last" {
		t.Fatalf("watch armed on %q, want the last message sent", id)
	}
}

func TestArmReadWatchSkipsAlreadyRead(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	chat := "33612345678@s.whatsapp.net"
	if err := putMessage(ctx, db, storedMessage{ID: "m1", ChatJID: chat, FromMe: true, TS: 100, Kind: "text", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := markRead(ctx, db, chat, "m1", 150); err != nil {
		t.Fatal(err)
	}
	if err := armReadWatch(ctx, db, chat); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM read_watches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d watch armed on a message already read", n)
	}
}
