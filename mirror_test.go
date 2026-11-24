package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
)

// realCopy gives a test its own mirror: a copy of WAGO_TEST_MIRROR when set (a snapshot of a real account, for
// checks that need real-world data), else a synthetic one built by syntheticMirror.
func realCopy(t *testing.T) string {
	dst := filepath.Join(t.TempDir(), "mirror.sqlite3")
	src := os.Getenv("WAGO_TEST_MIRROR")
	if src == "" {
		syntheticMirror(t, dst)
		return dst
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, _ := os.Create(dst)
	io.Copy(out, in)
	out.Close()
	return dst
}

// syntheticMirror writes a small account: six people and a group, a few dozen messages each in two languages,
// contacts and hidden-address aliases. Enough for paging, search and lookups; nothing real.
func syntheticMirror(t *testing.T, path string) {
	m, err := openMirror(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	lines := []string{"Здравствуйте, когда доставка?", "Good morning, is the shop open today?", "Спасибо большое за заказ",
		"Can I pay by card on delivery?", "Пришлите пожалуйста фотографию", "The parcel arrived, thank you",
		"Добрый вечер, есть свободные столики?", "See you tomorrow at ten"}
	base := int64(1795000000)
	for c := 0; c < 7; c++ {
		chat := "4470000000" + itoa(c) + "@c.us"
		name := "Person " + itoa(c)
		if c == 6 {
			chat, name = "120363000000000001@g.us", "Weekend group"
		}
		m.Push(Op{ChatID: chat, ChatName: name, ChatGroup: c == 6})
		var rows []Row
		for i := 0; i < 15+c*3; i++ {
			fromMe := i%3 == 2
			r := Row{WaID: "SYN" + itoa(c) + "X" + itoa(i), ChatID: chat, Ts: base + int64(c*100000+i*60), FromMe: fromMe,
				Body: lines[(c+i)%len(lines)], Kind: "conversation"}
			if !fromMe {
				r.Sender, r.SenderName = chat, name
				if c == 6 {
					r.Sender = "4470000000" + itoa(i%5) + "@c.us"
				}
			}
			rows = append(rows, r)
		}
		m.Push(Op{Rows: rows})
		if c < 6 {
			m.Push(Op{ContactJID: chat, ContactName: name})
			m.Push(Op{Alias: "1000000000" + itoa(c) + "@lid", Canonical: chat})
		}
	}
	m.Flush()
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(q, err)
	}
	return n
}

func TestMirrorWrites(t *testing.T) {
	m, err := openMirror(realCopy(t))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	before := count(t, m.r, "SELECT COUNT(*) FROM messages")

	m.Push(Op{Rows: []Row{
		{WaID: "T1", ChatID: "111111@lid", Ts: 100, Sender: "111111@lid", Body: "уникальнослово первое", Kind: "conversation"},
		{WaID: "T2", ChatID: "996700000002@c.us", Ts: 200, Sender: "996700000002@c.us", Body: "второе", Kind: "conversation"},
	}})
	m.Flush()
	if n := count(t, m.r, "SELECT COUNT(*) FROM messages"); n != before+2 {
		t.Fatalf("rows %d, want %d", n, before+2)
	}

	// the same message again with an empty body keeps the text (history after live)
	m.Push(Op{Rows: []Row{{WaID: "T2", ChatID: "996700000002@c.us", Ts: 200, Kind: "conversation"}}})
	// learning the alias moves the hidden chat and its sender to the phone address
	m.Learn("111111@lid", "996700000002@c.us")
	m.Push(Op{EditID: "T2", EditBody: "второе исправленное", EditTs: 300})
	m.Push(Op{ReactID: "T2", Reactor: "111111@lid", Reaction: "👍"})
	m.Push(Op{ReactID: "T2", Reactor: "996700000009@c.us", Reaction: "🔥"})
	m.Push(Op{ReactID: "T2", Reactor: "996700000009@c.us", Reaction: ""})
	m.Push(Op{RevokeID: "T1"})
	m.Flush()

	var body, reactions string
	var edited int64
	m.r.QueryRow("SELECT body, edited_ts, reactions FROM messages WHERE wa_id='T2'").Scan(&body, &edited, &reactions)
	if body != "второе исправленное" || edited != 300 || reactions != `{"996700000002@c.us":"👍"}` {
		t.Fatalf("T2: %q %d %s", body, edited, reactions)
	}
	if n := count(t, m.r, "SELECT COUNT(*) FROM messages WHERE chat_id='111111@lid' OR sender='111111@lid'"); n != 0 {
		t.Fatalf("%d rows still under the hidden address", n)
	}
	if n := count(t, m.r, "SELECT COUNT(*) FROM chats WHERE chat_id='111111@lid'"); n != 0 {
		t.Fatal("hidden chat left behind")
	}
	if n := count(t, m.r, "SELECT revoked FROM messages WHERE wa_id='T1' AND chat_id='996700000002@c.us'"); n != 1 {
		t.Fatal("revoke")
	}
	// the search index follows inserts and edits
	if n := count(t, m.r, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", `"исправленное"`); n != 1 {
		t.Fatal("fts after edit")
	}
	if n := count(t, m.r, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", `"уникальнослово"`); n != 1 {
		t.Fatal("fts after insert")
	}
	// contacts: a push name never overwrites an address book name
	m.Push(Op{ContactJID: "996700000002@c.us", ContactName: "Али Бухгалтер"})
	m.Push(Op{ContactJID: "996700000002@c.us", ContactName: "ali", ContactWeak: true})
	m.Push(Op{ChatID: "996700000099@c.us", ChatName: "Никто", RenameOnly: true})
	m.Flush()
	var name string
	m.r.QueryRow("SELECT name FROM contacts WHERE jid='996700000002@c.us'").Scan(&name)
	if name != "Али Бухгалтер" {
		t.Fatal("weak name overwrote", name)
	}
	if n := count(t, m.r, "SELECT COUNT(*) FROM chats WHERE chat_id='996700000099@c.us'"); n != 0 {
		t.Fatal("rename created an empty chat")
	}
}

func TestRepairOnRealMirror(t *testing.T) {
	src := realCopy(t)
	if os.Getenv("WAGO_TEST_MIRROR") == "" {
		t.Skip("no real mirror snapshot")
	}
	raw, _ := sql.Open("sqlite3", src)
	lidBefore := count(t, raw, "SELECT COUNT(*) FROM messages WHERE sender LIKE '%@lid'")
	envBefore := count(t, raw, "SELECT COUNT(*) FROM messages WHERE kind IN ('messageContextInfo','senderKeyDistributionMessage','protocolMessage')")
	total := count(t, raw, "SELECT COUNT(*) FROM messages")
	raw.Close()
	m, err := openMirror(src) // repairs on open
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	env := count(t, m.r, "SELECT COUNT(*) FROM messages WHERE kind IN ('messageContextInfo','senderKeyDistributionMessage','protocolMessage')")
	removed := total - count(t, m.r, "SELECT COUNT(*) FROM messages")
	t.Logf("envelope kinds %d -> %d, empty control rows removed %d, hidden senders %d -> %d", envBefore, env, removed,
		lidBefore, count(t, m.r, "SELECT COUNT(*) FROM messages WHERE sender LIKE '%@lid'"))
	if env != 0 {
		t.Fatal("envelope kinds left")
	}
	if n := count(t, m.r, "SELECT COUNT(*) FROM messages WHERE body='' AND has_media=0 AND kind NOT IN ('placeholderMessage','stickerMessage')"); n > 50 {
		t.Logf("empty rows left: %d", n)
	}
	// the index still matches the table after deletes
	if a, b := count(t, m.r, "SELECT COUNT(*) FROM messages"), count(t, m.r, "SELECT COUNT(*) FROM messages_fts_docsize"); a != b {
		t.Fatalf("fts out of step: %d rows, %d indexed", a, b)
	}
	if f, _ := m.repair(); len(f) != 0 {
		t.Fatal("repair is not idempotent", f)
	}
}

func TestWebhookSignature(t *testing.T) {
	got := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha512.New, []byte("k"))
		mac.Write(body)
		var ev envelope
		json.Unmarshal(body, &ev)
		got <- hmac.Equal([]byte(r.Header.Get("X-Webhook-Hmac")), []byte(hex.EncodeToString(mac.Sum(nil)))) && ev.Event == "message"
	}))
	defer srv.Close()
	h := startHook(Webhook{URL: srv.URL, Events: []string{"message"}, HMAC: &struct {
		Key string `json:"key"`
	}{"k"}})
	defer close(h.stop)
	h.enqueue(marshal(envelope{Event: "message", Payload: map[string]string{"body": "x"}}))
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("signature does not verify the way the receiver checks it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not delivered")
	}
}

func TestWebhookRetries(t *testing.T) {
	hits := make(chan int, 5)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := int(n.Add(1))
		hits <- k
		if k < 3 {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	h := startHook(Webhook{URL: srv.URL, Events: []string{"*"}, Retries: &struct {
		Policy       string `json:"policy,omitempty"`
		DelaySeconds int    `json:"delaySeconds,omitempty"`
		Attempts     int    `json:"attempts,omitempty"`
	}{"constant", 0, 5}})
	defer close(h.stop)
	h.enqueue([]byte(`{}`))
	deadline := time.After(15 * time.Second)
	for {
		select {
		case k := <-hits:
			if k == 3 {
				return
			}
		case <-deadline:
			t.Fatal("did not retry to success")
		}
	}
}

var _ = strings.Contains
