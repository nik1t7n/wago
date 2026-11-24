package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func testManager(t *testing.T) (*Manager, *httptest.Server) {
	mg := &Manager{home: t.TempDir(), apiKey: "secret", waLog: waLog.Noop, sessions: map[string]*Session{}, started: time.Now(),
		defMirror: realCopy(t), agentDefaults: defaultAgent(), policyDefaults: defaultPolicy(), processors: map[string]*Processor{}}
	if _, err := mg.create("default", SessionConfig{}); err != nil {
		t.Fatal(err)
	}
	mg.routes()
	srv := httptest.NewServer(mg)
	mg.publicURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		for _, s := range mg.list() {
			s.close()
		}
	})
	return mg, srv
}

func get(t *testing.T, srv *httptest.Server, path string, key string, into any) int {
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func TestAPI(t *testing.T) {
	mg, srv := testManager(t)
	if get(t, srv, "/api/sessions", "", nil) != 401 || get(t, srv, "/api/sessions", "wrong", nil) != 401 {
		t.Fatal("auth")
	}
	if get(t, srv, "/ping", "", nil) != 200 {
		t.Fatal("ping must be open")
	}
	var sess map[string]any
	if get(t, srv, "/api/sessions/default", "secret", &sess) != 200 || sess["status"] != StatusStopped {
		t.Fatal(sess)
	}
	if get(t, srv, "/api/default/auth/qr", "secret", nil) != 422 {
		t.Fatal("QR of a stopped session must be refused")
	}
	if get(t, srv, "/api/nope/chats", "secret", nil) != 404 {
		t.Fatal("unknown session")
	}

	m, _ := mg.get("default").Mirror()
	realChats := count(t, m.r, "SELECT COUNT(*) FROM chats")
	var chats []map[string]any
	get(t, srv, "/api/default/chats?limit=100000", "secret", &chats)
	if len(chats) != realChats {
		t.Fatalf("chats %d, mirror has %d", len(chats), realChats)
	}
	if realChats == 0 {
		t.Skip("no real mirror snapshot: read checks need data")
	}

	var ov []map[string]any
	get(t, srv, "/api/default/chats/overview?limit=5", "secret", &ov)
	if len(ov) != 5 || ov[0]["lastMessage"] == nil {
		t.Fatalf("overview %v", ov)
	}
	top := ov[0]["id"].(string)

	// paging a chat: pages join without gaps or repeats, in time order
	total := count(t, m.r, "SELECT COUNT(*) FROM messages WHERE chat_id=?", top)
	seen := map[string]bool{}
	var last float64 = 1 << 62
	for off := 0; ; off += 7 {
		var pg []map[string]any
		get(t, srv, "/api/default/chats/"+top+"/messages?limit=7&offset="+itoa(off), "secret", &pg)
		for _, x := range pg {
			id := x["id"].(string)
			if seen[id] {
				t.Fatal("repeat", id)
			}
			seen[id] = true
			if ts := x["timestamp"].(float64); ts > last {
				t.Fatal("order")
			} else {
				last = ts
			}
		}
		if len(pg) < 7 {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("paged %d of %d", len(seen), total)
	}

	// one message by WAHA id and by bare id
	var one map[string]any
	var anyID string
	for id := range seen {
		anyID = id
		break
	}
	if get(t, srv, "/api/default/chats/"+top+"/messages/"+anyID, "secret", &one) != 200 || one["id"] != anyID {
		t.Fatal("message by id", one)
	}
	if get(t, srv, "/api/default/chats/"+top+"/messages/"+tail(anyID), "secret", &one) != 200 || one["id"] != anyID {
		t.Fatal("message by bare id", one)
	}

	// search finds the same rows as the index, newest first
	var word string
	m.r.QueryRow("SELECT body FROM messages WHERE length(body) BETWEEN 6 AND 40 AND body GLOB '[А-Яа-я]*' ORDER BY ts DESC LIMIT 1").Scan(&word)
	var hits []map[string]any
	if code := get(t, srv, "/api/default/search?q="+urlq(word)+"&limit=1000", "secret", &hits); code != 200 || len(hits) == 0 {
		t.Fatalf("search %q: %d %d", word, code, len(hits))
	}
	if want := count(t, m.r, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", ftsQuery(word)); len(hits) != want {
		t.Fatalf("search %d hits, index %d", len(hits), want)
	}

	var contacts, lids []map[string]any
	get(t, srv, "/api/contacts/all?session=default", "secret", &contacts)
	get(t, srv, "/api/default/lids?limit=100000", "secret", &lids)
	if len(contacts) == 0 || len(lids) != count(t, m.r, "SELECT COUNT(*) FROM aliases") {
		t.Fatalf("contacts %d lids %d", len(contacts), len(lids))
	}
	if lid := lids[0]["lid"].(string); true {
		var one map[string]any
		get(t, srv, "/api/default/lids/"+lid, "secret", &one)
		if one["pn"] != lids[0]["pn"] {
			t.Fatal("lid lookup", one)
		}
		get(t, srv, "/api/default/chats/"+lid+"/messages?limit=1", "secret", nil) // hidden address resolves, no error
	}
}

func urlq(s string) string {
	r := ""
	for _, b := range []byte(s) {
		r += "%" + hex2(b)
	}
	return r
}
func hex2(b byte) string { const h = "0123456789ABCDEF"; return string([]byte{h[b>>4], h[b&15]}) }

var _ = os.Getenv
