package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func post(t *testing.T, srv *httptest.Server, method, path string, body any, into any) int {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func TestActions(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	fc := &fakeConn{fixed: true}
	s.fake = fc
	s.Start()
	m, _ := s.Mirror()
	ali := "996700000002@c.us"
	grp := "120363000000000001@g.us"
	m.Push(Op{Rows: []Row{
		{WaID: "IN1", ChatID: ali, Ts: 1795400000, Sender: ali, Body: "привет", Kind: "conversation"},
		{WaID: "IN2", ChatID: ali, Ts: 1795400001, Sender: ali, Body: "как дела", Kind: "conversation"},
		{WaID: "G1", ChatID: grp, Ts: 1795400002, Sender: "996700000003@c.us", Body: "в группе", Kind: "conversation"},
	}})
	m.Flush()

	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "default", "chatId": ali, "text": "x"}, nil); code != 403 {
		t.Fatalf("sending off must refuse, got %d", code)
	}
	c := s.cfg()
	c.Sending = true
	s.configure(c)

	var out map[string]any
	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "default", "chatId": "+996700000002", "text": "ответ", "reply_to": "false_" + ali + "_IN1"}, &out); code != 201 {
		t.Fatal(code, out)
	}
	l := fc.last()
	ci := l.msg.GetExtendedTextMessage().GetContextInfo()
	if l.to.String() != "996700000002@s.whatsapp.net" || l.msg.GetExtendedTextMessage().GetText() != "ответ" || ci.GetStanzaID() != "IN1" ||
		ci.GetParticipant() != "996700000002@s.whatsapp.net" || ci.GetQuotedMessage().GetConversation() != "привет" {
		t.Fatalf("reply %v", l.msg)
	}
	if out["fromMe"] != true || out["body"] != "ответ" || out["id"] != "true_"+ali+"_OUT1" {
		t.Fatalf("response %v", out)
	}
	var body string
	var fromMe bool
	m.r.QueryRow("SELECT body, from_me FROM messages WHERE wa_id='OUT1' AND chat_id=?", ali).Scan(&body, &fromMe)
	if body != "ответ" || !fromMe {
		t.Fatal("sent message not mirrored")
	}

	// an image: uploaded as an image, with dimensions and a preview
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	img.Set(5, 5, color.RGBA{255, 0, 0, 255})
	var pngb bytes.Buffer
	png.Encode(&pngb, img)
	if code := post(t, srv, "POST", "/api/sendImage", map[string]any{"session": "default", "chatId": grp, "caption": "счёт",
		"file": map[string]string{"mimetype": "image/png", "data": base64.StdEncoding.EncodeToString(pngb.Bytes())}}, &out); code != 201 {
		t.Fatal(code, out)
	}
	im := fc.last().msg.GetImageMessage()
	if im.GetWidth() != 400 || im.GetHeight() != 200 || len(im.GetJPEGThumbnail()) == 0 || im.GetCaption() != "счёт" || fc.uploads[0] != whatsmeow.MediaImage {
		t.Fatalf("image %v", im)
	}
	if out["hasMedia"] != true {
		t.Fatal("image row", out)
	}

	// a voice note from any audio is converted to OGG/Opus with its length
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		wav, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-f", "wav", "pipe:1").Output()
		if err != nil {
			t.Fatal(err)
		}
		if code := post(t, srv, "POST", "/api/sendVoice", map[string]any{"session": "default", "chatId": ali,
			"file": map[string]string{"mimetype": "audio/wav", "data": base64.StdEncoding.EncodeToString(wav)}}, &out); code != 201 {
			t.Fatal(code, out)
		}
		a := fc.last().msg.GetAudioMessage()
		if !a.GetPTT() || a.GetMimetype() != "audio/ogg; codecs=opus" || a.GetSeconds() != 2 {
			t.Fatalf("voice %v", a)
		}
	}

	if _, err := exec.LookPath("ffmpeg"); err == nil {
		dir := t.TempDir()
		if err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=10",
			"-pix_fmt", "yuv420p", dir+"/v.mp4").Run(); err != nil {
			t.Fatal(err)
		}
		mp4, _ := os.ReadFile(dir + "/v.mp4")
		if code := post(t, srv, "POST", "/api/sendVideo", map[string]any{"session": "default", "chatId": ali, "caption": "видео",
			"file": map[string]string{"mimetype": "video/mp4", "data": base64.StdEncoding.EncodeToString(mp4)}}, &out); code != 201 {
			t.Fatal(code, out)
		}
		v := fc.last().msg.GetVideoMessage()
		if v.GetSeconds() != 3 || v.GetWidth() != 320 || len(v.GetJPEGThumbnail()) == 0 || v.GetCaption() != "видео" {
			t.Fatalf("video %v", v)
		}
	}
	post(t, srv, "POST", "/api/sendLocation", map[string]any{"session": "default", "chatId": ali, "latitude": 42.87, "longitude": 74.59, "title": "Офис"}, nil)
	if fc.last().msg.GetLocationMessage().GetName() != "Офис" {
		t.Fatal("location")
	}
	post(t, srv, "POST", "/api/sendContactVcard", map[string]any{"session": "default", "chatId": ali, "contacts": []map[string]string{{"fullName": "Али", "phoneNumber": "+996 700 000 002"}}}, nil)
	if v := fc.last().msg.GetContactMessage().GetVcard(); !bytes.Contains([]byte(v), []byte("waid=996700000002")) {
		t.Fatal("vcard", v)
	}
	post(t, srv, "POST", "/api/sendPoll", map[string]any{"session": "default", "chatId": ali, "poll": map[string]any{"name": "Когда?", "options": []string{"Пн", "Вт"}}}, nil)
	if fc.last().msg.GetPollCreationMessageV3().GetName() != "Когда?" {
		t.Fatal("poll")
	}
	post(t, srv, "POST", "/api/forwardMessage", map[string]any{"session": "default", "chatId": grp, "messageId": "false_" + ali + "_IN2"}, nil)
	if f := fc.last(); f.to.Server != types.GroupServer || f.msg.GetExtendedTextMessage().GetText() != "как дела" || !f.msg.GetExtendedTextMessage().GetContextInfo().GetIsForwarded() {
		t.Fatal("forward", f.msg)
	}

	// reaction on someone else's group message: key names them; the mirror shows our reaction
	if code := post(t, srv, "PUT", "/api/reaction", map[string]any{"session": "default", "messageId": "false_" + grp + "_G1_996700000003@c.us", "reaction": "👍"}, &out); code != 201 {
		t.Fatal(code, out)
	}
	k := fc.last().msg.GetReactionMessage().GetKey()
	if k.GetID() != "G1" || k.GetFromMe() || k.GetParticipant() != "996700000003@s.whatsapp.net" {
		t.Fatal("reaction key", k)
	}
	var reactions string
	m.r.QueryRow("SELECT reactions FROM messages WHERE wa_id='G1'").Scan(&reactions)
	if reactions != `{"99600000001@c.us":"👍"}` {
		t.Fatal("reaction mirrored", reactions)
	}

	// edit and delete our own message
	if code := post(t, srv, "PUT", "/api/default/chats/"+ali+"/messages/OUT1", map[string]any{"text": "ответ, исправлено"}, &out); code != 201 || out["body"] != "ответ, исправлено" {
		t.Fatal("edit", code, out)
	}
	if code := post(t, srv, "PUT", "/api/default/chats/"+ali+"/messages/IN1", map[string]any{"text": "x"}, nil); code != 422 {
		t.Fatal("editing someone else's message must be refused")
	}
	post(t, srv, "DELETE", "/api/default/chats/"+ali+"/messages/OUT1", nil, nil)
	var revoked bool
	m.r.QueryRow("SELECT revoked FROM messages WHERE wa_id='OUT1'").Scan(&revoked)
	if !revoked || fc.last().msg.GetProtocolMessage().GetType() != waE2E.ProtocolMessage_REVOKE {
		t.Fatal("delete")
	}

	// read receipts: unread incoming since our last message, grouped by sender
	fc.reads = nil
	m.Push(Op{Rows: []Row{{WaID: "IN3", ChatID: ali, Ts: 1795600000, Sender: ali, Body: "ещё", Kind: "conversation"}}})
	m.Flush()
	post(t, srv, "POST", "/api/sendSeen", map[string]any{"session": "default", "chatId": ali}, &out)
	if len(fc.reads[ali]) != 1 || fc.reads[ali][0] != "IN3" {
		t.Fatal("seen", fc.reads)
	}

	// a new session cannot take a name the API paths use
	if code := post(t, srv, "POST", "/api/sessions", map[string]any{"name": "contacts", "start": false}, nil); code != 400 {
		t.Fatal("reserved name accepted")
	}
}

// Settings apply live: a webhook added through the API gets events at once, the connection is not dropped,
// and a hook still hears the session stop.
func TestLiveConfig(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	s.fake = &fakeConn{fixed: true}
	s.Start()
	got := make(chan string, 16)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e struct{ Event string }
		json.NewDecoder(r.Body).Decode(&e)
		got <- e.Event + " " + r.Header.Get("X-Webhook-Hmac-Algorithm")
	}))
	defer hook.Close()
	cfg := map[string]any{"config": map[string]any{"sending": true, "webhooks": []map[string]any{{"url": hook.URL,
		"events": []string{"message.any", "session.status"}, "hmac": map[string]string{"key": "k"}}}}}
	if code := post(t, srv, "PUT", "/api/sessions/default", cfg, nil); code != 200 {
		t.Fatal("update", code)
	}
	if st, _, _ := s.Status(); st != StatusWorking {
		t.Fatal("a settings change must not drop the connection, status", st)
	}
	wait := func(want string) {
		select {
		case e := <-got:
			if e != want {
				t.Fatalf("event %q, want %q", e, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %q delivered", want)
		}
	}
	post(t, srv, "POST", "/api/sendText", map[string]any{"session": "default", "chatId": "996700000002@c.us", "text": "x"}, nil)
	wait("message.any sha512")
	s.Stop()
	wait("session.status sha512")
	// the key is shown as *** and survives being sent back that way
	var info map[string]any
	cfg["config"].(map[string]any)["webhooks"].([]map[string]any)[0]["hmac"] = map[string]string{"key": "***"}
	post(t, srv, "PUT", "/api/sessions/default", cfg, &info)
	if k := s.cfg().Webhooks[0].HMAC; k == nil || k.Key != "k" {
		t.Fatal("hmac key lost on a round trip")
	}
	// a WAHA client's config has no sending switch: leaving it out keeps the owner's choice, sending it changes it
	waha := map[string]any{"name": "default", "engine": "NOWEB", "config": map[string]any{"webhooks": []map[string]any{{"url": hook.URL, "events": []string{"message"}}}}}
	post(t, srv, "PUT", "/api/sessions/default", waha, nil)
	if !s.cfg().Sending {
		t.Fatal("a config without the sending field turned sending off")
	}
	cfg["config"].(map[string]any)["sending"] = false
	post(t, srv, "PUT", "/api/sessions/default", cfg, nil)
	if s.cfg().Sending {
		t.Fatal("sending:false was ignored")
	}
}

// A file is served from the session's cache by content hash; without a connection and without a cached copy the
// answer says so; a media op replaces a message's keys.
func TestMediaCache(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	m, _ := s.Mirror()
	content := []byte("\x89PNG fake image bytes for the cache")
	sum := sha256.Sum256(content)
	mime := "image/png"
	blob, _ := proto.Marshal(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{FileSHA256: sum[:], Mimetype: &mime}})
	chat := "996700000009@c.us"
	m.Push(Op{Rows: []Row{{WaID: "IMG1", ChatID: chat, Ts: 1795400000, Sender: chat, Kind: "imageMessage", HasMedia: true, MediaType: mime, Media: blob}}})
	m.Flush()
	path := "/api/files/default/" + chat + "/false_" + chat + "_IMG1"
	if code := get(t, srv, path, "secret", nil); code != 503 {
		t.Fatal("no connection and no cache must be 503, got", code)
	}
	key := hex.EncodeToString(sum[:])
	os.MkdirAll(filepath.Join(s.dir, "files", key[:2]), 0o700)
	os.WriteFile(filepath.Join(s.dir, "files", key[:2], key), content, 0o600)
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "\x89PNG" || resp.Header.Get("Content-Type") != mime || resp.Header.Get("Cache-Control") == "" {
		t.Fatal("cached file", resp.StatusCode, resp.Header, b)
	}
	m.Push(Op{MediaChat: chat, MediaID: "IMG1", MediaMsg: []byte("new")})
	m.Flush()
	var got []byte
	m.r.QueryRow("SELECT msg FROM media WHERE chat_id=? AND wa_id='IMG1'", chat).Scan(&got)
	if string(got) != "new" {
		t.Fatal("media op", got)
	}
}
