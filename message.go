package main

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// describe reduces a WhatsApp message to a kind and a readable body.
// An empty kind means the message carries no user-visible content and is skipped.
func describe(msg *waE2E.Message) (kind, body string) {
	if msg == nil {
		return "", ""
	}
	switch {
	case msg.GetConversation() != "":
		return "text", msg.GetConversation()

	case msg.GetExtendedTextMessage() != nil:
		return "text", msg.GetExtendedTextMessage().GetText()

	case msg.GetImageMessage() != nil:
		return "image", msg.GetImageMessage().GetCaption()

	case msg.GetVideoMessage() != nil:
		if msg.GetVideoMessage().GetGifPlayback() {
			return "gif", msg.GetVideoMessage().GetCaption()
		}
		return "video", msg.GetVideoMessage().GetCaption()

	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "voice", ""
		}
		return "audio", ""

	case msg.GetDocumentMessage() != nil:
		d := msg.GetDocumentMessage()
		body := d.GetFileName()
		if c := d.GetCaption(); c != "" {
			body = strings.TrimSpace(body + " — " + c)
		}
		return "document", body

	case msg.GetStickerMessage() != nil:
		return "sticker", ""

	case msg.GetContactMessage() != nil:
		return "contact", msg.GetContactMessage().GetDisplayName()

	case msg.GetContactsArrayMessage() != nil:
		return "contact", msg.GetContactsArrayMessage().GetDisplayName()

	case msg.GetLocationMessage() != nil:
		l := msg.GetLocationMessage()
		name := l.GetName()
		if name == "" {
			name = l.GetAddress()
		}
		return "location", name

	case msg.GetLiveLocationMessage() != nil:
		return "location", msg.GetLiveLocationMessage().GetCaption()

	case msg.GetReactionMessage() != nil:
		return "reaction", msg.GetReactionMessage().GetText()

	case msg.GetPollCreationMessage() != nil:
		return "poll", msg.GetPollCreationMessage().GetName()
	case msg.GetPollCreationMessageV2() != nil:
		return "poll", msg.GetPollCreationMessageV2().GetName()
	case msg.GetPollCreationMessageV3() != nil:
		return "poll", msg.GetPollCreationMessageV3().GetName()

	case msg.GetGroupInviteMessage() != nil:
		return "invite", msg.GetGroupInviteMessage().GetGroupName()

	// Bookkeeping traffic with nothing to show.
	case msg.GetProtocolMessage() != nil,
		msg.GetSenderKeyDistributionMessage() != nil,
		msg.GetPollUpdateMessage() != nil,
		msg.GetKeepInChatMessage() != nil:
		return "", ""
	}
	if name := nameOfSetField(msg); name != "" {
		return name, ""
	}
	return "other", ""
}

// editTarget reads an edit or a delete-for-everyone.
//
// Both travel as a ProtocolMessage carrying the key of the message they act on,
// so neither is a message of its own: they are corrections to one already
// stored. `describe` deliberately returns nothing for them, which is why the
// caller has to ask here before falling back to the normal path.
//
// A MESSAGE_EDIT carries the replacement content; a REVOKE carries none, and
// the sender's own client shows the bubble as deleted rather than dropping it.
func editTarget(msg *waE2E.Message) (targetID, kind, body string, revoked, ok bool) {
	pm := msg.GetProtocolMessage()
	if pm == nil {
		return "", "", "", false, false
	}
	targetID = pm.GetKey().GetID()
	if targetID == "" {
		return "", "", "", false, false
	}
	switch pm.GetType() {
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		kind, body = describe(pm.GetEditedMessage())
		if kind == "" {
			return "", "", "", false, false
		}
		return targetID, kind, body, false, true
	case waE2E.ProtocolMessage_REVOKE:
		return targetID, "", "", true, true
	}
	return "", "", "", false, false
}

// nameOfSetField says which field a message actually carries, for the types
// `describe` does not know yet. Without it every unknown lands as a bare
// "other", which reads as "they sent something" and hides what it was — the
// exact blindness that let a silently dropped edit look like a sticker.
func nameOfSetField(msg *waE2E.Message) string {
	name := ""
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch fd.Name() {
		case "messageContextInfo", "deviceSentMessage":
			return true // envelope, not content
		}
		name = strings.TrimSuffix(string(fd.Name()), "Message")
		return false
	})
	return name
}
