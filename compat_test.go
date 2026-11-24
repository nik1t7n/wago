package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// What WAHA clients rely on beyond the plain routes (found in real WAHA clients): their own message ids with
// safe retries, one ack event per message in WAHA's shape, hook keys readable with the API key, /api/version.
func TestWAHAClientContract(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	fc := &fakeConn{fixed: true}
	s.fake = fc
	s.Start()
	c := s.cfg()
	c.Sending = true
	c.Webhooks = []Webhook{{URL: "http://127.0.0.1:1/hook", Events: []string{"message.any"}}}
	c.Webhooks[0].HMAC = &struct {
		Key string `json:"key"`
	}{"k123"}
	s.configure(c)
	ali := "996700000002@c.us"

	var out map[string]any
	body := map[string]any{"session": "default", "chatId": ali, "text": "раз", "id": "A1B2C3D4E5F60718"}
	if code := post(t, srv, "POST", "/api/sendText", body, &out); code != 201 || out["id"] != "true_"+ali+"_A1B2C3D4E5F60718" {
		t.Fatal("own id", code, out["id"])
	}
	n := len(fc.sent)
	if code := post(t, srv, "POST", "/api/sendText", body, &out); code != 201 || len(fc.sent) != n || out["body"] != "раз" {
		t.Fatal("a repeat with the same id must return the stored message and send nothing", code, len(fc.sent)-n)
	}
	body["id"] = "bad id!"
	if code := post(t, srv, "POST", "/api/sendText", body, nil); code != 400 {
		t.Fatal("bad id", code)
	}

	got := acks(s, &events.Receipt{MessageSource: types.MessageSource{Chat: types.NewJID("996700000002", types.DefaultUserServer)},
		MessageIDs: []types.MessageID{"A1", "A2"}, Type: types.ReceiptTypeRead, Timestamp: time.Unix(1795500000, 0)})
	if len(got) != 2 || got[0]["id"] != "true_"+ali+"_A1" || got[1]["ack"] != 3 || got[0]["fromMe"] != true || got[0]["to"] != ali {
		t.Fatal("acks", got)
	}
	if acks(s, &events.Receipt{Type: types.ReceiptTypeSender, MessageIDs: []types.MessageID{"A1"}}) != nil {
		t.Fatal("a sender receipt is not a delivery state")
	}

	var info map[string]any
	if code := get(t, srv, "/api/sessions/default", "secret", &info); code != 200 {
		t.Fatal(code)
	}
	hook := info["config"].(map[string]any)["webhooks"].([]any)[0].(map[string]any)
	if hook["hmac"].(map[string]any)["key"] != "k123" {
		t.Fatal("a client with the API key reads its hook key back", hook)
	}
	if code := get(t, srv, "/api/version", "secret", nil); code != 200 {
		t.Fatal("/api/version", code)
	}
}

// -dev-fake incoming traffic reaches webhooks in WAHA's shape: message, edit, revoke, ack.
func TestDevIncoming(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	s.fake = &fakeConn{fixed: true}
	s.Start()
	got := make(chan map[string]any, 16)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e map[string]any
		json.NewDecoder(r.Body).Decode(&e)
		got <- e
	}))
	defer hook.Close()
	c := s.cfg()
	c.Webhooks = []Webhook{{URL: hook.URL, Events: []string{"message", "message.edited", "message.revoked", "message.ack"}}}
	s.configure(c)
	ali := "996700000002@c.us"
	next := func(event string) map[string]any {
		select {
		case e := <-got:
			if e["event"] != event {
				t.Fatalf("event %v, want %s", e["event"], event)
			}
			return e["payload"].(map[string]any)
		case <-time.After(3 * time.Second):
			t.Fatal("no", event)
		}
		return nil
	}
	var out map[string]any
	if code := post(t, srv, "POST", "/api/default/dev/incoming", map[string]any{"chatId": ali, "text": "есть пельмени?", "id": "IN9", "pushName": "Али"}, &out); code != 200 {
		t.Fatal(code, out)
	}
	p := next("message")
	if p["id"] != "false_"+ali+"_IN9" || p["body"] != "есть пельмени?" || p["from"] != ali || p["fromMe"] != false {
		t.Fatal("message", p)
	}
	post(t, srv, "POST", "/api/default/dev/incoming", map[string]any{"chatId": ali, "edit": "IN9", "text": "есть манты?"}, nil)
	if p = next("message.edited"); p["editedMessageId"] != "IN9" || p["body"] != "есть манты?" {
		t.Fatal("edit", p)
	}
	post(t, srv, "POST", "/api/default/dev/incoming", map[string]any{"chatId": ali, "revoke": "false_" + ali + "_IN9"}, nil)
	if p = next("message.revoked"); p["revokedMessageId"] != "IN9" {
		t.Fatal("revoke", p)
	}
	post(t, srv, "POST", "/api/default/dev/incoming", map[string]any{"chatId": ali, "ack": "read", "ackIds": []string{"OUT1"}}, nil)
	if p = next("message.ack"); p["id"] != "true_"+ali+"_OUT1" || p["ack"] != float64(3) {
		t.Fatal("ack", p)
	}
	var msg map[string]any
	if code := get(t, srv, "/api/default/chats/"+ali+"/messages/IN9", "secret", &msg); code != 200 || msg["body"] != "есть манты?" || msg["revoked"] != true {
		t.Fatal("mirror", code, msg)
	}
}

// WAHA's server-wide webhook from the environment, as WAHA deployments configure it.
func TestEnvHook(t *testing.T) {
	env := map[string]string{"WHATSAPP_HOOK_URL": "http://app:8080/hook", "WHATSAPP_HOOK_EVENTS": "message.any, message.ack",
		"WHATSAPP_HOOK_HMAC_KEY": "s", "WHATSAPP_HOOK_RETRIES_ATTEMPTS": "8", "WHATSAPP_HOOK_RETRIES_POLICY": "exponential",
		"WHATSAPP_HOOK_CUSTOM_HEADERS": "X-A: 1;X-B:2"}
	h := envHook(func(k string) string { return env[k] })
	if len(h) != 1 || h[0].URL != "http://app:8080/hook" || len(h[0].Events) != 2 || h[0].Events[1] != "message.ack" ||
		h[0].HMAC.Key != "s" || h[0].Retries.Attempts != 8 || len(h[0].CustomHeaders) != 2 || h[0].CustomHeaders[0].Value != "1" {
		t.Fatalf("%+v", h)
	}
	if envHook(func(string) string { return "" }) != nil {
		t.Fatal("no URL, no hook")
	}
}

// In -dev-fake every session is a stand-in, also one created through the API after the server started: it is WORKING at
// once, survives restart and logout without dialing WhatsApp, records sends and accepts /dev/incoming.
func TestDevFakeNewSessions(t *testing.T) {
	mg, srv := testManager(t)
	mg.devFake = true
	var out map[string]any
	if code := post(t, srv, "POST", "/api/sessions", map[string]any{"name": "probe-1", "start": true, "config": map[string]any{"sending": true}}, &out); code != 201 {
		t.Fatal("create", code, out)
	}
	s := mg.get("probe-1")
	if st, _, _ := s.Status(); s.fake == nil || st != StatusWorking {
		t.Fatal("a session created later must be a stand-in and WORKING", st)
	}
	post(t, srv, "POST", "/api/sessions/probe-1/stop", nil, nil)
	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "probe-1", "chatId": "996700000002@c.us", "text": "x"}, nil); code != 422 {
		t.Fatal("a stopped stand-in answers like a disconnected session", code)
	}
	for _, act := range []string{"restart", "stop", "start", "logout", "start"} {
		post(t, srv, "POST", "/api/sessions/probe-1/"+act, nil, nil)
	}
	if st, _, _ := s.Status(); st != StatusWorking || s.Client() != nil {
		t.Fatal("restart/logout must stay on the stand-in")
	}
	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "probe-1", "chatId": "996700000002@c.us", "text": "x", "id": "ABCDEF0123456789"}, &out); code != 201 {
		t.Fatal("send", code, out)
	}
	if code := post(t, srv, "POST", "/api/probe-1/dev/incoming", map[string]any{"chatId": "996700000002@c.us", "text": "hi"}, &out); code != 200 && code != 201 {
		t.Fatal("dev/incoming", code, out)
	}
	if code := post(t, srv, "POST", "/api/probe-1/auth/request-code", map[string]any{"phoneNumber": "996700000002"}, &out); code != 422 || !strings.Contains(fmt.Sprint(out), StatusWorking) {
		t.Fatal("a pairing code on a linked session names its real state", code, out)
	}
	s.fake = nil
	if err := s.Start(); err == nil {
		t.Fatal("fake mode must never connect a session to WhatsApp")
	}
}

// Deleting, editing or reacting through the API raises the same webhook a change from the phone does.
func TestOwnChangesEmitEvents(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	s.fake = &fakeConn{fixed: true}
	s.Start()
	got := make(chan map[string]any, 16)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e map[string]any
		json.NewDecoder(r.Body).Decode(&e)
		got <- e
	}))
	defer hook.Close()
	c := s.cfg()
	c.Sending = true
	c.Webhooks = []Webhook{{URL: hook.URL, Events: []string{"message.revoked", "message.edited"}}}
	s.configure(c)
	chat := "996700000002@c.us"
	var out map[string]any
	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "default", "chatId": chat, "text": "a", "id": "OWN0000000000001"}, &out); code != 201 {
		t.Fatal(code)
	}
	if code := post(t, srv, "PUT", "/api/default/chats/"+chat+"/messages/"+out["id"].(string), map[string]any{"text": "b"}, nil); code != 201 && code != 200 {
		t.Fatal("edit", code)
	}
	if code := post(t, srv, "DELETE", "/api/default/chats/"+chat+"/messages/"+out["id"].(string), nil, nil); code != 201 && code != 200 {
		t.Fatal("delete", code)
	}
	seen := map[string]map[string]any{}
	for len(seen) < 2 {
		select {
		case e := <-got:
			seen[e["event"].(string)] = e["payload"].(map[string]any)
		case <-time.After(3 * time.Second):
			t.Fatal("events", seen)
		}
	}
	if seen["message.revoked"]["revokedMessageId"] != "OWN0000000000001" || seen["message.edited"]["body"] != "b" || seen["message.revoked"]["fromMe"] != true {
		t.Fatal(seen)
	}
}

// A WAHA client asks for the contact of anyone who writes, saved in the address book or not: the answer is never a
// 404, and the person's own profile name comes back as pushname.
func TestContactOfAnyNumber(t *testing.T) {
	mg, srv := testManager(t)
	s := mg.get("default")
	s.fake = &fakeConn{fixed: true}
	s.Start()
	chat := "996700000777@c.us"
	if code := post(t, srv, "POST", "/api/default/dev/incoming", map[string]any{"chatId": chat, "text": "hi", "pushName": "Ann Petrova"}, nil); code != 200 && code != 201 {
		t.Fatal("dev/incoming", code)
	}
	m, _ := s.Mirror()
	m.Flush()
	var c map[string]any
	if code := get(t, srv, "/api/contacts?session=default&contactId="+chat, "secret", &c); code != 200 || c["pushname"] != "Ann Petrova" || c["number"] != "996700000777" || c["id"] != chat {
		t.Fatal("a new person's contact", code, c)
	}
	if code := get(t, srv, "/api/contacts?session=default&contactId=996700000778", "secret", &c); code != 200 || c["name"] != nil || c["pushname"] != nil || c["id"] != "996700000778@c.us" {
		t.Fatal("a number with no history is still a contact", code, c)
	}
	var all []map[string]any
	if code := get(t, srv, "/api/contacts/all?session=default", "secret", &all); code != 200 || len(all) == 0 {
		t.Fatal("contacts/all", code)
	}
	for _, x := range all {
		if _, ok := x["pushname"]; !ok {
			t.Fatal("every listed contact carries pushname", x)
		}
	}
}
