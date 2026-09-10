// wa mirrors the WhatsApp account of this machine's owner into a local SQLite
// file and reads it back from the command line. It links as a companion
// device, exactly like WhatsApp Web.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const usage = `wa — read the WhatsApp messages mirrored on this machine

  wa [options]           read the mirrored messages
                           --hours <n>      only the last n hours
                           --since <date>   from this date on
                           --until <date>   up to this date, its whole span included
                           --from <text>    only this correspondent or group
                           --search <text>  only messages containing this
                           --limit <n>      how many to show (default 20)
                           --threads        one line per conversation instead of the messages
                           --json           machine-readable output
                         A date is 2024-04, 2024-04-15 or 2024-04-15 08:30, read in local time.
                         A message you sent carries "(delivered hh:mm)" or "(read hh:mm)" once
                         the recipient's device says so. No mark means nothing was heard, not
                         that it went unread: receipts only arrive live, so a message sent while
                         the mirror was down never gets one, and a correspondent who turned read
                         receipts off never sends the read one.

  wa --send <who> [msg]  send a message, and/or a picture with --image <path>
                           --to <who>       name, phone number or JID of the correspondent
                           --text <msg>     what to write (also the picture's caption)
                           --image <path>   a picture to send, JPEG or PNG
                         An id already seen here reaches its conversation on its own, group
                         or person alike. A group never written to yet needs its full JID,
                         "@g.us" included, or its digits are taken for a phone number.

  wa --backfill <who>    ask the phone for the messages older than those held here
                           --count <n>      how many to pull per conversation (default 500)
                           --chunk <n>      how many per request (default 50)
                           --recent         re-read the last messages instead of going back,
                                            to fetch attachments the mirror missed
                           --all            every conversation, the liveliest first
                           --limit <n>      with --all, how many conversations to walk
                         The phone must be online: it is the only place old messages exist.

                         Pictures, voice notes and documents are downloaded to
                         ~/.wa/media and their path is shown next to the message.

  wa --last-seen <who>   when this person was last online, as the app shows it
                           --top <n>        the n liveliest conversations instead of a name
                           --wait <n>       how many seconds to wait for the answers (default 20)
                         WhatsApp only pushes presence to a client that is itself online, so
                         this marks you online for the few seconds it runs, and there is no
                         history to read: only what the server volunteers right now.

  wa serve               stay connected and keep mirroring (this is the daemon)
  wa --pair              link this machine to the phone by QR code
  wa --status            whether it is linked, and how fresh the mirror is
  wa --logout            unlink this machine
  wa --install-daemon    keep the mirror running in the background across reboots
  wa --uninstall-daemon  stop and remove that background service
  wa --verbose           add to serve or --pair to see the library's logs
`

const defaultLimit = 20

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "wa: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	ctx := context.Background()

	verbose := false
	kept := args[:0:0]
	for _, a := range args {
		if a == "--verbose" || a == "-v" {
			verbose = true
			continue
		}
		kept = append(kept, a)
	}
	args = kept

	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return nil
		case "serve":
			return serve(ctx, verbose, false)
		case "--pair", "pair":
			return pair(ctx, verbose)
		case "--send", "send":
			return send(ctx, args[1:])
		case "--backfill", "backfill":
			return backfill(ctx, args[1:])
		case "--last-seen", "--lastseen":
			return lastSeen(ctx, args[1:])
		case "--status", "status":
			return status(ctx)
		case "--logout", "logout":
			return logout(ctx)
		case "--install-daemon":
			return installDaemon()
		case "--uninstall-daemon":
			return uninstallDaemon()
		}
	}
	return read(ctx, args)
}

func serve(ctx context.Context, verbose, pairOnly bool) error {
	// Under launchd there is nobody to show a QR code to, so an unlinked store
	// waits for `wa --pair` to happen instead of exiting and being respawned forever.
	for {
		m, err := connect(ctx, verbose)
		if err != nil {
			return err
		}
		if m.linked() {
			return m.run(ctx, pairOnly)
		}
		m.db.Close()
		fmt.Fprintln(os.Stderr, "wa: not linked yet — waiting for `wa --pair`")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}
	}
}

func pair(ctx context.Context, verbose bool) error {
	m, err := connect(ctx, verbose)
	if err != nil {
		return err
	}
	if m.linked() {
		return fmt.Errorf("already linked as %s — run `wa --logout` first to link again", m.cli.Store.ID.User)
	}
	// The background daemon holds the same store and would fight over the session,
	// so it steps aside for the duration of the pairing.
	resume := pauseDaemon()
	defer resume()
	return m.run(ctx, true)
}

func status(ctx context.Context) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}

	// The device table is the authority on being linked: `own_jid` only appears
	// once a connection has completed, which lags the pairing itself.
	var device sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT jid FROM whatsmeow_device LIMIT 1`).Scan(&device)
	if device.String == "" {
		fmt.Println("Not linked. Run `wa --pair` and scan the code with the phone.")
		return nil
	}
	fmt.Printf("Linked as %s\n", shortJID(device.String))

	var count int
	var first, last sql.NullInt64
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(ts), MAX(ts) FROM messages`).Scan(&count, &first, &last)
	if count == 0 {
		fmt.Println("No message mirrored yet — run `wa serve` and leave it connected.")
		return nil
	}
	var chats int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&chats)
	fmt.Printf("%d messages across %d conversations, %s → %s\n",
		count, chats, stamp(first.Int64), stamp(last.Int64))

	if c := getMeta(ctx, db, "last_connected"); c != "" {
		if ts, err := strconv.ParseInt(c, 10, 64); err == nil {
			fmt.Printf("Last connected %s\n", stamp(ts))
		}
	}
	fmt.Printf("Store: %s\n", dbPath())
	return nil
}

func logout(ctx context.Context) error {
	m, err := connect(ctx, false)
	if err != nil {
		return err
	}
	if !m.linked() {
		return fmt.Errorf("not linked")
	}
	if err := m.cli.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := m.cli.Logout(ctx); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	fmt.Println("Unlinked. The mirrored messages are still in", dbPath())
	return nil
}

func read(ctx context.Context, args []string) error {
	f := filter{limit: defaultLimit}
	threads, asJSON := false, false

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
		case "--threads":
			threads = true
		case "--json":
			asJSON = true
		case "--hours":
			v, err := value()
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s is not a number of hours", v)
			}
			since := time.Now().Add(-time.Duration(n) * time.Hour)
			f.since = &since
		case "--since":
			v, err := value()
			if err != nil {
				return err
			}
			t, err := parseMoment(v, false)
			if err != nil {
				return err
			}
			f.since = &t
		case "--until":
			v, err := value()
			if err != nil {
				return err
			}
			t, err := parseMoment(v, true)
			if err != nil {
				return err
			}
			f.until = &t
		case "--from":
			v, err := value()
			if err != nil {
				return err
			}
			f.from = v
		case "--search":
			v, err := value()
			if err != nil {
				return err
			}
			f.search = v
		case "--chat":
			v, err := value()
			if err != nil {
				return err
			}
			f.chat = v
		case "--limit":
			v, err := value()
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("%s is not a limit", v)
			}
			f.limit = n
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown option %s — see `wa --help`", arg)
			}
			// A bare word is the correspondent, so `wa alex` just works.
			f.from = arg
		}
	}
	// --threads summarises, so it needs a wide net rather than the reading default.
	if threads && f.limit == defaultLimit {
		f.limit = 100000
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, db); err != nil {
		return err
	}

	rows, err := query(ctx, db, f)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		var total int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&total)
		if total == 0 {
			fmt.Println("No message mirrored yet. Run `wa --pair`, then `wa serve`.")
		} else {
			fmt.Printf("No message matches. %d held in total.\n", total)
		}
		return nil
	}

	switch {
	case asJSON:
		printJSON(rows)
	case threads:
		printThreads(rows)
	default:
		printMessages(rows)
	}
	return nil
}
