package main

// How the agent layer hangs on a session, and its REST API.
//
// A session's config gains two optional blocks, `agent` and `policy`, stored as given and laid over the server
// defaults from wago.yaml. A WAHA client that PUTs its own config without them keeps what is set.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// effective computes the agent and policy settings a session config means, or why it is invalid.
func (mg *Manager) effective(cfg SessionConfig) (AgentConfig, PolicyConfig, error) {
	a, err := decodeAgent(cfg.Agent, mg.agentDefaults)
	if err != nil {
		return a, PolicyConfig{}, err
	}
	p, err := decodePolicy(cfg.Policy, mg.policyDefaults)
	if err != nil {
		return a, p, err
	}
	for _, name := range []string{a.Understand.Audio, a.Understand.Image, a.Understand.Video, a.Understand.Document} {
		if name != "" && mg.processors[name] == nil {
			return a, p, httpError{400, "agent.understand: no processor named " + name + " in the server config"}
		}
	}
	return a, p, nil
}

func (s *Session) agentCfg() AgentConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentV
}

func (s *Session) policyCfg() PolicyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policyV
}

func (s *Session) eng() *Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// adb is the session's agent database, opened on first use (every session records what it sent, for the policy).
func (s *Session) adb() *agentDB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adbLocked()
}

func (s *Session) adbLocked() *agentDB {
	if s.agentDB == nil && s.dir != "" {
		if err := mkdirPrivate(s.dir); err == nil {
			if db, err := openAgentDB(filepath.Join(s.dir, "agent.sqlite3")); err == nil {
				s.agentDB = db
			}
		}
	}
	return s.agentDB
}

// applyAgent starts or stops the engine to match the config. Called with s.mu held, from configure.
func (s *Session) applyAgent() {
	if s.mgr == nil {
		return
	}
	a, p, err := s.mgr.effective(s.Config)
	if err != nil { // validated before it got here; a server default changed under it: keep it off
		a.Enabled = false
	}
	s.agentV, s.policyV = a, p
	switch {
	case a.Enabled && s.engine == nil:
		if db := s.adbLocked(); db != nil {
			s.engine = newEngine(s, "", db, mirrorHistory{s}, wallClock)
			go s.engine.start()
		}
	case !a.Enabled && s.engine != nil:
		e := s.engine
		s.engine = nil
		go e.close()
	}
}

func (s *Session) closeAgent() {
	s.mu.Lock()
	e, db, reps := s.engine, s.agentDB, s.replays
	s.engine, s.agentDB, s.replays = nil, nil, nil
	s.mu.Unlock()
	for _, r := range reps {
		r.stop()
	}
	if e != nil {
		e.close()
	}
	if db != nil {
		db.Close()
	}
}

// ---------------------------------------------------------------- REST

func (mg *Manager) agentRoutes(rt *router, with func(func(http.ResponseWriter, *http.Request, *Session, params) error) func(http.ResponseWriter, *http.Request, params)) {
	ok := func(w http.ResponseWriter, code int, v any) error { writeJSON(w, code, v); return nil }
	engineOf := func(s *Session, turnOrItem string) (*Engine, error) {
		if rid := replayOf(turnOrItem); rid != "" {
			if r := s.replay(rid); r != nil {
				return r.eng, nil
			}
			return nil, httpError{404, "no replay " + rid}
		}
		e := s.eng()
		if e == nil {
			return nil, httpError{409, "the agent layer is off for this session: set config.agent.enabled"}
		}
		return e, nil
	}
	limit := func(r *http.Request, def int) int { return min(max(atoi(r.URL.Query().Get("limit"), def), 1), 1000) }

	rt.add("GET", "/api/{session}/agent", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		out := map[string]any{"config": s.agentCfg(), "policy": s.policyCfg(), "enabled": s.eng() != nil}
		if db := s.adb(); db != nil {
			out["killSwitch"] = killed(db, s.policyCfg())
			counts := map[string]int{}
			rows, err := db.db.Query("SELECT 'turn.'||state, COUNT(*) FROM turns GROUP BY state UNION ALL SELECT 'outbox.'||state, COUNT(*) FROM outbox GROUP BY state")
			if err == nil {
				for rows.Next() {
					var k string
					var n int
					rows.Scan(&k, &n)
					counts[k] = n
				}
				rows.Close()
			}
			out["counts"] = counts
		}
		return ok(w, 200, out)
	}))

	// turns
	rt.add("GET", "/api/{session}/turns", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		q, where, args := r.URL.Query(), []string{"1=1"}, []any{}
		if st := q.Get("state"); st != "" {
			where, args = append(where, "state=?"), append(args, st)
		}
		if c := q.Get("chatId"); c != "" {
			where, args = append(where, "chat=?"), append(args, s.canonChat(c))
		}
		ts, err := e.db.turns("WHERE "+strings.Join(where, " AND ")+" ORDER BY first_at DESC LIMIT "+strconv.Itoa(limit(r, 50)), args...)
		if err != nil {
			return err
		}
		return ok(w, 200, ts)
	}))
	rt.add("GET", "/api/{session}/turns/{turn}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, p["turn"])
		if err != nil {
			return err
		}
		t, err := e.db.turn(p["turn"])
		if err != nil {
			return err
		}
		items, _ := e.db.outs("WHERE turn_id=? ORDER BY created", t.ID)
		return ok(w, 200, map[string]any{"turn": t, "replies": items})
	}))
	rt.add("POST", "/api/{session}/turns/{turn}/reply", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, p["turn"])
		if err != nil {
			return err
		}
		var req replyReq
		if err := decode(r, &req); err != nil {
			return err
		}
		it, code, err := e.reply(p["turn"], req)
		if err != nil {
			return err
		}
		return ok(w, code, it)
	}))
	rt.add("POST", "/api/{session}/turns/{turn}/skip", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, p["turn"])
		if err != nil {
			return err
		}
		var req struct {
			Reason string `json:"reason"`
		}
		decode(r, &req)
		t, err := e.skip(p["turn"], req.Reason)
		if err != nil {
			return err
		}
		return ok(w, 200, t)
	}))

	// outbox
	rt.add("POST", "/api/{session}/outbox", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		var req replyReq
		if err := decode(r, &req); err != nil {
			return err
		}
		it, code, err := e.enqueue(req)
		if err != nil {
			return err
		}
		return ok(w, code, it)
	}))
	rt.add("GET", "/api/{session}/outbox", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		q, where, args := r.URL.Query(), []string{"1=1"}, []any{}
		if st := q.Get("state"); st != "" {
			where, args = append(where, "state=?"), append(args, st)
		}
		if c := q.Get("chatId"); c != "" {
			where, args = append(where, "chat=?"), append(args, s.canonChat(c))
		}
		items, err := e.db.outs("WHERE "+strings.Join(where, " AND ")+" ORDER BY created DESC LIMIT "+strconv.Itoa(limit(r, 50)), args...)
		if err != nil {
			return err
		}
		return ok(w, 200, items)
	}))
	rt.add("GET", "/api/{session}/outbox/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, p["id"])
		if err != nil {
			return err
		}
		it, err := e.db.out(p["id"])
		if err != nil {
			return err
		}
		return ok(w, 200, it)
	}))
	rt.add("DELETE", "/api/{session}/outbox/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, p["id"])
		if err != nil {
			return err
		}
		it, err := e.cancelItem(p["id"])
		if err != nil {
			return err
		}
		return ok(w, 200, it)
	}))

	// ownership
	rt.add("GET", "/api/{session}/owners", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		os, err := e.db.owners()
		if err != nil {
			return err
		}
		return ok(w, 200, os)
	}))
	rt.add("GET", "/api/{session}/chats/{chat}/owner", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		o, err := e.db.owner(s.canonChat(p["chat"]))
		if err != nil {
			return err
		}
		return ok(w, 200, o)
	}))
	rt.add("PUT", "/api/{session}/chats/{chat}/owner", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		var req struct {
			Owner   string `json:"owner"`
			Minutes int    `json:"minutes"` // human: hand back after this long (default ownership.handbackMinutes; 0 with forever)
			Forever bool   `json:"forever"` // human: until someone sets the owner back
			Reason  string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		chat := s.canonChat(p["chat"])
		switch req.Owner {
		case "human":
			until := time.Time{}
			if !req.Forever {
				m := req.Minutes
				if m <= 0 {
					m = s.agentCfg().Ownership.HandbackMinutes
				}
				until = e.now().Add(time.Duration(m) * time.Minute)
			}
			e.takeover(chat, firstNonEmpty(req.Reason, "handed to a person through the API"), until)
		case "agent":
			e.release(chat, firstNonEmpty(req.Reason, "handed back through the API"))
		default:
			return httpError{400, "owner is human or agent"}
		}
		o, _ := e.db.owner(chat)
		return ok(w, 200, o)
	}))

	// context and understanding
	rt.add("GET", "/api/{session}/chats/{chat}/context", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		cfg := s.agentCfg()
		c := cfg.Context
		q := r.URL.Query()
		c.Messages, c.Chars = atoi(q.Get("messages"), c.Messages), atoi(q.Get("chars"), c.Chars)
		return ok(w, 200, e.context(r.Context(), s.canonChat(p["chat"]), e.now(), c, cfg))
	}))
	rt.add("GET", "/api/{session}/chats/{chat}/messages/{id}/understanding", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		e, err := engineOf(s, "")
		if err != nil {
			return err
		}
		cfg := s.agentCfg()
		text := e.understand(r.Context(), s.canonChat(p["chat"]), p["id"], cfg)
		if text == "" {
			return httpError{404, "no text for this message: it has no media, no processor is set for its type, or the processor failed"}
		}
		return ok(w, 200, map[string]any{"text": text})
	}))

	// policy
	rt.add("GET", "/api/{session}/policy", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		db := s.adb()
		if db == nil {
			return httpError{500, "no agent database"}
		}
		e := s.eng()
		if e == nil { // signals work without the agent layer too
			e = &Engine{s: s, db: db, hist: mirrorHistory{s}, clk: wallClock, policy: s.policyCfg, agent: s.agentCfg}
		}
		return ok(w, 200, map[string]any{"policy": s.policyCfg(), "killSwitch": killed(db, s.policyCfg()), "signals": e.signals(r.Context(), time.Now())})
	}))
	rt.add("POST", "/api/{session}/policy/kill", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		var req struct {
			On *bool `json:"on"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if req.On == nil {
			return httpError{400, `{"on": true} stops every send, {"on": false} lets them go again`}
		}
		db := s.adb()
		if db == nil {
			return httpError{500, "no agent database"}
		}
		db.set("kill", map[bool]string{true: "1", false: "0"}[*req.On])
		s.emit("policy.kill", map[string]any{"on": *req.On})
		if e := s.eng(); e != nil {
			e.poke()
		}
		return ok(w, 200, map[string]any{"killSwitch": killed(db, s.policyCfg())})
	}))

	mg.replayRoutes(rt, with)
}

// sentOrigin tags sends for the record of what this server sent.
type originKey struct{}

func withOrigin(ctx context.Context, o string) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}

func originOf(ctx context.Context) string {
	if o, ok := ctx.Value(originKey{}).(string); ok {
		return o
	}
	return "api"
}

// mergeAgentBlocks keeps a session's agent and policy blocks when a WAHA client PUTs a config without them.
func mergeAgentBlocks(raw json.RawMessage, cfg *SessionConfig, old SessionConfig) {
	var given struct {
		Agent  *json.RawMessage `json:"agent"`
		Policy *json.RawMessage `json:"policy"`
	}
	json.Unmarshal(raw, &given)
	if given.Agent == nil {
		cfg.Agent = old.Agent
	}
	if given.Policy == nil {
		cfg.Policy = old.Policy
	}
}
