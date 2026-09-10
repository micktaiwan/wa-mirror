package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type filter struct {
	since  *time.Time
	until  *time.Time
	from   string
	search string
	limit  int
	chat   string
}

type row struct {
	ID          string `json:"id"`
	ChatJID     string `json:"chat_jid"`
	ChatName    string `json:"chat"`
	SenderJID   string `json:"sender_jid"`
	SenderName  string `json:"sender"`
	FromMe      bool   `json:"from_me"`
	IsGroup     bool   `json:"is_group"`
	TS          int64  `json:"ts"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	EditedAt    int64  `json:"edited_at,omitempty"`
	DeletedAt   int64  `json:"deleted_at,omitempty"`
	DeliveredAt int64  `json:"delivered_at,omitempty"`
	ReadAt      int64  `json:"read_at,omitempty"`
	File        string `json:"file,omitempty"`
}

const selectMessages = `
SELECT m.id, m.chat_jid, m.sender_jid, m.from_me, m.is_group, m.ts, m.kind, m.body,
       m.edited_at, m.deleted_at, m.media_path, m.delivered_at, m.read_at,
       COALESCE(NULLIF(c.name, ''), NULLIF(ct.name, ''), '') AS chat_name,
       COALESCE(NULLIF(cs.name, ''), NULLIF(m.sender_name, ''), '') AS sender_name
FROM messages m
LEFT JOIN chats    c  ON c.jid  = m.chat_jid
LEFT JOIN contacts ct ON ct.jid = m.chat_jid
LEFT JOIN contacts cs ON cs.jid = m.sender_jid
`

func query(ctx context.Context, db *sql.DB, f filter) ([]row, error) {
	where := []string{}
	args := []any{}

	if f.since != nil {
		where = append(where, "m.ts >= ?")
		args = append(args, f.since.Unix())
	}
	if f.until != nil {
		where = append(where, "m.ts <= ?")
		args = append(args, f.until.Unix())
	}
	if f.search != "" {
		where = append(where, "m.body LIKE ?")
		args = append(args, "%"+f.search+"%")
	}
	// --from matches a correspondent by any name we know for them, or by number.
	if f.from != "" {
		where = append(where, `(
			COALESCE(c.name, '')  LIKE ? OR
			COALESCE(ct.name, '') LIKE ? OR
			COALESCE(cs.name, '') LIKE ? OR
			m.sender_name         LIKE ? OR
			m.chat_jid            LIKE ?
		)`)
		like := "%" + f.from + "%"
		args = append(args, like, like, like, like, like)
	}
	if f.chat != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, f.chat)
	}

	sqlText := selectMessages
	if len(where) > 0 {
		sqlText += "WHERE " + strings.Join(where, " AND ") + "\n"
	}
	sqlText += "ORDER BY m.ts DESC LIMIT ?"
	args = append(args, f.limit)

	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []row
	for rows.Next() {
		var r row
		var fromMe, isGroup int
		var file string
		if err := rows.Scan(&r.ID, &r.ChatJID, &r.SenderJID, &fromMe, &isGroup,
			&r.TS, &r.Kind, &r.Body, &r.EditedAt, &r.DeletedAt, &file,
			&r.DeliveredAt, &r.ReadAt,
			&r.ChatName, &r.SenderName); err != nil {
			return nil, err
		}
		r.File = mediaFullPath(file)
		r.FromMe = fromMe == 1
		r.IsGroup = isGroup == 1
		if r.ChatName == "" {
			r.ChatName = shortJID(r.ChatJID)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Read oldest first: a conversation makes sense in the order it happened.
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

func printMessages(rows []row) {
	for _, r := range rows {
		direction := "←"
		if r.FromMe {
			direction = "→"
		}
		who := r.ChatName
		// In a group, the chat name says where, the sender name says who.
		if r.IsGroup && !r.FromMe && r.SenderName != "" {
			who = fmt.Sprintf("%s / %s", r.ChatName, r.SenderName)
		}
		fmt.Printf("%s  %s %s%s\n", stamp(r.TS), direction, who, receiptMark(r))

		body := r.Body
		if r.Kind != "text" {
			tag := "[" + r.Kind + "]"
			// The path is what makes an attachment readable rather than just
			// announced: it is the file to open, right next to the caption.
			if r.File != "" {
				tag += " " + r.File
			}
			if body == "" {
				body = tag
			} else {
				body = tag + " " + body
			}
		}
		// Say that the text on screen is not the one that was first sent:
		// reading a corrected message as if it were the original is how a typo
		// the sender had already fixed ends up quoted back at them.
		if r.EditedAt > 0 {
			body += "  (edited " + stampShort(r.EditedAt) + ")"
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}
}

// receiptMark says what the recipients did with a message we sent: nothing for
// an incoming one, and nothing either when no receipt ever reached us — an
// unmarked message is not a message left unread, it is one we know nothing
// about (receipts arrive live only, so anything sent while the mirror was down
// stays blank, and a correspondent who turned read receipts off never sends the
// blue one).
func receiptMark(r row) string {
	if !r.FromMe {
		return ""
	}
	switch {
	case r.ReadAt > 0:
		return "  (read " + stampShort(r.ReadAt) + ")"
	case r.DeliveredAt > 0:
		return "  (delivered " + stampShort(r.DeliveredAt) + ")"
	}
	return ""
}

// printThreads gives one line per conversation: how many messages and the span they cover.
func printThreads(rows []row) {
	type agg struct {
		name        string
		count       int
		first, last int64
	}
	byChat := map[string]*agg{}
	for _, r := range rows {
		a, ok := byChat[r.ChatJID]
		if !ok {
			a = &agg{name: r.ChatName, first: r.TS, last: r.TS}
			byChat[r.ChatJID] = a
		}
		a.count++
		if r.TS < a.first {
			a.first = r.TS
		}
		if r.TS > a.last {
			a.last = r.TS
		}
	}

	list := make([]*agg, 0, len(byChat))
	for _, a := range byChat {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].name < list[j].name
	})

	for _, a := range list {
		fmt.Printf("%5d  %-28s  %s → %s\n", a.count, truncate(a.name, 28), stamp(a.first), stamp(a.last))
	}
	fmt.Printf("\n%d messages, %d conversations\n", len(rows), len(list))
}

func printJSON(rows []row) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rows)
}

// stampShort gives the time alone: an edit is read next to the message it
// changed, and the date is almost always the same one.
func stampShort(ts int64) string {
	return time.Unix(ts, 0).Local().Format("15:04")
}

func stamp(ts int64) string {
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
}

func shortJID(jid string) string {
	if i := strings.IndexByte(jid, '@'); i > 0 {
		return jid[:i]
	}
	return jid
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// parseMoment reads 2024-04, 2024-04-15 or 2024-04-15 08:30 in local time.
// endOfSpan pushes a bare date to the last instant it covers, so --until includes its whole day.
func parseMoment(text string, endOfSpan bool) (time.Time, error) {
	text = strings.TrimSpace(text)
	loc := time.Local

	if t, err := time.ParseInLocation("2006-01-02 15:04", text, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", text, loc); err == nil {
		if endOfSpan {
			return t.AddDate(0, 0, 1).Add(-time.Second), nil
		}
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01", text, loc); err == nil {
		if endOfSpan {
			return t.AddDate(0, 1, 0).Add(-time.Second), nil
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%s is not a date — try 2024-04, 2024-04-15 or 2024-04-15 08:30", text)
}
