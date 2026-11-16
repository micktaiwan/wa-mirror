package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// person is one sender address seen in the mirror, with every name and number
// the mirror can tie to it.
type person struct {
	JID      string
	Number   string // phone number, resolved through the @lid map when the address is an alias
	Contact  string // name in the address book
	Pushname string // name the sender chose for themselves
	Messages int
	Statuses int
	Last     int64
}

const whoSQL = `
WITH senders AS (
  SELECT sender_jid AS jid,
         MAX(NULLIF(sender_name, '')) AS push,
         COUNT(*) AS n,
         SUM(chat_jid = 'status@broadcast') AS st,
         MAX(ts) AS last
  FROM messages WHERE from_me = 0 AND sender_jid <> '' GROUP BY sender_jid
), resolved AS (
  SELECT s.jid, s.push, s.n, s.st, s.last,
         CASE WHEN s.jid LIKE '%@lid' THEN COALESCE(l.pn, '')
              ELSE substr(s.jid, 1, instr(s.jid, '@') - 1) END AS number
  FROM senders s
  LEFT JOIN whatsmeow_lid_map l ON s.jid = l.lid || '@lid'
)
SELECT r.jid, r.number, COALESCE(NULLIF(c1.name, ''), NULLIF(c2.name, ''), ''),
       COALESCE(r.push, ''), r.n, COALESCE(r.st, 0), r.last
FROM resolved r
LEFT JOIN contacts c1 ON c1.jid = r.jid
LEFT JOIN contacts c2 ON r.number <> '' AND c2.jid = r.number || '@s.whatsapp.net'
WHERE COALESCE(r.push, '') LIKE ? OR COALESCE(c1.name, '') LIKE ?
   OR COALESCE(c2.name, '') LIKE ? OR r.jid LIKE ? OR r.number LIKE ?
ORDER BY r.last DESC`

// whoIs finds who is behind a name, a push name, a number or an address.
func whoIs(ctx context.Context, db *sql.DB, text string) ([]person, error) {
	like := "%" + text + "%"
	rows, err := db.QueryContext(ctx, whoSQL, like, like, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.JID, &p.Number, &p.Contact, &p.Pushname, &p.Messages, &p.Statuses, &p.Last); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return preferExact(out, text), nil
}

// preferExact keeps only the people whose name, push name, number or address
// equals text, plus every other address that shares one of their numbers. A
// one-letter push name ("L") otherwise matches half the address book. When
// nothing matches exactly, the substring matches are returned as they are.
func preferExact(all []person, text string) []person {
	numbers := map[string]bool{}
	for _, p := range all {
		for _, f := range []string{p.Contact, p.Pushname, p.Number, p.JID} {
			if f != "" && strings.EqualFold(strings.TrimSpace(f), strings.TrimSpace(text)) {
				numbers[p.Number+"|"+p.JID] = true
				if p.Number != "" {
					numbers[p.Number] = true
				}
			}
		}
	}
	if len(numbers) == 0 {
		return all
	}
	var out []person
	for _, p := range all {
		if numbers[p.Number+"|"+p.JID] || (p.Number != "" && numbers[p.Number]) {
			out = append(out, p)
		}
	}
	return out
}

func who(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("--who needs a name, push name, number or address")
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	people, err := whoIs(ctx, db, strings.Join(args, " "))
	if err != nil {
		return err
	}
	if len(people) == 0 {
		fmt.Println("nobody in the mirror matches that")
		return nil
	}
	for _, p := range people {
		number := p.Number
		if number == "" {
			number = "unknown (no @lid mapping yet)"
		}
		contact := p.Contact
		if contact == "" {
			contact = "not in the address book"
		}
		fmt.Printf("%s\n  number: %s\n  contact: %s\n  push name: %s\n  %d messages (%d statuses), last %s\n\n",
			p.JID, number, contact, orDash(p.Pushname), p.Messages, p.Statuses, stamp(p.Last))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
