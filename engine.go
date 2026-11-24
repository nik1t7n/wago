package main

// The agent engine: what turns WhatsApp traffic into work an agent can do well, and the agent's answers into
// messages that read like a person wrote them.
//
//	incoming messages ──► turn (collecting) ──quiet──► turn.ready ──► agent ──► POST /turns/{id}/reply
//	                         ▲  a new message while the agent works: turn.superseded, the reply in flight stops
//	reply ──► outbox (durable) ──► policy gate ──► seen ─► typing… ─► part ─► pause ─► part … ──► outbox.sent
//	someone writes from the phone ──► the chat is theirs (chat.owner) until they go quiet
//
// Scheduling is one loop over due times, no timers: easy to reason about, and the same code runs on the wall
// clock, on a replay's scaled clock and in tests.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// enginePeriod is how often the engine looks at due times (tests shorten it).
var enginePeriod = 100 * time.Millisecond

type Engine struct {
	s      *Session
	key    string // "" for the live engine, the replay id otherwise
	db     *agentDB
	hist   history
	clk    clock
	shadow bool                         // replays: never send, whatever the config says
	emitFn func(ev string, payload any) // where events go
	period time.Duration                // loop period in wall time
	agent  func() AgentConfig
	policy func() PolicyConfig

	mu       sync.Mutex
	pending  map[string]*Turn              // chat → the turn still collecting messages
	typing   map[string]time.Time          // chat → the person is typing until
	busy     map[string]context.CancelFunc // chat → the delivery in flight
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	risk     string
	lastRisk time.Time
}

func newEngine(s *Session, key string, db *agentDB, h history, clk clock) *Engine {
	return &Engine{s: s, key: key, db: db, hist: h, clk: clk, period: enginePeriod,
		agent: s.agentCfg, policy: s.policyCfg, emitFn: s.emit,
		pending: map[string]*Turn{}, typing: map[string]time.Time{}, busy: map[string]context.CancelFunc{},
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

func (e *Engine) start() {
	e.recover()
	go e.loop()
}

func (e *Engine) close() {
	select {
	case <-e.stop:
		return
	default:
	}
	close(e.stop)
	e.mu.Lock()
	for _, cancel := range e.busy {
		cancel()
	}
	e.mu.Unlock()
	<-e.done
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) emit(ev string, payload any) {
	if e.key != "" {
		if m, ok := payload.(map[string]any); ok {
			m["replay"] = e.key
		}
	}
	e.emitFn(ev, payload)
}

func (e *Engine) now() time.Time   { return e.clk.Now() }
func (e *Engine) nowMs() int64     { return e.clk.Now().UnixMilli() }
func (e *Engine) shadowMode() bool { return e.shadow || e.agent().Mode == "shadow" }

// recover picks up after a restart: deliveries cut mid-way go back to the queue (sent parts are known by their
// ids, so nothing is sent twice) and turns that were still collecting become ready.
func (e *Engine) recover() {
	e.db.db.Exec("UPDATE outbox SET state='queued' WHERE state='sending'")
	ts, _ := e.db.turns("WHERE state='collecting'")
	for _, t := range ts {
		e.pending[t.Chat] = t
	}
}

func (e *Engine) loop() {
	defer close(e.done)
	tk := time.NewTicker(e.period)
	defer tk.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tk.C:
		case <-e.wake:
		}
		e.tick()
	}
}

func (e *Engine) tick() {
	now := e.now()
	cfg := e.agent()

	// turns whose quiet window has passed
	e.mu.Lock()
	var ready []*Turn
	for chat, t := range e.pending {
		quietEnd := time.UnixMilli(t.LastAt).Add(ms(cfg.Turn.QuietMs))
		if tu := e.typing[chat]; tu.After(quietEnd) {
			quietEnd = tu
		}
		if hard := time.UnixMilli(t.FirstAt).Add(ms(cfg.Turn.MaxWaitMs)); hard.Before(quietEnd) {
			quietEnd = hard
		}
		if !now.Before(quietEnd) {
			delete(e.pending, chat)
			delete(e.typing, chat)
			t.State, t.ReadyAt = "ready", now.UnixMilli()
			e.db.saveTurn(t)
			ready = append(ready, t)
		}
	}
	e.mu.Unlock()
	for _, t := range ready {
		go e.announce(t)
	}

	// turns nobody answered
	if cfg.Turn.ReplyTimeoutSec > 0 {
		cut := now.Add(-time.Duration(cfg.Turn.ReplyTimeoutSec) * time.Second).UnixMilli()
		if ts, _ := e.db.turns("WHERE state='ready' AND ready_at<?", cut); len(ts) > 0 {
			for _, t := range ts {
				e.finishTurn(t, "expired", "no reply in time")
				e.emit("turn.expired", map[string]any{"turn": t})
			}
		}
	}

	// chats a person went quiet in come back to the agent
	if os, _ := e.db.owners(); len(os) > 0 {
		for _, o := range os {
			if o.Owner == "human" && o.Until > 0 && o.Until <= now.UnixMilli() {
				o.Owner, o.Since, o.Until, o.Reason = "agent", now.UnixMilli(), 0, "the person went quiet"
				e.db.setOwner(o)
				e.emit("chat.owner", map[string]any{"chatId": o.Chat, "owner": o.Owner, "reason": o.Reason})
			}
		}
	}

	e.dispatch(now, cfg)
	e.watchRisk(now)
}

// ---------------------------------------------------------------- incoming

type inbound struct {
	Chat   string
	ID     string
	At     time.Time
	FromMe bool
	Device uint16 // the sending device of our own account (0 is the main phone)
	Group  bool
}

func (e *Engine) onInbound(in inbound) {
	cfg := e.agent()
	now := e.now()
	if strings.HasSuffix(in.Chat, "@broadcast") || strings.HasSuffix(in.Chat, "@newsletter") {
		return
	}
	if in.FromMe {
		if cfg.Ownership.HumanDevices == "any" || in.Device == 0 {
			e.takeover(in.Chat, "a person wrote from their own device", now.Add(time.Duration(cfg.Ownership.HandbackMinutes)*time.Minute))
		}
		return
	}
	if in.Group && !cfg.Turn.Groups {
		return
	}
	if cfg.Turn.MaxAgeSec > 0 && now.Sub(in.At) > time.Duration(cfg.Turn.MaxAgeSec)*time.Second {
		return
	}
	own, _ := e.db.owner(in.Chat)
	if own.Owner == "human" && cfg.Ownership.WhileHuman == "pause" {
		return
	}

	e.mu.Lock()
	t := e.pending[in.Chat]
	if t == nil {
		t = &Turn{ID: e.newID("t"), Chat: in.Chat, State: "collecting", FirstAt: now.UnixMilli(), Owner: own.Owner}
		e.supersede(t, cfg)
		e.pending[in.Chat] = t
	}
	t.Messages = append(t.Messages, in.ID)
	t.LastAt = now.UnixMilli()
	e.db.saveTurn(t)
	e.mu.Unlock()
	e.poke()
}

// supersede: the person wrote again while the agent was thinking or answering. The older turn is closed, its
// unanswered messages move into the new one, and (by default) the reply being typed stops before its next part.
// Called with e.mu held.
func (e *Engine) supersede(next *Turn, cfg AgentConfig) {
	olds, _ := e.db.turns("WHERE chat=? AND state IN ('ready','replying')", next.Chat)
	for _, old := range olds {
		items, _ := e.db.outs("WHERE turn_id=?", old.ID)
		answered := false
		for _, it := range items {
			if len(it.Sent) > 0 {
				answered = true
			}
		}
		if !answered {
			next.Messages = append(next.Messages, old.Messages...)
		}
		old.State, old.SupersededBy, old.DoneAt = "superseded", next.ID, e.nowMs()
		e.db.saveTurn(old)
		e.emit("turn.superseded", map[string]any{"turn": old, "by": next.ID})
		if cfg.Turn.SupersedeOnReply {
			if cancel := e.busy[next.Chat]; cancel != nil {
				cancel()
			}
			for _, it := range items {
				if it.State == "queued" || it.State == "held" {
					e.endItem(it, "cancelled", "", "the person wrote again")
				}
			}
		}
	}
}

func (e *Engine) onTyping(chat string, typing bool) {
	cfg := e.agent()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending[chat] == nil {
		return
	}
	if typing {
		e.typing[chat] = e.now().Add(ms(cfg.Turn.TypingGraceMs))
	} else {
		delete(e.typing, chat)
	}
}

// announce sends turn.ready once the turn's media is understood and its context is gathered.
func (e *Engine) announce(t *Turn) {
	cfg := e.agent()
	ctx, cancel := context.WithTimeout(context.Background(), ms(cfg.Understand.TimeoutMs)+10*time.Second)
	defer cancel()
	msgs := make([]any, 0, len(t.Messages))
	for _, id := range t.Messages {
		j, err := e.hist.message(ctx, t.Chat, id)
		if err != nil {
			continue
		}
		if txt := e.understand(ctx, t.Chat, id, cfg); txt != "" {
			j["understanding"] = txt
		}
		msgs = append(msgs, j)
	}
	cur, err := e.db.turn(t.ID)
	if err != nil || cur.State != "ready" { // superseded or cancelled while we were preparing it
		return
	}
	e.emit("turn.ready", map[string]any{"turn": cur, "messages": msgs, "context": e.context(ctx, t.Chat, e.now(), cfg.Context, cfg)})
}

func (e *Engine) finishTurn(t *Turn, state, reason string) {
	t.State, t.Reason, t.DoneAt = state, reason, e.nowMs()
	e.db.saveTurn(t)
}

// ---------------------------------------------------------------- ownership

// takeover gives the chat to a person until `until`: turns stop, the agent's reply in flight stops, pending ones
// for this chat are cancelled. Each new message from the person moves `until` on.
func (e *Engine) takeover(chat, reason string, until time.Time) {
	now := e.now()
	prev, _ := e.db.owner(chat)
	o := &Owner{Chat: chat, Owner: "human", Since: now.UnixMilli(), Until: until.UnixMilli(), Reason: reason}
	if prev.Owner == "human" {
		o.Since = prev.Since
	}
	e.db.setOwner(o)
	if prev.Owner != "human" {
		e.emit("chat.owner", map[string]any{"chatId": chat, "owner": "human", "until": o.Until, "reason": reason})
	}
	e.mu.Lock()
	if t := e.pending[chat]; t != nil {
		delete(e.pending, chat)
		e.finishTurn(t, "cancelled", "a person took over the chat")
		e.emit("turn.cancelled", map[string]any{"turn": t})
	}
	if cancel := e.busy[chat]; cancel != nil {
		cancel()
	}
	e.mu.Unlock()
	ts, _ := e.db.turns("WHERE chat=? AND state IN ('ready','replying')", chat)
	for _, t := range ts {
		e.finishTurn(t, "cancelled", "a person took over the chat")
		e.emit("turn.cancelled", map[string]any{"turn": t})
	}
	items, _ := e.db.outs("WHERE chat=? AND turn_id IS NOT NULL AND state IN ('queued','held')", chat)
	for _, it := range items {
		e.endItem(it, "cancelled", "", "a person took over the chat")
	}
}

func (e *Engine) release(chat, reason string) {
	o := &Owner{Chat: chat, Owner: "agent", Since: e.nowMs(), Reason: reason}
	e.db.setOwner(o)
	e.emit("chat.owner", map[string]any{"chatId": chat, "owner": "agent", "reason": reason})
	e.poke()
}

// ---------------------------------------------------------------- replies and the outbox

type replyReq struct {
	ID      string `json:"id"` // idempotency key: the same id never queues or sends twice
	ChatID  string `json:"chatId"`
	Parts   []Part `json:"parts"`
	Text    string `json:"text"` // shorthand for one text part
	ReplyTo string `json:"replyTo"`
	// standalone items only: not before this time (unix ms)
	NotBefore int64 `json:"notBefore"`
}

func (r *replyReq) normalize() error {
	if r.Text != "" {
		r.Parts = append([]Part{{Text: r.Text}}, r.Parts...)
	}
	if len(r.Parts) == 0 {
		return httpError{400, "a reply needs text or parts"}
	}
	if len(r.Parts) > 20 {
		return httpError{400, "at most 20 parts"}
	}
	for i, p := range r.Parts {
		if strings.TrimSpace(p.Text) == "" && p.File == nil {
			return httpError{400, "part " + itoa(i) + " has neither text nor file"}
		}
	}
	if r.ID != "" && !validClientID(r.ID) {
		return httpError{400, "id must be 8 to 64 letters and digits"}
	}
	return nil
}

// reply queues the agent's answer to a turn. A turn the person has since added to is refused with 409 and the id
// of the newer turn: the answer would miss what they just said.
func (e *Engine) reply(turnID string, r replyReq) (*OutItem, int, error) {
	if err := r.normalize(); err != nil {
		return nil, 0, err
	}
	t, err := e.db.turn(turnID)
	if err != nil {
		return nil, 0, err
	}
	if r.ID == "" {
		r.ID = "R" + strings.ToUpper(hex.EncodeToString(sha256Bytes(turnID)[:10]))
		if e.key != "" {
			r.ID = "R_" + e.key + "_" + r.ID[1:]
		}
	}
	if prev, err := e.db.out(r.ID); err == nil { // a retry of an accepted reply
		return prev, 200, nil
	}
	switch t.State {
	case "ready", "replying":
	case "superseded":
		return nil, 0, conflict("the person wrote again; answer turn "+t.SupersededBy, map[string]any{"supersededBy": t.SupersededBy})
	default:
		return nil, 0, conflict("turn "+t.ID+" is "+t.State, nil)
	}
	if o, _ := e.db.owner(t.Chat); o.Owner == "human" {
		return nil, 0, conflict("a person is handling this chat", map[string]any{"owner": o})
	}
	it, created, err := e.db.insertOut(&OutItem{ID: r.ID, Chat: t.Chat, Turn: t.ID, Parts: r.Parts, State: "queued", NextAt: e.nowMs(),
		ReplyTo: r.ReplyTo, Created: e.nowMs()})
	if err != nil {
		return nil, 0, err
	}
	if created {
		t.State = "replying"
		e.db.saveTurn(t)
	}
	e.poke()
	return it, map[bool]int{true: 202, false: 200}[created], nil
}

// enqueue queues a message that answers no turn (a reminder, a notification). Same delivery, same policy.
func (e *Engine) enqueue(r replyReq) (*OutItem, int, error) {
	if err := r.normalize(); err != nil {
		return nil, 0, err
	}
	to, err := e.s.target(r.ChatID)
	if err != nil {
		return nil, 0, err
	}
	if r.ID == "" {
		r.ID = e.newID("O")
	}
	next := e.nowMs()
	if r.NotBefore > next {
		next = r.NotBefore
	}
	it, created, err := e.db.insertOut(&OutItem{ID: r.ID, Chat: chatID(to), Parts: r.Parts, State: "queued", NextAt: next, ReplyTo: r.ReplyTo, Created: e.nowMs()})
	if err != nil {
		return nil, 0, err
	}
	e.poke()
	return it, map[bool]int{true: 202, false: 200}[created], nil
}

func (e *Engine) skip(turnID, reason string) (*Turn, error) {
	t, err := e.db.turn(turnID)
	if err != nil {
		return nil, err
	}
	if t.State != "ready" && t.State != "replying" {
		return nil, conflict("turn "+t.ID+" is "+t.State, nil)
	}
	if reason == "" {
		reason = "the agent chose not to answer"
	}
	e.finishTurn(t, "done", reason)
	return t, nil
}

func (e *Engine) cancelItem(id string) (*OutItem, error) {
	it, err := e.db.out(id)
	if err != nil {
		return nil, err
	}
	switch it.State {
	case "queued", "held":
		e.endItem(it, "cancelled", "", "cancelled through the API")
		return it, nil
	case "sending":
		e.mu.Lock()
		if cancel := e.busy[it.Chat]; cancel != nil {
			cancel()
		}
		e.mu.Unlock()
		return it, nil
	}
	return nil, conflict("item "+id+" is already "+it.State, nil)
}

func (e *Engine) endItem(it *OutItem, state, class, msg string) {
	it.State, it.ErrorClass, it.Error, it.Updated = state, class, msg, e.nowMs()
	e.db.saveOut(it)
}

// dispatch starts deliveries that are due: the oldest open item of each chat, never two at once in one chat, at
// most outbox.parallel chats at a time.
func (e *Engine) dispatch(now time.Time, cfg AgentConfig) {
	items, err := e.db.outs("WHERE state IN ('queued','held','sending') ORDER BY created, rowid")
	if err != nil || len(items) == 0 {
		return
	}
	seen := map[string]bool{}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range items {
		if seen[it.Chat] {
			continue
		}
		seen[it.Chat] = true
		if e.busy[it.Chat] != nil || it.State == "sending" || it.NextAt > now.UnixMilli() || len(e.busy) >= cfg.Outbox.Parallel {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		e.busy[it.Chat] = cancel
		go func(it *OutItem) {
			defer func() {
				cancel()
				e.mu.Lock()
				delete(e.busy, it.Chat)
				e.mu.Unlock()
				e.poke()
			}()
			e.deliver(ctx, it)
		}(it)
	}
}

// partID is the WhatsApp id of part i of an item: derived, so a retry after a crash or a timeout sends the same
// message id again and WhatsApp (and the mirror check below) treat it as one message.
func partID(item string, i int) string {
	return "3EB0" + strings.ToUpper(hex.EncodeToString(sha256Bytes(item + "/" + itoa(i))[:9]))
}

func sha256Bytes(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func (e *Engine) jitter(d time.Duration) time.Duration {
	j := e.agent().Delivery.Jitter
	if j <= 0 || d <= 0 {
		return d
	}
	return time.Duration(float64(d) * (1 + j*(2*mrand.Float64()-1)))
}

func (e *Engine) typingTime(p Part, cfg DeliveryConfig) time.Duration {
	n := len([]rune(p.Text + p.Caption))
	d := time.Duration(float64(n) / cfg.CharsPerSecond * float64(time.Second))
	d = max(d, ms(cfg.MinTypingMs))
	d = min(d, ms(cfg.MaxTypingMs))
	return e.jitter(d)
}

func (e *Engine) deliver(ctx context.Context, it *OutItem) {
	cfg := e.agent()
	now := e.now()

	if o, _ := e.db.owner(it.Chat); o.Owner == "human" {
		if it.Turn != "" {
			e.endItem(it, "cancelled", "", "a person took over the chat")
			e.emit("outbox.cancelled", map[string]any{"item": it})
			return
		}
		e.hold(it, time.UnixMilli(max(o.Until, now.Add(time.Minute).UnixMilli())), "a person is handling this chat")
		return
	}
	if e.shadowMode() {
		e.deliverShadow(ctx, it, cfg)
		return
	}
	hold, why, err := e.s.policyGate(ctx, e.db, e.hist, it.Chat, now)
	if err != nil {
		e.endItem(it, "failed", "policy", err.Error())
		e.emit("outbox.failed", map[string]any{"item": it})
		return
	}
	if !hold.IsZero() {
		e.hold(it, hold, why)
		return
	}
	c, err := e.s.conn(true)
	if err != nil {
		e.failed(it, err, cfg)
		return
	}
	to, err := e.s.target(it.Chat)
	if err != nil {
		e.endItem(it, "failed", "permanent", err.Error())
		e.emit("outbox.failed", map[string]any{"item": it})
		return
	}
	it.State, it.Updated = "sending", e.nowMs()
	e.db.saveOut(it)

	var turn *Turn
	if it.Turn != "" {
		turn, _ = e.db.turn(it.Turn)
	}
	if it.NextPart == 0 && cfg.Delivery.Seen && turn != nil && len(turn.Messages) > 0 {
		if e.clk.Sleep(ctx, e.jitter(ms(cfg.Delivery.SeenDelayMs))) != nil {
			e.interrupted(it)
			return
		}
		e.markSeen(ctx, c, to, turn)
	}
	for i := it.NextPart; i < len(it.Parts); i++ {
		p := it.Parts[i]
		pid := partID(it.ID, i)
		if prev, err := e.hist.get(ctx, it.Chat, pid); err == nil && prev.FromMe { // sent before a crash
			it.Sent, it.NextPart = append(it.Sent, pid), i+1
			e.db.saveOut(it)
			continue
		}
		if cfg.Delivery.Typing {
			media := types.ChatPresenceMediaText
			if p.File != nil && p.Kind == "voice" {
				media = types.ChatPresenceMediaAudio
			}
			c.SendChatPresence(ctx, to, types.ChatPresenceComposing, media)
			if e.clk.Sleep(ctx, e.typingTime(p, cfg.Delivery)) != nil {
				c.SendChatPresence(context.Background(), to, types.ChatPresencePaused, media)
				e.interrupted(it)
				return
			}
		}
		if ctx.Err() != nil {
			e.interrupted(it)
			return
		}
		// the send itself is never cut half-way by an interruption
		sctx, cancel := context.WithTimeout(withOrigin(context.Background(), "outbox"), 2*time.Minute)
		msg, err := e.build(sctx, c, it, i)
		if err == nil {
			_, err = e.s.sendID(sctx, c, to, msg, pid)
		}
		cancel()
		if err != nil {
			e.failed(it, err, cfg)
			return
		}
		it.Sent, it.NextPart, it.Updated = append(it.Sent, pid), i+1, e.nowMs()
		e.db.saveOut(it)
		if i < len(it.Parts)-1 && e.clk.Sleep(ctx, e.jitter(ms(cfg.Delivery.PauseMs))) != nil {
			e.interrupted(it)
			return
		}
	}
	if cfg.Delivery.Typing {
		c.SendChatPresence(context.Background(), to, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}
	e.endItem(it, "sent", "", "")
	if turn != nil && turn.State == "replying" {
		e.finishTurn(turn, "done", "")
	}
	e.emit("outbox.sent", map[string]any{"item": it})
}

// deliverShadow runs the same timing and records what would have gone out.
func (e *Engine) deliverShadow(ctx context.Context, it *OutItem, cfg AgentConfig) {
	it.State = "sending"
	e.db.saveOut(it)
	var at []int64
	for i := it.NextPart; i < len(it.Parts); i++ {
		if e.clk.Sleep(ctx, e.typingTime(it.Parts[i], cfg.Delivery)) != nil {
			e.interrupted(it)
			return
		}
		it.Sent, it.NextPart = append(it.Sent, partID(it.ID, i)), i+1
		at = append(at, e.nowMs())
		e.db.saveOut(it)
		if i < len(it.Parts)-1 && e.clk.Sleep(ctx, e.jitter(ms(cfg.Delivery.PauseMs))) != nil {
			e.interrupted(it)
			return
		}
	}
	e.endItem(it, "shadowed", "", "")
	if t, err := e.db.turn(it.Turn); err == nil && t.State == "replying" {
		e.finishTurn(t, "done", "shadow")
	}
	e.emit("outbox.shadowed", map[string]any{"item": it, "at": at})
}

func (e *Engine) markSeen(ctx context.Context, c waConn, to types.JID, t *Turn) {
	ids := make([]types.MessageID, 0, len(t.Messages))
	for _, id := range t.Messages {
		ids = append(ids, types.MessageID(tail(id)))
	}
	var sender types.JID
	if to.Server == types.GroupServer {
		if m, err := e.hist.get(ctx, t.Chat, t.Messages[len(t.Messages)-1]); err == nil {
			sender, _ = parseChatID(m.Sender)
		}
	}
	c.MarkRead(ctx, ids, time.Now(), to, sender)
}

func (e *Engine) build(ctx context.Context, c waConn, it *OutItem, i int) (*waE2E.Message, error) {
	p := it.Parts[i]
	var msg *waE2E.Message
	if p.File != nil {
		kind := p.Kind
		if kind == "" {
			kind = kindOfMime(p.File.Mimetype)
		}
		m, err := mediaMessage(ctx, c, kind, p.File, firstNonEmpty(p.Caption, p.Text), p.Convert)
		if err != nil {
			return nil, err
		}
		msg = m
	} else {
		msg = &waE2E.Message{Conversation: proto.String(p.Text)}
	}
	if i == 0 && it.ReplyTo != "" { // only the first part quotes, as a person would
		ci, err := e.s.replyContext(ctx, it.Chat, it.ReplyTo, nil)
		if err != nil {
			return nil, err
		}
		msg = withContext(msg, ci)
	}
	return msg, nil
}

func kindOfMime(m string) string {
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "video/"):
		return "video"
	case strings.HasPrefix(m, "audio/"):
		return "voice"
	}
	return "file"
}

func (e *Engine) hold(it *OutItem, until time.Time, why string) {
	was := it.State
	it.State, it.NextAt, it.ErrorClass, it.Error, it.Updated = "held", until.UnixMilli(), "held", why, e.nowMs()
	e.db.saveOut(it)
	if was != "held" {
		e.emit("outbox.held", map[string]any{"item": it})
	}
}

func (e *Engine) interrupted(it *OutItem) {
	state := "interrupted"
	if len(it.Sent) == 0 {
		state = "cancelled"
	}
	if t, err := e.db.turn(it.Turn); err == nil && t.State == "replying" {
		e.finishTurn(t, "done", "interrupted")
	}
	e.endItem(it, state, "", "stopped before the remaining parts: the person wrote again, took over, or it was cancelled")
	e.emit("outbox."+state, map[string]any{"item": it})
}

// failed sorts an error by what to do about it:
//
//	held       sending is off, or the kill switch: wait, it may be switched on
//	transient  not connected, server hiccup: retry with backoff
//	uncertain  timed out after the request left: retry with the same message id, which is safe
//	permanent  the message or the address is wrong: retrying cannot help
func (e *Engine) failed(it *OutItem, err error, cfg AgentConfig) {
	class := classify(err)
	if class == "held" {
		e.hold(it, e.now().Add(30*time.Second), err.Error())
		return
	}
	it.Attempts++
	if class == "permanent" || it.Attempts >= cfg.Outbox.MaxAttempts {
		if t, err := e.db.turn(it.Turn); err == nil && t.State == "replying" {
			e.finishTurn(t, "done", "reply failed")
		}
		e.endItem(it, "failed", class, err.Error())
		e.emit("outbox.failed", map[string]any{"item": it})
		return
	}
	backoff := min(time.Duration(1<<min(it.Attempts, 9))*time.Second, 5*time.Minute)
	it.State, it.ErrorClass, it.Error, it.NextAt, it.Updated = "queued", class, err.Error(), e.now().Add(e.jitter(backoff)).UnixMilli(), e.nowMs()
	e.db.saveOut(it)
}

func classify(err error) string {
	var he httpError
	if errors.As(err, &he) {
		switch he.code {
		case 403, 429:
			return "held"
		case 400, 404, 409, 413, 415:
			return "permanent"
		case 422, 503:
			return "transient"
		}
	}
	switch {
	case errors.Is(err, whatsmeow.ErrMessageTimedOut), errors.Is(err, whatsmeow.ErrIQTimedOut), errors.Is(err, context.DeadlineExceeded):
		return "uncertain"
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn), errors.Is(err, whatsmeow.ErrIQDisconnected):
		return "transient"
	case errors.Is(err, whatsmeow.ErrRecipientNotOnWhatsApp), errors.Is(err, whatsmeow.ErrUnknownServer), errors.Is(err, whatsmeow.ErrRecipientADJID),
		errors.Is(err, whatsmeow.ErrNoPermissionInGroup), errors.Is(err, whatsmeow.ErrBroadcastListUnsupported), errors.Is(err, whatsmeow.ErrInvalidImageFormat):
		return "permanent"
	}
	return "transient"
}

func (e *Engine) newID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	if e.key != "" {
		return prefix + "_" + e.key + "_" + hex.EncodeToString(b)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// conflictError is a 409 that carries what the client needs to recover (the newer turn, the owner).
type conflictError struct {
	httpError
	extra map[string]any
}

func conflict(msg string, extra map[string]any) error {
	return conflictError{httpError{409, msg}, extra}
}

func itoa(n int) string { return strconv.Itoa(n) }
