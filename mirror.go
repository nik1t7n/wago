package main

// The mirror is the store: one SQLite file per session with the schema the inspect-whatsapp CLI reads
// (chats, messages + FTS5, aliases, contacts, meta). Every write goes through one goroutine that groups
// operations into transactions, so a history sync of thousands of messages costs one fsync per batch and
// readers (REST, CLI, agents) never wait on the writer (WAL).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const mirrorSchema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS chats(
  chat_id TEXT PRIMARY KEY, name TEXT, is_group INTEGER NOT NULL DEFAULT 0,
  last_ts INTEGER, backfilled INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS messages(
  pk INTEGER PRIMARY KEY,
  wa_id TEXT NOT NULL,
  chat_id TEXT NOT NULL, ts INTEGER NOT NULL, from_me INTEGER NOT NULL,
  sender TEXT, sender_name TEXT, body TEXT NOT NULL DEFAULT '',
  kind TEXT, has_media INTEGER NOT NULL DEFAULT 0, media_type TEXT, media_name TEXT,
  reply_to TEXT, edited_ts INTEGER, revoked INTEGER NOT NULL DEFAULT 0, reactions TEXT);
CREATE UNIQUE INDEX IF NOT EXISTS messages_identity ON messages(chat_id, wa_id);
CREATE INDEX IF NOT EXISTS messages_wa ON messages(wa_id);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages(chat_id, ts DESC, pk DESC);
CREATE INDEX IF NOT EXISTS messages_ts ON messages(ts DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  body, content='messages', content_rowid='pk', tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER IF NOT EXISTS m_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, body) VALUES (new.pk, new.body); END;
CREATE TRIGGER IF NOT EXISTS m_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES('delete', old.pk, old.body); END;
CREATE TRIGGER IF NOT EXISTS m_au AFTER UPDATE OF body ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES('delete', old.pk, old.body);
  INSERT INTO messages_fts(rowid, body) VALUES (new.pk, new.body); END;
CREATE TABLE IF NOT EXISTS aliases(alias TEXT PRIMARY KEY, canonical TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS contacts(jid TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
-- new: what is needed to fetch a message's media later (the encrypted file stays on WhatsApp's servers)
CREATE TABLE IF NOT EXISTS media(chat_id TEXT NOT NULL, wa_id TEXT NOT NULL, msg BLOB NOT NULL, PRIMARY KEY(chat_id, wa_id)) WITHOUT ROWID;
`

// Row is one message as the mirror stores it.
type Row struct {
	WaID       string
	ChatID     string
	Ts         int64
	FromMe     bool
	Sender     string
	SenderName string
	Body       string
	Kind       string
	HasMedia   bool
	MediaType  string
	MediaName  string
	ReplyTo    string
	Media      []byte // serialized media message, for downloads
}

// Op is one change to the mirror. Exactly one field group is set.
type Op struct {
	Rows []Row

	EditID, EditBody string
	EditTs           int64

	RevokeID string

	ReactID, Reactor, Reaction string

	Alias, Canonical string

	ContactJID, ContactName string
	ContactWeak             bool // a push name: only fills a missing name

	ChatID, ChatName string
	ChatGroup        bool
	Backfilled       bool
	RenameOnly       bool // only names a chat that already exists (no empty chats from the address book)

	MetaK, MetaV string

	MediaChat, MediaID string // replaces a message's media keys (a new path after a media retry)
	MediaMsg           []byte

	done chan struct{} // closed after the batch holding this op committed (tests, flush)
}

type Mirror struct {
	path string
	w    *sql.DB // the single writer
	r    *sql.DB // readers
	ops  chan Op
	wg   sync.WaitGroup

	mu      sync.RWMutex
	aliases map[string]string // @lid -> @c.us

	OnCommit func(n int) // observability
}

func openMirror(path string) (*Mirror, error) {
	w, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=15000&_txlock=immediate&_foreign_keys=off")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(mirrorSchema); err != nil {
		w.Close()
		return nil, fmt.Errorf("mirror schema: %w", err)
	}
	r, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_query_only=true")
	if err != nil {
		return nil, err
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(2)
	r.SetConnMaxIdleTime(time.Minute)
	m := &Mirror{path: path, w: w, r: r, ops: make(chan Op, 4096), aliases: map[string]string{}}
	rows, err := w.Query("SELECT alias, canonical FROM aliases")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, c string
		rows.Scan(&a, &c)
		m.aliases[a] = c
	}
	rows.Close()
	if fixed, err := m.repair(); err != nil {
		log.Printf("mirror repair: %v", err)
	} else if len(fixed) > 0 {
		log.Printf("mirror repaired: %v", fixed)
	}
	m.wg.Add(1)
	go m.loop()
	return m, nil
}

// repair heals what earlier receivers stored wrongly; every step is idempotent and touches only bad rows.
// Envelope kinds (fields that ride along with any message) were taken for the content: empty ones are control
// messages, media ones get their kind from the file type, text ones are text.
func (m *Mirror) repair() (map[string]int64, error) {
	env := "('messageContextInfo','senderKeyDistributionMessage','protocolMessage','reactionMessage')"
	steps := []struct{ name, q string }{
		{"control rows removed", "DELETE FROM messages WHERE kind IN " + env + " AND body='' AND has_media=0"},
		{"media kinds fixed", `UPDATE messages SET kind = CASE
			WHEN media_type LIKE 'image/webp%' THEN 'stickerMessage' WHEN media_type LIKE 'image/%' THEN 'imageMessage'
			WHEN media_type LIKE 'video/%' THEN 'videoMessage' WHEN media_type LIKE 'audio/%' THEN 'audioMessage'
			ELSE 'documentMessage' END WHERE kind IN ` + env + " AND has_media=1"},
		{"text kinds fixed", "UPDATE messages SET kind='conversation' WHERE kind IN " + env + " AND has_media=0 AND body<>''"},
		{"senders moved to phone numbers", "UPDATE messages SET sender=(SELECT canonical FROM aliases WHERE alias=messages.sender) WHERE sender LIKE '%@lid' AND sender IN (SELECT alias FROM aliases)"},
	}
	out := map[string]int64{}
	for _, st := range steps {
		res, err := m.w.Exec(st.q)
		if err != nil {
			return out, fmt.Errorf("%s: %w", st.name, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out[st.name] = n
		}
	}
	return out, nil
}

// Canon maps a hidden @lid address to the phone address when the mapping is known.
func (m *Mirror) Canon(jid string) string {
	if !strings.HasSuffix(jid, "@lid") {
		return jid
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.aliases[jid]; ok {
		return c
	}
	return jid
}

// Learn records an alias in memory at once (so the next event is already canonical) and in the file.
func (m *Mirror) Learn(lid, pn string) bool {
	if !strings.HasSuffix(lid, "@lid") || !strings.HasSuffix(pn, "@c.us") {
		return false
	}
	m.mu.Lock()
	old := m.aliases[lid]
	m.aliases[lid] = pn
	m.mu.Unlock()
	if old == pn {
		return false
	}
	m.Push(Op{Alias: lid, Canonical: pn})
	return true
}

func (m *Mirror) Push(op Op) { m.ops <- op }

// Flush waits until everything pushed so far is committed.
func (m *Mirror) Flush() {
	d := make(chan struct{})
	m.ops <- Op{done: d}
	<-d
}

func (m *Mirror) Close() {
	close(m.ops)
	m.wg.Wait()
	m.w.Close()
	m.r.Close()
}

func (m *Mirror) loop() {
	defer m.wg.Done()
	for op := range m.ops {
		batch := []Op{op}
		n := len(op.Rows)
	more:
		for n < 5000 && len(batch) < 2000 {
			select {
			case o, ok := <-m.ops:
				if !ok {
					break more
				}
				batch = append(batch, o)
				n += len(o.Rows) + 1
			default:
				break more
			}
		}
		if err := m.apply(batch); err != nil {
			log.Printf("mirror write failed (%d ops): %v", len(batch), err)
		} else if m.OnCommit != nil {
			m.OnCommit(len(batch))
		}
		for _, o := range batch {
			if o.done != nil {
				close(o.done)
			}
		}
	}
}

const upsertSQL = `INSERT INTO messages(wa_id,chat_id,ts,from_me,sender,sender_name,body,kind,has_media,media_type,media_name,reply_to)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chat_id,wa_id) DO UPDATE SET
  body=CASE WHEN excluded.body<>'' THEN excluded.body ELSE messages.body END,
  sender=COALESCE(excluded.sender,messages.sender),
  sender_name=COALESCE(excluded.sender_name,messages.sender_name),
  kind=COALESCE(excluded.kind,messages.kind),
  has_media=MAX(messages.has_media,excluded.has_media),
  media_type=COALESCE(excluded.media_type,messages.media_type),
  media_name=COALESCE(excluded.media_name,messages.media_name),
  reply_to=COALESCE(excluded.reply_to,messages.reply_to)`

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (m *Mirror) apply(batch []Op) error {
	ctx := context.Background()
	tx, err := m.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	up, err := tx.Prepare(upsertSQL)
	if err != nil {
		return err
	}
	defer up.Close()
	chatTs := map[string]int64{}
	for _, o := range batch {
		switch {
		case o.MediaMsg != nil:
			if _, err := tx.Exec("INSERT OR REPLACE INTO media(chat_id,wa_id,msg) VALUES(?,?,?)", m.Canon(o.MediaChat), o.MediaID, o.MediaMsg); err != nil {
				return err
			}
		case o.Rows != nil:
			for _, r := range o.Rows {
				r.ChatID, r.Sender = m.Canon(r.ChatID), m.Canon(r.Sender)
				if _, err := up.Exec(r.WaID, r.ChatID, r.Ts, b2i(r.FromMe), nullable(r.Sender), nullable(r.SenderName), r.Body,
					nullable(r.Kind), b2i(r.HasMedia), nullable(r.MediaType), nullable(r.MediaName), nullable(r.ReplyTo)); err != nil {
					return fmt.Errorf("upsert %s: %w", r.WaID, err)
				}
				if len(r.Media) > 0 {
					if _, err := tx.Exec("INSERT OR REPLACE INTO media(chat_id,wa_id,msg) VALUES(?,?,?)", r.ChatID, r.WaID, r.Media); err != nil {
						return err
					}
				}
				if r.Ts > chatTs[r.ChatID] {
					chatTs[r.ChatID] = r.Ts
				}
			}
		case o.EditID != "":
			if _, err := tx.Exec("UPDATE messages SET body=?, edited_ts=? WHERE wa_id=?", o.EditBody, o.EditTs, o.EditID); err != nil {
				return err
			}
		case o.RevokeID != "":
			if _, err := tx.Exec("UPDATE messages SET revoked=1 WHERE wa_id=?", o.RevokeID); err != nil {
				return err
			}
		case o.ReactID != "":
			if err := react(tx, o.ReactID, m.Canon(o.Reactor), o.Reaction); err != nil {
				return err
			}
		case o.Alias != "":
			if err := mergeAlias(tx, o.Alias, o.Canonical); err != nil {
				return err
			}
		case o.ContactJID != "":
			q := "INSERT INTO contacts(jid,name) VALUES(?,?) ON CONFLICT(jid) DO UPDATE SET name=excluded.name"
			if o.ContactWeak {
				q = "INSERT OR IGNORE INTO contacts(jid,name) VALUES(?,?)"
			}
			if _, err := tx.Exec(q, o.ContactJID, o.ContactName); err != nil {
				return err
			}
		case o.ChatID != "" && o.RenameOnly:
			if _, err := tx.Exec("UPDATE chats SET name=? WHERE chat_id=? AND COALESCE(name,'')<>?", o.ChatName, m.Canon(o.ChatID), o.ChatName); err != nil {
				return err
			}
		case o.ChatID != "":
			id := m.Canon(o.ChatID)
			if _, err := tx.Exec(`INSERT INTO chats(chat_id,name,is_group,backfilled) VALUES(?,?,?,?)
				ON CONFLICT(chat_id) DO UPDATE SET name=COALESCE(excluded.name,chats.name), backfilled=MAX(chats.backfilled,excluded.backfilled)`,
				id, nullable(o.ChatName), b2i(o.ChatGroup || strings.HasSuffix(id, "@g.us")), b2i(o.Backfilled)); err != nil {
				return err
			}
		case o.MetaK != "":
			if _, err := tx.Exec("INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", o.MetaK, o.MetaV); err != nil {
				return err
			}
		}
	}
	for id, ts := range chatTs {
		if _, err := tx.Exec(`INSERT INTO chats(chat_id,is_group,last_ts) VALUES(?,?,?)
			ON CONFLICT(chat_id) DO UPDATE SET last_ts=MAX(COALESCE(chats.last_ts,0),excluded.last_ts)`,
			id, b2i(strings.HasSuffix(id, "@g.us")), ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func react(tx *sql.Tx, target, reactor, text string) error {
	var cur sql.NullString
	err := tx.QueryRow("SELECT reactions FROM messages WHERE wa_id=? LIMIT 1", target).Scan(&cur)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	d := map[string]string{}
	if cur.Valid && cur.String != "" {
		json.Unmarshal([]byte(cur.String), &d)
	}
	if reactor == "" {
		reactor = "?"
	}
	if text == "" {
		delete(d, reactor) // an empty reaction removes it
	} else {
		d[reactor] = text
	}
	var v any
	if len(d) > 0 {
		b, _ := json.Marshal(d)
		v = string(b)
	}
	_, err = tx.Exec("UPDATE messages SET reactions=? WHERE wa_id=?", v, target)
	return err
}

// mergeAlias moves everything stored under a hidden @lid address to the phone address.
func mergeAlias(tx *sql.Tx, alias, canonical string) error {
	stmts := []struct {
		q    string
		args []any
	}{
		{"INSERT OR REPLACE INTO aliases(alias,canonical) VALUES(?,?)", []any{alias, canonical}},
		{"UPDATE OR IGNORE messages SET chat_id=? WHERE chat_id=?", []any{canonical, alias}},
		{"DELETE FROM messages WHERE chat_id=?", []any{alias}}, // duplicates of rows already under the phone address
		{"UPDATE messages SET sender=? WHERE sender=?", []any{canonical, alias}},
		{"UPDATE OR IGNORE media SET chat_id=? WHERE chat_id=?", []any{canonical, alias}},
		{"DELETE FROM media WHERE chat_id=?", []any{alias}},
		{`INSERT INTO chats(chat_id,name,is_group,last_ts)
		  SELECT ?, (SELECT name FROM chats WHERE chat_id=?), 0, MAX(ts) FROM messages WHERE chat_id=? HAVING COUNT(*)>0
		  ON CONFLICT(chat_id) DO UPDATE SET last_ts=MAX(COALESCE(chats.last_ts,0),COALESCE(excluded.last_ts,0)),
		    name=COALESCE(chats.name,excluded.name)`, []any{canonical, alias, canonical}},
		{"DELETE FROM chats WHERE chat_id=?", []any{alias}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s.q, s.args...); err != nil {
			return fmt.Errorf("alias %s: %w", alias, err)
		}
	}
	return nil
}
