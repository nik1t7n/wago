package main

// The agent layer keeps its own state in a small SQLite file next to the session (agent.sqlite3), apart from the
// mirror: the mirror is history as WhatsApp tells it, this is what the agent layer decided and promised (turns,
// the outbox, who owns a chat, which messages this server sent). It survives restarts, so a reply accepted with
// 202 is delivered even if the process dies before sending it.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const agentSchema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS turns(
  id TEXT PRIMARY KEY, chat TEXT NOT NULL, state TEXT NOT NULL, messages TEXT NOT NULL,
  first_at INTEGER NOT NULL, last_at INTEGER NOT NULL, ready_at INTEGER, done_at INTEGER,
  superseded_by TEXT, owner TEXT NOT NULL DEFAULT 'agent', reason TEXT);
CREATE INDEX IF NOT EXISTS turns_chat ON turns(chat, first_at DESC);
CREATE INDEX IF NOT EXISTS turns_state ON turns(state);
CREATE TABLE IF NOT EXISTS outbox(
  id TEXT PRIMARY KEY, chat TEXT NOT NULL, turn_id TEXT, parts TEXT NOT NULL, next_part INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL, error TEXT, error_class TEXT,
  sent TEXT NOT NULL DEFAULT '[]', reply_to TEXT, created INTEGER NOT NULL, updated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS outbox_due ON outbox(state, next_at);
CREATE INDEX IF NOT EXISTS outbox_chat ON outbox(chat, created);
CREATE TABLE IF NOT EXISTS owners(chat TEXT PRIMARY KEY, owner TEXT NOT NULL, since INTEGER NOT NULL, until INTEGER, reason TEXT);
CREATE TABLE IF NOT EXISTS sent(id TEXT PRIMARY KEY, chat TEXT NOT NULL, at INTEGER NOT NULL, new_chat INTEGER NOT NULL DEFAULT 0, origin TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS sent_at ON sent(at);
CREATE INDEX IF NOT EXISTS sent_chat ON sent(chat, at);
CREATE TABLE IF NOT EXISTS understanding(chat TEXT NOT NULL, wa_id TEXT NOT NULL, text TEXT NOT NULL, processor TEXT NOT NULL,
  at INTEGER NOT NULL, PRIMARY KEY(chat, wa_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

type agentDB struct{ db *sql.DB }

// openAgentDB opens path, or a private in-memory database when path is empty (replays).
func openAgentDB(path string) (*agentDB, error) {
	dsn := "file:" + path + "?_busy_timeout=10000&_foreign_keys=on"
	if path == "" {
		dsn = "file::memory:"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // small and write-heavy: one connection keeps it simple and serial
	if _, err := db.Exec(agentSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &agentDB{db}, nil
}

func (a *agentDB) Close() error { return a.db.Close() }

// ---------------------------------------------------------------- turns

type Turn struct {
	ID           string   `json:"id"`
	Chat         string   `json:"chatId"`
	State        string   `json:"state"` // collecting, ready, replying, done, superseded, expired, cancelled
	Messages     []string `json:"messages"`
	FirstAt      int64    `json:"firstAt"` // unix ms
	LastAt       int64    `json:"lastAt"`
	ReadyAt      int64    `json:"readyAt,omitempty"`
	DoneAt       int64    `json:"doneAt,omitempty"`
	SupersededBy string   `json:"supersededBy,omitempty"`
	Owner        string   `json:"owner"`
	Reason       string   `json:"reason,omitempty"`
}

const turnCols = "id, chat, state, messages, first_at, last_at, COALESCE(ready_at,0), COALESCE(done_at,0), COALESCE(superseded_by,''), owner, COALESCE(reason,'')"

func scanTurn(sc interface{ Scan(...any) error }) (*Turn, error) {
	var t Turn
	var msgs string
	if err := sc.Scan(&t.ID, &t.Chat, &t.State, &msgs, &t.FirstAt, &t.LastAt, &t.ReadyAt, &t.DoneAt, &t.SupersededBy, &t.Owner, &t.Reason); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(msgs), &t.Messages)
	return &t, nil
}

func (a *agentDB) saveTurn(t *Turn) error {
	msgs, _ := json.Marshal(t.Messages)
	_, err := a.db.Exec(`INSERT INTO turns(id, chat, state, messages, first_at, last_at, ready_at, done_at, superseded_by, owner, reason)
		VALUES(?,?,?,?,?,?,NULLIF(?,0),NULLIF(?,0),NULLIF(?,''),?,NULLIF(?,''))
		ON CONFLICT(id) DO UPDATE SET state=excluded.state, messages=excluded.messages, last_at=excluded.last_at,
		ready_at=excluded.ready_at, done_at=excluded.done_at, superseded_by=excluded.superseded_by, owner=excluded.owner, reason=excluded.reason`,
		t.ID, t.Chat, t.State, string(msgs), t.FirstAt, t.LastAt, t.ReadyAt, t.DoneAt, t.SupersededBy, t.Owner, t.Reason)
	return err
}

func (a *agentDB) turn(id string) (*Turn, error) {
	t, err := scanTurn(a.db.QueryRow("SELECT "+turnCols+" FROM turns WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, httpError{404, "no turn " + id}
	}
	return t, err
}

func (a *agentDB) turns(where string, args ...any) ([]*Turn, error) {
	rows, err := a.db.Query("SELECT "+turnCols+" FROM turns "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Turn{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- outbox

// Part is one message of a reply. Text, or a file with an optional caption.
type Part struct {
	Text    string  `json:"text,omitempty"`
	File    *fileIn `json:"file,omitempty"`
	Kind    string  `json:"kind,omitempty"` // with file: image, video, voice, file (default from the mimetype)
	Caption string  `json:"caption,omitempty"`
	Convert bool    `json:"convert,omitempty"`
}

type OutItem struct {
	ID         string   `json:"id"`
	Chat       string   `json:"chatId"`
	Turn       string   `json:"turnId,omitempty"`
	Parts      []Part   `json:"parts"`
	NextPart   int      `json:"nextPart"`
	State      string   `json:"state"` // queued, sending, held, sent, shadowed, failed, cancelled, interrupted
	Attempts   int      `json:"attempts"`
	NextAt     int64    `json:"nextAt"` // unix ms
	Error      string   `json:"error,omitempty"`
	ErrorClass string   `json:"errorClass,omitempty"` // transient, uncertain, permanent, policy, held
	Sent       []string `json:"sent"`                 // WhatsApp message ids of the parts sent so far
	ReplyTo    string   `json:"replyTo,omitempty"`
	Created    int64    `json:"created"`
	Updated    int64    `json:"updated"`
}

const outCols = "id, chat, COALESCE(turn_id,''), parts, next_part, state, attempts, next_at, COALESCE(error,''), COALESCE(error_class,''), sent, COALESCE(reply_to,''), created, updated"

func scanOut(sc interface{ Scan(...any) error }) (*OutItem, error) {
	var o OutItem
	var parts, sent string
	if err := sc.Scan(&o.ID, &o.Chat, &o.Turn, &parts, &o.NextPart, &o.State, &o.Attempts, &o.NextAt, &o.Error, &o.ErrorClass, &sent, &o.ReplyTo, &o.Created, &o.Updated); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(parts), &o.Parts)
	json.Unmarshal([]byte(sent), &o.Sent)
	if o.Sent == nil {
		o.Sent = []string{}
	}
	return &o, nil
}

// insertOut adds an item; an existing id is returned as it is (idempotent enqueue).
func (a *agentDB) insertOut(o *OutItem) (*OutItem, bool, error) {
	parts, _ := json.Marshal(o.Parts)
	res, err := a.db.Exec(`INSERT INTO outbox(id, chat, turn_id, parts, next_part, state, attempts, next_at, sent, reply_to, created, updated)
		VALUES(?,?,NULLIF(?,''),?,0,?,0,?,'[]',NULLIF(?,''),?,?) ON CONFLICT(id) DO NOTHING`,
		o.ID, o.Chat, o.Turn, string(parts), o.State, o.NextAt, o.ReplyTo, o.Created, o.Created)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	cur, err := a.out(o.ID)
	return cur, n == 1, err
}

func (a *agentDB) saveOut(o *OutItem) error {
	sent, _ := json.Marshal(o.Sent)
	_, err := a.db.Exec(`UPDATE outbox SET next_part=?, state=?, attempts=?, next_at=?, error=NULLIF(?,''), error_class=NULLIF(?,''),
		sent=?, updated=? WHERE id=?`, o.NextPart, o.State, o.Attempts, o.NextAt, o.Error, o.ErrorClass, string(sent), o.Updated, o.ID)
	return err
}

func (a *agentDB) out(id string) (*OutItem, error) {
	o, err := scanOut(a.db.QueryRow("SELECT "+outCols+" FROM outbox WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, httpError{404, "no outbox item " + id}
	}
	return o, err
}

func (a *agentDB) outs(where string, args ...any) ([]*OutItem, error) {
	rows, err := a.db.Query("SELECT "+outCols+" FROM outbox "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*OutItem{}
	for rows.Next() {
		o, err := scanOut(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- owners, sent, understanding, kv

type Owner struct {
	Chat   string `json:"chatId"`
	Owner  string `json:"owner"` // agent or human
	Since  int64  `json:"since"`
	Until  int64  `json:"until,omitempty"` // a human owner hands the chat back at this time (unix ms)
	Reason string `json:"reason,omitempty"`
}

func (a *agentDB) owner(chat string) (*Owner, error) {
	o := Owner{Chat: chat, Owner: "agent"}
	var until sql.NullInt64
	var reason sql.NullString
	err := a.db.QueryRow("SELECT owner, since, until, reason FROM owners WHERE chat=?", chat).Scan(&o.Owner, &o.Since, &until, &reason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	o.Until, o.Reason = until.Int64, reason.String
	return &o, nil
}

func (a *agentDB) setOwner(o *Owner) error {
	_, err := a.db.Exec(`INSERT INTO owners(chat, owner, since, until, reason) VALUES(?,?,?,NULLIF(?,0),NULLIF(?,''))
		ON CONFLICT(chat) DO UPDATE SET owner=excluded.owner, since=excluded.since, until=excluded.until, reason=excluded.reason`,
		o.Chat, o.Owner, o.Since, o.Until, o.Reason)
	return err
}

func (a *agentDB) owners() ([]*Owner, error) {
	rows, err := a.db.Query("SELECT chat, owner, since, COALESCE(until,0), COALESCE(reason,'') FROM owners ORDER BY since DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Owner{}
	for rows.Next() {
		var o Owner
		if err := rows.Scan(&o.Chat, &o.Owner, &o.Since, &o.Until, &o.Reason); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (a *agentDB) recordSent(id, chat string, at time.Time, newChat bool, origin string) {
	a.db.Exec("INSERT OR IGNORE INTO sent(id, chat, at, new_chat, origin) VALUES(?,?,?,?,?)", id, chat, at.UnixMilli(), newChat, origin)
}

func (a *agentDB) wasSent(id string) bool {
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM sent WHERE id=?", id).Scan(&n)
	return n > 0
}

func (a *agentDB) countSent(since time.Time, chat string, newOnly bool) int {
	q, args := "SELECT COUNT(*) FROM sent WHERE at>=?", []any{since.UnixMilli()}
	if chat != "" {
		q, args = q+" AND chat=?", append(args, chat)
	}
	if newOnly {
		q += " AND new_chat=1"
	}
	var n int
	a.db.QueryRow(q, args...).Scan(&n)
	return n
}

func (a *agentDB) understood(chat, id string) (string, bool) {
	var t string
	err := a.db.QueryRow("SELECT text FROM understanding WHERE chat=? AND wa_id=?", chat, id).Scan(&t)
	return t, err == nil
}

func (a *agentDB) saveUnderstanding(chat, id, text, proc string, at time.Time) {
	a.db.Exec("INSERT OR REPLACE INTO understanding(chat, wa_id, text, processor, at) VALUES(?,?,?,?,?)", chat, id, text, proc, at.UnixMilli())
}

func (a *agentDB) get(k string) string {
	var v string
	a.db.QueryRow("SELECT v FROM kv WHERE k=?", k).Scan(&v)
	return v
}

func (a *agentDB) set(k, v string) {
	a.db.Exec("INSERT INTO kv(k, v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, v)
}
