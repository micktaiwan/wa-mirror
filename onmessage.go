package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A hook: a shell command the daemon runs for every message received live, with
// the message as one JSON object on its standard input. It is how another program
// reacts to a message the moment it lands, without polling the database.
//
// The command is stored in the database like the --notify setting, so the
// daemon reads it at every message and a change needs no restart.

const onMessageMetaKey = "on_message"

// A hook that hangs must not pile up processes: it is killed after this long.
const onMessageTimeout = 60 * time.Second

// hookMessage is what the command reads on stdin. Addresses are given both as
// received and, for a sender behind an @lid alias, as a phone number, so a
// script can match a person by number whichever address the message came on.
type hookMessage struct {
	ID          string `json:"id"`
	Chat        string `json:"chat"`
	Sender      string `json:"sender"`
	SenderPhone string `json:"sender_phone,omitempty"`
	SenderName  string `json:"sender_name"`
	IsGroup     bool   `json:"is_group"`
	Timestamp   int64  `json:"ts"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	// FromMe marks a message written from this account, on the phone or another
	// linked device. ChatPhone is the number of a one-to-one chat, so a script can
	// tell which conversation an own message went to.
	FromMe    bool   `json:"from_me,omitempty"`
	ChatPhone string `json:"chat_phone,omitempty"`
}

// onMessage runs the hook for a message received live, when one is set.
// Replays after downtime, statuses and reactions are skipped, as for desktop
// alerts. Your own messages are passed on, marked from_me, for a script that
// takes orders from you as well as from the other side.
func (m *mirror) onMessage(ctx context.Context, evt *events.Message) {
	command := getMeta(ctx, m.db, onMessageMetaKey)
	if command == "" {
		return
	}
	info := evt.Info
	if info.Chat == types.StatusBroadcastJID || info.Chat.Server == types.BroadcastServer {
		return
	}
	if time.Since(info.Timestamp) > notifyMaxAge {
		return
	}
	kind, body := describe(evt.Message)
	if kind == "" || kind == "reaction" {
		return
	}
	sender := info.Sender.ToNonAD()
	msg := hookMessage{
		ID:         info.ID,
		Chat:       info.Chat.String(),
		Sender:     sender.String(),
		SenderName: m.personName(ctx, sender, info.PushName),
		IsGroup:    info.IsGroup,
		Timestamp:  info.Timestamp.Unix(),
		Kind:       kind,
		Body:       body,
		FromMe:     info.IsFromMe,
	}
	msg.SenderPhone = m.phoneFor(ctx, sender)
	if !info.IsGroup {
		msg.ChatPhone = m.phoneFor(ctx, info.Chat.ToNonAD())
	}
	go runHook(command, msg)
}

// phoneFor is the number behind an address, resolving an @lid alias when known.
func (m *mirror) phoneFor(ctx context.Context, jid types.JID) string {
	if jid.Server == types.HiddenUserServer {
		if pn, err := m.cli.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			return pn.User
		}
	}
	return phoneOf(jid)
}

// phoneOf is the number of a phone-number address, empty for any other kind.
func phoneOf(jid types.JID) string {
	if jid.Server == types.DefaultUserServer {
		return jid.User
	}
	return ""
}

func runHook(command string, msg hookMessage) {
	payload, err := json.Marshal(msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wa: on-message hook: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), onMessageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "wa: on-message hook failed: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
}

// onMessageSetting sets, clears or shows the hook command.
func onMessageSetting(ctx context.Context, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}
	if len(args) == 0 {
		return printOnMessageSetting(ctx, db)
	}
	command := strings.TrimSpace(strings.Join(args, " "))
	if command == "off" {
		command = ""
	}
	if err := setMeta(ctx, db, onMessageMetaKey, command); err != nil {
		return err
	}
	return printOnMessageSetting(ctx, db)
}

func printOnMessageSetting(ctx context.Context, db *sql.DB) error {
	command := getMeta(ctx, db, onMessageMetaKey)
	if command == "" {
		fmt.Println("on-message hook: off")
		return nil
	}
	fmt.Printf("on-message hook: %s\n", command)
	return nil
}
