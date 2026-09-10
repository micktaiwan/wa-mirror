package main

import (
	"context"
	"database/sql"
	"testing"
)

// seedChat records one conversation the way the mirror would, so that recipient
// resolution can be exercised without a phone at the other end.
func seedChat(t *testing.T, db *sql.DB, jid, name string, isGroup bool, lastTS int64) {
	t.Helper()
	if err := putChat(context.Background(), db, jid, name, isGroup, lastTS); err != nil {
		t.Fatal(err)
	}
}

// A group id typed on its own used to be read as a phone number and sent to
// @s.whatsapp.net, where the server answers "no LID found" for a group that is
// right there in the database.
func TestResolveRecipientFindsGroupByBareID(t *testing.T) {
	db := testDB(t)
	seedChat(t, db, "120363000000000001@g.us", "Book club", true, 500)

	jid, name, err := resolveRecipient(context.Background(), db, "120363000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got := jid.String(); got != "120363000000000001@g.us" {
		t.Errorf("got %q, want the group's own address", got)
	}
	if name != "Book club" {
		t.Errorf("got name %q, want %q", name, "Book club")
	}
}

// A phone number keeps the address it has always been reached at, even when an
// @lid alias for the same person is on file and was used more recently.
func TestResolveRecipientPrefersPhoneServer(t *testing.T) {
	db := testDB(t)
	seedChat(t, db, "33612345678@lid", "Alex", false, 900)
	seedChat(t, db, "33612345678@s.whatsapp.net", "Alex", false, 100)

	jid, _, err := resolveRecipient(context.Background(), db, "33612345678")
	if err != nil {
		t.Fatal(err)
	}
	if got := jid.String(); got != "33612345678@s.whatsapp.net" {
		t.Errorf("got %q, want the phone-number address", got)
	}
}

// An id the mirror has never seen stays a phone number, which is what someone
// writing to a new contact means.
func TestResolveRecipientUnknownIDStaysAPhoneNumber(t *testing.T) {
	db := testDB(t)

	jid, _, err := resolveRecipient(context.Background(), db, "33698765432")
	if err != nil {
		t.Fatal(err)
	}
	if got := jid.String(); got != "33698765432@s.whatsapp.net" {
		t.Errorf("got %q, want the phone-number address", got)
	}
}
