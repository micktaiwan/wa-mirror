package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seed(t *testing.T, db *sql.DB, id, body string) {
	t.Helper()
	err := putMessage(context.Background(), db, storedMessage{
		ID: id, ChatJID: "c@lid", SenderJID: "s@lid", TS: 100, Kind: "text", Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readBack(t *testing.T, db *sql.DB, id string) (kind, body string, edited, deleted int64) {
	t.Helper()
	err := db.QueryRow(`SELECT kind, body, edited_at, deleted_at FROM messages WHERE id = ?`, id).
		Scan(&kind, &body, &edited, &deleted)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// An edit replaces the text in place, keeps the id, and leaves a mark.
func TestApplyEdit(t *testing.T) {
	db := testDB(t)
	seed(t, db, "M1", "see you tomorow")

	ok, err := applyEdit(context.Background(), db, "c@lid", "M1", "text", "see you tomorrow", 200)
	if err != nil || !ok {
		t.Fatalf("applyEdit: ok=%v err=%v", ok, err)
	}
	kind, body, edited, deleted := readBack(t, db, "M1")
	if body != "see you tomorrow" || kind != "text" || edited != 200 || deleted != 0 {
		t.Fatalf("got kind=%q body=%q edited=%d deleted=%d", kind, body, edited, deleted)
	}
}

// An edit for a message the mirror never saw changes nothing and says so.
func TestApplyEditUnknownTarget(t *testing.T) {
	db := testDB(t)
	ok, err := applyEdit(context.Background(), db, "c@lid", "NOPE", "text", "x", 200)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an unknown target must not report an applied edit")
	}
}

// A delete-for-everyone drops the text rather than keeping a copy.
func TestApplyRevoke(t *testing.T) {
	db := testDB(t)
	seed(t, db, "M2", "oops")

	ok, err := applyRevoke(context.Background(), db, "c@lid", "M2", 300)
	if err != nil || !ok {
		t.Fatalf("applyRevoke: ok=%v err=%v", ok, err)
	}
	kind, body, _, deleted := readBack(t, db, "M2")
	if kind != "deleted" || body != "" || deleted != 300 {
		t.Fatalf("got kind=%q body=%q deleted=%d", kind, body, deleted)
	}
}

// The protocol message that carries an edit must be recognised as one, and the
// replacement text pulled out of it. This is the shape whatsmeow hands over on
// the live path, and the one that used to fall through and be dropped.
func TestEditTargetReadsEdit(t *testing.T) {
	msg := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:           &waCommon.MessageKey{ID: proto.String("M1")},
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String("fixed")},
	}}
	id, kind, body, revoked, ok := editTarget(msg)
	if !ok || revoked || id != "M1" || kind != "text" || body != "fixed" {
		t.Fatalf("got id=%q kind=%q body=%q revoked=%v ok=%v", id, kind, body, revoked, ok)
	}
}

func TestEditTargetReadsRevoke(t *testing.T) {
	msg := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("M2")},
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
	}}
	id, _, _, revoked, ok := editTarget(msg)
	if !ok || !revoked || id != "M2" {
		t.Fatalf("got id=%q revoked=%v ok=%v", id, revoked, ok)
	}
}

// A plain message is not an edit, and must go down the normal path.
func TestEditTargetIgnoresPlainMessage(t *testing.T) {
	if _, _, _, _, ok := editTarget(&waE2E.Message{Conversation: proto.String("hello")}); ok {
		t.Fatal("a plain message must not be taken for an edit")
	}
}

// An unknown message type is named after the field it carries, instead of the
// opaque "other" that hid what was actually received.
func TestDescribeNamesUnknownType(t *testing.T) {
	kind, _ := describe(&waE2E.Message{PtvMessage: &waE2E.VideoMessage{}})
	if kind != "ptv" {
		t.Fatalf("got kind=%q, want ptv", kind)
	}
}
