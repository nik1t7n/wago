package main

// Turning whatsmeow events into mirror operations. Pure functions over the event and a resolver, so they are
// tested without a connection.

import (
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Resolver gives the phone address for a hidden one, and our own address.
type Resolver interface {
	PN(lid types.JID) types.JID // zero JID when unknown
	Me() types.JID
}

// chatID renders a JID the way the mirror and WAHA write it: no device part, @c.us for people.
func chatID(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return strings.Replace(j.ToNonAD().String(), "@"+types.DefaultUserServer, "@c.us", 1)
}

func parseChatID(s string) (types.JID, error) {
	s = strings.Replace(s, "@c.us", "@"+types.DefaultUserServer, 1)
	if !strings.Contains(s, "@") {
		s = strings.TrimPrefix(s, "+") + "@" + types.DefaultUserServer
	}
	return types.ParseJID(s)
}

// phoneOf returns the phone address for j, learning the alias when j is hidden and the phone is known.
func phoneOf(j, alt types.JID, res Resolver, learn func(lid, pn string)) string {
	if j.Server != types.HiddenUserServer {
		return chatID(j)
	}
	pn := alt
	if pn.Server != types.DefaultUserServer {
		pn = res.PN(j)
	}
	if pn.Server == types.DefaultUserServer {
		learn(chatID(j), chatID(pn))
		return chatID(pn)
	}
	return chatID(j)
}

// controlKinds are protocol messages that change other messages or carry keys, not content of their own.
func isControl(m *waE2E.Message) bool {
	return m.GetProtocolMessage() != nil || m.GetReactionMessage() != nil || m.GetEncReactionMessage() != nil ||
		m.GetPollUpdateMessage() != nil || m.GetKeepInChatMessage() != nil || m.GetPinInChatMessage() != nil ||
		m.GetEncEventResponseMessage() != nil || m.GetSecretEncryptedMessage() != nil
}

// contentKind is the name of the field that holds the content (imageMessage, conversation...), skipping the
// envelope fields that ride along with any message.
func contentKind(m *waE2E.Message) string {
	best, bestNum := "", protoreflect.FieldNumber(1<<29)
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch fd.JSONName() {
		case "messageContextInfo", "senderKeyDistributionMessage", "fastRatchetKeySenderKeyDistributionMessage":
			return true
		}
		if fd.Number() < bestNum {
			best, bestNum = fd.JSONName(), fd.Number()
		}
		return true
	})
	return best
}

func join(parts ...string) string {
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

// text is what a person reads in the message.
func text(m *waE2E.Message) string {
	switch {
	case m.Conversation != nil:
		return m.GetConversation()
	case m.ExtendedTextMessage != nil:
		return m.GetExtendedTextMessage().GetText()
	case m.ImageMessage != nil:
		return m.GetImageMessage().GetCaption()
	case m.VideoMessage != nil:
		return m.GetVideoMessage().GetCaption()
	case m.PtvMessage != nil:
		return m.GetPtvMessage().GetCaption()
	case m.DocumentMessage != nil:
		return m.GetDocumentMessage().GetCaption()
	case m.LocationMessage != nil:
		l := m.GetLocationMessage()
		return join(l.GetName(), l.GetAddress(), coords(l.GetDegreesLatitude(), l.GetDegreesLongitude()))
	case m.LiveLocationMessage != nil:
		l := m.GetLiveLocationMessage()
		return join(l.GetCaption(), coords(l.GetDegreesLatitude(), l.GetDegreesLongitude()))
	case m.ContactMessage != nil:
		return m.GetContactMessage().GetDisplayName()
	case m.ContactsArrayMessage != nil:
		names := []string{m.GetContactsArrayMessage().GetDisplayName()}
		for _, c := range m.GetContactsArrayMessage().GetContacts() {
			names = append(names, c.GetDisplayName())
		}
		return join(names...)
	case m.GroupInviteMessage != nil:
		g := m.GetGroupInviteMessage()
		return join(g.GetGroupName(), g.GetCaption())
	case m.ButtonsMessage != nil:
		return join(m.GetButtonsMessage().GetContentText(), m.GetButtonsMessage().GetFooterText())
	case m.TemplateMessage != nil:
		h := m.GetTemplateMessage().GetHydratedTemplate()
		return join(h.GetHydratedTitleText(), h.GetHydratedContentText(), h.GetHydratedFooterText())
	case m.InteractiveMessage != nil:
		i := m.GetInteractiveMessage()
		return join(i.GetHeader().GetTitle(), i.GetBody().GetText(), i.GetFooter().GetText())
	case m.ListMessage != nil:
		return join(m.GetListMessage().GetTitle(), m.GetListMessage().GetDescription())
	case m.ButtonsResponseMessage != nil:
		return m.GetButtonsResponseMessage().GetSelectedDisplayText()
	case m.ListResponseMessage != nil:
		return m.GetListResponseMessage().GetTitle()
	case m.TemplateButtonReplyMessage != nil:
		return m.GetTemplateButtonReplyMessage().GetSelectedDisplayText()
	case m.EventMessage != nil:
		return join(m.GetEventMessage().GetName(), m.GetEventMessage().GetDescription())
	case m.ProductMessage != nil:
		p := m.GetProductMessage().GetProduct()
		return join(p.GetTitle(), p.GetDescription())
	}
	if p := poll(m); p != nil {
		opts := []string{p.GetName()}
		for _, o := range p.GetOptions() {
			opts = append(opts, "- "+o.GetOptionName())
		}
		return join(opts...)
	}
	return ""
}

func poll(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(), m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6()} {
		if p != nil {
			return p
		}
	}
	return nil
}

func coords(lat, lng float64) string {
	if lat == 0 && lng == 0 {
		return ""
	}
	return strconv.FormatFloat(lat, 'f', 6, 64) + "," + strconv.FormatFloat(lng, 'f', 6, 64)
}

type mediaInfo struct {
	mime, name string
	msg        *waE2E.Message // only the media part, enough to download it later
}

func media(m *waE2E.Message) *mediaInfo {
	switch {
	case m.ImageMessage != nil:
		return &mediaInfo{m.GetImageMessage().GetMimetype(), "", &waE2E.Message{ImageMessage: m.ImageMessage}}
	case m.VideoMessage != nil:
		return &mediaInfo{m.GetVideoMessage().GetMimetype(), "", &waE2E.Message{VideoMessage: m.VideoMessage}}
	case m.PtvMessage != nil:
		return &mediaInfo{m.GetPtvMessage().GetMimetype(), "", &waE2E.Message{PtvMessage: m.PtvMessage}}
	case m.AudioMessage != nil:
		return &mediaInfo{m.GetAudioMessage().GetMimetype(), "", &waE2E.Message{AudioMessage: m.AudioMessage}}
	case m.DocumentMessage != nil:
		d := m.GetDocumentMessage()
		name := d.GetFileName()
		if name == "" {
			name = d.GetTitle()
		}
		return &mediaInfo{d.GetMimetype(), name, &waE2E.Message{DocumentMessage: d}}
	case m.StickerMessage != nil:
		return &mediaInfo{m.GetStickerMessage().GetMimetype(), "", &waE2E.Message{StickerMessage: m.StickerMessage}}
	}
	return nil
}

// contextInfo of whatever content the message carries (for replies).
func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	var ci *waE2E.ContextInfo
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		sub := v.Message()
		f := sub.Descriptor().Fields().ByName("contextInfo")
		if f != nil && sub.Has(f) {
			if c, ok := sub.Get(f).Message().Interface().(*waE2E.ContextInfo); ok {
				ci = c
				return false
			}
		}
		return true
	})
	return ci
}

// Converted is what one message event means for the mirror.
type Converted struct {
	Row   *Row
	Ops   []Op
	Event string // the WAHA event name it maps to besides message.any (message.edited / revoked / reaction)
}

// convertMessage maps a decrypted message to a row or to changes of earlier rows.
func convertMessage(evt *events.Message, res Resolver, learn func(lid, pn string)) Converted {
	info := evt.Info
	m := evt.Message
	if m == nil {
		return Converted{}
	}
	var chat string
	switch {
	case info.IsGroup || info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer:
		chat = chatID(info.Chat)
	case info.IsFromMe:
		chat = phoneOf(info.Chat, info.RecipientAlt, res, learn)
	default:
		chat = phoneOf(info.Chat, info.SenderAlt, res, learn)
	}
	sender := ""
	if info.IsFromMe {
		// mirror convention: our own messages carry no sender
	} else if !info.IsGroup && info.Chat.Server != types.BroadcastServer {
		sender = chat
	} else {
		sender = phoneOf(info.Sender, info.SenderAlt, res, learn)
	}
	ts := info.Timestamp.Unix()

	if pm := m.GetProtocolMessage(); pm != nil {
		target := pm.GetKey().GetID()
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			if target != "" {
				return Converted{Ops: []Op{{RevokeID: target}}, Event: "message.revoked"}
			}
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if nb := text(pm.GetEditedMessage()); target != "" && nb != "" {
				return Converted{Ops: []Op{{EditID: target, EditBody: nb, EditTs: ts}}, Event: "message.edited"}
			}
		}
		return Converted{}
	}
	if r := m.GetReactionMessage(); r != nil {
		reactor := sender
		if info.IsFromMe {
			reactor = chatID(res.Me())
		}
		if t := r.GetKey().GetID(); t != "" {
			return Converted{Ops: []Op{{ReactID: t, Reactor: reactor, Reaction: r.GetText()}}, Event: "message.reaction"}
		}
		return Converted{}
	}
	if isControl(m) {
		return Converted{}
	}
	kind := contentKind(m)
	if kind == "" {
		return Converted{}
	}
	row := &Row{WaID: info.ID, ChatID: chat, Ts: ts, FromMe: info.IsFromMe, Sender: sender, SenderName: info.PushName,
		Body: text(m), Kind: kind}
	if info.IsFromMe {
		row.SenderName = ""
	}
	if md := media(m); md != nil {
		row.HasMedia, row.MediaType, row.MediaName = true, md.mime, md.name
		row.Media, _ = proto.Marshal(md.msg)
	}
	if ci := contextInfo(m); ci != nil {
		row.ReplyTo = ci.GetStanzaID()
	}
	return Converted{Row: row}
}
