package main

// history is where the agent layer reads messages from: the session's mirror when live or replaying, a scripted
// conversation in the simulator. The engine never touches the mirror directly, so both run the same code.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"
)

type histMsg struct {
	ID         string
	FromMe     bool
	Sender     string
	SenderName string
	Body       string
	MediaType  string
	At         time.Time
}

type history interface {
	message(ctx context.Context, chat, id string) (map[string]any, error) // the message as the API shows it
	recent(ctx context.Context, chat string, upto time.Time, n int) ([]histMsg, error)
	get(ctx context.Context, chat, id string) (*histMsg, error)
	media(ctx context.Context, chat, id string) ([]byte, string, error)
	lastInbound(ctx context.Context, chat string, upto time.Time) time.Time // zero: they never wrote
	empty(ctx context.Context, chat string) bool                            // no message at all in the chat
}

// ---------------------------------------------------------------- the mirror

type mirrorHistory struct{ s *Session }

func (h mirrorHistory) message(ctx context.Context, chat, id string) (map[string]any, error) {
	return h.s.message(ctx, chat, id)
}

const histCols = "wa_id, from_me, COALESCE(sender,''), COALESCE(sender_name,''), body, COALESCE(media_type,''), ts"

func scanHist(sc interface{ Scan(...any) error }) (histMsg, error) {
	var m histMsg
	var ts int64
	err := sc.Scan(&m.ID, &m.FromMe, &m.Sender, &m.SenderName, &m.Body, &m.MediaType, &ts)
	m.At = time.Unix(ts, 0)
	return m, err
}

func (h mirrorHistory) recent(ctx context.Context, chat string, upto time.Time, n int) ([]histMsg, error) {
	m, err := h.s.Mirror()
	if err != nil {
		return nil, err
	}
	rows, err := m.r.QueryContext(ctx, "SELECT "+histCols+" FROM messages WHERE chat_id=? AND ts<=? AND revoked=0 ORDER BY ts DESC, pk DESC LIMIT ?",
		h.s.canonChat(chat), upto.Unix(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []histMsg
	for rows.Next() {
		x, err := scanHist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (h mirrorHistory) get(ctx context.Context, chat, id string) (*histMsg, error) {
	m, err := h.s.Mirror()
	if err != nil {
		return nil, err
	}
	x, err := scanHist(m.r.QueryRowContext(ctx, "SELECT "+histCols+" FROM messages WHERE chat_id=? AND wa_id=?", h.s.canonChat(chat), tail(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, httpError{404, "message " + id + " is not in the mirror"}
	}
	return &x, err
}

func (h mirrorHistory) media(ctx context.Context, chat, id string) ([]byte, string, error) {
	data, mime, _, err := h.s.messageFile(ctx, chat, id)
	return data, mime, err
}

func (h mirrorHistory) lastInbound(ctx context.Context, chat string, upto time.Time) time.Time {
	m, err := h.s.Mirror()
	if err != nil {
		return time.Time{}
	}
	var ts sql.NullInt64
	m.r.QueryRowContext(ctx, "SELECT MAX(ts) FROM messages WHERE chat_id=? AND from_me=0 AND ts<=?", h.s.canonChat(chat), upto.Unix()).Scan(&ts)
	if !ts.Valid {
		return time.Time{}
	}
	return time.Unix(ts.Int64, 0)
}

func (h mirrorHistory) empty(ctx context.Context, chat string) bool {
	m, err := h.s.Mirror()
	if err != nil {
		return false
	}
	var n int
	m.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT 1 FROM messages WHERE chat_id=? LIMIT 1)", h.s.canonChat(chat)).Scan(&n)
	return n == 0
}

// ---------------------------------------------------------------- a scripted conversation (the simulator)

type scriptHistory struct {
	mu   sync.Mutex
	chat string
	me   string
	msgs []histMsg // in time order, appended as the script plays
}

func (h *scriptHistory) add(m histMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, m)
	sort.SliceStable(h.msgs, func(i, j int) bool { return h.msgs[i].At.Before(h.msgs[j].At) })
}

func (h *scriptHistory) find(id string) *histMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.msgs {
		if h.msgs[i].ID == tail(id) {
			m := h.msgs[i]
			return &m
		}
	}
	return nil
}

func (h *scriptHistory) message(ctx context.Context, chat, id string) (map[string]any, error) {
	m := h.find(id)
	if m == nil {
		return nil, httpError{404, "no message " + id}
	}
	from, to := h.chat, h.me
	if m.FromMe {
		from, to = h.me, h.chat
	}
	return map[string]any{"id": map[bool]string{true: "true", false: "false"}[m.FromMe] + "_" + h.chat + "_" + m.ID,
		"timestamp": m.At.Unix(), "from": from, "to": to, "fromMe": m.FromMe, "body": m.Body, "hasMedia": false,
		"_data": map[string]any{"notifyName": m.SenderName}}, nil
}

func (h *scriptHistory) recent(ctx context.Context, chat string, upto time.Time, n int) ([]histMsg, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []histMsg
	for i := len(h.msgs) - 1; i >= 0 && len(out) < n; i-- {
		if !h.msgs[i].At.After(upto) {
			out = append(out, h.msgs[i])
		}
	}
	return out, nil
}

func (h *scriptHistory) get(ctx context.Context, chat, id string) (*histMsg, error) {
	if m := h.find(id); m != nil {
		return m, nil
	}
	return nil, httpError{404, "no message " + id}
}

func (h *scriptHistory) media(ctx context.Context, chat, id string) ([]byte, string, error) {
	return nil, "", httpError{404, "the simulator has no files"}
}

func (h *scriptHistory) lastInbound(ctx context.Context, chat string, upto time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.msgs) - 1; i >= 0; i-- {
		if !h.msgs[i].FromMe && !h.msgs[i].At.After(upto) {
			return h.msgs[i].At
		}
	}
	return time.Time{}
}

func (h *scriptHistory) empty(ctx context.Context, chat string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs) == 0
}
