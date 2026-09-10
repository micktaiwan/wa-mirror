package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// backfill deepens the mirror by asking the phone for the messages that come
// before the oldest ones already held. WhatsApp keeps nothing on its servers, so
// the primary device is the only place old conversations exist: a peer message
// asks it for a chunk of history, and it answers with the same HistorySync blob
// the linking pushes. Like sending, it stands the background mirror down for the
// duration, since a companion device holds one connection at a time.
func backfill(ctx context.Context, args []string) error {
	who := ""
	want, chunk, chats := 500, 50, 0
	all, recent := false, false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		number := func() (int, error) {
			v, err := value()
			if err != nil {
				return 0, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("%s is not a number", v)
			}
			return n, nil
		}
		var err error
		switch arg {
		case "--all":
			all = true
		case "--recent":
			recent = true
		case "--from", "--to":
			who, err = value()
		case "--count":
			want, err = number()
		case "--chunk":
			chunk, err = number()
		case "--limit":
			chats, err = number()
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown option %s — see `wa --help`", arg)
			}
			if who != "" {
				return fmt.Errorf("unexpected extra argument %q", arg)
			}
			who = arg
		}
		if err != nil {
			return err
		}
	}
	if who == "" && !all {
		return fmt.Errorf("which conversation? give a name, or --all for every one of them")
	}
	if who != "" && all {
		return fmt.Errorf("--all takes every conversation, so it cannot be narrowed to %q", who)
	}
	// The phone answers one chunk per request; asking for more than it sends
	// wastes a round trip. 50 is what the library recommends.
	if chunk > want {
		chunk = want
	}
	if recent {
		// One request, anchored at the other end of the conversation. Walking
		// backwards makes no sense here: the point is to see the last messages
		// again, not to go further back.
		if want > chunk {
			want = chunk
		}
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return err
	}

	var targets []backfillTarget
	if all {
		targets, err = conversationsByRecency(ctx, db, chats)
	} else {
		targets, err = conversationsNamed(ctx, db, who)
	}
	db.Close()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no conversation to deepen — run `wa --threads` to see what is held")
	}

	resume := pauseDaemon()
	defer resume()

	m, err := connect(ctx, false)
	if err != nil {
		return err
	}
	defer m.db.Close()
	if !m.linked() {
		return fmt.Errorf("not linked — run `wa --pair` first")
	}

	// Two handlers, in this order: the mirror stores what arrives, then the
	// second one says out loud that a blob landed. Whatsmeow runs them in
	// registration order, so by the time we are woken the rows are written.
	arrived := make(chan struct{}, 8)
	m.cli.AddEventHandler(m.handle)
	m.cli.AddEventHandler(func(raw any) {
		if _, ok := raw.(*events.HistorySync); ok {
			select {
			case arrived <- struct{}{}:
			default:
			}
		}
	})

	if err := m.cli.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer m.cli.Disconnect()
	if !m.cli.WaitForConnection(30 * time.Second) {
		return fmt.Errorf("could not reach WhatsApp within 30s")
	}

	if recent {
		for _, t := range targets {
			if err := pullRecent(ctx, m, t, want, arrived); err != nil {
				return err
			}
		}
		m.stopMedia()
		var files int
		_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE media_path <> ''`).Scan(&files)
		fmt.Printf("%d attachments held in total.\n", files)
		return nil
	}

	total := 0
	for _, t := range targets {
		added, err := deepen(ctx, m, t, want, chunk, arrived)
		total += added
		if err != nil {
			return err
		}
	}
	m.stopMedia()

	var held int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&held)
	fmt.Printf("Pulled %d older messages. %d held in total.\n", total, held)
	return nil
}

// pullRecent asks the phone to send the tail of a conversation again.
//
// Ordinary backfilling only ever walks backwards from the oldest message held,
// so it never revisits the recent ones — which is why anything the mirror
// mishandled while it was running stayed broken for good (an edit received by an
// old build, an attachment that arrived before downloads existed). Anchoring at
// the *newest* message asks for the window just before it, and the second pass
// through storeMessage repairs those rows in place.
func pullRecent(ctx context.Context, m *mirror, t backfillTarget, count int, arrived chan struct{}) error {
	chat, err := types.ParseJID(t.jid)
	if err != nil {
		return fmt.Errorf("%s is not a JID: %w", t.jid, err)
	}
	anchor, ok := newestMessage(ctx, m.db, t.jid)
	if !ok {
		fmt.Printf("  %-28s nothing held yet, so there is nothing to ask from\n", t.name)
		return nil
	}
	anchor.Chat = chat
	anchor.IsGroup = chat.Server == types.GroupServer

	before := countMedia(ctx, m.db, t.jid)
	got, answered := askHistory(ctx, m, anchor, count, arrived)
	if !answered {
		fmt.Printf("  %-28s the phone did not answer within a minute — is it online?\n", t.name)
		return nil
	}
	// A conversation the mirror files under the @lid alias is one the phone
	// knows by its phone number, and an on-demand request addressed to the alias
	// comes back empty. The message key is the same on both sides, so asking
	// again under the number is what actually reaches the thread.
	if got == 0 && chat.Server == types.HiddenUserServer {
		if pn, err := m.cli.Store.LIDs.GetPNForLID(ctx, chat); err == nil && !pn.IsEmpty() {
			anchor.Chat = pn
			if _, ok := askHistory(ctx, m, anchor, count, arrived); !ok {
				fmt.Printf("  %-28s the phone did not answer within a minute — is it online?\n", t.name)
				return nil
			}
		}
	}
	// Downloads are queued by the handler and run behind it; wait for them
	// before counting, or the number printed is always one pass late.
	m.stopMedia()
	files := countMedia(ctx, m.db, t.jid) - before
	fmt.Printf("  %-28s re-read the last %d messages, +%d attachment(s)\n", t.name, count, files)
	return nil
}

// askHistory sends one history request and waits for the blobs it triggers,
// reporting how many messages they carried.
func askHistory(ctx context.Context, m *mirror, anchor *types.MessageInfo, count int, arrived chan struct{}) (int, bool) {
	m.histMsgs.Store(0)
	drain(arrived)
	if _, err := m.cli.SendPeerMessage(ctx, m.cli.BuildHistorySyncRequest(anchor, count)); err != nil {
		return 0, false
	}
	if !waitFor(ctx, arrived, time.Minute) {
		return 0, false
	}
	// A single answer can arrive as several blobs; let the rest land.
	for waitFor(ctx, arrived, 3*time.Second) {
	}
	return int(m.histMsgs.Load()), true
}

// newestMessage is the anchor pullRecent asks from: the phone answers with what
// comes immediately before it.
func newestMessage(ctx context.Context, db *sql.DB, chatJID string) (*types.MessageInfo, bool) {
	var id string
	var fromMe int
	var ts int64
	err := db.QueryRowContext(ctx, `
		SELECT id, from_me, ts FROM messages
		WHERE chat_jid = ? ORDER BY ts DESC LIMIT 1`, chatJID).Scan(&id, &fromMe, &ts)
	if err != nil || id == "" {
		return nil, false
	}
	return &types.MessageInfo{
		ID:        id,
		Timestamp: time.Unix(ts, 0),
		MessageSource: types.MessageSource{
			IsFromMe: fromMe == 1,
		},
	}, true
}

func countMedia(ctx context.Context, db *sql.DB, chatJID string) int {
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE chat_jid = ? AND media_path <> ''`, chatJID).Scan(&n)
	return n
}

type backfillTarget struct {
	jid  string
	name string
}

// conversationsNamed is where deepening parts ways with sending: one message
// goes to one address, but a person can hold several conversations — their phone
// number and the @lid alias WhatsApp now hands out — and only one of them may
// carry the old history. So a name here means all of that person's addresses,
// not just the one they last wrote from.
func conversationsNamed(ctx context.Context, db *sql.DB, who string) ([]backfillTarget, error) {
	if strings.Contains(who, "@") || isPhoneNumber(who) {
		jid, name, err := resolveRecipient(ctx, db, who)
		if err != nil {
			return nil, err
		}
		return []backfillTarget{{jid: jid.String(), name: name}}, nil
	}

	found, err := candidates(ctx, db, who)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("nobody here matches %q — try `wa --threads` to see the names", who)
	}
	if narrowed := startingWith(found, who); len(narrowed) > 0 {
		found = narrowed
	}
	if len(found) > 1 && !sameName(found[0].name, names(found)) {
		var lines []string
		for _, f := range found {
			lines = append(lines, fmt.Sprintf("  %s  (%s)", f.name, shortJID(f.jid)))
		}
		return nil, fmt.Errorf("%q matches several people — say which one:\n%s", who, strings.Join(lines, "\n"))
	}

	out := make([]backfillTarget, 0, len(found))
	for _, f := range found {
		out = append(out, backfillTarget{jid: f.jid, name: f.name})
	}
	return out, nil
}

func isPhoneNumber(who string) bool {
	digits := strings.Map(keepDigits, who)
	return digits == who && len(digits) >= 8
}

// deepen walks one conversation backwards, chunk by chunk, until the phone stops
// answering with anything new or the wanted number of messages is reached.
func deepen(ctx context.Context, m *mirror, t backfillTarget, want, chunk int, arrived chan struct{}) (int, error) {
	chat, err := types.ParseJID(t.jid)
	if err != nil {
		return 0, fmt.Errorf("%s is not a JID: %w", t.jid, err)
	}

	added := 0
	for added < want {
		anchor, ok := oldestMessage(ctx, m.db, t.jid)
		if !ok {
			fmt.Printf("  %-28s nothing held yet, so there is nothing to ask from\n", t.name)
			return added, nil
		}
		anchor.Chat = chat
		anchor.IsGroup = chat.Server == types.GroupServer

		before := countMessages(ctx, m.db, t.jid)
		drain(arrived)

		ask := chunk
		if want-added < ask {
			ask = want - added
		}
		if _, err := m.cli.SendPeerMessage(ctx, m.cli.BuildHistorySyncRequest(anchor, ask)); err != nil {
			return added, fmt.Errorf("ask %s for history: %w", t.name, err)
		}

		if !waitFor(ctx, arrived, time.Minute) {
			fmt.Printf("  %-28s the phone did not answer within a minute — is it online?\n", t.name)
			return added, nil
		}
		// A single answer can arrive as several blobs; let the rest land.
		for waitFor(ctx, arrived, 3*time.Second) {
		}

		got := countMessages(ctx, m.db, t.jid) - before
		if got <= 0 {
			// The phone has nothing older to give: this is the start of the chat,
			// or as far back as it keeps it.
			break
		}
		added += got
		fmt.Printf("  %-28s +%d, back to %s\n", t.name, got, stamp(anchorTS(ctx, m.db, t.jid)))
	}
	if added > 0 {
		return added, nil
	}
	fmt.Printf("  %-28s nothing older to pull\n", t.name)
	return added, nil
}

// oldestMessage is the anchor a history request is made from: the phone answers
// with what comes immediately before it.
func oldestMessage(ctx context.Context, db *sql.DB, chatJID string) (*types.MessageInfo, bool) {
	var id string
	var fromMe int
	var ts int64
	err := db.QueryRowContext(ctx, `
		SELECT id, from_me, ts FROM messages
		WHERE chat_jid = ? ORDER BY ts ASC LIMIT 1`, chatJID).Scan(&id, &fromMe, &ts)
	if err != nil || id == "" {
		return nil, false
	}
	return &types.MessageInfo{
		ID:        id,
		Timestamp: time.Unix(ts, 0),
		MessageSource: types.MessageSource{
			IsFromMe: fromMe == 1,
		},
	}, true
}

func anchorTS(ctx context.Context, db *sql.DB, chatJID string) int64 {
	var ts sql.NullInt64
	_ = db.QueryRowContext(ctx, `SELECT MIN(ts) FROM messages WHERE chat_jid = ?`, chatJID).Scan(&ts)
	return ts.Int64
}

func countMessages(ctx context.Context, db *sql.DB, chatJID string) int {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, chatJID).Scan(&n)
	return n
}

// conversationsByRecency lists what to deepen, the liveliest first, so an
// interrupted `--all` has already done the conversations that matter.
func conversationsByRecency(ctx context.Context, db *sql.DB, limit int) ([]backfillTarget, error) {
	sqlText := `
		SELECT m.chat_jid, COALESCE(NULLIF(c.name, ''), NULLIF(ct.name, ''), m.chat_jid)
		FROM (SELECT chat_jid, MAX(ts) AS last_ts FROM messages GROUP BY chat_jid) m
		LEFT JOIN chats c    ON c.jid  = m.chat_jid
		LEFT JOIN contacts ct ON ct.jid = m.chat_jid
		ORDER BY m.last_ts DESC`
	args := []any{}
	if limit > 0 {
		sqlText += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []backfillTarget
	for rows.Next() {
		var t backfillTarget
		if err := rows.Scan(&t.jid, &t.name); err != nil {
			return nil, err
		}
		if r := []rune(t.name); len(r) > 28 {
			t.name = string(r[:27]) + "…"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func drain(c chan struct{}) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

func waitFor(ctx context.Context, c chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}
