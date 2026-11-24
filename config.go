package main

// Server configuration. One shape, four layers, later ones win:
//
//	built-in defaults < wago.yaml < environment (WAGO_*, and WAHA's own names) < command-line flags
//
// The file and the REST API share the same keys (camelCase, as in WAHA's JSON), so a block copied from one works in
// the other. Secrets are never written into the file itself: a secret field holds a reference, resolved at start.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type ServerConfig struct {
	Listen     string `json:"listen"`     // address to listen on; keep it on loopback or a private network
	Home       string `json:"home"`       // state directory: device keys, session list, mirrors
	PublicURL  string `json:"publicUrl"`  // base of media links the API hands out (default http://<listen>)
	APIKey     string `json:"apiKey"`     // secret reference, see resolveSecret
	DeviceName string `json:"deviceName"` // name the phone shows under Linked devices
	LogLevel   string `json:"logLevel"`   // whatsmeow log level: ERROR, WARN, INFO, DEBUG
	MemoryMB   int    `json:"memoryMb"`   // soft limit for the Go heap
	DevFake    bool   `json:"devFake"`    // development: every session is a recording stand-in, nothing reaches WhatsApp
	Mirror     string `json:"mirror"`     // mirror file of the default session (empty: inside home)

	Webhooks   []Webhook            `json:"webhooks,omitempty"`   // server-wide hooks, added to every session
	Processors map[string]Processor `json:"processors,omitempty"` // named media processors (voice to text, image to text)
	Agent      json.RawMessage      `json:"agent,omitempty"`      // defaults for every session's agent block
	Policy     json.RawMessage      `json:"policy,omitempty"`     // defaults for every session's policy block
}

func defaultServerConfig() ServerConfig {
	return ServerConfig{Listen: "127.0.0.1:3000", Home: defaultHome(), DeviceName: "WaGo", LogLevel: "WARN", MemoryMB: 96,
		APIKey: "env:WAGO_API_KEY"}
}

// defaultHome follows the platform's convention for application data.
func defaultHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "wago")
	}
	h, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "wago")
	}
	return filepath.Join(h, ".local", "share", "wago")
}

// configFile finds the file to read: an explicit path, else the first of ./wago.yaml and the user's config dir.
func configFile(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file: %w", err)
		}
		return explicit, nil
	}
	cands := []string{"wago.yaml", "wago.yml"}
	if d, err := os.UserConfigDir(); err == nil {
		cands = append(cands, filepath.Join(d, "wago", "wago.yaml"))
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		cands = append(cands, filepath.Join(d, "wago", "wago.yaml"))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", nil
}

// yamlToJSON lets one set of json tags describe both the file and the API.
func yamlToJSON(b []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (c *ServerConfig) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	j, err := yamlToJSON(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(j)))
	dec.DisallowUnknownFields() // a misspelt key is an error, not a silently ignored setting
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// loadEnv applies WAGO_* variables and WAHA's own names, so a WAHA deployment's environment keeps working.
func (c *ServerConfig) loadEnv(getenv func(string) string) error {
	str := func(dst *string, names ...string) {
		for _, n := range names {
			if v := getenv(n); v != "" {
				*dst = v
				return
			}
		}
	}
	str(&c.Listen, "WAGO_LISTEN")
	str(&c.Home, "WAGO_HOME")
	str(&c.PublicURL, "WAGO_PUBLIC_URL", "WAHA_BASE_URL")
	str(&c.DeviceName, "WAGO_DEVICE_NAME")
	str(&c.LogLevel, "WAGO_LOG_LEVEL")
	str(&c.Mirror, "WAGO_MIRROR")
	if getenv("WAGO_API_KEY") != "" {
		c.APIKey = "env:WAGO_API_KEY"
	} else if getenv("WAHA_API_KEY") != "" {
		c.APIKey = "env:WAHA_API_KEY"
	}
	if v := getenv("WAGO_MEMORY_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("WAGO_MEMORY_MB: %w", err)
		}
		c.MemoryMB = n
	}
	if v := getenv("WAGO_DEV_FAKE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("WAGO_DEV_FAKE: %w", err)
		}
		c.DevFake = b
	}
	c.Webhooks = append(c.Webhooks, envHook(getenv)...)
	return nil
}

// flags registers the command-line layer on fs; apply copies only the flags that were given.
type cliFlags struct {
	fs                                                  *flag.FlagSet
	config, listen, home, public, key, device, log, mir *string
	mem                                                 *int
	fake                                                *bool
}

func newFlags(fs *flag.FlagSet) *cliFlags {
	f := &cliFlags{fs: fs}
	f.config = fs.String("config", os.Getenv("WAGO_CONFIG"), "config file (default: ./wago.yaml, then the user config dir; env WAGO_CONFIG)")
	f.listen = fs.String("addr", "", "listen address (env WAGO_LISTEN; default 127.0.0.1:3000)")
	f.home = fs.String("home", "", "state directory (env WAGO_HOME)")
	f.public = fs.String("public-url", "", "base URL clients use to reach WaGo, for media links (env WAGO_PUBLIC_URL or WAHA_BASE_URL)")
	f.key = fs.String("api-key", "", "API key source: env:NAME, file:PATH, keychain:SERVICE[/ACCOUNT] (env WAGO_API_KEY or WAHA_API_KEY)")
	f.device = fs.String("device-name", "", "name the phone shows under Linked devices")
	f.log = fs.String("log", "", "whatsmeow log level")
	f.mir = fs.String("mirror", "", "mirror file of the default session")
	f.mem = fs.Int("mem", 0, "soft memory limit for the Go heap, MiB")
	f.fake = fs.Bool("dev-fake", false, "development: sessions use a recording stand-in instead of WhatsApp (nothing is sent)")
	return f
}

func (f *cliFlags) apply(c *ServerConfig) {
	f.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "addr":
			c.Listen = *f.listen
		case "home":
			c.Home = *f.home
		case "public-url":
			c.PublicURL = *f.public
		case "api-key":
			c.APIKey = *f.key
		case "device-name":
			c.DeviceName = *f.device
		case "log":
			c.LogLevel = *f.log
		case "mirror":
			c.Mirror = *f.mir
		case "mem":
			c.MemoryMB = *f.mem
		case "dev-fake":
			c.DevFake = *f.fake
		}
	})
}

// loadConfig builds the effective configuration from all four layers.
func loadConfig(f *cliFlags, getenv func(string) string) (ServerConfig, string, error) {
	c := defaultServerConfig()
	path, err := configFile(*f.config)
	if err != nil {
		return c, "", err
	}
	if path != "" {
		if err := c.loadFile(path); err != nil {
			return c, path, err
		}
	}
	if err := c.loadEnv(getenv); err != nil {
		return c, path, err
	}
	f.apply(&c)
	c.Home = expandHome(c.Home)
	c.Mirror = expandHome(c.Mirror)
	if c.PublicURL == "" {
		c.PublicURL = "http://" + c.Listen
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.Agent != nil {
		if _, err := decodeAgent(c.Agent, defaultAgent()); err != nil {
			return c, path, fmt.Errorf("agent: %w", err)
		}
	}
	if c.Policy != nil {
		if _, err := decodePolicy(c.Policy, defaultPolicy()); err != nil {
			return c, path, fmt.Errorf("policy: %w", err)
		}
	}
	for name, p := range c.Processors {
		if err := p.validate(); err != nil {
			return c, path, fmt.Errorf("processor %s: %w", name, err)
		}
	}
	return c, path, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

// resolveSecret reads a secret from where its reference points:
//
//	env:NAME                 an environment variable
//	file:PATH                a file (trailing newline trimmed), e.g. a Docker or Kubernetes secret
//	keychain:SERVICE[/ACCT]  the macOS Keychain (account defaults to the current user)
//
// Anything else is taken as the value itself, which is fine for tests and discouraged elsewhere.
func resolveSecret(ref string, getenv func(string) string) (string, error) {
	kind, rest, found := strings.Cut(ref, ":")
	if !found {
		return ref, nil
	}
	switch kind {
	case "env":
		v := getenv(rest)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", rest)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(expandHome(rest))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case "keychain":
		svc, acct, _ := strings.Cut(rest, "/")
		if acct == "" {
			if u, err := user.Current(); err == nil {
				acct = u.Username
			}
		}
		out, err := exec.Command("security", "find-generic-password", "-a", acct, "-s", svc, "-w").Output()
		if err != nil {
			return "", fmt.Errorf("keychain item %s for %s not found", svc, acct)
		}
		return strings.TrimSpace(string(out)), nil
	}
	return ref, nil // a value that merely contains a colon
}

var errNoKey = errors.New("no API key: set WAGO_API_KEY, or apiKey in wago.yaml (env:NAME, file:PATH or keychain:SERVICE)")

// redacted is the configuration as `wago config` prints it: references stay, values never appear.
func (c ServerConfig) redacted() ServerConfig {
	r := c
	if !strings.Contains(r.APIKey, ":") {
		r.APIKey = "***"
	}
	r.Webhooks = make([]Webhook, len(c.Webhooks))
	for i, h := range c.Webhooks {
		r.Webhooks[i] = h
		if h.HMAC != nil {
			r.Webhooks[i].HMAC = &struct {
				Key string `json:"key"`
			}{"***"}
		}
	}
	r.Processors = map[string]Processor{}
	for n, p := range c.Processors {
		if !strings.Contains(p.APIKey, ":") && p.APIKey != "" {
			p.APIKey = "***"
		}
		r.Processors[n] = p
	}
	return r
}
