package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// send delivers one message — a line of text, a picture, or a picture with a
// caption — to a correspondent, then hangs up. WhatsApp lets a companion device
// hold one connection at a time, so it stands the background mirror down for the
// duration exactly the way pairing does, and puts it back up afterwards.
func send(ctx context.Context, args []string) error {
	var to, text, imagePath string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		var err error
		switch arg {
		case "--to":
			to, err = value()
		case "--text", "--message":
			text, err = value()
		case "--image", "--photo":
			imagePath, err = value()
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown option %s — see `wa --help`", arg)
			}
			// The first bare word is the correspondent, the second the message,
			// so `wa --send alex "hello"` reads the way it sounds.
			if to == "" {
				to = arg
			} else if text == "" {
				text = arg
			} else {
				return fmt.Errorf("unexpected extra argument %q", arg)
			}
		}
		if err != nil {
			return err
		}
	}

	if to == "" {
		return fmt.Errorf("who to? give a name, a phone number or a JID")
	}
	if text == "" && imagePath == "" {
		return fmt.Errorf("nothing to send — pass a message, --image, or both")
	}

	var picture []byte
	var width, height int
	if imagePath != "" {
		var err error
		picture, width, height, err = readAsJPEG(imagePath)
		if err != nil {
			return err
		}
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}

	target, label, err := resolveRecipient(ctx, db, to)
	if err != nil {
		return err
	}
	db.Close()

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
	if err := m.cli.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer m.cli.Disconnect()
	if !m.cli.WaitForConnection(30 * time.Second) {
		return fmt.Errorf("could not reach WhatsApp within 30s")
	}

	msg, kind, body, err := buildMessage(ctx, m.cli, picture, width, height, text)
	if err != nil {
		return err
	}

	resp, err := m.cli.SendMessage(ctx, target, msg)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}

	// The mirror is down while we hold the connection, so the message we just
	// sent would be missing from `wa` until the next history sync. Write it here.
	own := ""
	if id := m.cli.Store.ID; id != nil {
		own = id.ToNonAD().String()
	}
	_ = putMessage(ctx, m.db, storedMessage{
		ID:        resp.ID,
		ChatJID:   target.String(),
		SenderJID: own,
		FromMe:    true,
		IsGroup:   target.Server == types.GroupServer,
		TS:        resp.Timestamp.Unix(),
		Kind:      kind,
		Body:      body,
	})
	_ = putChat(ctx, m.db, target.String(), "", target.Server == types.GroupServer, resp.Timestamp.Unix())

	what := "message"
	if picture != nil {
		what = "picture"
	}
	fmt.Printf("Sent the %s to %s at %s\n", what, label, resp.Timestamp.Local().Format("15:04"))
	return nil
}

type match struct{ jid, name string }

// knownJIDFor turns a bare id into the conversation the mirror already holds for
// it, so that nobody has to know which server a given id lives on. A phone number
// keeps the @s.whatsapp.net address it has always been reached at, even when an
// @lid alias is also on file; anything else — a group id, above all — reaches the
// conversation it actually names. It reports false when the id is unknown here,
// which leaves the caller free to treat it as a phone number never written to yet.
func knownJIDFor(ctx context.Context, db *sql.DB, digits string) (types.JID, string, bool) {
	rows, err := db.QueryContext(ctx,
		`SELECT jid, name FROM chats WHERE jid LIKE ? || '@%' ORDER BY last_ts DESC`, digits)
	if err != nil {
		return types.EmptyJID, "", false
	}
	defer rows.Close()

	var best match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.jid, &m.name); err != nil {
			continue
		}
		if strings.HasSuffix(m.jid, "@"+types.DefaultUserServer) {
			best = m
			break
		}
		// Rows arrive newest first, so the liveliest conversation wins by default.
		if best.jid == "" {
			best = m
		}
	}
	if best.jid == "" {
		return types.EmptyJID, "", false
	}
	jid, err := types.ParseJID(best.jid)
	if err != nil {
		return types.EmptyJID, "", false
	}
	return jid, best.name, true
}

// candidates lists every address whose name contains what was typed, looking at
// both the contacts and the conversation names.
func candidates(ctx context.Context, db *sql.DB, who string) ([]match, error) {
	var found []match
	seen := map[string]bool{}
	add := func(jid, name string) {
		if jid == "" || seen[jid] {
			return
		}
		seen[jid] = true
		found = append(found, match{jid, name})
	}

	for _, table := range []string{"contacts", "chats"} {
		rows, err := db.QueryContext(ctx,
			`SELECT jid, name FROM `+table+` WHERE name <> '' AND lower(name) LIKE '%' || lower(?) || '%'`, who)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var jid, name string
			if err := rows.Scan(&jid, &name); err == nil {
				add(jid, name)
			}
		}
		rows.Close()
	}
	return found, nil
}

// resolveRecipient turns what was typed — a name, a phone number, a full JID —
// into the one correspondent it means, and refuses to guess between several.
func resolveRecipient(ctx context.Context, db *sql.DB, who string) (types.JID, string, error) {
	if strings.Contains(who, "@") {
		jid, err := types.ParseJID(who)
		if err != nil {
			return types.EmptyJID, "", fmt.Errorf("%q is not a JID: %w", who, err)
		}
		return jid, nameFor(ctx, db, jid.String()), nil
	}

	// A bare run of digits is a phone number in international form — unless the
	// mirror already holds a conversation carrying exactly those digits. A group
	// id reads like a very long phone number, and addressing it as one reaches
	// nobody: the server answers "no LID found" for an id that is right there in
	// the database, one column away.
	if digits := strings.Map(keepDigits, who); digits == who && len(digits) >= 8 {
		if jid, name, ok := knownJIDFor(ctx, db, digits); ok {
			return jid, name, nil
		}
		jid := types.NewJID(digits, types.DefaultUserServer)
		return jid, nameFor(ctx, db, jid.String()), nil
	}

	found, err := candidates(ctx, db, who)
	if err != nil {
		return types.EmptyJID, "", err
	}

	switch len(found) {
	case 0:
		return types.EmptyJID, "", fmt.Errorf("nobody here matches %q — try `wa --threads` to see the names", who)
	case 1:
		jid, err := types.ParseJID(found[0].jid)
		if err != nil {
			return types.EmptyJID, "", err
		}
		return jid, found[0].name, nil
	}

	// A name typed on its own means the person it opens, not every conversation
	// that happens to mention it: "alex" is the person called Alex, not the group
	// called "Alex's birthday".
	if narrowed := startingWith(found, who); len(narrowed) > 0 {
		found = narrowed
	}

	// Several addresses for one and the same person is the ordinary case since
	// WhatsApp started handing out @lid aliases: take the one that has been used
	// most recently rather than asking about a distinction that means nothing.
	if len(found) == 1 || sameName(found[0].name, names(found)) {
		best, bestTS := found[0], int64(-1)
		for _, f := range found {
			var ts sql.NullInt64
			_ = db.QueryRowContext(ctx, `SELECT MAX(ts) FROM messages WHERE chat_jid = ?`, f.jid).Scan(&ts)
			if ts.Int64 > bestTS {
				best, bestTS = f, ts.Int64
			}
		}
		jid, err := types.ParseJID(best.jid)
		if err != nil {
			return types.EmptyJID, "", err
		}
		return jid, best.name, nil
	}

	var lines []string
	for _, f := range found {
		lines = append(lines, fmt.Sprintf("  %s  (%s)", f.name, shortJID(f.jid)))
	}
	return types.EmptyJID, "", fmt.Errorf("%q matches several people — say which one:\n%s",
		who, strings.Join(lines, "\n"))
}

// startingWith keeps the matches whose name opens with what was typed, which is
// how someone is addressed by their first name.
func startingWith(all []match, who string) []match {
	var kept []match
	for _, m := range all {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.name)), strings.ToLower(strings.TrimSpace(who))) {
			kept = append(kept, m)
		}
	}
	return kept
}

func names(all []match) []string {
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.name)
	}
	return out
}

func sameName(first string, all []string) bool {
	for _, n := range all {
		if !strings.EqualFold(strings.TrimSpace(n), strings.TrimSpace(first)) {
			return false
		}
	}
	return true
}

func nameFor(ctx context.Context, db *sql.DB, jid string) string {
	var name string
	_ = db.QueryRowContext(ctx, `SELECT name FROM contacts WHERE jid = ?`, jid).Scan(&name)
	if name == "" {
		_ = db.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	}
	if name == "" {
		return shortJID(jid)
	}
	return name
}

func keepDigits(r rune) rune {
	if r >= '0' && r <= '9' {
		return r
	}
	return -1
}

// buildMessage assembles what goes over the wire: a plain text message, or an
// uploaded picture carrying the text as its caption.
func buildMessage(ctx context.Context, cli *whatsmeow.Client, picture []byte, width, height int, text string) (*waE2E.Message, string, string, error) {
	if picture == nil {
		return &waE2E.Message{Conversation: proto.String(text)}, "text", text, nil
	}

	up, err := cli.Upload(ctx, picture, whatsmeow.MediaImage)
	if err != nil {
		return nil, "", "", fmt.Errorf("upload the picture: %w", err)
	}
	img := &waE2E.ImageMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String("image/jpeg"),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
		Width:         proto.Uint32(uint32(width)),
		Height:        proto.Uint32(uint32(height)),
	}
	if text != "" {
		img.Caption = proto.String(text)
	}
	if thumb, err := thumbnail(picture); err == nil {
		img.JPEGThumbnail = thumb
	}
	return &waE2E.Message{ImageMessage: img}, "image", text, nil
}

// readAsJPEG loads a picture from disk as the JPEG bytes WhatsApp expects,
// converting a PNG on the way in rather than asking for one to be prepared.
func readAsJPEG(path string) ([]byte, int, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%s is not a picture I can read: %w", path, err)
	}
	b := img.Bounds()
	if format == "jpeg" {
		return raw, b.Dx(), b.Dy(), nil
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 92}); err != nil {
		return nil, 0, 0, fmt.Errorf("convert %s to JPEG: %w", path, err)
	}
	return out.Bytes(), b.Dx(), b.Dy(), nil
}

// thumbnail builds the small preview WhatsApp shows before the full picture has
// downloaded. Nearest-neighbour is plenty at this size and keeps the tool free
// of an image-resizing dependency.
func thumbnail(jpegBytes []byte) ([]byte, error) {
	src, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return nil, err
	}
	const side = 200
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("empty picture")
	}
	tw, th := side, side*h/w
	if h > w {
		th, tw = side, side*w/h
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*w/tw, b.Min.Y+y*h/th))
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
