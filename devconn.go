package main

// A recording stand-in for the WhatsApp connection: tests and the -dev-fake mode (click through the dashboard
// against a copy of the mirror without a phone). Nothing leaves the machine.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type sent struct {
	to  types.JID
	msg *waE2E.Message
}

type fakeConn struct {
	mu      sync.Mutex
	sent    []sent
	uploads []whatsmeow.MediaType
	reads   map[string][]string
	n       int
	fixed   bool // deterministic timestamps for tests

	presences []string // composing/paused, in order
	fail      []error  // tests: the next sends fail with these, one each
}

func (f *fakeConn) Me() types.JID { return types.NewJID("99600000001", types.DefaultUserServer) }
func (f *fakeConn) SendMessage(ctx context.Context, to types.JID, m *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.fail) > 0 {
		err := f.fail[0]
		f.fail = f.fail[1:]
		return whatsmeow.SendResponse{}, err
	}
	f.n++
	f.sent = append(f.sent, sent{to, m})
	id := fmt.Sprintf("OUT%d", f.n)
	if !f.fixed { // ids stay unique across restarts, as real WhatsApp ids are
		id = fmt.Sprintf("DEV%X", time.Now().UnixNano())
	}
	if len(extra) > 0 && extra[0].ID != "" {
		id = string(extra[0].ID)
	}
	return whatsmeow.SendResponse{ID: id, Timestamp: f.now()}, nil
}
func (f *fakeConn) Upload(ctx context.Context, b []byte, t whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	f.uploads = append(f.uploads, t)
	f.mu.Unlock()
	return whatsmeow.UploadResponse{URL: "https://mmg/x", DirectPath: "/x", MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}}, nil
}
func (f *fakeConn) BuildMessageKey(chat, sender types.JID, id types.MessageID) *waCommon.MessageKey {
	k := &waCommon.MessageKey{FromMe: proto.Bool(sender.IsEmpty() || sender.User == f.Me().User), ID: proto.String(id), RemoteJID: proto.String(chat.String())}
	if !k.GetFromMe() && chat.Server == types.GroupServer {
		k.Participant = proto.String(sender.String())
	}
	return k
}
func (f *fakeConn) BuildPollCreation(name string, opts []string, n int) *waE2E.Message {
	p := &waE2E.PollCreationMessage{Name: proto.String(name), SelectableOptionsCount: proto.Uint32(uint32(n))}
	for _, o := range opts {
		p.Options = append(p.Options, &waE2E.PollCreationMessage_Option{OptionName: proto.String(o)})
	}
	return &waE2E.Message{PollCreationMessageV3: p}
}
func (f *fakeConn) MarkRead(ctx context.Context, ids []types.MessageID, _ time.Time, chat, sender types.JID, _ ...types.ReceiptType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads == nil {
		f.reads = map[string][]string{}
	}
	f.reads[chatID(sender)] = append(f.reads[chatID(sender)], ids...)
	return nil
}
func (f *fakeConn) SendChatPresence(_ context.Context, _ types.JID, state types.ChatPresence, _ types.ChatPresenceMedia) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presences = append(f.presences, string(state))
	return nil
}
func (f *fakeConn) SendPresence(context.Context, types.Presence) error     { return nil }
func (f *fakeConn) SendAppState(context.Context, appstate.PatchInfo) error { return nil }
func (f *fakeConn) last() sent                                             { f.mu.Lock(); defer f.mu.Unlock(); return f.sent[len(f.sent)-1] }

func (f *fakeConn) now() time.Time {
	if f.fixed {
		return time.Unix(1795500000+int64(f.n), 0)
	}
	return time.Now()
}
