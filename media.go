package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

// Media files are downloaded and kept on disk, next to the mirror itself.
//
// Without them a picture read back would be an unreadable "[image]" whose only
// copy stays on the phone. The files land in the clear under ~/.wa/media,
// protected by disk encryption and nothing else, exactly like the message text
// already in wa.db.
//
// Two bounds keep this from turning into a disk-filling mirror of every
// attachment ever received:
//   - only messages younger than mediaMaxAge are fetched, because WhatsApp drops
//     the encrypted blob from its servers after a while anyway and an old
//     backfill would spend its time on 404s;
//   - downloads run on one background worker, so a slow fetch never stalls the
//     event handler that stores messages.
const mediaMaxAge = 30 * 24 * time.Hour

// mediaQueue is how many pending downloads are held before new ones are dropped.
// A dropped one is not lost for good: `wa --backfill <who> --recent` asks the
// phone to send the window again, and the second pass fetches what is missing.
const mediaQueue = 256

type mediaJob struct {
	id      string
	chatJID string
	msg     *waE2E.Message
}

func mediaDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".wa", "media")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// downloadable picks the part of a message that carries a file, and the
// extension to store it under. Anything else returns nil and is skipped.
func downloadable(msg *waE2E.Message) (whatsmeow.DownloadableMessage, string) {
	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage(), extensionFor(msg.GetImageMessage().GetMimetype(), ".jpg")
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage(), extensionFor(msg.GetVideoMessage().GetMimetype(), ".mp4")
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage(), extensionFor(msg.GetAudioMessage().GetMimetype(), ".ogg")
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage(), extensionFor(msg.GetStickerMessage().GetMimetype(), ".webp")
	case msg.GetDocumentMessage() != nil:
		doc := msg.GetDocumentMessage()
		if ext := filepath.Ext(doc.GetFileName()); ext != "" {
			return doc, ext
		}
		return doc, extensionFor(doc.GetMimetype(), ".bin")
	}
	return nil, ""
}

// extensionFor maps a mime type to a file extension. mime.ExtensionsByType is
// not used on purpose: it returns every registered spelling in no defined order,
// so an image/jpeg could just as well be saved as .jfif.
func extensionFor(mimetype, fallback string) string {
	if i := strings.IndexByte(mimetype, ';'); i > 0 {
		mimetype = mimetype[:i]
	}
	switch strings.TrimSpace(strings.ToLower(mimetype)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/3gpp":
		return ".3gp"
	case "audio/ogg", "audio/opus":
		return ".ogg"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/aac":
		return ".m4a"
	case "audio/amr":
		return ".amr"
	case "application/pdf":
		return ".pdf"
	}
	return fallback
}

// queueMedia hands a message to the download worker, unless there is nothing to
// download, it is too old to still exist on WhatsApp's servers, or its file is
// already on disk.
func (m *mirror) queueMedia(ctx context.Context, id, chatJID string, ts int64, msg *waE2E.Message) {
	if dl, _ := downloadable(msg); dl == nil {
		return
	}
	if time.Since(time.Unix(ts, 0)) > mediaMaxAge {
		return
	}
	var held string
	_ = m.db.QueryRowContext(ctx,
		`SELECT media_path FROM messages WHERE id = ? AND chat_jid = ?`, id, chatJID).Scan(&held)
	if held != "" {
		if _, err := os.Stat(mediaFullPath(held)); err == nil {
			return
		}
	}

	m.mediaOnce.Do(m.startMedia)
	select {
	case m.mediaQ <- mediaJob{id: id, chatJID: chatJID, msg: msg}:
	default:
		fmt.Fprintf(os.Stderr, "wa: media queue full, skipping %s — `wa --backfill <who> --recent` will pick it up\n", id)
	}
}

func (m *mirror) startMedia() {
	m.mediaQ = make(chan mediaJob, mediaQueue)
	m.mediaWG.Add(1)
	go func() {
		defer m.mediaWG.Done()
		for job := range m.mediaQ {
			m.fetchMedia(job)
		}
	}()
}

// stopMedia waits for the pending downloads. It has to be called before the
// client disconnects: a download needs the live connection, so tearing it down
// first would silently lose whatever is still queued.
func (m *mirror) stopMedia() {
	m.mediaStop.Do(func() {
		if m.mediaQ == nil {
			return
		}
		close(m.mediaQ)
		m.mediaWG.Wait()
	})
}

func (m *mirror) fetchMedia(job mediaJob) {
	dl, ext := downloadable(job.msg)
	if dl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	data, err := m.cli.Download(ctx, dl)
	if err != nil {
		// Expired blobs are the normal fate of anything old, and saying so on
		// every one of them would drown the log. Only the rest is worth a word.
		fmt.Fprintf(os.Stderr, "wa: download %s: %v\n", job.id, err)
		return
	}

	dir, err := mediaDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wa: media directory: %v\n", err)
		return
	}
	name := safeName(job.id) + ext
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "wa: write %s: %v\n", name, err)
		return
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE messages SET media_path = ? WHERE id = ? AND chat_jid = ?`,
		name, job.id, job.chatJID); err != nil {
		fmt.Fprintf(os.Stderr, "wa: record %s: %v\n", name, err)
	}
}

// mediaFullPath turns the stored file name into the path a reader can open.
// Only the name is kept in the database, so moving ~/.wa somewhere else does
// not orphan every row.
func mediaFullPath(name string) string {
	if name == "" {
		return ""
	}
	dir, err := mediaDir()
	if err != nil {
		return name
	}
	return filepath.Join(dir, name)
}

func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}
