package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Desktop alerts say who wrote, never what: the body stays out of the
// notification centre, its history and any screen someone else can see.

const notifyMetaKey = "notify"

// A message older than this when it arrives is a replay after downtime, not
// news: alerting on it would fire a burst of stale banners at reconnection.
const notifyMaxAge = 10 * time.Minute

const whatsAppBundleID = "net.whatsapp.WhatsApp"

// notify raises a desktop alert for a message received live, when alerts are on.
// It is called from the live path only, never from history sync.
func (m *mirror) notify(ctx context.Context, evt *events.Message) {
	if getMeta(ctx, m.db, notifyMetaKey) != "on" {
		return
	}
	info := evt.Info
	if info.IsFromMe || info.Chat == types.StatusBroadcastJID || info.Chat.Server == types.BroadcastServer {
		return
	}
	if time.Since(info.Timestamp) > notifyMaxAge {
		return
	}
	kind, _ := describe(evt.Message)
	if kind == "" || kind == "reaction" {
		return
	}
	if settings, err := m.cli.Store.ChatSettings.GetChatSettings(ctx, info.Chat); err == nil &&
		settings.Found && settings.MutedUntil.After(time.Now()) {
		return
	}

	sender := m.personName(ctx, info.Sender.ToNonAD(), info.PushName)
	text := "Message from " + sender
	if info.IsGroup {
		text += " in " + m.groupName(ctx, info.Chat)
	}
	if err := showAlert("WhatsApp", text, info.Chat.String()); err != nil {
		fmt.Fprintf(os.Stderr, "wa: desktop alert: %v\n", err)
	}
}

// personName is the name the address book gives this person, trying their
// phone number when the message came from their @lid alias.
func (m *mirror) personName(ctx context.Context, jid types.JID, pushName string) string {
	candidates := []types.JID{jid}
	if jid.Server == types.HiddenUserServer {
		if pn, err := m.cli.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			candidates = append(candidates, pn)
		}
	}
	for _, c := range candidates {
		if info, err := m.cli.Store.Contacts.GetContact(ctx, c); err == nil {
			if name := contactName(info); name != "" {
				return name
			}
		}
	}
	if pushName != "" {
		return pushName
	}
	return shortJID(jid.String())
}

func (m *mirror) groupName(ctx context.Context, chat types.JID) string {
	var name string
	err := m.db.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, chat.String()).Scan(&name)
	if err == nil && name != "" {
		return name
	}
	return "a group"
}

// showAlert prefers terminal-notifier, which groups banners per conversation and
// opens WhatsApp on click, and falls back to AppleScript, which ships with macOS.
// launchd hands agents a bare PATH, hence the Homebrew locations tried by hand.
func showAlert(title, text, group string) error {
	notifier := ""
	if p, err := exec.LookPath("terminal-notifier"); err == nil {
		notifier = p
	} else {
		for _, p := range []string{"/opt/homebrew/bin/terminal-notifier", "/usr/local/bin/terminal-notifier"} {
			if _, err := os.Stat(p); err == nil {
				notifier = p
				break
			}
		}
	}
	if notifier != "" {
		return exec.Command(notifier, "-title", title, "-message", text,
			"-group", group, "-activate", whatsAppBundleID, "-sound", "default").Run()
	}
	// Names travel as arguments, never spliced into the script.
	return exec.Command("osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv) sound name \"default\"",
		"-e", "end run",
		title, text).Run()
}

// notifySetting turns desktop alerts on or off, or says where they stand. The
// daemon reads the setting at every message, so no restart is needed.
func notifySetting(ctx context.Context, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}
	if len(args) == 0 {
		return printNotifySetting(ctx, db)
	}
	switch args[0] {
	case "on", "off":
		if err := setMeta(ctx, db, notifyMetaKey, args[0]); err != nil {
			return err
		}
		return printNotifySetting(ctx, db)
	case "test":
		return showAlert("WhatsApp", "Message from Alex in Example group", "wa-test")
	}
	return fmt.Errorf("--notify takes on, off or test")
}

func printNotifySetting(ctx context.Context, db *sql.DB) error {
	state := getMeta(ctx, db, notifyMetaKey)
	if state != "on" {
		state = "off"
	}
	fmt.Printf("desktop alerts: %s\n", state)
	return nil
}
