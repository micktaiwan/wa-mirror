package main

import (
	"context"
	"testing"
)

// A message filed under both addresses of one person is read back once,
// and --limit counts it once.
func TestQueryDedupesCopiesAcrossAliases(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, chat := range []string{"111@lid", "33612345678@s.whatsapp.net"} {
		for i, id := range []string{"M1", "M2"} {
			err := putMessage(ctx, db, storedMessage{
				ID: id, ChatJID: chat, SenderJID: "222@lid", TS: int64(100 + i), Kind: "text", Body: "hello " + id,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := query(ctx, db, filter{limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "M1" || got[1].ID != "M2" {
		t.Fatalf("want M1, M2 once each, got %+v", got)
	}
	if got[0].ChatJID != "33612345678@s.whatsapp.net" {
		t.Fatalf("want the phone-number copy kept, got %s", got[0].ChatJID)
	}
}
