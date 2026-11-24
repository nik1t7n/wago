package main

// Dashboard sign-in. The API key never has to be typed or shown: `wago open` asks the running server for a
// one-time link (valid two minutes, usable once) and opens it; the server answers with an HttpOnly, SameSite=Strict
// cookie. Typing the key on the sign-in page works too. Cookie requests that change something must come from the
// dashboard's own origin, so another site cannot act through the browser.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

const cookieName = "wago"

type webAuth struct {
	mu      sync.Mutex
	file    string
	tokens  map[string]int64 // sha256(cookie) -> expiry
	oneTime map[string]int64 // sha256(link token) -> expiry
}

func newWebAuth(home string) *webAuth {
	a := &webAuth{file: filepath.Join(home, "web-sessions.json"), tokens: map[string]int64{}, oneTime: map[string]int64{}}
	if b, err := os.ReadFile(a.file); err == nil {
		json.Unmarshal(b, &a.tokens)
	}
	return a
}

func randToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func digest(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (a *webAuth) saveLocked() {
	now := time.Now().Unix()
	for k, exp := range a.tokens {
		if exp < now {
			delete(a.tokens, k)
		}
	}
	b, _ := json.Marshal(a.tokens)
	os.WriteFile(a.file, b, 0o600)
}

func (a *webAuth) issue(w http.ResponseWriter) {
	t := randToken()
	a.mu.Lock()
	a.tokens[digest(t)] = time.Now().Add(30 * 24 * time.Hour).Unix()
	a.saveLocked()
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
}

func (a *webAuth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tokens[digest(c.Value)]
	return ok && exp > time.Now().Unix()
}

func (a *webAuth) revoke(r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.tokens, digest(c.Value))
		a.saveLocked()
		a.mu.Unlock()
	}
}

func (a *webAuth) link() string {
	t := randToken()
	a.mu.Lock()
	a.oneTime[digest(t)] = time.Now().Add(2 * time.Minute).Unix()
	a.mu.Unlock()
	return t
}

func (a *webAuth) redeem(t string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := digest(t)
	exp, ok := a.oneTime[d]
	delete(a.oneTime, d)
	return ok && exp > time.Now().Unix()
}

// sameOrigin: a cookie-authenticated change must come from the dashboard itself.
func sameOrigin(r *http.Request) bool {
	if r.Method == "GET" || r.Method == "HEAD" {
		return true
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin"
	}
	o := r.Header.Get("Origin")
	return o == "" || strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://") == r.Host
}

// serveWeb handles the dashboard page and sign-in; it reports whether it answered.
func (mg *Manager) serveWeb(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/":
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	case "/favicon.ico":
		w.WriteHeader(204)
	case "/dashboard", "/dashboard/":
		b, _ := webFS.ReadFile("web/index.html")
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		w.Write(b)
	case "/login":
		if t := r.URL.Query().Get("t"); t != "" && mg.web.redeem(t) {
			mg.web.issue(w)
		}
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	case "/api/auth/login":
		if r.Method != "POST" || !sameOrigin(r) {
			writeJSON(w, 405, map[string]string{"message": "POST from the dashboard"})
			return true
		}
		var body struct {
			Key string `json:"key"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
		if subtle.ConstantTimeCompare([]byte(body.Key), []byte(mg.apiKey)) != 1 {
			time.Sleep(700 * time.Millisecond) // slows guessing
			writeJSON(w, 401, map[string]string{"message": "wrong key"})
			return true
		}
		mg.web.issue(w)
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "/api/auth/logout":
		mg.web.revoke(r)
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "/api/auth/link":
		if r.Method != "POST" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), []byte(mg.apiKey)) != 1 {
			writeJSON(w, 401, map[string]string{"message": "needs the API key"})
			return true
		}
		writeJSON(w, 200, map[string]string{"url": mg.publicURL + "/login?t=" + mg.web.link()})
	case "/api/auth/me":
		writeJSON(w, 200, map[string]bool{"signedIn": mg.web.valid(r)})
	default:
		return false
	}
	return true
}
