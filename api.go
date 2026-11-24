package main

// WAHA's REST paths, so existing clients work unchanged, served by a small first-match router
// (WAHA's paths overlap: /api/sessions/{name} and /api/{session}/chats cannot both live in net/http's mux).

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"database/sql"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow/types"
)

type params map[string]string

type route struct {
	method string
	segs   []string
	h      func(http.ResponseWriter, *http.Request, params)
}

type router struct{ routes []route }

func (rt *router) add(method, pattern string, h func(http.ResponseWriter, *http.Request, params)) {
	rt.routes = append(rt.routes, route{method, strings.Split(strings.Trim(pattern, "/"), "/"), h})
}

func (rt *router) match(method, path string) (route, params, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, r := range rt.routes {
		if r.method != method || len(r.segs) != len(segs) {
			continue
		}
		p := params{}
		ok := true
		for i, s := range r.segs {
			if strings.HasPrefix(s, "{") {
				p[s[1:len(s)-1]] = segs[i]
			} else if s != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return r, p, true
		}
	}
	return route{}, nil, false
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	var he httpError
	var ce conflictError
	var se sendError
	switch {
	case errors.As(err, &ce):
		body := map[string]any{"statusCode": 409, "message": ce.msg}
		for k, v := range ce.extra {
			body[k] = v
		}
		writeJSON(w, 409, body)
	case errors.As(err, &se):
		writeJSON(w, se.code, map[string]any{"statusCode": se.code, "message": se.msg})
	case errors.As(err, &he):
		writeJSON(w, he.code, map[string]any{"statusCode": he.code, "message": he.msg})
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, 404, map[string]any{"statusCode": 404, "message": "not found"})
	default:
		writeJSON(w, 500, map[string]any{"statusCode": 500, "message": err.Error()})
	}
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func paging(r *http.Request, def, max int) page {
	q := r.URL.Query()
	l := atoi(q.Get("limit"), def)
	if l <= 0 || l > max {
		l = max
	}
	o := atoi(q.Get("offset"), 0)
	if o < 0 {
		o = 0
	}
	return page{l, o}
}

func (mg *Manager) authorized(r *http.Request) bool {
	k := r.Header.Get("X-Api-Key")
	if k == "" {
		k = r.URL.Query().Get("x-api-key")
	}
	if k != "" {
		return subtle.ConstantTimeCompare([]byte(k), []byte(mg.apiKey)) == 1
	}
	return mg.web != nil && mg.web.valid(r) && sameOrigin(r)
}

// byKey: the request carries the API key (a program), not the dashboard's cookie.
func byKey(r *http.Request) bool {
	return r.Header.Get("X-Api-Key") != "" || r.URL.Query().Get("x-api-key") != ""
}

func (mg *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/ping":
		writeJSON(w, 200, map[string]string{"message": "pong"})
		return
	case path == "/health":
		writeJSON(w, 200, mg.health())
		return
	}
	if mg.serveWeb(w, r) {
		return
	}
	if !mg.authorized(r) {
		writeJSON(w, 401, map[string]any{"statusCode": 401, "message": "Unauthorized"})
		return
	}
	if path == "/ws" {
		mg.hub.serve(w, r)
		return
	}
	rt, p, ok := mg.router.match(r.Method, path)
	if !ok {
		writeJSON(w, 404, map[string]any{"statusCode": 404, "message": "Cannot " + r.Method + " " + path})
		return
	}
	rt.h(w, r, p)
}

func (mg *Manager) session(p params, r *http.Request) (*Session, error) {
	name := p["session"]
	if name == "" {
		name = r.URL.Query().Get("session")
	}
	if name == "" {
		name = "default"
	}
	if s := mg.get(name); s != nil {
		return s, nil
	}
	return nil, httpError{404, "session " + name + " not found"}
}

func (mg *Manager) routes() {
	rt := &mg.router
	with := func(h func(http.ResponseWriter, *http.Request, *Session, params) error) func(http.ResponseWriter, *http.Request, params) {
		return func(w http.ResponseWriter, r *http.Request, p params) {
			s, err := mg.session(p, r)
			if err == nil {
				err = h(w, r, s, p)
			}
			if err != nil {
				fail(w, err)
			}
		}
	}
	ok := func(w http.ResponseWriter, v any) error { writeJSON(w, 200, v); return nil }

	devRoutes(rt, with)
	mg.agentRoutes(rt, with)
	for _, path := range []string{"/api/server/version", "/api/version"} { // WAHA keeps the old path for health checks
		rt.add("GET", path, func(w http.ResponseWriter, r *http.Request, _ params) {
			writeJSON(w, 200, map[string]any{"version": version, "api": apiVersion, "engine": "GOWS", "tier": "CORE", "browser": nil})
		})
	}
	rt.add("GET", "/api/server/status", func(w http.ResponseWriter, r *http.Request, _ params) {
		writeJSON(w, 200, mg.health())
	})

	mg.actionRoutes()

	// sessions
	rt.add("GET", "/api/sessions", func(w http.ResponseWriter, r *http.Request, _ params) {
		all := r.URL.Query().Get("all") == "true"
		out := []any{}
		for _, s := range mg.list() {
			if st, _, _ := s.Status(); all || st != StatusStopped {
				out = append(out, s.info(byKey(r)))
			}
		}
		writeJSON(w, 200, out)
	})
	rt.add("POST", "/api/sessions", func(w http.ResponseWriter, r *http.Request, _ params) {
		var body struct {
			Name   string        `json:"name"`
			Start  *bool         `json:"start"`
			Config SessionConfig `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			fail(w, httpError{400, "bad JSON: " + err.Error()})
			return
		}
		if body.Name == "" {
			body.Name = "default"
		}
		s, err := mg.create(body.Name, body.Config)
		if err != nil {
			fail(w, err)
			return
		}
		if body.Start == nil || *body.Start {
			if err := mg.start(s); err != nil {
				fail(w, err)
				return
			}
		}
		writeJSON(w, 201, s.info(byKey(r)))
	})
	rt.add("GET", "/api/sessions/{session}", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		return ok(w, s.info(byKey(r)))
	}))
	rt.add("PUT", "/api/sessions/{session}", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		var body struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return httpError{400, "bad JSON: " + err.Error()}
		}
		var cfg SessionConfig
		var given struct {
			Sending *bool `json:"sending"`
		}
		if len(body.Config) > 0 {
			if err := json.Unmarshal(body.Config, &cfg); err != nil {
				return httpError{400, "bad config: " + err.Error()}
			}
			json.Unmarshal(body.Config, &given)
		}
		// a WAHA client does not know the sending switch and sends its whole config on every start; leaving the
		// field out keeps the owner's choice instead of turning sending off
		if given.Sending == nil {
			cfg.Sending = s.cfg().Sending
		}
		mergeAgentBlocks(body.Config, &cfg, s.cfg())
		if err := mg.update(s, cfg); err != nil {
			return err
		}
		return ok(w, s.info(byKey(r)))
	}))
	rt.add("DELETE", "/api/sessions/{session}", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		if err := mg.remove(s); err != nil {
			return err
		}
		return ok(w, map[string]any{"deleted": s.Name, "mirror_kept": s.mirrorFor(s.cfg())})
	}))
	for _, act := range []string{"start", "stop", "restart", "logout"} {
		act := act
		rt.add("POST", "/api/sessions/{session}/"+act, with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
			var err error
			switch act {
			case "start":
				err = mg.start(s)
			case "stop":
				mg.stop(s)
			case "restart":
				mg.stop(s)
				err = mg.start(s)
			case "logout":
				ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
				err = s.Logout(ctx)
				cancel()
				mg.save()
			}
			if err != nil {
				return err
			}
			return ok(w, s.info(byKey(r)))
		}))
	}
	rt.add("GET", "/api/sessions/{session}/me", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		return ok(w, s.meJSON())
	}))

	// pairing
	rt.add("GET", "/api/{session}/auth/qr", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		code := s.QR()
		if code == "" {
			st, _, _ := s.Status()
			return httpError{422, "session is " + st + "; a QR is only available in " + StatusQR}
		}
		if r.URL.Query().Get("format") == "raw" {
			return ok(w, map[string]string{"value": code})
		}
		png, err := qrcode.Encode(code, qrcode.Medium, 512)
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			return ok(w, map[string]string{"mimetype": "image/png", "data": base64.StdEncoding.EncodeToString(png)})
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
		return nil
	}))
	rt.add("POST", "/api/{session}/auth/request-code", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		var body struct {
			PhoneNumber string `json:"phoneNumber"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.PhoneNumber == "" {
			return httpError{400, "phoneNumber is required"}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		code, err := s.PairCode(ctx, body.PhoneNumber)
		if err != nil {
			return httpError{422, err.Error()}
		}
		return ok(w, map[string]string{"code": code})
	}))

	// chats and messages, all from the mirror: milliseconds, and they work while the session is stopped
	rt.add("GET", "/api/{session}/chats", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		v, err := s.chats(r.Context(), r.URL.Query(), paging(r, 100, 5000), false)
		if err != nil {
			return err
		}
		return ok(w, v)
	}))
	rt.add("GET", "/api/{session}/chats/overview", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		v, err := s.chats(r.Context(), r.URL.Query(), paging(r, 20, 1000), true)
		if err != nil {
			return err
		}
		return ok(w, v)
	}))
	rt.add("GET", "/api/{session}/chats/{chat}/messages", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		v, err := s.chatMessages(r.Context(), p["chat"], r.URL.Query(), paging(r, 100, 10000))
		if err != nil {
			return err
		}
		return ok(w, v)
	}))
	rt.add("GET", "/api/{session}/chats/{chat}/messages/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		v, err := s.message(r.Context(), p["chat"], p["id"])
		if err != nil {
			return err
		}
		return ok(w, v)
	}))
	rt.add("GET", "/api/{session}/search", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		v, err := s.search(r.Context(), r.URL.Query(), paging(r, 50, 1000))
		if err != nil {
			return httpError{400, err.Error()}
		}
		return ok(w, v)
	}))

	// contacts
	rt.add("GET", "/api/contacts/all", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		v, err := s.contacts(r.Context(), paging(r, 100000, 100000))
		if err != nil {
			return err
		}
		return ok(w, v)
	}))
	rt.add("GET", "/api/contacts", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		id := s.canonChat(r.URL.Query().Get("contactId"))
		m, err := s.Mirror()
		if err != nil {
			return err
		}
		var name string
		if err := m.r.QueryRowContext(r.Context(), "SELECT name FROM contacts WHERE jid=?", id).Scan(&name); err != nil {
			return err
		}
		return ok(w, map[string]any{"id": id, "name": name, "number": strings.TrimSuffix(id, "@c.us")})
	}))
	rt.add("GET", "/api/contacts/check-exists", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		cli := s.Client()
		if cli == nil || !cli.IsConnected() {
			return httpError{422, "session is not connected"}
		}
		phone := strings.TrimLeft(r.URL.Query().Get("phone"), "+ ")
		res, err := cli.IsOnWhatsApp(r.Context(), []string{"+" + phone})
		if err != nil {
			return err
		}
		out := map[string]any{"numberExists": false, "chatId": nil}
		if len(res) > 0 && res[0].IsIn {
			out = map[string]any{"numberExists": true, "chatId": chatID(res[0].JID)}
		}
		return ok(w, out)
	}))

	// groups (live from WhatsApp, cached five minutes)
	rt.add("GET", "/api/{session}/groups", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		gs, err := s.Groups(r.Context(), r.URL.Query().Get("refresh") == "true")
		if err != nil {
			return httpError{422, err.Error()}
		}
		out := make([]any, 0, len(gs))
		for _, g := range gs {
			out = append(out, groupJSON(g))
		}
		return ok(w, out)
	}))
	rt.add("GET", "/api/{session}/groups/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		cli := s.Client()
		if cli == nil || !cli.IsConnected() {
			return httpError{422, "session is not connected"}
		}
		j, err := types.ParseJID(p["id"])
		if err != nil {
			return httpError{400, err.Error()}
		}
		g, err := cli.GetGroupInfo(r.Context(), j)
		if err != nil {
			return err
		}
		return ok(w, groupJSON(g))
	}))

	// hidden addresses
	rt.add("GET", "/api/{session}/lids", with(func(w http.ResponseWriter, r *http.Request, s *Session, _ params) error {
		v, err := s.lids(r.Context(), paging(r, 100, 100000))
		if err != nil {
			return err
		}
		return ok(w, v)
	}))
	rt.add("GET", "/api/{session}/lids/{lid}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		lid := p["lid"]
		if !strings.Contains(lid, "@") {
			lid += "@lid"
		}
		pn := s.canonChat(lid)
		return ok(w, map[string]any{"lid": lid, "pn": nullable(strings.TrimPrefix(pn, lid))})
	}))
	rt.add("GET", "/api/{session}/lids/pn/{pn}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		pn := s.canonChat(p["pn"])
		m, err := s.Mirror()
		if err != nil {
			return err
		}
		var lid sql.NullString
		m.r.QueryRowContext(r.Context(), "SELECT alias FROM aliases WHERE canonical=? LIMIT 1", pn).Scan(&lid)
		return ok(w, map[string]any{"pn": pn, "lid": nullable(lid.String)})
	}))

	// media: fetched from WhatsApp on first view, then served from the session's file cache (media.go)
	rt.add("GET", "/api/files/{session}/{chat}/{id}", with(func(w http.ResponseWriter, r *http.Request, s *Session, p params) error {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		data, mime, name, err := s.messageFile(ctx, p["chat"], p["id"])
		if err != nil {
			return err
		}
		h := w.Header()
		if mime != "" {
			h.Set("Content-Type", mime)
		}
		if name != "" {
			h.Set("Content-Disposition", `inline; filename*=UTF-8''`+url.PathEscape(name))
		}
		h.Set("Cache-Control", "private, max-age=31536000, immutable") // one message's file never changes
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		return nil
	}))
}

// messageFile is a message's file: from the session's cache, else fetched from WhatsApp (media.go).
func (s *Session) messageFile(ctx context.Context, chat, id string) (data []byte, mime, name string, err error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, "", "", err
	}
	ref := mediaRef{chat: s.canonChat(chat), id: tail(id)}
	var mt, nm, sender sql.NullString
	if err := m.r.QueryRowContext(ctx, `SELECT md.msg, m.media_type, m.media_name, m.from_me, m.sender FROM media md
		JOIN messages m ON m.chat_id=md.chat_id AND m.wa_id=md.wa_id WHERE md.chat_id=? AND md.wa_id=?`, ref.chat, ref.id).Scan(&ref.blob, &mt, &nm, &ref.fromMe, &sender); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", "", httpError{404, "this file is not synced yet: older history gets its files with the first sync after linking"}
		}
		return nil, "", "", err
	}
	ref.sender = sender.String
	data, err = s.mediaFile(ctx, ref)
	return data, mt.String, nm.String, err
}
