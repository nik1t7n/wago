package main

// Reading the mirror back in WAHA's shapes, plus what WAHA cannot do: full-text search across chats.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type stored struct {
	PK         int64
	WaID       string
	ChatID     string
	Ts         int64
	FromMe     bool
	Sender     sql.NullString
	SenderName sql.NullString
	Body       string
	Kind       sql.NullString
	HasMedia   bool
	MediaType  sql.NullString
	MediaName  sql.NullString
	ReplyTo    sql.NullString
	EditedTs   sql.NullInt64
	Revoked    bool
	Reactions  sql.NullString
}

const msgCols = "pk,wa_id,chat_id,ts,from_me,sender,sender_name,body,kind,has_media,media_type,media_name,reply_to,edited_ts,revoked,reactions"

func scanMsg(sc interface{ Scan(...any) error }) (stored, error) {
	var m stored
	err := sc.Scan(&m.PK, &m.WaID, &m.ChatID, &m.Ts, &m.FromMe, &m.Sender, &m.SenderName, &m.Body, &m.Kind, &m.HasMedia,
		&m.MediaType, &m.MediaName, &m.ReplyTo, &m.EditedTs, &m.Revoked, &m.Reactions)
	return m, err
}

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func rowToStored(r Row) stored {
	return stored{WaID: r.WaID, ChatID: r.ChatID, Ts: r.Ts, FromMe: r.FromMe, Sender: ns(r.Sender), SenderName: ns(r.SenderName),
		Body: r.Body, Kind: ns(r.Kind), HasMedia: r.HasMedia, MediaType: ns(r.MediaType), MediaName: ns(r.MediaName), ReplyTo: ns(r.ReplyTo)}
}

// wahaID is WAHA's message id: <fromMe>_<chat>_<id>[_<participant>].
func wahaID(m stored) string {
	id := fmt.Sprintf("%t_%s_%s", m.FromMe, m.ChatID, m.WaID)
	if strings.HasSuffix(m.ChatID, "@g.us") && !m.FromMe && m.Sender.Valid {
		id += "_" + m.Sender.String
	}
	return id
}

// tail takes WhatsApp's own id out of a WAHA id (JIDs never contain an underscore).
func tail(id string) string {
	if p := strings.Split(id, "_"); len(p) >= 3 {
		return p[2]
	}
	return id
}

func (s *Session) messageJSON(m stored) map[string]any {
	me := chatID(s.Me())
	from, to := m.ChatID, me
	if m.FromMe {
		from, to = me, m.ChatID
	}
	out := map[string]any{
		"id": wahaID(m), "timestamp": m.Ts, "from": from, "fromMe": m.FromMe, "to": nullable(to),
		"participant": nil, "body": m.Body, "hasMedia": m.HasMedia, "media": nil, "ack": nil,
		"replyTo": nil, "source": "app", "chatId": m.ChatID, "kind": m.Kind.String, "senderName": nullable(m.SenderName.String), "_data": map[string]any{"kind": m.Kind.String, "pushName": m.SenderName.String, "notifyName": m.SenderName.String,
			"key": map[string]any{"id": m.WaID, "remoteJid": m.ChatID, "fromMe": m.FromMe, "remoteJidAlt": nil}},
	}
	if m.FromMe { // it is in the mirror because WhatsApp's server took it
		out["ack"], out["ackName"] = 1, "SERVER"
	}
	if strings.HasSuffix(m.ChatID, "@lid") { // a hidden address whose phone became known after the message was stored
		if pn := s.canonChat(m.ChatID); pn != m.ChatID {
			out["_data"].(map[string]any)["key"].(map[string]any)["remoteJidAlt"] = pn
		}
	}
	if strings.HasSuffix(m.ChatID, "@g.us") {
		out["from"] = m.ChatID
		if !m.FromMe {
			out["participant"] = nullable(m.Sender.String)
		}
	}
	if m.HasMedia {
		out["media"] = map[string]any{"url": fmt.Sprintf("%s/api/files/%s/%s/%s", s.mgr.publicURL, url.PathEscape(s.Name),
			url.PathEscape(m.ChatID), url.PathEscape(m.WaID)), "mimetype": nullable(m.MediaType.String), "filename": nullable(m.MediaName.String)}
	}
	if m.ReplyTo.Valid {
		out["replyTo"] = map[string]any{"id": m.ReplyTo.String}
		out["quotedMsgId"] = m.ReplyTo.String
	}
	if m.EditedTs.Valid {
		out["editedAt"] = m.EditedTs.Int64
	}
	if m.Revoked {
		out["revoked"] = true
	}
	if m.Reactions.Valid && m.Reactions.String != "" {
		var r map[string]string
		if json.Unmarshal([]byte(m.Reactions.String), &r) == nil {
			out["reactions"] = r
		}
	}
	return out
}

// acks: WAHA sends one message.ack per message, shaped like the message (WAHA id, from, to, fromMe) plus the ack level.
// A receipt from someone else is about our messages; one from our own device (read on the phone) about theirs.
func acks(s *Session, v *events.Receipt) []map[string]any {
	ack, name := 2, "DEVICE"
	switch v.Type {
	case types.ReceiptTypeDelivered:
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		ack, name = 3, "READ"
	case types.ReceiptTypePlayed, types.ReceiptTypePlayedSelf:
		ack, name = 4, "PLAYED"
	case types.ReceiptTypeServerError:
		ack, name = -1, "ERROR"
	default: // sender, retry, inactive, peer and history receipts are not delivery states
		return nil
	}
	chat, me := chatID(v.Chat), chatID(s.Me())
	ours := !v.IsFromMe
	out := make([]map[string]any, 0, len(v.MessageIDs))
	for _, id := range v.MessageIDs {
		from, to := chat, me
		if ours {
			from, to = me, chat
		}
		out = append(out, map[string]any{"id": fmt.Sprintf("%t_%s_%s", ours, chat, id), "from": from, "to": to,
			"participant": nullable(chatID(v.Sender)), "fromMe": ours, "ack": ack, "ackName": name, "timestamp": v.Timestamp.Unix()})
	}
	return out
}

func groupJSON(g *types.GroupInfo) map[string]any {
	parts := make([]map[string]any, 0, len(g.Participants))
	for _, p := range g.Participants {
		role := "participant"
		if p.IsSuperAdmin {
			role = "superadmin"
		} else if p.IsAdmin {
			role = "admin"
		}
		item := map[string]any{"id": chatID(p.JID), "role": role}
		if !p.PhoneNumber.IsEmpty() {
			item["pn"] = chatID(p.PhoneNumber)
		}
		if !p.LID.IsEmpty() {
			item["lid"] = chatID(p.LID)
		}
		parts = append(parts, item)
	}
	return map[string]any{"id": chatID(g.JID), "subject": g.Name, "description": g.Topic, "owner": nullable(chatID(g.OwnerJID)),
		"creation": unixOrNil(g.GroupCreated), "announce": g.IsAnnounce, "restrict": g.IsLocked, "size": len(g.Participants),
		"participants": parts}
}

// canonChat accepts any spelling of a chat (phone, @s.whatsapp.net, @c.us, @lid) and returns the mirror's.
func (s *Session) canonChat(id string) string {
	if j, err := parseChatID(id); err == nil {
		id = chatID(j)
	}
	if s.mirror != nil {
		return s.mirror.Canon(id)
	}
	return id
}

type page struct{ limit, offset int }

func (s *Session) chatMessages(ctx context.Context, chat string, q url.Values, p page) ([]map[string]any, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	cond, args := []string{"chat_id=?"}, []any{s.canonChat(chat)}
	if v := q.Get("filter.timestamp.gte"); v != "" {
		cond, args = append(cond, "ts>=?"), append(args, atoi(v, 0))
	}
	if v := q.Get("filter.timestamp.lte"); v != "" {
		cond, args = append(cond, "ts<=?"), append(args, atoi(v, 0))
	}
	if v := q.Get("filter.fromMe"); v != "" {
		cond, args = append(cond, "from_me=?"), append(args, b2i(v == "true"))
	}
	order := "DESC"
	if strings.EqualFold(q.Get("sortOrder"), "asc") {
		order = "ASC"
	}
	rows, err := m.r.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM messages WHERE %s ORDER BY ts %s, pk %s LIMIT ? OFFSET ?",
		msgCols, strings.Join(cond, " AND "), order, order), append(args, p.limit, p.offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		sm, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.messageJSON(sm))
	}
	return out, rows.Err()
}

func (s *Session) message(ctx context.Context, chat, id string) (map[string]any, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	r := m.r.QueryRowContext(ctx, "SELECT "+msgCols+" FROM messages WHERE chat_id=? AND wa_id=?", s.canonChat(chat), tail(id))
	sm, err := scanMsg(r)
	if err != nil {
		return nil, err
	}
	return s.messageJSON(sm), nil
}

// names: address book first, then chat names (the CLI's rule).
func (s *Session) names(ctx context.Context, m *Mirror) map[string]string {
	out := map[string]string{}
	for _, q := range []string{"SELECT jid,name FROM contacts", "SELECT chat_id,name FROM chats WHERE COALESCE(name,'')<>''"} {
		rows, err := m.r.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		for rows.Next() {
			var k, v string
			rows.Scan(&k, &v)
			out[k] = v
		}
		rows.Close()
	}
	return out
}

func (s *Session) chats(ctx context.Context, q url.Values, p page, overview bool) ([]map[string]any, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	sortBy := map[string]string{"id": "chat_id", "name": "name", "conversationTimestamp": "last_ts", "messageTimestamp": "last_ts"}[q.Get("sortBy")]
	if sortBy == "" {
		sortBy = "last_ts"
	}
	order := "DESC"
	if strings.EqualFold(q.Get("sortOrder"), "asc") {
		order = "ASC"
	}
	cond, args := "", []any{}
	if name := strings.TrimSpace(q.Get("q")); name != "" {
		// names live in chats and in the address book; numbers match the id
		cond = `WHERE name LIKE ? ESCAPE '!' OR chat_id LIKE ? ESCAPE '!' OR chat_id IN (SELECT jid FROM contacts WHERE name LIKE ? ESCAPE '!')`
		like := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(name) + "%"
		args = append(args, like, like, like)
	} else if ids := q["ids"]; len(ids) > 0 {
		cond = "WHERE chat_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, id := range ids {
			args = append(args, s.canonChat(id))
		}
	}
	rows, err := m.r.QueryContext(ctx, fmt.Sprintf(`SELECT chat_id, name, is_group, last_ts FROM chats %s
		ORDER BY %s IS NULL, %s %s LIMIT ? OFFSET ?`, cond, sortBy, sortBy, order), append(args, p.limit, p.offset)...)
	if err != nil {
		return nil, err
	}
	type chat struct {
		id    string
		name  sql.NullString
		group bool
		ts    sql.NullInt64
	}
	var list []chat
	for rows.Next() {
		var c chat
		rows.Scan(&c.id, &c.name, &c.group, &c.ts)
		list = append(list, c)
	}
	rows.Close()
	var nm map[string]string
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		name := c.name.String
		if name == "" {
			if nm == nil {
				nm = s.names(ctx, m)
			}
			name = nm[c.id]
		}
		item := map[string]any{"id": c.id, "name": nullable(name), "isGroup": c.group, "conversationTimestamp": c.ts.Int64}
		if overview {
			item["picture"] = nil
			item["lastMessage"] = nil
			r := m.r.QueryRowContext(ctx, "SELECT "+msgCols+" FROM messages WHERE chat_id=? ORDER BY ts DESC, pk DESC LIMIT 1", c.id)
			if sm, err := scanMsg(r); err == nil {
				item["lastMessage"] = s.messageJSON(sm)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// ftsQuery: every word must match, the last one as a prefix; case and diacritics are folded by the index.
func ftsQuery(q string) string {
	words := strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range words {
		words[i] = `"` + w + `"`
		if i == len(words)-1 {
			words[i] += "*"
		}
	}
	return strings.Join(words, " ")
}

func (s *Session) search(ctx context.Context, q url.Values, p page) ([]map[string]any, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	fq := ftsQuery(q.Get("q"))
	if fq == "" {
		return nil, fmt.Errorf("empty query")
	}
	cond, args := []string{"messages_fts MATCH ?"}, []any{fq}
	if c := q.Get("chatId"); c != "" {
		cond, args = append(cond, "m.chat_id=?"), append(args, s.canonChat(c))
	}
	if v := q.Get("from"); v != "" {
		cond, args = append(cond, "m.ts>=?"), append(args, atoi(v, 0))
	}
	if v := q.Get("to"); v != "" {
		cond, args = append(cond, "m.ts<=?"), append(args, atoi(v, 0))
	}
	order := "m.ts DESC"
	if q.Get("rank") == "true" {
		order = "bm25(messages_fts), m.ts DESC"
	}
	cols := "m." + strings.ReplaceAll(msgCols, ",", ",m.")
	rows, err := m.r.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM messages_fts JOIN messages m ON m.pk=messages_fts.rowid WHERE %s ORDER BY %s LIMIT ? OFFSET ?",
		cols, strings.Join(cond, " AND "), order), append(args, p.limit, p.offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	var nm map[string]string
	for rows.Next() {
		sm, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		if nm == nil {
			nm = s.names(ctx, m)
		}
		j := s.messageJSON(sm)
		j["chatName"] = nullable(nm[sm.ChatID])
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Session) contacts(ctx context.Context, p page) ([]map[string]any, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	rows, err := m.r.QueryContext(ctx, `SELECT c.jid, c.name, (SELECT alias FROM aliases WHERE canonical=c.jid LIMIT 1)
		FROM contacts c WHERE c.jid NOT LIKE '%@lid' OR c.jid NOT IN (SELECT alias FROM aliases) ORDER BY c.name LIMIT ? OFFSET ?`, p.limit, p.offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name string
		var lid sql.NullString
		rows.Scan(&id, &name, &lid)
		item := map[string]any{"id": id, "name": name, "pushname": nil, "isGroup": strings.HasSuffix(id, "@g.us"), "lid": nullable(lid.String)}
		if strings.HasSuffix(id, "@c.us") {
			item["number"] = strings.TrimSuffix(id, "@c.us")
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Session) lids(ctx context.Context, p page) ([]map[string]string, error) {
	m, err := s.Mirror()
	if err != nil {
		return nil, err
	}
	rows, err := m.r.QueryContext(ctx, "SELECT alias, canonical FROM aliases ORDER BY alias LIMIT ? OFFSET ?", p.limit, p.offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var a, c string
		rows.Scan(&a, &c)
		out = append(out, map[string]string{"lid": a, "pn": c})
	}
	return out, rows.Err()
}
