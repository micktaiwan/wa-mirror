package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// presenceTarget is one person the question is about: who they are here, what
// the mirror holds as their last message, and what the server said about them.
type presenceTarget struct {
	jid    types.JID
	label  string
	body   string
	ts     int64
	fromMe bool

	addrs []types.JID // every address the subscription is sent to
	got   *events.Presence
	auto  bool // picked by --top rather than named on the command line
}

// lastSeen answers "when was this person last online?".
//
// WhatsApp never stores that anywhere we can query: presence is pushed, not
// pulled. The server only pushes it to a client that is itself online, and only
// for the people that client has explicitly subscribed to. So this goes online,
// subscribes, waits for the push, prints it, and goes back offline — which is
// also why it is a one-shot command and not a column in the mirror: nothing
// about the past is recoverable, only what the server volunteers right now.
//
// Two things can silence the answer, and they look alike from here: the person
// hides their last seen (the server sends the presence with last="deny", which
// whatsmeow turns into a zero time), or nothing comes back at all.
func lastSeen(ctx context.Context, args []string) error {
	var who []string
	top := 0
	wait := 20 * time.Second

	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--to", "--from":
			v, err := value()
			if err != nil {
				return err
			}
			who = append(who, v)
		case "--top":
			v, err := value()
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s is not a number of people", v)
			}
			top = n
		case "--wait":
			v, err := value()
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s is not a number of seconds", v)
			}
			wait = time.Duration(n) * time.Second
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown option %s — see `wa --help`", arg)
			}
			who = append(who, arg)
		}
	}
	if len(who) == 0 && top == 0 {
		return fmt.Errorf("whose presence? give a name, a phone number, a JID, or --top <n>")
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return err
	}

	var targets []presenceTarget
	if top > 0 {
		targets, err = livelistPeople(ctx, db, top)
	}
	if err == nil {
		for _, w := range who {
			var t presenceTarget
			t, err = personNamed(ctx, db, w)
			if err != nil {
				break
			}
			targets = append(targets, t)
		}
	}
	db.Close()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no one-to-one conversation to ask about — try `wa --threads`")
	}

	// A companion device holds one connection at a time, so the background
	// mirror steps aside exactly the way it does for --backfill.
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

	var mu sync.Mutex
	byAddr := map[string]int{}
	answered := make(chan int, 4*len(targets))
	m.cli.AddEventHandler(func(raw any) {
		p, ok := raw.(*events.Presence)
		if !ok {
			return
		}
		mu.Lock()
		idx, known := byAddr[p.From.ToNonAD().String()]
		if known && targets[idx].got == nil {
			targets[idx].got = p
		}
		mu.Unlock()
		if known {
			select {
			case answered <- idx:
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

	// The server only talks about other people to a client that says it is
	// online, so being visible online for the length of this command is the
	// price of the question. Undone below, before hanging up.
	if err := m.cli.SendPresence(ctx, types.PresenceAvailable); err != nil {
		return fmt.Errorf("could not go online, which is what makes the server share presence: %w", err)
	}
	defer func() {
		_ = m.cli.SendPresence(context.Background(), types.PresenceUnavailable)
	}()

	for i := range targets {
		targets[i].addrs = addressesOf(ctx, m, targets[i].jid)
		nameFromStore(ctx, m, &targets[i])
	}
	// Two conversations that resolve to the same addresses are one person seen
	// twice — the mirror files their number and their @lid alias separately, and
	// the two can even carry different names. Only now, with the aliases
	// resolved, can they be folded together.
	targets = foldSamePerson(targets)
	if top > 0 {
		targets = trimAuto(targets, top)
	}

	subscribed := 0
	var subErr error
	for i := range targets {
		ok := false
		for _, jid := range targets[i].addrs {
			mu.Lock()
			byAddr[jid.ToNonAD().String()] = i
			mu.Unlock()
			if err := m.cli.SubscribePresence(ctx, jid); err != nil {
				subErr = err
				continue
			}
			ok = true
		}
		if ok {
			subscribed++
		}
	}
	if subscribed == 0 {
		return fmt.Errorf("no subscription went through: %w", subErr)
	}
	if subErr != nil && os.Getenv("WA_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "wa: at least one address refused the subscription: %v\n", subErr)
	}

	deadline := time.After(wait)
	for waiting := true; waiting; {
		mu.Lock()
		missing := 0
		for i := range targets {
			if targets[i].got == nil {
				missing++
			}
		}
		mu.Unlock()
		if missing == 0 {
			break
		}
		select {
		case <-answered:
		case <-deadline:
			waiting = false
		case <-ctx.Done():
			waiting = false
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, t := range targets {
		fmt.Println(t.line())
	}
	return nil
}

// line renders one person the way the list is read aloud: who, when they were
// last online, and what the last thing said in that conversation was.
func (t presenceTarget) line() string {
	out := fmt.Sprintf("- %s [%s]", t.label, presencePhrase(t.got))
	if t.body != "" {
		who := ""
		if t.fromMe {
			who = "me: "
		}
		out += fmt.Sprintf(" %s%s", who, truncate(oneLine(t.body), 70))
	}
	return out
}

func presencePhrase(p *events.Presence) string {
	switch {
	case p == nil:
		return "no answer"
	case !p.Unavailable:
		return "online now"
	case p.LastSeen.IsZero():
		return "last seen hidden"
	default:
		return humanAgo(time.Since(p.LastSeen)) + " ago"
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// addressesOf lists every address the same person can answer on. A thread filed
// under the @lid alias is the same person the phone knows by their number, and
// there is no telling in advance which one the server will use, so both are
// subscribed when both are known.
func addressesOf(ctx context.Context, m *mirror, jid types.JID) []types.JID {
	addrs := []types.JID{jid}
	switch jid.Server {
	case types.HiddenUserServer:
		if pn, err := m.cli.Store.LIDs.GetPNForLID(ctx, jid.ToNonAD()); err == nil && !pn.IsEmpty() {
			addrs = append(addrs, pn)
		}
	case types.DefaultUserServer:
		if lid, err := m.cli.Store.LIDs.GetLIDForPN(ctx, jid.ToNonAD()); err == nil && !lid.IsEmpty() {
			addrs = append(addrs, lid)
		}
	}
	return addrs
}

// foldSamePerson keeps one entry per person, the liveliest conversation first
// and the most telling name kept.
func foldSamePerson(all []presenceTarget) []presenceTarget {
	var kept []presenceTarget
	at := map[string]int{}
	for _, t := range all {
		found := -1
		for _, jid := range t.addrs {
			if i, ok := at[jid.ToNonAD().String()]; ok {
				found = i
				break
			}
		}
		if found < 0 {
			kept = append(kept, t)
			found = len(kept) - 1
		} else {
			// The liveliest conversation names the person: two saved contacts
			// can point at one account, and the one still being written to is
			// the name that means something. A bare id gives way to any name.
			if isDigits(kept[found].label) && !isDigits(t.label) {
				kept[found].label = t.label
			}
			kept[found].auto = kept[found].auto && t.auto
		}
		for _, jid := range t.addrs {
			at[jid.ToNonAD().String()] = found
		}
		// The addresses of both copies are worth subscribing to: there is no
		// telling which one the server will answer on.
		if found < len(kept) {
			kept[found].addrs = mergeJIDs(kept[found].addrs, t.addrs)
		}
	}
	return kept
}

// trimAuto keeps at most n of the people --top brought in, in the order the
// conversations were last alive, and every person named on the command line.
func trimAuto(all []presenceTarget, n int) []presenceTarget {
	var kept []presenceTarget
	auto := 0
	for _, t := range all {
		if t.auto {
			if auto == n {
				continue
			}
			auto++
		}
		kept = append(kept, t)
	}
	return kept
}

func isDigits(s string) bool {
	return s != "" && strings.Map(keepDigits, s) == s
}

func mergeJIDs(a, b []types.JID) []types.JID {
	seen := map[string]bool{}
	var out []types.JID
	for _, jid := range append(a, b...) {
		key := jid.ToNonAD().String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, jid)
	}
	return out
}

// nameFromStore gives a face to a conversation the mirror only knows by digits.
// An @lid alias carries no phone number and no push name, so the label falls back
// to the raw id; the library's own contact store usually holds the name, and the
// phone number behind the alias is a better last resort than the alias itself.
func nameFromStore(ctx context.Context, m *mirror, t *presenceTarget) {
	if t.label == "" {
		t.label = shortJID(t.jid.String())
	}
	if !isDigits(t.label) {
		return // already a name
	}
	for _, jid := range t.addrs {
		if info, err := m.cli.Store.Contacts.GetContact(ctx, jid.ToNonAD()); err == nil {
			if name := contactName(info); name != "" {
				t.label = name
				return
			}
		}
	}
	for _, jid := range t.addrs {
		if jid.Server == types.DefaultUserServer {
			t.label = "+" + jid.User
			return
		}
	}
}

// personNamed turns what was typed into one target, with the last message the
// mirror holds for that conversation.
func personNamed(ctx context.Context, db *sql.DB, who string) (presenceTarget, error) {
	jid, label, err := resolveRecipient(ctx, db, who)
	if err != nil {
		return presenceTarget{}, err
	}
	if jid.Server == types.GroupServer {
		return presenceTarget{}, fmt.Errorf("%s is a group — presence belongs to a person, not to a conversation", label)
	}
	if label == "" {
		label = shortJID(jid.String())
	}
	t := presenceTarget{jid: jid, label: label}
	t.body, t.ts, t.fromMe = lastMessageOf(ctx, db, jid.String())
	return t, nil
}

// livelistPeople picks the people written to or heard from most recently, groups
// left out. It hands back more candidates than asked: the same person often holds
// two conversations — their number and their @lid alias — and which ones are the
// same person is only known once the client is connected, so the trimming to n
// happens after that, not here.
func livelistPeople(ctx context.Context, db *sql.DB, n int) ([]presenceTarget, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT chat_jid, MAX(ts) AS last_ts
		FROM messages
		WHERE is_group = 0 AND deleted_at = 0
		GROUP BY chat_jid
		ORDER BY last_ts DESC
		LIMIT ?`, 8*n)
	if err != nil {
		return nil, err
	}
	// The store allows a single connection, so the rows are drained before any
	// per-conversation lookup: querying inside the loop deadlocks on itself.
	type chatRow struct {
		jid string
		ts  int64
	}
	var recent []chatRow
	for rows.Next() {
		var c chatRow
		if err := rows.Scan(&c.jid, &c.ts); err == nil {
			recent = append(recent, c)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	var out []presenceTarget
	seen := map[string]bool{}
	for _, c := range recent {
		jid, err := types.ParseJID(c.jid)
		if err != nil || !isPerson(jid) {
			continue
		}
		label := nameFor(ctx, db, c.jid)
		key := strings.ToLower(label)
		if seen[key] {
			continue
		}
		seen[key] = true
		t := presenceTarget{jid: jid, label: label, ts: c.ts, auto: true}
		t.body, _, t.fromMe = lastMessageOf(ctx, db, c.jid)
		out = append(out, t)
		if len(out) == 3*n {
			break
		}
	}
	return out, nil
}

// isPerson keeps out everything that is not somebody: a newsletter channel, a
// broadcast list, and the WhatsApp account itself, which all sit in the message
// table next to real conversations.
func isPerson(jid types.JID) bool {
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return false
	}
	return jid.User != "0" && jid.User != ""
}

// lastMessageOf reads the final line of a conversation, rendered the way the
// message list renders it: an attachment says what it is, a deleted message says
// it is gone.
func lastMessageOf(ctx context.Context, db *sql.DB, chat string) (string, int64, bool) {
	var ts, deleted int64
	var kind, body, file string
	var fromMe int
	err := db.QueryRowContext(ctx, `
		SELECT ts, from_me, kind, body, media_path, deleted_at
		FROM messages WHERE chat_jid = ? ORDER BY ts DESC LIMIT 1`, chat).
		Scan(&ts, &fromMe, &kind, &body, &file, &deleted)
	if err != nil {
		return "", 0, false
	}
	if deleted > 0 {
		return "[deleted]", ts, fromMe == 1
	}
	if kind != "text" {
		tag := "[" + kind + "]"
		if body == "" {
			body = tag
		} else {
			body = tag + " " + body
		}
	}
	return body, ts, fromMe == 1
}

// humanAgo says how long ago in the coarsest unit that still means something.
func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
