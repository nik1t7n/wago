package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// fast agent settings: the same behaviour as production, in milliseconds instead of seconds
const fastAgent = `{"enabled": true,
 "turn": {"quietMs": 60, "typingGraceMs": 300, "maxWaitMs": 3000, "replyTimeoutSec": 30},
 "delivery": {"seenDelayMs": 5, "minTypingMs": 20, "maxTypingMs": 40, "pauseMs": 5, "charsPerSecond": 1000, "jitter": 0}}`

type hookRecorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	evs []map[string]any
	ch  chan map[string]any
}

func newRecorder(t *testing.T) *hookRecorder {
	h := &hookRecorder{ch: make(chan map[string]any, 256)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e map[string]any
		json.NewDecoder(r.Body).Decode(&e)
		h.mu.Lock()
		h.evs = append(h.evs, e)
		h.mu.Unlock()
		h.ch <- e
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// wait returns the next event named ev (skipping others), failing after a timeout.
func (h *hookRecorder) wait(t *testing.T, ev string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-h.ch:
			if e["event"] == ev {
				return e["payload"].(map[string]any)
			}
		case <-deadline:
			t.Fatalf("no %s event", ev)
		}
	}
}

// agentSession: a fake session with the agent layer on, sending allowed, all events to a recorder.
func agentSession(t *testing.T, agent, policy string) (*Manager, *httptest.Server, *Session, *fakeConn, *hookRecorder) {
	t.Helper()
	enginePeriod = 10 * time.Millisecond
	mg, srv := testManager(t)
	s := mg.get("default")
	fc := &fakeConn{}
	s.fake = fc
	s.Start()
	rec := newRecorder(t)
	c := s.cfg()
	c.Sending = true
	c.Webhooks = []Webhook{{URL: rec.srv.URL, Events: []string{"*"}}}
	c.Agent = json.RawMessage(agent)
	if policy != "" {
		c.Policy = json.RawMessage(policy)
	}
	if err := mg.update(s, c); err != nil {
		t.Fatal(err)
	}
	if s.eng() == nil {
		t.Fatal("agent layer did not start")
	}
	return mg, srv, s, fc, rec
}

func incoming(t *testing.T, srv *httptest.Server, body map[string]any) string {
	t.Helper()
	var out map[string]any
	if code := post(t, srv, "POST", "/api/default/dev/incoming", body, &out); code != 200 {
		t.Fatal("dev/incoming", code, out)
	}
	id, _ := out["id"].(string)
	return id
}

func turnOf(p map[string]any) map[string]any { return p["turn"].(map[string]any) }

const alice = "996700000101@c.us"

// Stage 1: messages gather into one turn; the reply is delivered like a person (seen, typing, parts in order).
func TestTurnAndHumanDelivery(t *testing.T) {
	_, srv, _, fc, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hi", "pushName": "Alice"})
	incoming(t, srv, map[string]any{"chatId": alice, "text": "do you deliver on sunday?"})
	ready := rec.wait(t, "turn.ready")
	turn := turnOf(ready)
	if n := len(ready["messages"].([]any)); n != 2 {
		t.Fatal("two quick messages are one turn", n)
	}
	ctx := ready["context"].([]any)
	if len(ctx) < 2 || ctx[len(ctx)-1].(map[string]any)["text"] != "do you deliver on sunday?" || ctx[len(ctx)-1].(map[string]any)["role"] != "user" {
		t.Fatal("context ends with the turn, oldest first", ctx)
	}

	id := turn["id"].(string)
	var it map[string]any
	reply := map[string]any{"id": "REPLY00000001", "parts": []any{map[string]any{"text": "Hi Alice!"}, map[string]any{"text": "Yes, on Sundays too."}}}
	if code := post(t, srv, "POST", "/api/default/turns/"+id+"/reply", reply, &it); code != 202 {
		t.Fatal("reply", code, it)
	}
	if code := post(t, srv, "POST", "/api/default/turns/"+id+"/reply", reply, nil); code != 200 {
		t.Fatal("the same reply id is accepted once", code)
	}
	sent := rec.wait(t, "outbox.sent")["item"].(map[string]any)
	if ids := sent["sent"].([]any); len(ids) != 2 || ids[0] != partID("REPLY00000001", 0) {
		t.Fatal("both parts with derived ids", ids)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.sent) != 2 || fc.sent[0].msg.GetConversation() != "Hi Alice!" || fc.sent[1].msg.GetConversation() != "Yes, on Sundays too." {
		t.Fatal("parts in order", len(fc.sent))
	}
	if len(fc.reads[""]) != 2 {
		t.Fatal("the turn's messages are marked read first", fc.reads)
	}
	if strings.Join(fc.presences, ",") != "composing,composing,paused" {
		t.Fatal("typing before each part, paused at the end", fc.presences)
	}
	var tv map[string]any
	get(t, srv, "/api/default/turns/"+id, "secret", &tv)
	if tv["turn"].(map[string]any)["state"] != "done" {
		t.Fatal("turn done", tv)
	}
}

// Stage 1: the person keeps writing while the agent thinks: the old turn is superseded, its messages carried over,
// and an answer to it is refused with the newer turn's id.
func TestSupersede(t *testing.T) {
	_, srv, _, fc, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "price of the cake?"})
	first := turnOf(rec.wait(t, "turn.ready"))["id"].(string)
	incoming(t, srv, map[string]any{"chatId": alice, "text": "the chocolate one"})
	sup := rec.wait(t, "turn.superseded")
	if turnOf(sup)["id"] != first {
		t.Fatal(sup)
	}
	next := rec.wait(t, "turn.ready")
	if len(next["messages"].([]any)) != 2 {
		t.Fatal("the unanswered message moves into the new turn", next["messages"])
	}
	var out map[string]any
	if code := post(t, srv, "POST", "/api/default/turns/"+first+"/reply", map[string]any{"text": "late"}, &out); code != 409 || out["supersededBy"] != turnOf(next)["id"] {
		t.Fatal("answer to a superseded turn", code, out)
	}
	if len(fc.sent) != 0 {
		t.Fatal("nothing sent")
	}
}

// Stage 1: a new message interrupts a reply that is being typed; the remaining parts are not sent.
func TestInterruption(t *testing.T) {
	slow := strings.Replace(fastAgent, `"minTypingMs": 20, "maxTypingMs": 40`, `"minTypingMs": 400, "maxTypingMs": 400`, 1)
	_, srv, _, fc, rec := agentSession(t, slow, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hello"})
	id := turnOf(rec.wait(t, "turn.ready"))["id"].(string)
	post(t, srv, "POST", "/api/default/turns/"+id+"/reply", map[string]any{"parts": []any{map[string]any{"text": "one"}, map[string]any{"text": "two"}, map[string]any{"text": "three"}}}, nil)
	time.Sleep(500 * time.Millisecond) // the first part is out, the second is being typed
	incoming(t, srv, map[string]any{"chatId": alice, "text": "wait, actually"})
	ev := rec.wait(t, "outbox.interrupted")["item"].(map[string]any)
	if ev["nextPart"].(float64) != 1 {
		t.Fatal("stopped after the first part", ev)
	}
	time.Sleep(300 * time.Millisecond)
	fc.mu.Lock()
	n := len(fc.sent)
	fc.mu.Unlock()
	if n != 1 {
		t.Fatal("only the first part went out", n)
	}
}

// Stage 1: while the person types, the turn waits for them (up to typingGraceMs).
func TestTypingExtendsTurn(t *testing.T) {
	_, srv, _, _, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "so"})
	incoming(t, srv, map[string]any{"chatId": alice, "typing": true})
	time.Sleep(150 * time.Millisecond) // beyond quietMs, inside the typing grace
	incoming(t, srv, map[string]any{"chatId": alice, "text": "about my order"})
	ready := rec.wait(t, "turn.ready")
	if len(ready["messages"].([]any)) != 2 {
		t.Fatal("typing kept the turn open", ready["messages"])
	}
}

// Stage 2: the owner writes from the phone: the chat is theirs, turns stop, the agent's answer is refused; the
// chat comes back through the API (or by itself after handbackMinutes).
func TestOwnership(t *testing.T) {
	_, srv, s, _, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "is anyone there?"})
	id := turnOf(rec.wait(t, "turn.ready"))["id"].(string)
	incoming(t, srv, map[string]any{"chatId": alice, "text": "Hi, it's Maria, I'll help you", "fromMe": true})
	own := rec.wait(t, "chat.owner")
	if own["owner"] != "human" {
		t.Fatal(own)
	}
	rec.wait(t, "turn.cancelled")
	var out map[string]any
	if code := post(t, srv, "POST", "/api/default/turns/"+id+"/reply", map[string]any{"text": "bot here"}, &out); code != 409 {
		t.Fatal("the agent cannot answer a cancelled turn", code, out)
	}
	incoming(t, srv, map[string]any{"chatId": alice, "text": "thanks Maria"})
	time.Sleep(200 * time.Millisecond)
	if ts, _ := s.eng().db.turns("WHERE state='ready'"); len(ts) != 0 {
		t.Fatal("no turns while a person owns the chat", ts)
	}
	if code := post(t, srv, "PUT", "/api/default/chats/"+alice+"/owner", map[string]any{"owner": "agent"}, &out); code != 200 || out["owner"] != "agent" {
		t.Fatal(code, out)
	}
	incoming(t, srv, map[string]any{"chatId": alice, "text": "one more question"})
	rec.wait(t, "turn.ready")

	// the context tells the agent who said what
	var ctx []map[string]any
	get(t, srv, "/api/default/chats/"+alice+"/context", "secret", &ctx)
	roles := []string{}
	for _, l := range ctx {
		roles = append(roles, l["role"].(string))
	}
	if !strings.Contains(strings.Join(roles, ","), "user,human,user") {
		t.Fatal("roles", roles)
	}
}

// With humanDevices "phone", another linked device (another bot beside WaGo during a shadow run) is not a person.
func TestOwnershipPhoneOnly(t *testing.T) {
	cfg := strings.Replace(fastAgent, `"enabled": true,`, `"enabled": true, "ownership": {"humanDevices": "phone"},`, 1)
	_, srv, s, _, _ := agentSession(t, cfg, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "from the other bot", "fromMe": true, "device": 7})
	if o, _ := s.eng().db.owner(alice); o.Owner != "agent" {
		t.Fatal(o)
	}
	incoming(t, srv, map[string]any{"chatId": alice, "text": "from the phone", "fromMe": true, "device": 0})
	if o, _ := s.eng().db.owner(alice); o.Owner != "human" {
		t.Fatal(o)
	}
}

// Stage 3: the outbox survives a restart: an item cut mid-way resumes, a part already sent is not sent again.
func TestOutboxDurable(t *testing.T) {
	mg, srv, s, fc, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hello"}) // they wrote first, so this is not cold outreach
	rec.wait(t, "turn.ready")
	e := s.eng()
	// as if the process died after part 0 left: the mirror has it, the outbox still says sending
	to, _ := s.target(alice)
	c, _ := s.conn(true)
	if _, err := s.sendID(context.Background(), c, to, textMsg("first"), partID("CRASH0000001", 0)); err != nil {
		t.Fatal(err)
	}
	e.db.insertOut(&OutItem{ID: "CRASH0000001", Chat: alice, Parts: []Part{{Text: "first"}, {Text: "second"}}, State: "sending", NextAt: 0, Created: time.Now().UnixMilli()})
	e.db.db.Exec("UPDATE outbox SET state='sending' WHERE id='CRASH0000001'")
	cfg := s.cfg()
	cfg.Agent = json.RawMessage(`{"enabled": false}`)
	mg.update(s, cfg)
	time.Sleep(50 * time.Millisecond)
	cfg.Agent = json.RawMessage(fastAgent)
	mg.update(s, cfg)
	it := rec.wait(t, "outbox.sent")["item"].(map[string]any)
	if it["id"] != "CRASH0000001" || len(it["sent"].([]any)) != 2 {
		t.Fatal(it)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.sent) != 2 || fc.sent[1].msg.GetConversation() != "second" {
		t.Fatal("the first part was not sent twice", len(fc.sent))
	}
}

// Stage 3: failures are sorted: sending off waits, a timeout is retried with the same id, a bad address fails.
func TestOutboxErrors(t *testing.T) {
	mg, srv, s, fc, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hello"})
	rec.wait(t, "turn.ready")

	cfg := s.cfg()
	cfg.Sending = false
	mg.update(s, cfg)
	var it map[string]any
	post(t, srv, "POST", "/api/default/outbox", map[string]any{"chatId": alice, "text": "reminder", "id": "REMIND000001"}, &it)
	held := rec.wait(t, "outbox.held")["item"].(map[string]any)
	if held["errorClass"] != "held" {
		t.Fatal(held)
	}
	fc.mu.Lock()
	fc.fail = []error{whatsmeow.ErrMessageTimedOut}
	fc.mu.Unlock()
	cfg.Sending = true
	mg.update(s, cfg)
	s.eng().db.db.Exec("UPDATE outbox SET next_at=0 WHERE id='REMIND000001'")
	time.Sleep(100 * time.Millisecond)
	got, _ := s.eng().db.out("REMIND000001")
	if got.ErrorClass != "uncertain" || got.Attempts != 1 {
		t.Fatal("a timeout is uncertain and retried", got)
	}
	s.eng().db.db.Exec("UPDATE outbox SET next_at=0 WHERE id='REMIND000001'")
	sent := rec.wait(t, "outbox.sent")["item"].(map[string]any)
	if sent["sent"].([]any)[0] != partID("REMIND000001", 0) {
		t.Fatal("the retry used the same message id", sent)
	}

	if classify(sendError{httpError{502, "x"}, whatsmeow.ErrRecipientNotOnWhatsApp}) != "permanent" ||
		classify(sendError{httpError{502, "x"}, errors.New("socket closed")}) != "transient" ||
		classify(httpError{422, "not connected"}) != "transient" {
		t.Fatal("classify")
	}
}

// Stage 3: ordering inside a chat: a later item never overtakes an earlier one that is waiting.
func TestOutboxOrder(t *testing.T) {
	_, srv, s, fc, rec := agentSession(t, fastAgent, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hello"})
	rec.wait(t, "turn.ready")
	future := time.Now().Add(400 * time.Millisecond).UnixMilli()
	post(t, srv, "POST", "/api/default/outbox", map[string]any{"chatId": alice, "text": "first", "notBefore": future}, nil)
	post(t, srv, "POST", "/api/default/outbox", map[string]any{"chatId": alice, "text": "second"}, nil)
	rec.wait(t, "outbox.sent")
	rec.wait(t, "outbox.sent")
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.sent) != 2 || fc.sent[0].msg.GetConversation() != "first" {
		t.Fatal("in order", len(fc.sent))
	}
	_ = s
}

// Stage 4: a voice note is turned into text by a processor before the turn goes out; the result is cached.
func TestUnderstanding(t *testing.T) {
	calls := 0
	proc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "audio/ogg" || string(b) != "OggS-voice" {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"text": "can I pay by card?"}`))
	}))
	defer proc.Close()
	enginePeriod = 10 * time.Millisecond
	mg, srv := testManager(t)
	mg.processors["stt"] = &Processor{Type: "http", URL: proc.URL}
	s := mg.get("default")
	s.fake = &fakeConn{}
	s.Start()
	rec := newRecorder(t)
	c := s.cfg()
	c.Webhooks = []Webhook{{URL: rec.srv.URL, Events: []string{"*"}}}
	c.Agent = json.RawMessage(strings.Replace(fastAgent, `"enabled": true,`, `"enabled": true, "understand": {"audio": "stt"},`, 1))
	if err := mg.update(s, c); err != nil {
		t.Fatal(err)
	}
	incoming(t, srv, map[string]any{"chatId": alice, "file": map[string]any{"mimetype": "audio/ogg", "data": base64.StdEncoding.EncodeToString([]byte("OggS-voice"))}})
	ready := rec.wait(t, "turn.ready")
	m := ready["messages"].([]any)[0].(map[string]any)
	if m["understanding"] != "can I pay by card?" {
		t.Fatal(m)
	}
	ctx := ready["context"].([]any)
	if last := ctx[len(ctx)-1].(map[string]any); last["text"] != "[audio] can I pay by card?" {
		t.Fatal("context carries the text, not the file", last)
	}
	var u map[string]any
	get(t, srv, "/api/default/chats/"+alice+"/messages/"+m["id"].(string)+"/understanding", "secret", &u)
	if u["text"] != "can I pay by card?" || calls != 1 {
		t.Fatal("cached", u, calls)
	}
	c.Agent = json.RawMessage(`{"enabled": true, "understand": {"audio": "missing"}}`)
	if err := mg.update(s, c); err == nil {
		t.Fatal("an unknown processor is refused")
	}
}

// Stage 5: the policy blocks cold outreach, rate-limits a chat, and the kill switch stops everything.
func TestPolicy(t *testing.T) {
	_, srv, s, fc, rec := agentSession(t, fastAgent, `{"enabled": true, "perChatPerMinute": 1}`)
	var out map[string]any
	stranger := "996700000999@c.us"
	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "default", "chatId": stranger, "text": "buy now"}, &out); code != 403 {
		t.Fatal("cold outreach is blocked for direct sends", code, out)
	}
	post(t, srv, "POST", "/api/default/outbox", map[string]any{"chatId": stranger, "text": "buy now"}, nil)
	if f := rec.wait(t, "outbox.failed")["item"].(map[string]any); f["errorClass"] != "policy" {
		t.Fatal(f)
	}
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hello"})
	rec.wait(t, "turn.ready")
	post(t, srv, "POST", "/api/default/outbox", map[string]any{"chatId": alice, "text": "one"}, nil)
	rec.wait(t, "outbox.sent")
	post(t, srv, "POST", "/api/default/outbox", map[string]any{"chatId": alice, "text": "two"}, nil)
	if h := rec.wait(t, "outbox.held")["item"].(map[string]any); !strings.Contains(h["error"].(string), "perChatPerMinute") {
		t.Fatal(h)
	}
	if code := post(t, srv, "POST", "/api/sendText", map[string]any{"session": "default", "chatId": alice, "text": "three"}, &out); code != 429 {
		t.Fatal("direct sends obey the limit too", code, out)
	}

	post(t, srv, "POST", "/api/default/policy/kill", map[string]any{"on": true}, nil)
	if code := post(t, srv, "POST", "/api/sendSeen", map[string]any{"session": "default", "chatId": alice}, &out); code != 403 {
		t.Fatal("kill switch stops every action", code)
	}
	post(t, srv, "POST", "/api/default/policy/kill", map[string]any{"on": false}, nil)
	var pol map[string]any
	get(t, srv, "/api/default/policy", "secret", &pol)
	sg := pol["signals"].(map[string]any)
	if sg["sent24h"].(float64) != 1 || sg["risk"] != "low" {
		t.Fatal(sg)
	}
	_ = s
	_ = fc
}

func TestQuietHours(t *testing.T) {
	q := &QuietHours{From: "22:00", To: "08:00", Timezone: "UTC"}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 24, h, m, 0, 0, time.UTC) }
	if end, _ := q.window(at(23, 30)); !end.Equal(at(8, 0).AddDate(0, 0, 1)) {
		t.Fatal(end)
	}
	if end, _ := q.window(at(7, 59)); !end.Equal(at(8, 0)) {
		t.Fatal(end)
	}
	if end, _ := q.window(at(12, 0)); !end.IsZero() {
		t.Fatal(end)
	}
	if !matches([]string{"996*"}, "996700000101@c.us") || matches([]string{"7*"}, "996700000101@c.us") || !matches([]string{alice}, alice) {
		t.Fatal("matches")
	}
}

// Stage 5: shadow mode runs everything and sends nothing.
func TestShadowMode(t *testing.T) {
	cfg := strings.Replace(fastAgent, `"enabled": true,`, `"enabled": true, "mode": "shadow",`, 1)
	_, srv, _, fc, rec := agentSession(t, cfg, "")
	incoming(t, srv, map[string]any{"chatId": alice, "text": "hello"})
	id := turnOf(rec.wait(t, "turn.ready"))["id"].(string)
	post(t, srv, "POST", "/api/default/turns/"+id+"/reply", map[string]any{"text": "hi"}, nil)
	sh := rec.wait(t, "outbox.shadowed")["item"].(map[string]any)
	if sh["state"] != "shadowed" || len(fc.sent) != 0 || len(fc.presences) != 0 {
		t.Fatal(sh, len(fc.sent))
	}
}

// Stage 5: a scripted conversation plays at 100x through the agent's real webhook; the timeline puts the agent's
// answers beside what was really said.
func TestReplayScript(t *testing.T) {
	// realistic windows: at speed N a millisecond of scheduling noise is N virtual milliseconds
	_, srv, _, fc, _ := agentSession(t, `{"enabled": true, "delivery": {"jitter": 0}}`, "")
	var agent *httptest.Server
	agent = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e map[string]any
		json.NewDecoder(r.Body).Decode(&e)
		if e["event"] == "turn.ready" {
			p := e["payload"].(map[string]any)
			id := turnOf(p)["id"].(string)
			b, _ := json.Marshal(map[string]any{"text": "agent answer " + itoa(len(p["messages"].([]any)))})
			req, _ := http.NewRequest("POST", srv.URL+"/api/default/turns/"+id+"/reply", strings.NewReader(string(b)))
			req.Header.Set("X-Api-Key", "secret")
			go func() {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}()
		}
	}))
	defer agent.Close()
	script := []any{
		map[string]any{"afterMs": 0, "text": "hi"},
		map[string]any{"afterMs": 1500, "text": "do you have cakes?"},
		map[string]any{"afterMs": 60000, "text": "real answer", "fromMe": true},
		map[string]any{"afterMs": 120000, "text": "thanks!"},
	}
	var rp map[string]any
	if code := post(t, srv, "POST", "/api/default/replays", map[string]any{"chatId": alice, "speed": 100, "script": script,
		"webhook": map[string]any{"url": agent.URL, "events": []string{"turn.ready"}}}, &rp); code != 202 {
		t.Fatal(code, rp)
	}
	var view map[string]any
	for i := 0; i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
		get(t, srv, "/api/default/replays/"+rp["id"].(string), "secret", &view)
		if view["state"] == "done" {
			break
		}
	}
	if view["state"] != "done" {
		t.Fatal("replay did not finish", view["state"])
	}
	kinds := []string{}
	for _, ev := range view["timeline"].([]any) {
		kinds = append(kinds, ev.(map[string]any)["kind"].(string))
	}
	got := strings.Join(kinds, ",")
	if got != "inbound,inbound,turn,agent,human,inbound,turn,agent" {
		t.Fatal("timeline", got)
	}
	if len(fc.sent) != 0 {
		t.Fatal("a replay never sends")
	}
}

// A WAHA client that PUTs its own config without agent or policy keeps them.
func TestWAHAConfigKeepsAgent(t *testing.T) {
	_, srv, s, _, _ := agentSession(t, fastAgent, `{"enabled": true}`)
	if code := post(t, srv, "PUT", "/api/sessions/default", map[string]any{"config": map[string]any{"webhooks": []any{}}}, nil); code != 200 {
		t.Fatal(code)
	}
	if s.eng() == nil || !s.policyCfg().Enabled {
		t.Fatal("agent and policy blocks survive a WAHA-style PUT")
	}
	var out map[string]any
	if code := post(t, srv, "PUT", "/api/sessions/default", map[string]any{"config": map[string]any{"agent": map[string]any{"enabled": true, "mode": "loud"}}}, &out); code != 400 {
		t.Fatal("a bad agent block is refused", code, out)
	}
}

func TestConfigLayers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wago.yaml")
	os.WriteFile(path, []byte("listen: 127.0.0.1:4000\ndeviceName: Shop\napiKey: file:"+filepath.Join(dir, "key")+"\nagent:\n  turn:\n    quietMs: 1000\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "key"), []byte("s3cret\n"), 0o600)
	env := map[string]string{"WAGO_DEVICE_NAME": "Shop Env", "WHATSAPP_HOOK_URL": "http://hook"}
	getenv := func(k string) string { return env[k] }
	fs := newFlagSet()
	f := newFlags(fs)
	fs.Parse([]string{"-config", path, "-addr", "127.0.0.1:5000"})
	c, _, err := loadConfig(f, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:5000" || c.DeviceName != "Shop Env" || c.PublicURL != "http://127.0.0.1:5000" || len(c.Webhooks) != 1 {
		t.Fatal("flags over env over file", c)
	}
	if k, _ := resolveSecret(c.APIKey, getenv); k != "s3cret" {
		t.Fatal("file secret", k)
	}
	if a, _ := decodeAgent(c.Agent, defaultAgent()); a.Turn.QuietMs != 1000 || a.Turn.MaxWaitMs != 30000 {
		t.Fatal("a partial agent block keeps the other defaults", a.Turn)
	}
	os.WriteFile(path, []byte("lisen: 1\n"), 0o600)
	fs = newFlagSet()
	f = newFlags(fs)
	fs.Parse([]string{"-config", path})
	if _, _, err := loadConfig(f, getenv); err == nil {
		t.Fatal("a misspelt key is an error")
	}
}

func textMsg(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func newFlagSet() *flag.FlagSet { return flag.NewFlagSet("test", flag.ContinueOnError) }
