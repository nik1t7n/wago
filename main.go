// wago: WhatsApp HTTP API in one native process. WAHA's REST paths and webhook envelope over whatsmeow
// (the Go library behind WAHA's GOWS engine), with the session's messages kept in a local SQLite mirror that
// doubles as the store: reads take milliseconds, survive a stopped session and include full-text search.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// version is the release; apiVersion is the REST contract: it changes only with an incompatible change
// (removed or renamed fields, routes or events), additions keep it. See CHANGELOG.md.
const (
	version    = "0.2.0"
	apiVersion = "1"
)

type Manager struct {
	home        string
	publicURL   string
	serverHooks []Webhook // wago.yaml webhooks and WAHA's server-wide WHATSAPP_HOOK_* webhook, on every session

	processors     map[string]*Processor // media understanding, by name (wago.yaml processors)
	agentDefaults  AgentConfig           // built-in defaults with wago.yaml's agent block laid over
	policyDefaults PolicyConfig
	apiKey         string
	waLog          waLog.Logger
	hub            Hub
	router         router
	web            *webAuth
	started        time.Time
	devFake        bool // every session, also ones created later, uses the recording stand-in

	mu        sync.Mutex
	sessions  map[string]*Session
	defMirror string
}

type savedSession struct {
	Name      string        `json:"name"`
	Config    SessionConfig `json:"config"`
	Autostart bool          `json:"autostart"`
}

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// names that are also the second segment of API paths (/api/sessions, /api/contacts...)
var reserved = map[string]bool{"sessions": true, "contacts": true, "server": true, "files": true, "reaction": true,
	"sendText": true, "sendImage": true, "sendFile": true, "sendVoice": true, "sendVideo": true, "sendLocation": true,
	"sendContactVcard": true, "sendPoll": true, "sendSeen": true, "startTyping": true, "stopTyping": true, "forwardMessage": true, "auth": true}

func (mg *Manager) get(name string) *Session {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return mg.sessions[name]
}

func (mg *Manager) list() []*Session {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	out := make([]*Session, 0, len(mg.sessions))
	for _, s := range mg.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (mg *Manager) newSession(name string, cfg SessionConfig) *Session {
	if cfg.Mirror == "" && name == "default" && mg.defMirror != "" {
		cfg.Mirror = mg.defMirror
	}
	s := &Session{Name: name, mgr: mg, dir: filepath.Join(mg.home, "sessions", name), status: StatusStopped, since: time.Now()}
	if mg.devFake {
		s.fake = &fakeConn{}
	}
	s.configure(cfg)
	return s
}

func (mg *Manager) create(name string, cfg SessionConfig) (*Session, error) {
	if !validName.MatchString(name) || reserved[name] {
		return nil, httpError{400, "session name: letters, digits, _ and - only, and not a word the API paths use"}
	}
	if _, _, err := mg.effective(cfg); err != nil {
		return nil, err
	}
	mg.mu.Lock()
	if _, ok := mg.sessions[name]; ok {
		mg.mu.Unlock()
		return nil, httpError{422, "session " + name + " already exists"}
	}
	s := mg.newSession(name, cfg)
	mg.sessions[name] = s
	mg.mu.Unlock()
	return s, mg.save()
}

func (mg *Manager) update(s *Session, cfg SessionConfig) error {
	old := s.cfg()
	if cfg.Mirror == "" {
		cfg.Mirror = old.Mirror
	}
	if cfg.Mirror != old.Mirror {
		return httpError{422, "the mirror path of an existing session cannot change"}
	}
	if _, _, err := mg.effective(cfg); err != nil {
		return err
	}
	// the API shows signing keys as ***; a hook sent back with *** keeps the key it had
	for i, h := range cfg.Webhooks {
		if h.HMAC != nil && h.HMAC.Key == "***" {
			cfg.Webhooks[i].HMAC = nil
			for _, old := range old.Webhooks {
				if old.URL == h.URL && old.HMAC != nil {
					k := *old.HMAC
					cfg.Webhooks[i].HMAC = &k
				}
			}
		}
	}
	s.configure(cfg)
	return mg.save()
}

func (mg *Manager) start(s *Session) error {
	if err := s.Start(); err != nil {
		return err
	}
	s.Autostart = true
	return mg.save()
}

func (mg *Manager) stop(s *Session) {
	s.Stop()
	s.Autostart = false
	mg.save()
}

func (mg *Manager) remove(s *Session) error {
	s.Stop()
	s.forgetDevice()
	s.close()
	mg.mu.Lock()
	delete(mg.sessions, s.Name)
	mg.mu.Unlock()
	return mg.save()
}

func (mg *Manager) save() error {
	var out []savedSession
	for _, s := range mg.list() {
		out = append(out, savedSession{s.Name, s.cfg(), s.Autostart})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	tmp := filepath.Join(mg.home, "sessions.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(mg.home, "sessions.json"))
}

func (mg *Manager) load() error {
	b, err := os.ReadFile(filepath.Join(mg.home, "sessions.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var saved []savedSession
	if err := json.Unmarshal(b, &saved); err != nil {
		return err
	}
	for _, sv := range saved {
		s := mg.newSession(sv.Name, sv.Config)
		s.Autostart = sv.Autostart
		mg.sessions[sv.Name] = s
	}
	return nil
}

// info is the session as the API shows it. Signing keys are shown as *** on the dashboard (it is on a screen); a
// caller with the API key gets them as WAHA gives them, since it can set them anyway and clients check their hook.
func (s *Session) info(reveal bool) map[string]any {
	st, reason, since := s.Status()
	cfg := s.cfg()
	hooks := make([]Webhook, len(cfg.Webhooks))
	for i, h := range cfg.Webhooks {
		hooks[i] = h
		if h.HMAC != nil && !reveal {
			hooks[i].HMAC = &struct {
				Key string `json:"key"`
			}{"***"} // never echo the signing key
		}
	}
	cfg.Webhooks = hooks
	return map[string]any{"name": s.Name, "status": st, "reason": nullable(reason), "since": since.Unix(), "config": cfg,
		"me": s.meJSON(), "engine": map[string]any{"engine": "GOWS"}, "presence": "offline"}
}

func (mg *Manager) health() map[string]any {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sessions := map[string]string{}
	for _, s := range mg.list() {
		st, _, _ := s.Status()
		sessions[s.Name] = st
	}
	return map[string]any{"status": "ok", "version": version, "uptime": int(time.Since(mg.started).Seconds()),
		"sessions": sessions, "heap_mb": float64(ms.HeapInuse) / (1 << 20), "goroutines": runtime.NumGoroutine()}
}

// revive restarts sessions that failed for a reason a restart can heal. A logout, a ban, a second client or
// a QR that nobody scanned stay as they are: only the owner can fix those.
func (mg *Manager) revive() {
	for range time.Tick(5 * time.Minute) {
		for _, s := range mg.list() {
			s.mu.Lock()
			heal := s.Autostart && s.status == StatusFailed && s.revivable
			s.mu.Unlock()
			if heal {
				log.Printf("session %s: restarting after failure", s.Name)
				s.Stop()
				s.Start()
			}
		}
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "open":
			openDashboard(os.Args[2:])
			return
		case "config":
			printConfig(os.Args[2:])
			return
		case "version", "-version", "--version":
			fmt.Printf("WaGo %s (API %s)\n", version, apiVersion)
			return
		}
	}
	f := newFlags(flag.CommandLine)
	flag.Parse()
	cfg, path, err := loadConfig(f, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	key, err := resolveSecret(cfg.APIKey, os.Getenv)
	if err != nil || key == "" {
		log.Fatalf("%v (%v)", errNoKey, err)
	}
	procs := map[string]*Processor{}
	for name, p := range cfg.Processors {
		p := p
		if p.APIKey != "" {
			if p.key, err = resolveSecret(p.APIKey, os.Getenv); err != nil {
				log.Fatalf("processor %s: api key: %v", name, err)
			}
		}
		procs[name] = &p
	}
	agentDef, _ := decodeAgent(cfg.Agent, defaultAgent())
	policyDef, _ := decodePolicy(cfg.Policy, defaultPolicy())
	addr, dir := &cfg.Listen, &cfg.Home

	syscall.Umask(0o077) // device keys, mirror and logs are the owner's only
	debug.SetMemoryLimit(int64(cfg.MemoryMB) << 20)
	log.SetFlags(log.LstdFlags)
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatal(err)
	}

	// how the linked device looks in the phone's list, and a request for the full history
	store.SetOSInfo(cfg.DeviceName, [3]uint32{15, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.DeviceProps.RequireFullSync = proto.Bool(true)

	mg := &Manager{home: *dir, publicURL: cfg.PublicURL, apiKey: key, waLog: waLog.Stdout("wa", cfg.LogLevel, false),
		sessions: map[string]*Session{}, started: time.Now(), defMirror: cfg.Mirror, serverHooks: cfg.Webhooks, devFake: cfg.DevFake,
		processors: procs, agentDefaults: agentDef, policyDefaults: policyDef}
	if err := mg.load(); err != nil {
		log.Fatal("sessions.json: ", err)
	}
	if mg.get("default") == nil {
		if _, err := mg.create("default", SessionConfig{}); err != nil {
			log.Fatal(err)
		}
	}
	mg.web = newWebAuth(*dir)
	if cfg.DevFake {
		for _, s := range mg.list() {
			s.Mirror()
			s.setStatus(StatusWorking, "dev-fake: nothing is sent", false)
			s.Autostart = false
		}
	}
	mg.routes()
	for _, s := range mg.list() {
		if s.Autostart {
			if err := s.Start(); err != nil {
				log.Printf("session %s: %v", s.Name, err)
			}
		}
	}
	go mg.revive()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: mg, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	from := "built-in defaults"
	if path != "" {
		from = path
	}
	fmt.Printf("WaGo %s on http://%s, state in %s, config from %s\n", version, *addr, *dir, from)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	for _, s := range mg.list() {
		auto := s.Autostart
		s.close()
		s.Autostart = auto
	}
	mg.save()
}

// openDashboard asks the running server for a one-time sign-in link and opens it in the browser. It reads the
// same config as the server (file, environment, flags), so it finds the address and the key by itself.
func openDashboard(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	f := newFlags(fs)
	fs.Parse(args)
	cfg, _, err := loadConfig(f, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	key, err := resolveSecret(cfg.APIKey, os.Getenv)
	if err != nil || key == "" {
		log.Fatalf("%v (%v)", errNoKey, err)
	}
	// a server listening on every interface (0.0.0.0, [::], ":3000") is reached on loopback; browsers refuse 0.0.0.0
	addr := cfg.Listen
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
	}
	req, _ := http.NewRequest("POST", "http://"+addr+"/api/auth/link", nil)
	req.Header.Set("X-Api-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal("server is not running at ", addr, ": ", err)
	}
	defer resp.Body.Close()
	var out struct {
		URL string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.URL == "" {
		log.Fatal("the server refused the key (", resp.Status, ")")
	}
	// the link goes to the address this command reached (an SSH tunnel to a server, say), not the server's own idea of it
	link := out.URL
	if u, err := url.Parse(out.URL); err == nil {
		link = "http://" + addr + u.RequestURI()
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, link).Run(); err != nil {
		fmt.Println(link)
		return
	}
	fmt.Println("opened the dashboard")
}

// printConfig prints the effective configuration with every secret left as its reference or ***.
func printConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	f := newFlags(fs)
	fs.Parse(args)
	cfg, path, err := loadConfig(f, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	r := cfg.redacted()
	a, _ := decodeAgent(cfg.Agent, defaultAgent())
	p, _ := decodePolicy(cfg.Policy, defaultPolicy())
	out := map[string]any{"file": path, "server": r, "agentDefaults": a, "policyDefaults": p}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func mkdirPrivate(dir string) error { return os.MkdirAll(dir, 0o700) }
