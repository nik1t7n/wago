package main

// Incoming traffic for -dev-fake. A service built on WaGo needs its inbound path tested without a phone: this
// route makes WaGo behave as if WhatsApp delivered a message, an edit, a delete or a receipt, through the same
// handler as real traffic (mirror, webhooks, WebSocket). It exists only for sessions on the fake connection.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type devIncoming struct {
	ChatID      string   `json:"chatId"`
	Participant string   `json:"participant"` // the sender in a group
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	PushName    string   `json:"pushName"`
	FromMe      bool     `json:"fromMe"` // as if the owner wrote from the phone
	Edit        string   `json:"edit"`   // id of the message this text replaces
	Revoke      string   `json:"revoke"` // id of the message deleted for everyone
	Ack         string   `json:"ack"`    // "delivered", "read" or "played" for ackIds (our messages)
	AckIDs      []string `json:"ackIds"`
	Typing      *bool    `json:"typing"` // the person starts (true) or stops (false) typing
	Device      uint16   `json:"device"` // with fromMe: the owner's device (0 is the phone, others are linked devices)
	File        *fileIn  `json:"file"`   // a voice note, photo, video or document: {mimetype, data (base64), filename}
}

// devMedia builds a media message whose file is already in the session's file cache, so downloads, the files
// endpoint and media understanding work in fake mode exactly as with a fetched file.
func (s *Session) devMedia(f *fileIn, caption string) (*waE2E.Message, error) {
	data, err := f.bytes(context.Background())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, "files", key[:2], key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	mime := firstNonEmpty(f.Mimetype, http.DetectContentType(data))
	n := uint64(len(data))
	switch kindOfMime(mime) {
	case "voice":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(mime), FileSHA256: sum[:], FileLength: &n, PTT: proto.Bool(true)}}, nil
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String(mime), FileSHA256: sum[:], FileLength: &n, Caption: strPtr(caption)}}, nil
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String(mime), FileSHA256: sum[:], FileLength: &n, Caption: strPtr(caption)}}, nil
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String(mime), FileSHA256: sum[:], FileLength: &n,
		FileName: strPtr(f.Filename), Caption: strPtr(caption)}}, nil
}

func devRoutes(rt *router, with func(func(http.ResponseWriter, *http.Request, *Session, params) error) func(http.ResponseWriter, *http.Request, params)) {
	rt.add("POST", "/api/{session}/dev/incoming", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		if _, ok := s.fake.(*fakeConn); !ok {
			return httpError{404, "only in -dev-fake mode"}
		}
		var in devIncoming
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return httpError{400, "bad JSON: " + err.Error()}
		}
		if _, err := s.Mirror(); err != nil { // real traffic finds it open; the handler needs it
			return err
		}
		chat, err := s.target(in.ChatID)
		if err != nil {
			return err
		}
		src := types.MessageSource{Chat: chat, Sender: chat, IsFromMe: in.FromMe, IsGroup: chat.Server == types.GroupServer}
		if in.FromMe {
			src.Sender = s.Me()
		} else if src.IsGroup {
			if src.Sender, err = s.target(in.Participant); err != nil {
				return httpError{400, "a group message needs participant"}
			}
		}
		now := time.Now()
		if in.Typing != nil {
			state := types.ChatPresencePaused
			if *in.Typing {
				state = types.ChatPresenceComposing
			}
			s.handle(&events.ChatPresence{MessageSource: src, State: state})
			writeJSON(w, 200, map[string]any{"ok": true})
			return nil
		}
		if in.FromMe {
			src.Sender.Device = in.Device
		}
		if in.Ack != "" {
			kinds := map[string]types.ReceiptType{"delivered": types.ReceiptTypeDelivered, "read": types.ReceiptTypeRead, "played": types.ReceiptTypePlayed}
			kind, known := kinds[in.Ack]
			if !known || len(in.AckIDs) == 0 {
				return httpError{400, "ack is delivered, read or played, with ackIds"}
			}
			ids := make([]string, len(in.AckIDs))
			for i, id := range in.AckIDs {
				ids[i] = tail(id)
			}
			s.handle(&events.Receipt{MessageSource: src, MessageIDs: ids, Type: kind, Timestamp: now})
			writeJSON(w, 200, map[string]any{"ok": true})
			return nil
		}
		if in.ID == "" {
			in.ID = fmt.Sprintf("DEVIN%X", now.UnixNano())
		}
		var msg *waE2E.Message
		switch {
		case in.Revoke != "":
			msg = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(),
				Key: &waCommon.MessageKey{ID: proto.String(tail(in.Revoke)), RemoteJID: proto.String(chat.String())}}}
		case in.Edit != "":
			msg = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
				Key:           &waCommon.MessageKey{ID: proto.String(tail(in.Edit)), RemoteJID: proto.String(chat.String())},
				EditedMessage: &waE2E.Message{Conversation: proto.String(in.Text)}}}
		case in.File != nil:
			if msg, err = s.devMedia(in.File, in.Text); err != nil {
				return httpError{400, "file: " + err.Error()}
			}
		case in.Text != "":
			msg = &waE2E.Message{Conversation: proto.String(in.Text)}
		default:
			return httpError{400, "text, file, edit, revoke, ack or typing is required"}
		}
		evt := &events.Message{Info: types.MessageInfo{MessageSource: src, ID: in.ID, Timestamp: now, PushName: in.PushName},
			Message: msg, RawMessage: msg}
		s.handle(evt)
		if m, err := s.Mirror(); err == nil {
			m.Flush()
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": in.ID})
		return nil
	}))
}
