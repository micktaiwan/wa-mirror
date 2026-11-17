package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestRunHookPassesMessageOnStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "got.json")
	msg := hookMessage{
		ID: "3EB0000000000000000001", Chat: "33612345678@s.whatsapp.net",
		Sender: "33612345678@s.whatsapp.net", SenderPhone: "33612345678",
		SenderName: "Alex", Timestamp: 1700000000, Kind: "text", Body: "hola",
	}
	runHook("cat > "+out, msg)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook wrote nothing: %v", err)
	}
	var got hookMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("stdin is not one JSON object: %v: %q", err, raw)
	}
	if got != msg {
		t.Fatalf("got %+v, want %+v", got, msg)
	}
}

func TestRunHookSurvivesAFailingCommand(t *testing.T) {
	runHook("exit 3", hookMessage{ID: "x"})
}

func TestPhoneOf(t *testing.T) {
	cases := []struct {
		jid  types.JID
		want string
	}{
		{types.NewJID("33612345678", types.DefaultUserServer), "33612345678"},
		{types.NewJID("123456789012345", types.HiddenUserServer), ""},
		{types.NewJID("120363000000000001", types.GroupServer), ""},
	}
	for _, c := range cases {
		if got := phoneOf(c.jid); got != c.want {
			t.Errorf("phoneOf(%s) = %q, want %q", c.jid, got, c.want)
		}
	}
}
