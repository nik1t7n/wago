package main

// Replays and the simulator: an agent tested on real conversations, fast, with nothing sent.
//
// A replay plays a chat's history from the mirror (or a scripted conversation) through its own engine on a
// scaled clock: at 60x an hour of conversation takes a minute. The agent receives turn.ready as it would live
// (with "replay": id in the payload) and answers through the normal reply endpoint; the replay's outbox is always
// in shadow. Each turn sees the real history up to that moment, so every answer can be set against what the
// person on the phone actually wrote next: the timeline puts both side by side.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type ReplayEvent struct {
	At   int64  `json:"at"`   // virtual time, unix ms
	Kind string `json:"kind"` // inbound (the other side), human (what was really answered), turn, agent (the agent's answer)
	ID   string `json:"id,omitempty"`
	Turn string `json:"turnId,omitempty"`
	Text string `json:"text,omitempty"`
}

type Replay struct {
	ID      string  `json:"id"`
	Chat    string  `json:"chatId"`
	Speed   float64 `json:"speed"`
	State   string  `json:"state"` // running, done, stopped
	Source  string  `json:"source"`
	Started int64   `json:"started"`
	Ended   int64   `json:"ended,omitempty"`

	mu       sync.Mutex
	timeline []ReplayEvent
	eng      *Engine
	cancel   context.CancelFunc
	hook     *hookWorker
}

type scriptLine struct {
	AfterMs int64  `json:"afterMs"` // from the start of the conversation
	Text    string `json:"text"`
	FromMe  bool   `json:"fromMe"` // a reply from our side, as it really happened (for comparison)
	Name    string `json:"name"`
}

type replayReq struct {
	ChatID  string       `json:"chatId"`
	Since   int64        `json:"since"` // unix seconds; default: the last `limit` messages
	Until   int64        `json:"until"`
	Limit   int          `json:"limit"`
	Speed   float64      `json:"speed"` // default 60
	Script  []scriptLine `json:"script"`
	Webhook *Webhook     `json:"webhook"` // the replay's events go only here (default: the session's hooks)
}

func replayOf(id string) string {
	if p := strings.Split(id, "_"); len(p) >= 3 && strings.HasPrefix(p[1], "rp") {
		return p[1]
	}
	return ""
}

func (s *Session) replay(id string) *Replay {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replays[id]
}

func (r *Replay) add(ev ReplayEvent) {
	r.mu.Lock()
	r.timeline = append(r.timeline, ev)
	r.mu.Unlock()
}

func (r *Replay) stop() {
	r.mu.Lock()
	if r.State == "running" {
		r.State = "stopped"
	}
	r.mu.Unlock()
	r.cancel()
}

func (r *Replay) view(full bool) map[string]any {
	r.mu.Lock()
	out := map[string]any{"id": r.ID, "chatId": r.Chat, "speed": r.Speed, "state": r.State, "source": r.Source, "started": r.Started, "ended": r.Ended}
	if full {
		tl := append([]ReplayEvent(nil), r.timeline...)
		sort.SliceStable(tl, func(i, j int) bool { return tl[i].At < tl[j].At })
		out["timeline"] = tl
	}
	r.mu.Unlock()
	if full {
		out["turns"], _ = r.eng.db.turns("ORDER BY first_at")
		out["replies"], _ = r.eng.db.outs("ORDER BY created")
	}
	return out
}

func (s *Session) startReplay(req replayReq) (*Replay, error) {
	if req.Speed <= 0 {
		req.Speed = 60
	}
	if req.Speed > 3600 {
		return nil, httpError{400, "speed is at most 3600"}
	}
	if req.Limit <= 0 {
		req.Limit = 200
	}
	to, err := s.target(req.ChatID)
	if err != nil {
		return nil, err
	}
	chat := chatID(to)

	var msgs []histMsg
	var h history
	source := "history"
	if len(req.Script) > 0 {
		source = "script"
		start := time.Now()
		sh := &scriptHistory{chat: chat, me: chatID(s.Me())}
		for i, l := range req.Script {
			msgs = append(msgs, histMsg{ID: "SIM" + strings.ToUpper(hex.EncodeToString(sha256Bytes(chat + itoa(i) + start.String())[:8])),
				FromMe: l.FromMe, Body: l.Text, SenderName: l.Name, At: start.Add(ms(int(l.AfterMs)))})
		}
		sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].At.Before(msgs[j].At) })
		h = sh
	} else {
		mh := mirrorHistory{s}
		m, err := s.Mirror()
		if err != nil {
			return nil, err
		}
		q, args := "SELECT "+histCols+" FROM messages WHERE chat_id=? AND revoked=0", []any{chat}
		if req.Since > 0 {
			q, args = q+" AND ts>=?", append(args, req.Since)
		}
		if req.Until > 0 {
			q, args = q+" AND ts<=?", append(args, req.Until)
		}
		order := " ORDER BY ts DESC, pk DESC LIMIT ?"
		if req.Since > 0 {
			order = " ORDER BY ts, pk LIMIT ?"
		}
		rows, err := m.r.Query(q+order, append(args, req.Limit)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			x, err := scanHist(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			msgs = append(msgs, x)
		}
		rows.Close()
		sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].At.Before(msgs[j].At) })
		h = mh
	}
	if len(msgs) == 0 {
		return nil, httpError{404, "nothing to replay in " + chat}
	}

	db, err := openAgentDB("")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 5)
	rand.Read(b)
	r := &Replay{ID: "rp" + hex.EncodeToString(b), Chat: chat, Speed: req.Speed, State: "running", Source: source, Started: time.Now().UnixMilli()}
	clk := newScaledClock(msgs[0].At.Add(-time.Second), req.Speed)
	e := newEngine(s, r.ID, db, h, clk)
	e.shadow = true
	e.period = 20 * time.Millisecond
	if req.Webhook != nil && req.Webhook.URL != "" {
		r.hook = startHook(*req.Webhook)
	}
	e.emitFn = func(ev string, payload any) {
		if m, ok := payload.(map[string]any); ok {
			switch ev {
			case "turn.ready":
				t := m["turn"].(*Turn)
				r.add(ReplayEvent{At: e.nowMs(), Kind: "turn", Turn: t.ID})
			case "outbox.shadowed":
				it := m["item"].(*OutItem)
				var texts []string
				for _, p := range it.Parts {
					texts = append(texts, firstNonEmpty(p.Text, p.Caption, "[file]"))
				}
				r.add(ReplayEvent{At: e.nowMs(), Kind: "agent", Turn: it.Turn, ID: it.ID, Text: strings.Join(texts, "\n")})
			}
		}
		if r.hook != nil {
			s.emitTo([]*hookWorker{r.hook}, ev, payload)
			return
		}
		s.emit(ev, payload)
	}
	s.mu.Lock()
	if s.replays == nil {
		s.replays = map[string]*Replay{}
	}
	running := 0
	for _, x := range s.replays {
		if x.State == "running" {
			running++
		}
	}
	if running >= 4 {
		s.mu.Unlock()
		db.Close()
		return nil, httpError{429, "at most 4 replays run at once in a session"}
	}
	s.pruneReplays()
	r.eng = e
	s.replays[r.ID] = r
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	e.start()
	go r.play(ctx, s, msgs, h)
	return r, nil
}

// pruneReplays keeps the 20 newest finished replays. Called with s.mu held.
func (s *Session) pruneReplays() {
	var done []*Replay
	for _, r := range s.replays {
		if r.State != "running" {
			done = append(done, r)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Started < done[j].Started })
	for len(done) > 20 {
		delete(s.replays, done[0].ID)
		done[0].eng.db.Close()
		done = done[1:]
	}
}

func (r *Replay) play(ctx context.Context, s *Session, msgs []histMsg, h history) {
	e := r.eng
	defer func() {
		e.close()
		if r.hook != nil {
			time.Sleep(2 * time.Second) // let the last events leave
			close(r.hook.stop)
		}
	}()
	sh, _ := h.(*scriptHistory)
	for _, m := range msgs {
		if e.clk.Sleep(ctx, m.At.Sub(e.now())) != nil {
			return
		}
		if sh != nil {
			sh.add(m)
		}
		kind := "inbound"
		if m.FromMe {
			kind = "human"
		}
		r.add(ReplayEvent{At: m.At.UnixMilli(), Kind: kind, ID: m.ID, Text: m.Body})
		if !m.FromMe {
			e.onInbound(inbound{Chat: r.Chat, ID: m.ID, At: m.At, Group: strings.HasSuffix(r.Chat, "@g.us")})
		}
	}
	// let the last turn be answered: until nothing is open, or the reply timeout passes
	cfg := e.agent()
	deadline := e.now().Add(ms(cfg.Turn.MaxWaitMs) + time.Duration(cfg.Turn.ReplyTimeoutSec)*time.Second)
	for e.now().Before(deadline) {
		if e.clk.Sleep(ctx, time.Second) != nil {
			return
		}
		e.mu.Lock()
		open := len(e.pending) + len(e.busy)
		e.mu.Unlock()
		var n int
		e.db.db.QueryRow("SELECT (SELECT COUNT(*) FROM turns WHERE state IN ('collecting','ready','replying')) + (SELECT COUNT(*) FROM outbox WHERE state IN ('queued','sending','held'))").Scan(&n)
		if open+n == 0 {
			break
		}
	}
	r.mu.Lock()
	r.State, r.Ended = "done", time.Now().UnixMilli()
	r.mu.Unlock()
	e.emitFn("replay.done", map[string]any{"replay": r.ID, "chatId": r.Chat})
}

func (mg *Manager) replayRoutes(rt *router, with func(func(http.ResponseWriter, *http.Request, *Session, params) error) func(http.ResponseWriter, *http.Request, params)) {
	rt.add("POST", "/api/{session}/replays", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		var req replayReq
		if err := decode(r, &req); err != nil {
			return err
		}
		rp, err := s.startReplay(req)
		if err != nil {
			return err
		}
		writeJSON(w, 202, rp.view(false))
		return nil
	}))
	rt.add("GET", "/api/{session}/replays", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		s.mu.Lock()
		out := []map[string]any{}
		for _, rp := range s.replays {
			out = append(out, rp.view(false))
		}
		s.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i]["started"].(int64) > out[j]["started"].(int64) })
		writeJSON(w, 200, out)
		return nil
	}))
	rt.add("GET", "/api/{session}/replays/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		rp := s.replay(p["id"])
		if rp == nil {
			return httpError{404, "no replay " + p["id"]}
		}
		writeJSON(w, 200, rp.view(true))
		return nil
	}))
	rt.add("DELETE", "/api/{session}/replays/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		rp := s.replay(p["id"])
		if rp == nil {
			return httpError{404, "no replay " + p["id"]}
		}
		rp.stop()
		writeJSON(w, 200, rp.view(false))
		return nil
	}))
}
