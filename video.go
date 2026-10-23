package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// videoFile is an mp4 read from disk with what the chat bubble needs before the
// recipient downloads it: its length, its frame size and a preview picture.
// Sent with --file instead, the same mp4 arrives as a document the recipient has
// to open, not a video that plays in the conversation.
type videoFile struct {
	data          []byte
	seconds       uint32
	width, height uint32
	thumbnail     []byte
}

// readVideo loads an mp4. Duration and size come from ffprobe and the preview
// from ffmpeg when they are installed; without them the video still goes out,
// only its bubble shows no length and a blank preview.
func readVideo(path string) (*videoFile, error) {
	if !strings.EqualFold(filepath.Ext(path), ".mp4") {
		return nil, fmt.Errorf("%s: --video takes an .mp4 (H.264), the format WhatsApp plays in the chat; send other files with --file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	v := &videoFile{data: data}
	if out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration", "-of", "json", path).Output(); err == nil {
		v.width, v.height, v.seconds = parseProbe(out)
	}
	if frame, err := exec.Command("ffmpeg", "-v", "error", "-ss", "0.5", "-i", path, "-frames:v", "1",
		"-vf", "scale=200:-2", "-f", "image2", "-c:v", "mjpeg", "pipe:1").Output(); err == nil && len(frame) > 0 {
		v.thumbnail = frame
	}
	return v, nil
}

// parseProbe reads ffprobe's JSON: the first video stream's size and the
// container's duration, rounded up to whole seconds as WhatsApp shows them.
func parseProbe(out []byte) (width, height, seconds uint32) {
	var probe struct {
		Streams []struct {
			Width  uint32 `json:"width"`
			Height uint32 `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &probe) != nil {
		return 0, 0, 0
	}
	if len(probe.Streams) > 0 {
		width, height = probe.Streams[0].Width, probe.Streams[0].Height
	}
	if d, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil && d > 0 {
		seconds = uint32(math.Ceil(d))
	}
	return width, height, seconds
}

// buildVideo uploads the clip as a WhatsApp video: it plays in the chat, with
// the text as its caption.
func buildVideo(ctx context.Context, cli *whatsmeow.Client, v *videoFile, text string) (*waE2E.Message, string, string, error) {
	up, err := cli.Upload(ctx, v.data, whatsmeow.MediaVideo)
	if err != nil {
		return nil, "", "", fmt.Errorf("upload the video: %w", err)
	}
	msg := &waE2E.VideoMessage{
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		Mimetype:      proto.String("video/mp4"),
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}
	if v.seconds > 0 {
		msg.Seconds = proto.Uint32(v.seconds)
	}
	if v.width > 0 && v.height > 0 {
		msg.Width = proto.Uint32(v.width)
		msg.Height = proto.Uint32(v.height)
	}
	if v.thumbnail != nil {
		msg.JPEGThumbnail = v.thumbnail
	}
	if text != "" {
		msg.Caption = proto.String(text)
	}
	return &waE2E.Message{VideoMessage: msg}, "video", text, nil
}
