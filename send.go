package main

// Sending and every other action that changes something on WhatsApp. All of it is off per session until the
// owner turns sending on (config.sending), because a linked device that sends raises WhatsApp's ban risk.
// What we send is written to the mirror at once: WhatsApp does not echo our own messages back to us.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// waConn is the part of the WhatsApp client that actions use; tests put a recorder in its place.
type waConn interface {
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	BuildMessageKey(chat, sender types.JID, id types.MessageID) *waCommon.MessageKey
	BuildPollCreation(name string, optionNames []string, selectableOptionCount int) *waE2E.Message
	MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID, receiptTypeExtra ...types.ReceiptType) error
	SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error
	SendPresence(ctx context.Context, state types.Presence) error
	SendAppState(ctx context.Context, patch appstate.PatchInfo) error
}

var errSendingOff = httpError{403, "sending is off for this session; turn it on in the dashboard settings"}

// sendError is WhatsApp refusing or failing a send (502), with the library's error kept for classify().
type sendError struct {
	httpError
	cause error
}

func (e sendError) Unwrap() error { return e.cause }

// conn returns the live connection for an action, or why there is none.
func (s *Session) conn(needSending bool) (waConn, error) {
	if needSending && !s.cfg().Sending {
		return nil, errSendingOff
	}
	if needSending && killed(s.adb(), s.policyCfg()) {
		return nil, httpError{403, "the kill switch is on: nothing is sent (POST /api/{session}/policy/kill {\"on\": false})"}
	}
	if s.fake != nil {
		if st, _, _ := s.Status(); st != StatusWorking { // a stopped stand-in fails like a disconnected session
			return nil, httpError{422, "session is not connected"}
		}
		return s.fake, nil
	}
	cli := s.Client()
	if cli == nil || !cli.IsConnected() || !cli.IsLoggedIn() {
		return nil, httpError{422, "session is not connected"}
	}
	return cli, nil
}

// target turns any spelling of a chat into the address to send to (phone address when known).
func (s *Session) target(chat string) (types.JID, error) {
	if chat == "" {
		return types.JID{}, httpError{400, "chatId is required"}
	}
	j, err := parseChatID(s.canonChat(chat))
	if err != nil {
		return types.JID{}, httpError{400, "bad chatId: " + err.Error()}
	}
	return j, nil
}

// quoted finds a message in the mirror by WAHA id or bare id, for replies, reactions, edits and deletes.
type quotedMsg struct {
	chat   types.JID
	id     string
	sender types.JID // empty for our own
	fromMe bool
	body   string
	ts     int64
	media  []byte
}

func (s *Session) lookup(ctx context.Context, chat, id string) (*quotedMsg, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	if chat == "" { // a WAHA id carries its chat
		if p := strings.Split(id, "_"); len(p) >= 3 {
			chat = p[1]
		}
	}
	q := "SELECT chat_id, wa_id, from_me, COALESCE(sender,''), body, ts, (SELECT msg FROM media md WHERE md.chat_id=m.chat_id AND md.wa_id=m.wa_id) FROM messages m WHERE wa_id=?"
	args := []any{tail(id)}
	if chat != "" {
		q += " AND chat_id=?"
		args = append(args, s.canonChat(chat))
	}
	var c, wid, sender string
	var r quotedMsg
	if err := m.r.QueryRowContext(ctx, q+" ORDER BY ts DESC LIMIT 1", args...).Scan(&c, &wid, &r.fromMe, &sender, &r.body, &r.ts, &r.media); err != nil {
		return nil, httpError{404, "message " + id + " is not in the mirror"}
	}
	r.chat, _ = parseChatID(c)
	r.id = wid
	if !r.fromMe && sender != "" {
		r.sender, _ = parseChatID(sender)
	}
	return &r, nil
}

func (s *Session) replyContext(ctx context.Context, chat, replyTo string, mentions []string) (*waE2E.ContextInfo, error) {
	var ci *waE2E.ContextInfo
	if replyTo != "" {
		q, err := s.lookup(ctx, chat, replyTo)
		if err != nil {
			return nil, err
		}
		ci = &waE2E.ContextInfo{StanzaID: proto.String(q.id), QuotedMessage: &waE2E.Message{Conversation: proto.String(q.body)}}
		participant := q.sender
		if q.fromMe {
			participant = s.Me()
		}
		if !participant.IsEmpty() {
			ci.Participant = proto.String(participant.String())
		}
	}
	for _, m := range mentions {
		j, err := parseChatID(s.canonChat(m))
		if err != nil {
			continue
		}
		if ci == nil {
			ci = &waE2E.ContextInfo{}
		}
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}
	return ci, nil
}

// send delivers one message and records it as ours.
func (s *Session) send(ctx context.Context, c waConn, to types.JID, msg *waE2E.Message) (map[string]any, error) {
	return s.sendID(ctx, c, to, msg, "")
}

// sendID is send with the client's own message id (empty: WhatsApp's usual generated one).
func (s *Session) sendID(ctx context.Context, c waConn, to types.JID, msg *waE2E.Message, id string) (map[string]any, error) {
	var extra []whatsmeow.SendRequestExtra
	if id != "" {
		extra = append(extra, whatsmeow.SendRequestExtra{ID: types.MessageID(id)})
	}
	fresh := mirrorHistory{s}.empty(ctx, chatID(to))
	resp, err := c.SendMessage(ctx, to, msg, extra...)
	if err != nil {
		return nil, sendError{httpError{502, "WhatsApp refused the message: " + err.Error()}, err}
	}
	if db := s.adb(); db != nil && msg.ReactionMessage == nil && msg.ProtocolMessage == nil {
		db.recordSent(resp.ID, chatID(to), time.Now(), fresh, originOf(ctx))
	}
	evt := &events.Message{Info: types.MessageInfo{ID: resp.ID, Timestamp: resp.Timestamp,
		MessageSource: types.MessageSource{Chat: to, Sender: s.Me(), IsFromMe: true, IsGroup: to.Server == types.GroupServer}},
		RawMessage: msg}
	evt.UnwrapRaw()
	conv := convertMessage(evt, s, s.learn)
	m, _ := s.Mirror()
	if conv.Row != nil {
		m.Push(Op{Rows: []Row{*conv.Row}})
		m.Flush() // the reply below and the next read see it
		j := s.messageJSON(rowToStored(*conv.Row))
		s.emit("message.any", j)
		return j, nil
	}
	for _, op := range conv.Ops {
		m.Push(op)
	}
	m.Flush()
	s.emitChange(evt, conv)
	return map[string]any{"id": resp.ID, "timestamp": resp.Timestamp.Unix()}, nil
}

// validClientID: a message id a client may choose; WhatsApp's own ids are uppercase hex of this length.
func validClientID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- building messages

type fileIn struct {
	Mimetype string `json:"mimetype"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	Data     string `json:"data"`
}

var fetchClient = &http.Client{Timeout: 2 * time.Minute}

const maxFile = 100 << 20

func (f *fileIn) bytes(ctx context.Context) ([]byte, error) {
	switch {
	case f == nil:
		return nil, httpError{400, "file is required"}
	case f.Data != "":
		b, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return nil, httpError{400, "file.data is not base64"}
		}
		return b, nil
	case f.URL != "":
		req, err := http.NewRequestWithContext(ctx, "GET", f.URL, nil)
		if err != nil {
			return nil, httpError{400, "file.url: " + err.Error()}
		}
		resp, err := fetchClient.Do(req)
		if err != nil {
			return nil, httpError{502, "file.url: " + err.Error()}
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, httpError{502, "file.url answered " + resp.Status}
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxFile {
			return nil, httpError{413, "file is over 100 MB"}
		}
		if f.Mimetype == "" {
			f.Mimetype = strings.Split(resp.Header.Get("Content-Type"), ";")[0]
		}
		return b, nil
	}
	return nil, httpError{400, "file needs data (base64) or url"}
}

func mimeOf(f *fileIn, b []byte) string {
	if f.Mimetype != "" {
		return f.Mimetype
	}
	return http.DetectContentType(b)
}

// thumbnail: a small JPEG for the preview the recipient sees before downloading.
func thumbnail(b []byte) (jpg []byte, w, h int) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, 0, 0
	}
	r := img.Bounds()
	w, h = r.Dx(), r.Dy()
	tw, th := 72, 72
	if w > h {
		th = max(1, 72*h/w)
	} else {
		tw = max(1, 72*w/h)
	}
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			small.Set(x, y, img.At(r.Min.X+x*w/tw, r.Min.Y+y*h/th))
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, small, &jpeg.Options{Quality: 60})
	return buf.Bytes(), w, h
}

// ffmpeg reads its input from a temporary file, because MP4 often keeps its index at the end where a pipe cannot seek.
func ffmpeg(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	f, err := os.CreateTemp("", "wago-media-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	f.Write(in)
	f.Close()
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-i", f.Name()}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// seconds reads the length from a temporary file: OGG and MP4 only state it where a stream reader cannot see it.
func seconds(ctx context.Context, b []byte) uint32 {
	f, err := os.CreateTemp("", "wago-media-*")
	if err != nil {
		return 0
	}
	defer os.Remove(f.Name())
	f.Write(b)
	f.Close()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", f.Name()).Output()
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return uint32(v + 0.5)
}

// mediaMessage uploads the file and builds the message of the given kind (image, video, voice, file).
func mediaMessage(ctx context.Context, c waConn, kind string, f *fileIn, caption string, convert bool) (*waE2E.Message, error) {
	data, err := f.bytes(ctx)
	if err != nil {
		return nil, err
	}
	mime := mimeOf(f, data)
	var thumb []byte
	var w, h int
	var secs uint32
	switch kind {
	case "voice":
		if convert || !strings.HasPrefix(mime, "audio/ogg") {
			if data, err = ffmpeg(ctx, data, "-vn", "-c:a", "libopus", "-b:a", "32k", "-ac", "1", "-ar", "48000", "-f", "ogg", "pipe:1"); err != nil {
				return nil, httpError{422, "voice must be OGG/Opus; converting failed: " + err.Error()}
			}
		}
		mime, secs = "audio/ogg; codecs=opus", seconds(ctx, data)
	case "video":
		if convert && mime != "video/mp4" {
			if data, err = ffmpeg(ctx, data, "-c:v", "libx264", "-preset", "veryfast", "-c:a", "aac", "-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1"); err != nil {
				return nil, httpError{422, "converting the video failed: " + err.Error()}
			}
			mime = "video/mp4"
		}
		secs = seconds(ctx, data)
		if frame, err := ffmpeg(ctx, data, "-frames:v", "1", "-f", "image2", "-c:v", "mjpeg", "pipe:1"); err == nil {
			thumb, w, h = thumbnail(frame)
		}
	case "image":
		thumb, w, h = thumbnail(data)
	}
	mt := map[string]whatsmeow.MediaType{"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo, "voice": whatsmeow.MediaAudio, "file": whatsmeow.MediaDocument}[kind]
	up, err := c.Upload(ctx, data, mt)
	if err != nil {
		return nil, httpError{502, "upload failed: " + err.Error()}
	}
	size := proto.Uint64(uint64(len(data)))
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return proto.String(s)
	}
	dim := func(v int) *uint32 {
		if v <= 0 {
			return nil
		}
		return proto.Uint32(uint32(v))
	}
	switch kind {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: opt(caption), Mimetype: proto.String(mime), URL: &up.URL,
			DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: size, JPEGThumbnail: thumb, Width: dim(w), Height: dim(h)}}, nil
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: opt(caption), Mimetype: proto.String(mime), URL: &up.URL,
			DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: size, JPEGThumbnail: thumb, Width: dim(w), Height: dim(h), Seconds: dim(int(secs))}}, nil
	case "voice":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(mime), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size,
			PTT: proto.Bool(true), Seconds: dim(int(secs))}}, nil
	}
	name := f.Filename
	if name == "" {
		name = "file"
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: opt(caption), Mimetype: proto.String(mime), FileName: proto.String(name),
		Title: proto.String(name), URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
		FileSHA256: up.FileSHA256, FileLength: size}}, nil
}

// withContext attaches reply and mention info to whatever content the message has.
func withContext(m *waE2E.Message, ci *waE2E.ContextInfo) *waE2E.Message {
	if ci == nil {
		return m
	}
	switch {
	case m.Conversation != nil:
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: m.Conversation, ContextInfo: ci}}
	case m.ExtendedTextMessage != nil:
		m.ExtendedTextMessage.ContextInfo = ci
	case m.ImageMessage != nil:
		m.ImageMessage.ContextInfo = ci
	case m.VideoMessage != nil:
		m.VideoMessage.ContextInfo = ci
	case m.AudioMessage != nil:
		m.AudioMessage.ContextInfo = ci
	case m.DocumentMessage != nil:
		m.DocumentMessage.ContextInfo = ci
	case m.LocationMessage != nil:
		m.LocationMessage.ContextInfo = ci
	case m.ContactMessage != nil:
		m.ContactMessage.ContextInfo = ci
	case m.ContactsArrayMessage != nil:
		m.ContactsArrayMessage.ContextInfo = ci
	}
	return m
}

type contactIn struct {
	FullName     string `json:"fullName"`
	Organization string `json:"organization"`
	PhoneNumber  string `json:"phoneNumber"`
	WhatsappID   string `json:"whatsappId"`
	Vcard        string `json:"vcard"`
}

func (c contactIn) card() string {
	if c.Vcard != "" {
		return c.Vcard
	}
	wa := c.WhatsappID
	if wa == "" {
		wa = strings.TrimLeft(strings.ReplaceAll(c.PhoneNumber, " ", ""), "+")
	}
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\nVERSION:3.0\nFN:" + c.FullName + "\n")
	if c.Organization != "" {
		b.WriteString("ORG:" + c.Organization + ";\n")
	}
	if c.PhoneNumber != "" {
		b.WriteString("TEL;type=CELL;type=VOICE;waid=" + wa + ":" + c.PhoneNumber + "\n")
	}
	b.WriteString("END:VCARD")
	return b.String()
}

func contactsMessage(cs []contactIn) (*waE2E.Message, error) {
	if len(cs) == 0 {
		return nil, httpError{400, "contacts are required"}
	}
	if len(cs) == 1 {
		return &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String(cs[0].FullName), Vcard: proto.String(cs[0].card())}}, nil
	}
	arr := &waE2E.ContactsArrayMessage{DisplayName: proto.String(fmt.Sprintf("%d contacts", len(cs)))}
	for _, c := range cs {
		arr.Contacts = append(arr.Contacts, &waE2E.ContactMessage{DisplayName: proto.String(c.FullName), Vcard: proto.String(c.card())})
	}
	return &waE2E.Message{ContactsArrayMessage: arr}, nil
}

// forwardable rebuilds a stored message so it can be sent again: its media, or its text.
func forwardable(q *quotedMsg) (*waE2E.Message, error) {
	fwd := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)}
	if len(q.media) > 0 {
		var m waE2E.Message
		if err := proto.Unmarshal(q.media, &m); err != nil {
			return nil, err
		}
		if q.body != "" {
			switch {
			case m.ImageMessage != nil:
				m.ImageMessage.Caption = proto.String(q.body)
			case m.VideoMessage != nil:
				m.VideoMessage.Caption = proto.String(q.body)
			case m.DocumentMessage != nil:
				m.DocumentMessage.Caption = proto.String(q.body)
			}
		}
		return withContext(&m, fwd), nil
	}
	if q.body == "" {
		return nil, httpError{422, "this message has neither text nor stored media to forward"}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(q.body), ContextInfo: fwd}}, nil
}

func lastKey(c waConn, q *quotedMsg, me types.JID) *waCommon.MessageKey {
	sender := q.sender
	if q.fromMe {
		sender = me
	}
	return c.BuildMessageKey(q.chat, sender, q.id)
}

var _ = errors.New
