package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

type mirror struct {
	cli *whatsmeow.Client
	db  *sql.DB

	// Media downloads run off the event handler, on one worker started at the
	// first attachment seen. See media.go.
	mediaQ    chan mediaJob
	mediaWG   sync.WaitGroup
	mediaOnce sync.Once
	mediaStop sync.Once

	// histMsgs counts the messages taken out of history blobs, so a caller can
	// tell an answer that carried something from one that came back empty.
	histMsgs atomic.Int64
}

// connect opens the store and builds a client for the linked device, if any.
func connect(ctx context.Context, verbose bool) (*mirror, error) {
	db, err := openDB()
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	// What the phone shows in "Linked devices"; the library defaults to "whatsmeow".
	store.DeviceProps.Os = proto.String("wa (Mac)")

	level := "ERROR"
	if verbose {
		level = "INFO"
	}
	log := waLog.Stdout("wa", level, true)

	container := sqlstore.NewWithDB(db, "sqlite", log)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade session store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("read device: %w", err)
	}
	cli := whatsmeow.NewClient(device, log)
	return &mirror{cli: cli, db: db}, nil
}

func (m *mirror) linked() bool {
	return m.cli.Store.ID != nil
}

// run connects, mirrors everything that arrives, and blocks until interrupted.
// When the store holds no device yet, it prints a QR code to link one.
func (m *mirror) run(ctx context.Context, pairOnly bool) error {
	m.cli.AddEventHandler(m.handle)

	if !m.linked() {
		qrChan, err := m.cli.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("qr channel: %w", err)
		}
		if err := m.cli.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		paired := false
		for item := range qrChan {
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				fmt.Println()
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
				fmt.Println("\nWhatsApp > Settings > Linked devices > Link a device, then scan this.")
			case "success":
				paired = true
			case whatsmeow.QRChannelEventError:
				return fmt.Errorf("pairing failed: %w", item.Error)
			default:
				if item.Error != nil {
					return fmt.Errorf("pairing failed: %w", item.Error)
				}
			}
			if paired {
				break
			}
		}
		if !paired {
			return fmt.Errorf("pairing did not complete")
		}
		// Prove the device really landed in the store: a phone that lists a linked
		// device tells us nothing about what this machine managed to persist.
		var stored string
		if err := m.db.QueryRow(`SELECT jid FROM whatsmeow_device LIMIT 1`).Scan(&stored); err != nil || stored == "" {
			return fmt.Errorf("the phone accepted the link but nothing was saved in %s — "+
				"unlink the device on the phone and try again (%v)", dbPath(), err)
		}
		fmt.Printf("Linked as %s and saved. Pulling history — leave this open.\n", shortJID(stored))
	} else {
		if err := m.cli.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}

	if pairOnly {
		// Stay up long enough for the first history sync blobs to land, and say out
		// loud what is arriving: a silent terminal looks exactly like a failure.
		deadline := time.After(3 * time.Minute)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for done := false; !done; {
			select {
			case <-ctx.Done():
				done = true
			case <-deadline:
				fmt.Println("Initial sync window over. The background mirror takes it from here.")
				done = true
			case <-tick.C:
				var msgs, chats int
				_ = m.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs)
				_ = m.db.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chats)
				fmt.Printf("  %d messages, %d conversations so far…\n", msgs, chats)
			}
		}
		m.stopMedia()
		m.cli.Disconnect()
		return nil
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
	case <-ctx.Done():
	}
	// Pending downloads first: they need the connection that Disconnect closes.
	m.stopMedia()
	m.cli.Disconnect()
	return nil
}

func (m *mirror) handle(rawEvt any) {
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Connected:
		_ = setMeta(ctx, m.db, "last_connected", strconv.FormatInt(time.Now().Unix(), 10))
		go m.refreshNames(ctx)
	case *events.Message:
		m.storeMessage(ctx, evt)
	case *events.HistorySync:
		m.storeHistory(ctx, evt)
	case *events.Receipt:
		m.storeReceipt(ctx, evt)
	case *events.LoggedOut:
		fmt.Fprintln(os.Stderr, "wa: this device was unlinked from the phone — run `wa --pair` again")
	}
}

// storeReceipt records what a recipient told us about messages we sent: two
// ticks (delivered) and blue ticks (read). This is the only place that
// knowledge exists — receipts are pushed live and never replayed, so nothing
// stamped here can be recovered afterwards, and a message sent while the daemon
// was down stays blank forever.
//
// Only receipts coming from someone else are ours to store: IsFromMe marks the
// ones our own other devices send when you read a chat on your phone, which
// says nothing about the recipient.
func (m *mirror) storeReceipt(ctx context.Context, evt *events.Receipt) {
	if evt.IsFromMe {
		return
	}
	at := evt.Timestamp.Unix()
	chatJID := evt.Chat.String()
	for _, id := range evt.MessageIDs {
		var err error
		switch evt.Type {
		case types.ReceiptTypeDelivered:
			err = markDelivered(ctx, m.db, chatJID, id, at)
		case types.ReceiptTypeRead, types.ReceiptTypePlayed:
			err = markRead(ctx, m.db, chatJID, id, at)
		default:
			// sender, retry, server-error, inactive: about routing, not about
			// whether a human saw the message.
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "wa: store receipt: %v\n", err)
			return
		}
	}
}

func (m *mirror) storeMessage(ctx context.Context, evt *events.Message) {
	chatJID := evt.Info.Chat.String()

	// An edit or a delete-for-everyone is not a message, it is a correction to
	// one already stored — apply it and stop. Only the live path needs this:
	// ParseWebMessage already rewrites the id and unwraps the content when the
	// same event comes back through a history sync, so an edit received while
	// the daemon was down repairs itself on the next backfill.
	if targetID, kind, body, revoked, ok := editTarget(evt.Message); ok {
		at := evt.Info.Timestamp.Unix()
		var applied bool
		var err error
		if revoked {
			applied, err = applyRevoke(ctx, m.db, chatJID, targetID, at)
		} else {
			applied, err = applyEdit(ctx, m.db, chatJID, targetID, kind, body, at)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "wa: apply edit: %v\n", err)
		} else if !applied {
			// The edited message predates the mirror. Nothing to correct, and
			// nothing to invent: --backfill is what brings it in.
			fmt.Fprintf(os.Stderr, "wa: edit for an unknown message %s in %s\n", targetID, chatJID)
		}
		return
	}

	kind, body := describe(evt.Message)
	if kind == "" {
		return
	}
	chat := evt.Info.Chat
	sm := storedMessage{
		ID:         evt.Info.ID,
		ChatJID:    chatJID,
		SenderJID:  evt.Info.Sender.ToNonAD().String(),
		SenderName: evt.Info.PushName,
		FromMe:     evt.Info.IsFromMe,
		IsGroup:    evt.Info.IsGroup,
		TS:         evt.Info.Timestamp.Unix(),
		Kind:       kind,
		Body:       body,
	}
	if err := putMessage(ctx, m.db, sm); err != nil {
		fmt.Fprintf(os.Stderr, "wa: store message: %v\n", err)
		return
	}
	m.queueMedia(ctx, sm.ID, sm.ChatJID, sm.TS, evt.Message)
	_ = putChat(ctx, m.db, chat.String(), "", evt.Info.IsGroup, sm.TS)
	if !evt.Info.IsGroup && !evt.Info.IsFromMe && evt.Info.PushName != "" {
		_ = putContact(ctx, m.db, sm.SenderJID, evt.Info.PushName)
	}
	_ = setMeta(ctx, m.db, "last_message", strconv.FormatInt(time.Now().Unix(), 10))
}

// storeHistory ingests the blobs of past conversations the phone pushes after linking.
func (m *mirror) storeHistory(ctx context.Context, evt *events.HistorySync) {
	if os.Getenv("WA_DEBUG") != "" {
		n := 0
		for _, conv := range evt.Data.GetConversations() {
			n += len(conv.GetMessages())
		}
		fmt.Fprintf(os.Stderr, "wa: history blob type=%s conversations=%d messages=%d\n",
			evt.Data.GetSyncType(), len(evt.Data.GetConversations()), n)
	}
	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		isGroup := chatJID.Server == types.GroupServer
		_ = putChat(ctx, m.db, chatJID.String(), conv.GetName(), isGroup, int64(conv.GetLastMsgTimestamp()))

		for _, histMsg := range conv.GetMessages() {
			parsed, err := m.cli.ParseWebMessage(chatJID, histMsg.GetMessage())
			if err != nil {
				if os.Getenv("WA_DEBUG") != "" {
					fmt.Fprintf(os.Stderr, "wa: parse web message: %v\n", err)
				}
				continue
			}
			m.histMsgs.Add(1)
			m.storeMessage(ctx, parsed)
		}
	}
}

// refreshNames repopulates the contact and group name tables on every connection,
// so reads never need a live client to show who is who.
func (m *mirror) refreshNames(ctx context.Context) {
	contacts, err := m.cli.Store.Contacts.GetAllContacts(ctx)
	if err == nil {
		for jid, info := range contacts {
			_ = putContact(ctx, m.db, jid.ToNonAD().String(), contactName(info))
		}
	}
	groups, err := m.cli.GetJoinedGroups(ctx)
	if err == nil {
		for _, g := range groups {
			_ = putChat(ctx, m.db, g.JID.String(), g.Name, true, 0)
		}
	}
	if id := m.cli.Store.ID; id != nil {
		_ = setMeta(ctx, m.db, "own_jid", id.ToNonAD().String())
	}
}

func contactName(info types.ContactInfo) string {
	for _, candidate := range []string{info.FullName, info.BusinessName, info.PushName, info.FirstName} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}
