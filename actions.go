package main

// WAHA's action endpoints: sending, reactions, edits, deletes, read receipts, typing, presence, chats,
// groups, contacts and profile.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type sendReq struct {
	ID        string      `json:"id"` // the client's own id for the new message: a repeat with it never sends twice
	Session   string      `json:"session"`
	ChatID    string      `json:"chatId"`
	Text      string      `json:"text"`
	ReplyTo   string      `json:"reply_to"`
	Mentions  []string    `json:"mentions"`
	File      *fileIn     `json:"file"`
	Caption   string      `json:"caption"`
	Convert   bool        `json:"convert"`
	Latitude  float64     `json:"latitude"`
	Longitude float64     `json:"longitude"`
	Title     string      `json:"title"`
	Address   string      `json:"address"`
	Contacts  []contactIn `json:"contacts"`
	Poll      *struct {
		Name            string   `json:"name"`
		Options         []string `json:"options"`
		MultipleAnswers bool     `json:"multipleAnswers"`
	} `json:"poll"`
	MessageID    string   `json:"messageId"`
	MessageIDs   []string `json:"messageIds"`
	Reaction     *string  `json:"reaction"`
	Presence     string   `json:"presence"`
	Status       string   `json:"status"`
	Subject      string   `json:"subject"`
	Description  *string  `json:"description"`
	Name         string   `json:"name"`
	ContactID    string   `json:"contactId"`
	Participants []struct {
		ID string `json:"id"`
	} `json:"participants"`
}

func decode(r *http.Request, into any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 150<<20))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, into); err != nil {
		return httpError{400, "bad JSON: " + err.Error()}
	}
	return nil
}

// sessionOf: actions name the session in the body (WAHA) or in the path.
func (mg *Manager) sessionOf(p params, r *http.Request, req *sendReq) (*Session, error) {
	if p["session"] == "" && req.Session != "" {
		p = params{"session": req.Session}
	}
	return mg.session(p, r)
}

func (mg *Manager) actionRoutes() {
	rt := &mg.router
	// act wraps an action: parse the body, find the session, give the handler a connection
	act := func(needSending bool, h func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error)) func(http.ResponseWriter, *http.Request, params) {
		return func(w http.ResponseWriter, r *http.Request, p params) {
			var req sendReq
			if err := decode(r, &req); err != nil {
				fail(w, err)
				return
			}
			s, err := mg.sessionOf(p, r, &req)
			if err != nil {
				fail(w, err)
				return
			}
			c, err := s.conn(needSending)
			if err != nil {
				fail(w, err)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
			defer cancel()
			out, err := h(ctx, s, c, &req, p)
			if err != nil {
				fail(w, err)
				return
			}
			if out == nil {
				out = map[string]bool{"ok": true}
			}
			writeJSON(w, 201, out)
		}
	}
	sendWith := func(build func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error)) func(http.ResponseWriter, *http.Request, params) {
		return act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
			if p["chat"] != "" {
				req.ChatID = p["chat"]
			}
			to, err := s.target(req.ChatID)
			if err != nil {
				return nil, err
			}
			if hold, why, err := s.policyGate(ctx, s.adb(), mirrorHistory{s}, chatID(to), time.Now()); err != nil {
				return nil, err
			} else if !hold.IsZero() {
				return nil, httpError{429, "policy: " + why + "; try again after " + hold.UTC().Format(time.RFC3339)}
			}
			if req.ID != "" {
				if !validClientID(req.ID) {
					return nil, httpError{400, "id must be 8 to 64 letters and digits"}
				}
				// a retry after a timeout or a lost answer: the message is already ours in the mirror, return it
				if prev, err := s.message(ctx, chatID(to), req.ID); err == nil && prev["fromMe"] == true {
					return prev, nil
				}
			}
			msg, err := build(ctx, s, c, req)
			if err != nil {
				return nil, err
			}
			ci, err := s.replyContext(ctx, req.ChatID, req.ReplyTo, req.Mentions)
			if err != nil {
				return nil, err
			}
			return s.sendID(ctx, c, to, withContext(msg, ci), req.ID)
		})
	}

	text := sendWith(func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error) {
		if strings.TrimSpace(req.Text) == "" {
			return nil, httpError{400, "text is required"}
		}
		return &waE2E.Message{Conversation: proto.String(req.Text)}, nil
	})
	rt.add("POST", "/api/sendText", text)
	rt.add("POST", "/api/{session}/chats/{chat}/messages", text)
	for path, kind := range map[string]string{"/api/sendImage": "image", "/api/sendFile": "file", "/api/sendVoice": "voice", "/api/sendVideo": "video"} {
		kind := kind
		rt.add("POST", path, sendWith(func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error) {
			caption := req.Caption
			if caption == "" {
				caption = req.Text
			}
			return mediaMessage(ctx, c, kind, req.File, caption, req.Convert)
		}))
	}
	rt.add("POST", "/api/sendLocation", sendWith(func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error) {
		if req.Latitude == 0 && req.Longitude == 0 {
			return nil, httpError{400, "latitude and longitude are required"}
		}
		return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(req.Latitude),
			DegreesLongitude: proto.Float64(req.Longitude), Name: strPtr(req.Title), Address: strPtr(req.Address)}}, nil
	}))
	rt.add("POST", "/api/sendContactVcard", sendWith(func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error) {
		return contactsMessage(req.Contacts)
	}))
	rt.add("POST", "/api/sendPoll", sendWith(func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error) {
		if req.Poll == nil || req.Poll.Name == "" || len(req.Poll.Options) < 2 {
			return nil, httpError{400, "poll needs a name and at least two options"}
		}
		n := 1
		if req.Poll.MultipleAnswers {
			n = 0
		}
		return c.BuildPollCreation(req.Poll.Name, req.Poll.Options, n), nil
	}))
	rt.add("POST", "/api/forwardMessage", sendWith(func(ctx context.Context, s *Session, c waConn, req *sendReq) (*waE2E.Message, error) {
		q, err := s.lookup(ctx, "", req.MessageID)
		if err != nil {
			return nil, err
		}
		return forwardable(q)
	}))

	react := act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
		if req.Reaction == nil {
			return nil, httpError{400, "reaction is required (empty string removes it)"}
		}
		q, err := s.lookup(ctx, p["chat"], firstNonEmpty(p["id"], req.MessageID))
		if err != nil {
			return nil, err
		}
		msg := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: lastKey(c, q, s.Me()), Text: proto.String(*req.Reaction),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli())}}
		if _, err := s.send(ctx, c, q.chat, msg); err != nil {
			return nil, err
		}
		return map[string]any{"messageId": q.id, "reaction": *req.Reaction}, nil
	})
	rt.add("PUT", "/api/reaction", react)
	rt.add("PUT", "/api/{session}/chats/{chat}/messages/{id}/reaction", react)

	rt.add("PUT", "/api/{session}/chats/{chat}/messages/{id}", act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
		q, err := s.lookup(ctx, p["chat"], p["id"])
		if err != nil {
			return nil, err
		}
		if !q.fromMe {
			return nil, httpError{422, "only your own messages can be edited"}
		}
		if strings.TrimSpace(req.Text) == "" {
			return nil, httpError{400, "text is required"}
		}
		edit := &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Key: lastKey(c, q, s.Me()), Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			EditedMessage: &waE2E.Message{Conversation: proto.String(req.Text)}, TimestampMS: proto.Int64(time.Now().UnixMilli())}}}}
		if _, err := s.send(ctx, c, q.chat, edit); err != nil {
			return nil, err
		}
		return s.message(ctx, chatID(q.chat), q.id)
	}))
	rt.add("DELETE", "/api/{session}/chats/{chat}/messages/{id}", act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
		q, err := s.lookup(ctx, p["chat"], p["id"])
		if err != nil {
			return nil, err
		}
		msg := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: lastKey(c, q, s.Me())}}
		if _, err := s.send(ctx, c, q.chat, msg); err != nil {
			return nil, err
		}
		return map[string]any{"revoked": q.id}, nil
	}))

	seen := act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
		chat := firstNonEmpty(p["chat"], req.ChatID)
		ids := req.MessageIDs
		if req.MessageID != "" {
			ids = append(ids, req.MessageID)
		}
		m, err := s.Mirror()
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 { // everything from others since our last message
			rows, err := m.r.QueryContext(ctx, `SELECT wa_id FROM messages WHERE chat_id=? AND from_me=0 AND ts > COALESCE(
				(SELECT MAX(ts) FROM messages WHERE chat_id=? AND from_me=1), 0) ORDER BY ts DESC LIMIT 50`, s.canonChat(chat), s.canonChat(chat))
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id string
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
		}
		// receipts go per sender (groups need it)
		bySender := map[types.JID][]types.MessageID{}
		var target types.JID
		for _, id := range ids {
			q, err := s.lookup(ctx, chat, id)
			if err != nil || q.fromMe {
				continue
			}
			target = q.chat
			bySender[q.sender] = append(bySender[q.sender], q.id)
		}
		for sender, list := range bySender {
			if err := c.MarkRead(ctx, list, time.Now(), target, sender); err != nil {
				return nil, httpError{502, err.Error()}
			}
		}
		n := 0
		for _, l := range bySender {
			n += len(l)
		}
		return map[string]any{"read": n}, nil
	})
	rt.add("POST", "/api/sendSeen", seen)
	rt.add("POST", "/api/{session}/chats/{chat}/messages/read", seen)

	typing := func(state types.ChatPresence) func(http.ResponseWriter, *http.Request, params) {
		return act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
			to, err := s.target(req.ChatID)
			if err != nil {
				return nil, err
			}
			return nil, c.SendChatPresence(ctx, to, state, "")
		})
	}
	rt.add("POST", "/api/startTyping", typing(types.ChatPresenceComposing))
	rt.add("POST", "/api/stopTyping", typing(types.ChatPresencePaused))
	rt.add("POST", "/api/{session}/presence", act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
		switch req.Presence {
		case "online":
			return nil, c.SendPresence(ctx, types.PresenceAvailable)
		case "offline":
			return nil, c.SendPresence(ctx, types.PresenceUnavailable)
		case "typing", "recording", "paused":
			to, err := s.target(req.ChatID)
			if err != nil {
				return nil, err
			}
			st, media := types.ChatPresenceComposing, types.ChatPresenceMediaText
			if req.Presence == "paused" {
				st = types.ChatPresencePaused
			} else if req.Presence == "recording" {
				media = types.ChatPresenceMediaAudio
			}
			return nil, c.SendChatPresence(ctx, to, st, media)
		}
		return nil, httpError{400, "presence: online, offline, typing, recording or paused"}
	}))

	archive := func(on bool) func(http.ResponseWriter, *http.Request, params) {
		return act(true, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
			to, err := s.target(p["chat"])
			if err != nil {
				return nil, err
			}
			m, _ := s.Mirror()
			var id string
			m.r.QueryRowContext(ctx, "SELECT wa_id FROM messages WHERE chat_id=? ORDER BY ts DESC LIMIT 1", chatID(to)).Scan(&id)
			var key *waCommon.MessageKey
			ts := time.Now()
			if id != "" {
				q, err := s.lookup(ctx, chatID(to), id)
				if err == nil {
					key = lastKey(c, q, s.Me())
					ts = time.Unix(q.ts, 0)
				}
			}
			return nil, c.SendAppState(ctx, appstate.BuildArchive(to, on, ts, key))
		})
	}
	rt.add("POST", "/api/{session}/chats/{chat}/archive", archive(true))
	rt.add("POST", "/api/{session}/chats/{chat}/unarchive", archive(false))

	// groups: changes need sending on, reads only a connection
	live := func(h func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error), needSending bool) func(http.ResponseWriter, *http.Request, params) {
		return act(needSending, func(ctx context.Context, s *Session, c waConn, req *sendReq, p params) (any, error) {
			cli, ok := c.(*whatsmeow.Client)
			if !ok {
				return nil, httpError{422, "needs a live connection"}
			}
			return h(ctx, s, cli, req, p)
		})
	}
	jids := func(s *Session, req *sendReq) []types.JID {
		var out []types.JID
		for _, p := range req.Participants {
			if j, err := s.target(p.ID); err == nil {
				out = append(out, j)
			}
		}
		return out
	}
	rt.add("POST", "/api/{session}/groups", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		if req.Name == "" {
			return nil, httpError{400, "name is required"}
		}
		g, err := cli.CreateGroup(ctx, whatsmeow.ReqCreateGroup{Name: req.Name, Participants: jids(s, req)})
		if err != nil {
			return nil, httpError{502, err.Error()}
		}
		s.mirror.Push(Op{ChatID: chatID(g.JID), ChatName: g.Name, ChatGroup: true})
		return groupJSON(g), nil
	}, true))
	rt.add("POST", "/api/{session}/groups/{id}/leave", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		j, err := types.ParseJID(p["id"])
		if err != nil {
			return nil, httpError{400, err.Error()}
		}
		return nil, cli.LeaveGroup(ctx, j)
	}, true))
	rt.add("PUT", "/api/{session}/groups/{id}/subject", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		j, err := types.ParseJID(p["id"])
		if err != nil {
			return nil, httpError{400, err.Error()}
		}
		return nil, cli.SetGroupName(ctx, j, req.Subject)
	}, true))
	rt.add("PUT", "/api/{session}/groups/{id}/description", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		j, err := types.ParseJID(p["id"])
		if err != nil || req.Description == nil {
			return nil, httpError{400, "description is required"}
		}
		return nil, cli.SetGroupDescription(ctx, j, *req.Description)
	}, true))
	rt.add("GET", "/api/{session}/groups/{id}/invite-code", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		j, err := types.ParseJID(p["id"])
		if err != nil {
			return nil, httpError{400, err.Error()}
		}
		link, err := cli.GetGroupInviteLink(ctx, j, false)
		return map[string]string{"link": link}, err
	}, false))
	rt.add("POST", "/api/{session}/groups/{id}/invite-code/revoke", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		j, err := types.ParseJID(p["id"])
		if err != nil {
			return nil, httpError{400, err.Error()}
		}
		link, err := cli.GetGroupInviteLink(ctx, j, true)
		return map[string]string{"link": link}, err
	}, true))
	for _, a := range []string{"add", "remove", "promote", "demote"} {
		a := a
		rt.add("POST", "/api/{session}/groups/{id}/participants/"+a, live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
			j, err := types.ParseJID(p["id"])
			if err != nil {
				return nil, httpError{400, err.Error()}
			}
			res, err := cli.UpdateGroupParticipants(ctx, j, jids(s, req), whatsmeow.ParticipantChange(a))
			if err != nil {
				return nil, httpError{502, err.Error()}
			}
			out := []map[string]any{}
			for _, r := range res {
				out = append(out, map[string]any{"id": chatID(r.JID), "error": r.Error})
			}
			s.invalidateGroups()
			return out, nil
		}, true))
	}

	// contacts and profile
	block := func(action events.BlocklistChangeAction) func(http.ResponseWriter, *http.Request, params) {
		return live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
			j, err := s.target(req.ContactID)
			if err != nil {
				return nil, err
			}
			_, err = cli.UpdateBlocklist(ctx, j, action, "")
			return nil, err
		}, true)
	}
	rt.add("POST", "/api/contacts/block", block(events.BlocklistChangeActionBlock))
	rt.add("POST", "/api/contacts/unblock", block(events.BlocklistChangeActionUnblock))
	rt.add("PUT", "/api/{session}/profile/status", live(func(ctx context.Context, s *Session, cli *whatsmeow.Client, req *sendReq, p params) (any, error) {
		return nil, cli.SetStatusMessage(ctx, types.SetStatusInput{Text: proto.String(req.Status)})
	}, true))
	rt.add("GET", "/api/contacts/about", mg.read(func(ctx context.Context, s *Session, cli *whatsmeow.Client, r *http.Request, p params) (any, error) {
		j, err := s.target(r.URL.Query().Get("contactId"))
		if err != nil {
			return nil, err
		}
		info, err := cli.GetUserInfo(ctx, []types.JID{j})
		if err != nil {
			return nil, err
		}
		return map[string]any{"about": info[j].Status}, nil
	}))
	picture := mg.read(func(ctx context.Context, s *Session, cli *whatsmeow.Client, r *http.Request, p params) (any, error) {
		j, err := s.target(firstNonEmpty(p["chat"], r.URL.Query().Get("contactId")))
		if err != nil {
			return nil, err
		}
		return s.pictures.get(ctx, cli, j, r.URL.Query().Get("refresh") == "true")
	})
	rt.add("GET", "/api/contacts/profile-picture", picture)
	rt.add("GET", "/api/{session}/chats/{chat}/picture", picture)
	rt.add("GET", "/api/{session}/chats/{chat}/avatar", mg.avatar)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// read wraps a live read that needs a connection but not sending.
func (mg *Manager) read(h func(ctx context.Context, s *Session, cli *whatsmeow.Client, r *http.Request, p params) (any, error)) func(http.ResponseWriter, *http.Request, params) {
	return func(w http.ResponseWriter, r *http.Request, p params) {
		s, err := mg.session(p, r)
		if err != nil {
			fail(w, err)
			return
		}
		cli := s.Client()
		if cli == nil || !cli.IsConnected() {
			fail(w, httpError{422, "session is not connected"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		out, err := h(ctx, s, cli, r, p)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, out)
	}
}

// pictureCache keeps profile picture answers for a day; a missing picture is an answer too.
type pictureCache struct {
	mu   sync.Mutex
	m    map[types.JID]picEntry
	slot chan struct{}
}

type picEntry struct {
	url string
	at  time.Time
	img []byte
}

func (pc *pictureCache) get(ctx context.Context, cli *whatsmeow.Client, j types.JID, refresh bool) (map[string]any, error) {
	e, err := pc.entry(ctx, cli, j, refresh, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": nullable(e.url)}, nil
}

func (pc *pictureCache) entry(ctx context.Context, cli *whatsmeow.Client, j types.JID, refresh, withImage bool) (picEntry, error) {
	pc.mu.Lock()
	if pc.m == nil {
		pc.m, pc.slot = map[types.JID]picEntry{}, make(chan struct{}, 4)
	}
	e, ok := pc.m[j]
	pc.mu.Unlock()
	if ok && !refresh && time.Since(e.at) < 24*time.Hour && (!withImage || e.url == "" || e.img != nil) {
		return e, nil
	}
	select {
	case pc.slot <- struct{}{}:
	case <-ctx.Done():
		return e, ctx.Err()
	}
	defer func() { <-pc.slot }()
	info, err := cli.GetProfilePictureInfo(ctx, j, &whatsmeow.GetProfilePictureParams{Preview: true})
	e = picEntry{at: time.Now()}
	if err == nil && info != nil {
		e.url = info.URL
	} else if err != nil && !strings.Contains(err.Error(), "not-authorized") && !strings.Contains(err.Error(), "item-not-found") && !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "401") {
		return e, err
	}
	if withImage && e.url != "" {
		req, _ := http.NewRequestWithContext(ctx, "GET", e.url, nil)
		if resp, err := fetchClient.Do(req); err == nil {
			e.img, _ = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
		}
	}
	pc.mu.Lock()
	if len(pc.m) > 2000 {
		pc.m = map[types.JID]picEntry{}
	}
	pc.m[j] = e
	pc.mu.Unlock()
	return e, nil
}

// avatar serves the picture itself, so the dashboard can use a plain <img>.
func (mg *Manager) avatar(w http.ResponseWriter, r *http.Request, p params) {
	s, err := mg.session(p, r)
	if err != nil {
		fail(w, err)
		return
	}
	cli := s.Client()
	if cli == nil || !cli.IsConnected() {
		w.WriteHeader(204)
		return
	}
	j, err := s.target(p["chat"])
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	e, err := s.pictures.entry(ctx, cli, j, false, true)
	if err != nil || len(e.img) == 0 {
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.WriteHeader(204)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(e.img))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(e.img)
}
