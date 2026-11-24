package main

// Events leave in WAHA's envelope through webhooks (signed, retried with backoff, one queue per hook so a slow
// receiver never blocks WhatsApp) and through a WebSocket stream. Nothing is marshalled when nobody listens.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Webhook struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
	HMAC   *struct {
		Key string `json:"key"`
	} `json:"hmac,omitempty"`
	Retries *struct {
		Policy       string `json:"policy,omitempty"`
		DelaySeconds int    `json:"delaySeconds,omitempty"`
		Attempts     int    `json:"attempts,omitempty"`
	} `json:"retries,omitempty"`
	CustomHeaders []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"customHeaders,omitempty"`
}

// envHook is WAHA's server-wide webhook from its environment (WHATSAPP_HOOK_*): it applies to every session, in
// addition to each session's own hooks, so a WAHA deployment configured that way keeps working unchanged.
func envHook(getenv func(string) string) []Webhook {
	u := strings.TrimSpace(getenv("WHATSAPP_HOOK_URL"))
	if u == "" {
		return nil
	}
	h := Webhook{URL: u, Events: []string{"message"}}
	if ev := getenv("WHATSAPP_HOOK_EVENTS"); ev != "" {
		h.Events = nil
		for _, e := range strings.Split(ev, ",") {
			if e = strings.TrimSpace(e); e != "" {
				h.Events = append(h.Events, e)
			}
		}
	}
	if k := getenv("WHATSAPP_HOOK_HMAC_KEY"); k != "" {
		h.HMAC = &struct {
			Key string `json:"key"`
		}{k}
	}
	if p, d, a := getenv("WHATSAPP_HOOK_RETRIES_POLICY"), getenv("WHATSAPP_HOOK_RETRIES_DELAY_SECONDS"), getenv("WHATSAPP_HOOK_RETRIES_ATTEMPTS"); p+d+a != "" {
		h.Retries = &struct {
			Policy       string `json:"policy,omitempty"`
			DelaySeconds int    `json:"delaySeconds,omitempty"`
			Attempts     int    `json:"attempts,omitempty"`
		}{p, atoi(d, 0), atoi(a, 0)}
	}
	for _, kv := range strings.Split(getenv("WHATSAPP_HOOK_CUSTOM_HEADERS"), ";") {
		if name, value, found := strings.Cut(kv, ":"); found && strings.TrimSpace(name) != "" {
			h.CustomHeaders = append(h.CustomHeaders, struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			}{strings.TrimSpace(name), strings.TrimSpace(value)})
		}
	}
	return []Webhook{h}
}

func wants(events []string, ev string) bool {
	return slices.Contains(events, "*") || slices.Contains(events, ev)
}

type hookWorker struct {
	cfg  Webhook
	q    chan []byte
	stop chan struct{}
}

var hookClient = &http.Client{Timeout: 15 * time.Second}

func startHook(cfg Webhook) *hookWorker {
	h := &hookWorker{cfg: cfg, q: make(chan []byte, 1024), stop: make(chan struct{})}
	go h.run()
	return h
}

func (h *hookWorker) enqueue(body []byte) {
	select {
	case h.q <- body:
	default:
		log.Printf("webhook %s: queue full, event dropped", h.cfg.URL)
	}
}

func (h *hookWorker) run() {
	attempts, delay, constant := 3, 2*time.Second, false
	if r := h.cfg.Retries; r != nil {
		if r.Attempts > 0 {
			attempts = r.Attempts
		}
		if r.DelaySeconds > 0 {
			delay = time.Duration(r.DelaySeconds) * time.Second
		}
		constant = r.Policy == "constant"
	}
	for {
		select {
		case <-h.stop:
			return
		case body := <-h.q:
			for i := 0; i < attempts; i++ {
				if h.post(body) {
					break
				}
				if i == attempts-1 {
					log.Printf("webhook %s: gave up after %d attempts", h.cfg.URL, attempts)
					break
				}
				wait := delay
				if !constant {
					wait = time.Duration(float64(delay) * math.Pow(2, float64(i)))
				}
				select {
				case <-h.stop:
					return
				case <-time.After(wait):
				}
			}
		}
	}
}

func (h *hookWorker) post(body []byte) bool {
	req, err := http.NewRequest("POST", h.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return true // a bad URL never gets better
	}
	req.Header.Set("Content-Type", "application/json")
	id := make([]byte, 12)
	rand.Read(id)
	req.Header.Set("X-Webhook-Request-Id", hex.EncodeToString(id))
	req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if h.cfg.HMAC != nil && h.cfg.HMAC.Key != "" {
		mac := hmac.New(sha512.New, []byte(h.cfg.HMAC.Key))
		mac.Write(body)
		req.Header.Set("X-Webhook-Hmac", hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-Webhook-Hmac-Algorithm", "sha512")
	}
	for _, c := range h.cfg.CustomHeaders {
		req.Header.Set(c.Name, c.Value)
	}
	resp, err := hookClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 300
}

// Hub fans events out to WebSocket clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
}

type wsClient struct {
	session string
	events  []string
	q       chan []byte
}

func (h *Hub) any() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients) > 0
}

func (h *Hub) wants(session, ev string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if (c.session == "*" || c.session == session) && wants(c.events, ev) {
			return true
		}
	}
	return false
}

func (h *Hub) send(session, ev string, body []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if (c.session == "*" || c.session == session) && wants(c.events, ev) {
			select {
			case c.q <- body:
			default: // a slow reader loses events rather than stalling everyone
			}
		}
	}
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c := &wsClient{session: q.Get("session"), events: q["events"], q: make(chan []byte, 256)}
	if c.session == "" {
		c.session = "*"
	}
	if len(c.events) == 0 {
		c.events = []string{"*"}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"localhost:*", "127.0.0.1:*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	h.mu.Lock()
	if h.clients == nil {
		h.clients = map[*wsClient]struct{}{}
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, c); h.mu.Unlock() }()
	ctx := conn.CloseRead(r.Context())
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-c.q:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

type envelope struct {
	ID          string         `json:"id"`
	Timestamp   int64          `json:"timestamp"`
	Event       string         `json:"event"`
	Session     string         `json:"session"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Me          any            `json:"me,omitempty"`
	Payload     any            `json:"payload"`
	Engine      string         `json:"engine"`
	Environment map[string]any `json:"environment"`
}

func eventID() string {
	b := make([]byte, 13)
	rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}

func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
