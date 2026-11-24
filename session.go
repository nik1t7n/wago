package main

// A session is one linked WhatsApp account: its whatsmeow device store, its mirror and its hooks.
// Passive by design: it never sends presence, so the phone stays the "online" device and keeps notifications.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	StatusStopped  = "STOPPED"
	StatusStarting = "STARTING"
	StatusQR       = "SCAN_QR_CODE"
	StatusWorking  = "WORKING"
	StatusFailed   = "FAILED"
)

type SessionConfig struct {
	Webhooks []Webhook       `json:"webhooks,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	Mirror   string          `json:"mirror,omitempty"` // where this session's mirror lives
	Sending  bool            `json:"sending"`          // actions that change WhatsApp (send, react, delete...) are allowed
	Agent    json.RawMessage `json:"agent,omitempty"`  // the agent layer: turns, delivery, ownership, context (agentcfg.go)
	Policy   json.RawMessage `json:"policy,omitempty"` // number policy: limits, quiet hours, cold outreach (agentcfg.go)
}

type Session struct {
	Name      string
	Config    SessionConfig
	Autostart bool

	mgr *Manager
	dir string

	mu        sync.Mutex
	status    string
	reason    string
	revivable bool // a failure that a restart can heal (network, server hiccup), not a logout or a ban
	since     time.Time
	qr        string
	container *sqlstore.Container
	client    *whatsmeow.Client
	cancel    context.CancelFunc
	mirror    *Mirror
	hooks     []*hookWorker

	agentV  AgentConfig // effective agent settings (server defaults + this session's block)
	policyV PolicyConfig
	engine  *Engine  // nil while the agent layer is off
	agentDB *agentDB // what this server sent, turns, outbox; opened on first use
	replays map[string]*Replay

	fake      waConn // tests only
	pictures  pictureCache
	downloads flights
	retries   map[string]chan *events.MediaRetry

	groupsMu sync.Mutex
	groups   []*types.GroupInfo
	groupsAt time.Time
}

func (s *Session) Status() (status, reason string, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.reason, s.since
}

func (s *Session) Client() *whatsmeow.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Session) QR() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusQR {
		return ""
	}
	return s.qr
}

func (s *Session) setStatus(st, reason string, revivable bool) {
	s.mu.Lock()
	changed := s.status != st
	s.status, s.reason, s.revivable = st, reason, revivable
	if changed {
		s.since = time.Now()
	}
	if st != StatusQR {
		s.qr = ""
	}
	m := s.mirror
	s.mu.Unlock()
	if !changed {
		return
	}
	log.Printf("session %s: %s %s", s.Name, st, reason)
	if m != nil {
		m.Push(Op{MetaK: "session_status", MetaV: string(marshal(map[string]any{"status": st, "at": time.Now().Unix()}))})
	}
	s.emit("session.status", map[string]any{"name": s.Name, "status": st, "reason": nullable(reason)})
}

// cfg is a copy of the config; it can change live (configure), so it is read under the lock.
func (s *Session) cfg() SessionConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Config
}

func (s *Session) mirrorFor(c SessionConfig) string {
	if c.Mirror != "" {
		return c.Mirror
	}
	return filepath.Join(s.dir, "mirror.sqlite3")
}

// Mirror opens the session's mirror on first use; reads work while the session is stopped.
func (s *Session) Mirror() (*Mirror, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mirror != nil {
		return s.mirror, nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	m, err := openMirror(s.mirrorFor(s.Config))
	if err != nil {
		return nil, err
	}
	s.mirror = m
	return m, nil
}

func (s *Session) Start() error {
	if s.fake != nil {
		s.Mirror()
		s.setStatus(StatusWorking, "dev-fake: nothing is sent", false)
		return nil
	}
	if s.mgr.devFake {
		return errors.New("dev-fake: this session has no stand-in, refusing to connect to WhatsApp")
	}
	m, err := s.Mirror()
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.client != nil {
		s.mu.Unlock()
		return nil
	}
	if s.container == nil {
		dsn := "file:" + filepath.Join(s.dir, "device.db") + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=10000"
		c, err := sqlstore.New(context.Background(), "sqlite3", dsn, waLog.Noop)
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("device store: %w", err)
		}
		s.container = c
	}
	dev, err := s.container.GetFirstDevice(context.Background())
	if err != nil {
		s.mu.Unlock()
		return err
	}
	cli := whatsmeow.NewClient(dev, s.mgr.waLog)
	cli.EnableAutoReconnect = true
	cli.AutomaticMessageRerequestFromPhone = true
	cli.AddEventHandler(s.handle)
	ctx, cancel := context.WithCancel(context.Background())
	s.client, s.cancel = cli, cancel
	s.mu.Unlock()
	_ = m

	s.setStatus(StatusStarting, "", false)
	if dev.ID == nil {
		qrc, err := cli.GetQRChannel(ctx)
		if err != nil {
			s.Stop()
			return err
		}
		go s.qrLoop(qrc)
	}
	go func() {
		if err := cli.ConnectContext(ctx); err != nil && ctx.Err() == nil {
			s.setStatus(StatusFailed, "connect: "+err.Error(), true)
		}
	}()
	return nil
}

func (s *Session) qrLoop(qrc <-chan whatsmeow.QRChannelItem) {
	for it := range qrc {
		switch it.Event {
		case whatsmeow.QRChannelEventCode:
			s.mu.Lock()
			s.qr = it.Code
			s.mu.Unlock()
			if st, _, _ := s.Status(); st != StatusQR {
				s.setStatus(StatusQR, "", false)
			}
			s.mu.Lock()
			s.qr = it.Code
			s.mu.Unlock()
		case "success":
			s.setStatus(StatusStarting, "paired", false)
		case "timeout":
			s.setStatus(StatusFailed, "QR code was not scanned in time; start the session again for a new one", false)
			go s.Stop()
		default:
			reason := it.Event
			if it.Error != nil {
				reason += ": " + it.Error.Error()
			}
			s.setStatus(StatusFailed, reason, false)
		}
	}
}

// PairCode links by phone number instead of QR: WhatsApp shows a notification, the owner types the code.
func (s *Session) PairCode(ctx context.Context, phone string) (string, error) {
	// the state decides first: a linked or stopped session gets its real status, whatever engine runs it
	if st, _, _ := s.Status(); st != StatusQR {
		return "", fmt.Errorf("session is %s; a code is only possible while it waits for pairing", st)
	}
	cli := s.Client()
	if cli == nil {
		return "", errors.New("session has no WhatsApp connection to pair")
	}
	phone = strings.TrimLeft(phone, "+ ")
	return cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (WaGo)")
}

func (s *Session) Stop() {
	s.mu.Lock()
	cli, cancel := s.client, s.cancel
	s.client, s.cancel = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cli != nil {
		cli.RemoveEventHandlers()
		cli.Disconnect()
	}
	s.setStatus(StatusStopped, "", false)
}

func (s *Session) Logout(ctx context.Context) error {
	if s.fake != nil {
		s.Stop()
		return nil
	}
	cli := s.Client()
	if cli == nil || cli.Store.ID == nil {
		s.Stop()
		return s.forgetDevice()
	}
	err := cli.Logout(ctx)
	s.Stop()
	if err != nil {
		return err
	}
	return s.forgetDevice()
}

func (s *Session) forgetDevice() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.container != nil {
		s.container.Close()
		s.container = nil
	}
	for _, f := range []string{"device.db", "device.db-wal", "device.db-shm"} {
		os.Remove(filepath.Join(s.dir, f))
	}
	return nil
}

// configure applies a new config live: webhooks are replaced, sending and metadata are read per use, so the
// WhatsApp connection is never dropped for a settings change. Hooks live as long as the session, connected or not,
// so a session.status STOPPED still reaches them.
func (s *Session) configure(cfg SessionConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Config = cfg
	for _, h := range s.hooks {
		close(h.stop)
	}
	s.hooks = nil
	for _, w := range cfg.Webhooks {
		s.hooks = append(s.hooks, startHook(w))
	}
	if s.mgr != nil {
		for _, w := range s.mgr.serverHooks {
			s.hooks = append(s.hooks, startHook(w))
		}
	}
	s.applyAgent()
}

func (s *Session) close() {
	s.Stop()
	s.closeAgent()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.hooks {
		close(h.stop)
	}
	s.hooks = nil
	if s.mirror != nil {
		s.mirror.Close()
		s.mirror = nil
	}
	if s.container != nil {
		s.container.Close()
		s.container = nil
	}
}

// Resolver over the mirror's aliases and the device's own LID map.
func (s *Session) PN(lid types.JID) types.JID {
	if m := s.mirror; m != nil {
		if c := m.Canon(chatID(lid)); !strings.HasSuffix(c, "@lid") {
			j, _ := parseChatID(c)
			return j
		}
	}
	if cli := s.Client(); cli != nil {
		if pn, err := cli.Store.LIDs.GetPNForLID(context.Background(), lid.ToNonAD()); err == nil {
			return pn
		}
	}
	return types.JID{}
}

func (s *Session) Me() types.JID {
	if f, ok := s.fake.(interface{ Me() types.JID }); ok {
		return f.Me()
	}
	if cli := s.Client(); cli != nil && cli.Store.ID != nil {
		return cli.Store.ID.ToNonAD()
	}
	return types.JID{}
}

func (s *Session) meJSON() any {
	if s.fake != nil {
		return map[string]any{"id": chatID(s.Me()), "pushName": "dev"}
	}
	cli := s.Client()
	if cli == nil || cli.Store.ID == nil {
		return nil
	}
	me := map[string]any{"id": chatID(*cli.Store.ID), "pushName": cli.Store.PushName}
	if !cli.Store.LID.IsEmpty() {
		me["lid"] = chatID(cli.Store.LID)
	}
	return me
}

func (s *Session) learn(lid, pn string) {
	if m := s.mirror; m != nil {
		m.Learn(lid, pn)
	}
}

// emit sends one event to the hooks and WebSocket clients that asked for it.
func (s *Session) emit(ev string, payload any) {
	s.mu.Lock()
	hooks := s.hooks
	s.mu.Unlock()
	s.emitTo(hooks, ev, payload)
}

// emitTo delivers one event to the given hooks (those that want it) and to WebSocket listeners.
func (s *Session) emitTo(all []*hookWorker, ev string, payload any) {
	s.mu.Lock()
	meta := s.Config.Metadata
	var hooks []*hookWorker
	for _, h := range all {
		if wants(h.cfg.Events, ev) {
			hooks = append(hooks, h)
		}
	}
	s.mu.Unlock()
	ws := s.mgr.hub.wants(s.Name, ev)
	if len(hooks) == 0 && !ws {
		return
	}
	if f, ok := payload.(func() any); ok {
		payload = f()
	}
	body := marshal(envelope{ID: eventID(), Timestamp: time.Now().UnixMilli(), Event: ev, Session: s.Name,
		Metadata: meta, Me: s.meJSON(), Payload: payload, Engine: "GOWS",
		Environment: map[string]any{"version": version, "engine": "GOWS", "tier": "CORE", "browser": nil}})
	for _, h := range hooks {
		h.enqueue(body)
	}
	if ws {
		s.mgr.hub.send(s.Name, ev, body)
	}
}

func (s *Session) handle(e any) {
	switch v := e.(type) {
	case *events.Message:
		s.onMessage(v)
	case *events.HistorySync:
		s.onHistory(v)
	case *events.MediaRetry:
		s.onMediaRetry(v)
	case *events.Connected:
		s.setStatus(StatusWorking, "", false)
		go s.afterConnect()
	case *events.Disconnected:
		if st, _, _ := s.Status(); st == StatusWorking {
			s.setStatus(StatusStarting, "reconnecting", false)
		}
	case *events.KeepAliveTimeout:
		if v.ErrorCount > 2 {
			s.setStatus(StatusStarting, "connection unstable", false)
		}
	case *events.KeepAliveRestored:
		s.setStatus(StatusWorking, "", false)
	case *events.PairSuccess:
		s.setStatus(StatusStarting, "paired", false)
	case *events.LoggedOut:
		s.setStatus(StatusFailed, "logged out on the phone ("+v.Reason.String()+"); pair again", false)
		go func() {
			s.Stop()
			s.forgetDevice()
			s.setStatus(StatusFailed, "logged out on the phone; pair again", false)
		}()
	case *events.StreamReplaced:
		s.setStatus(StatusFailed, "another program connected with this session", false)
		go s.Stop()
	case *events.TemporaryBan:
		s.setStatus(StatusFailed, "temporary ban: "+v.String(), false)
	case *events.ConnectFailure:
		s.setStatus(StatusFailed, fmt.Sprintf("connect failure %d %s", v.Reason, v.Message), v.Reason.IsLoggedOut() == false)
	case *events.ClientOutdated:
		s.setStatus(StatusFailed, "WhatsApp says this client version is outdated; update whatsmeow", false)
	case *events.OfflineSyncCompleted:
		go s.syncContacts()
	case *events.PushName:
		if v.NewPushName != "" {
			id := phoneOf(v.JID, v.JIDAlt, s, s.learn)
			s.mirror.Push(Op{ContactJID: id, ContactName: v.NewPushName, ContactWeak: true})
		}
	case *events.Contact:
		if a := v.Action; a != nil {
			name := a.GetFullName()
			if name == "" {
				name = a.GetFirstName()
			}
			if name != "" {
				id := phoneOf(v.JID, types.JID{}, s, s.learn)
				if pn := a.GetPnJID(); pn != "" && strings.HasSuffix(id, "@lid") {
					if j, err := types.ParseJID(pn); err == nil {
						s.learn(id, chatID(j))
						id = chatID(j)
					}
				}
				s.mirror.Push(Op{ContactJID: id, ContactName: name})
				s.mirror.Push(Op{ChatID: id, ChatName: name, RenameOnly: true})
			}
		}
	case *events.GroupInfo:
		if v.Name != nil {
			s.mirror.Push(Op{ChatID: chatID(v.JID), ChatName: v.Name.Name, ChatGroup: true})
		}
		s.invalidateGroups()
		s.emit("group.v2.update", func() any { return v })
	case *events.JoinedGroup:
		s.mirror.Push(Op{ChatID: chatID(v.JID), ChatName: v.Name, ChatGroup: true})
		for _, p := range v.Participants {
			if !p.LID.IsEmpty() && !p.PhoneNumber.IsEmpty() {
				s.learn(chatID(p.LID), chatID(p.PhoneNumber))
			}
		}
		s.invalidateGroups()
		s.emit("group.v2.join", func() any { return groupJSON(&v.GroupInfo) })
	case *events.Receipt:
		for _, a := range acks(s, v) {
			a := a
			s.emit("message.ack", func() any { return a })
		}
	case *events.Presence:
		s.emit("presence.update", func() any {
			return map[string]any{"id": chatID(v.From), "presences": []any{map[string]any{"participant": chatID(v.From),
				"lastKnownPresence": map[bool]string{true: "offline", false: "online"}[v.Unavailable], "lastSeen": unixOrNil(v.LastSeen)}}}
		})
	case *events.ChatPresence:
		if e := s.eng(); e != nil && v.Chat.Server != types.GroupServer {
			e.onTyping(s.canonChat(chatID(v.Chat)), v.State == types.ChatPresenceComposing)
		}
		s.emit("presence.update", func() any {
			return map[string]any{"id": chatID(v.Chat), "presences": []any{map[string]any{"participant": chatID(v.Sender),
				"lastKnownPresence": string(v.State)}}}
		})
	case *events.CallOffer:
		s.emit("call.received", func() any {
			return map[string]any{"id": v.CallID, "from": chatID(v.CallCreator), "timestamp": v.Timestamp.Unix()}
		})
	}
}

func unixOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func (s *Session) onMessage(evt *events.Message) {
	m := s.mirror
	if m == nil {
		return
	}
	c := convertMessage(evt, s, s.learn)
	if c.Row != nil {
		m.Push(Op{Rows: []Row{*c.Row}})
	}
	for _, op := range c.Ops {
		m.Push(op)
	}
	m.Push(Op{MetaK: "last_event:message.any", MetaV: fmt.Sprint(time.Now().Unix())})
	if c.Row != nil {
		r := *c.Row
		payload := func() any { return s.messageJSON(rowToStored(r)) }
		s.emit("message.any", payload)
		if !r.FromMe {
			s.emit("message", payload)
		}
		if e := s.eng(); e != nil {
			e.onInbound(inbound{Chat: r.ChatID, ID: r.WaID, At: time.Unix(r.Ts, 0), FromMe: r.FromMe,
				Device: evt.Info.Sender.Device, Group: evt.Info.IsGroup})
		}
		return
	}
	s.emitChange(evt, c)
}

// emitChange raises the event for an edit, a revoke or a reaction, whether it came from another device or from
// this server's own API call, so a client sees the same events either way.
func (s *Session) emitChange(evt *events.Message, c Converted) {
	if c.Event == "" || len(c.Ops) == 0 {
		return
	}
	op := c.Ops[0]
	chat, me := chatID(evt.Info.Chat), chatID(s.Me())
	from, to := chat, me
	if evt.Info.IsFromMe {
		from, to = me, chat
	}
	sm := stored{FromMe: evt.Info.IsFromMe, ChatID: chat, WaID: evt.Info.ID}
	if sender := chatID(evt.Info.Sender); sender != "" {
		sm.Sender.String, sm.Sender.Valid = sender, true
	}
	base := map[string]any{"id": wahaID(sm), "timestamp": evt.Info.Timestamp.Unix(), "from": from, "to": to,
		"fromMe": evt.Info.IsFromMe, "participant": nullable(chatID(evt.Info.Sender))}
	switch c.Event {
	case "message.edited":
		base["body"], base["editedMessageId"] = op.EditBody, op.EditID
	case "message.revoked":
		base["revokedMessageId"] = op.RevokeID
	case "message.reaction":
		base["reaction"] = map[string]any{"text": op.Reaction, "messageId": op.ReactID}
	}
	s.emit(c.Event, base)
}

func (s *Session) onHistory(v *events.HistorySync) {
	m, cli := s.mirror, s.Client()
	if m == nil || cli == nil || v.Data == nil {
		return
	}
	d := v.Data
	for _, p := range d.GetPhoneNumberToLidMappings() {
		lid, e1 := types.ParseJID(p.GetLidJID())
		pn, e2 := types.ParseJID(p.GetPnJID())
		if e1 == nil && e2 == nil {
			s.learn(chatID(lid), chatID(pn))
		}
	}
	var rows []Row
	var ops, chats []Op
	for _, conv := range d.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		if jid.Server == types.HiddenUserServer && conv.GetPnJID() != "" {
			if pn, err := types.ParseJID(conv.GetPnJID()); err == nil {
				s.learn(chatID(jid), chatID(pn))
			}
		} else if jid.Server == types.DefaultUserServer && conv.GetLidJID() != "" {
			if lid, err := types.ParseJID(conv.GetLidJID()); err == nil {
				s.learn(chatID(lid), chatID(jid))
			}
		}
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		chats = append(chats, Op{ChatID: chatID(jid), ChatName: name, ChatGroup: jid.Server == types.GroupServer, Backfilled: true})
		for _, hm := range conv.GetMessages() {
			evt, err := cli.ParseWebMessage(jid, hm.GetMessage())
			if err != nil {
				continue
			}
			c := convertMessage(evt, s, s.learn)
			if c.Row != nil {
				rows = append(rows, *c.Row)
			}
			ops = append(ops, c.Ops...)
		}
	}
	for _, p := range d.GetPushnames() {
		if j, err := types.ParseJID(p.GetID()); err == nil && p.GetPushname() != "" {
			ops = append(ops, Op{ContactJID: phoneOf(j, types.JID{}, s, s.learn), ContactName: p.GetPushname(), ContactWeak: true})
		}
	}
	if len(rows) > 0 {
		m.Push(Op{Rows: rows}) // rows first, so edits, revokes and reactions in the same chunk find their targets
	}
	for _, o := range chats {
		m.Push(o)
	}
	for _, o := range ops {
		m.Push(o)
	}
	m.Push(Op{MetaK: "last_history_sync", MetaV: fmt.Sprintf(`{"type":%q,"chunk":%d,"progress":%d,"messages":%d,"at":%d}`,
		d.GetSyncType().String(), d.GetChunkOrder(), d.GetProgress(), len(rows), time.Now().Unix())})
	log.Printf("session %s: history %s chunk %d: %d chats, %d messages", s.Name, d.GetSyncType(), d.GetChunkOrder(), len(d.GetConversations()), len(rows))
}

func (s *Session) afterConnect() {
	time.Sleep(15 * time.Second)
	if st, _, _ := s.Status(); st != StatusWorking {
		return
	}
	s.syncContacts()
	if gs, err := s.Groups(context.Background(), true); err == nil {
		for _, g := range gs {
			s.mirror.Push(Op{ChatID: chatID(g.JID), ChatName: g.Name, ChatGroup: true})
			for _, p := range g.Participants { // the member list pairs hidden addresses with phone numbers
				if !p.LID.IsEmpty() && !p.PhoneNumber.IsEmpty() {
					s.learn(chatID(p.LID), chatID(p.PhoneNumber))
				}
			}
		}
	}
}

// syncContacts copies the address book whatsmeow keeps (from app state and push names) into the mirror.
func (s *Session) syncContacts() {
	cli, m := s.Client(), s.mirror
	if cli == nil || m == nil {
		return
	}
	ctx := context.Background()
	all, err := cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return
	}
	for j, c := range all {
		id := phoneOf(j, types.JID{}, s, s.learn)
		switch {
		case c.FullName != "" || c.FirstName != "":
			name := c.FullName
			if name == "" {
				name = c.FirstName
			}
			m.Push(Op{ContactJID: id, ContactName: name})
			m.Push(Op{ChatID: id, ChatName: name, RenameOnly: true})
		case c.BusinessName != "":
			m.Push(Op{ContactJID: id, ContactName: c.BusinessName, ContactWeak: true})
		case c.PushName != "":
			m.Push(Op{ContactJID: id, ContactName: c.PushName, ContactWeak: true})
		}
	}
	m.Push(Op{MetaK: "last_sync", MetaV: fmt.Sprint(time.Now().Unix())})
}

func (s *Session) invalidateGroups() {
	s.groupsMu.Lock()
	s.groupsAt = time.Time{}
	s.groupsMu.Unlock()
}

// Groups asks WhatsApp for the joined groups, cached for five minutes.
func (s *Session) Groups(ctx context.Context, fresh bool) ([]*types.GroupInfo, error) {
	s.groupsMu.Lock()
	defer s.groupsMu.Unlock()
	if !fresh && time.Since(s.groupsAt) < 5*time.Minute {
		return s.groups, nil
	}
	cli := s.Client()
	if cli == nil || !cli.IsConnected() {
		return nil, errors.New("session is not connected")
	}
	gs, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	s.groups, s.groupsAt = gs, time.Now()
	return gs, nil
}
