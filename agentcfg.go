package main

// The agent layer's settings. Every field has a working default, so `agent: {enabled: true}` is a complete setup;
// anything given in wago.yaml (server defaults) or in a session's config (PUT /api/sessions/{s}) overrides only the
// fields it names. Durations are in milliseconds unless the name says otherwise.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type AgentConfig struct {
	Enabled bool   `json:"enabled"` // turns, the outbox, ownership and context are on for this session
	Mode    string `json:"mode"`    // "live": replies reach WhatsApp; "shadow": everything runs, nothing is sent

	Turn       TurnConfig       `json:"turn"`
	Delivery   DeliveryConfig   `json:"delivery"`
	Ownership  OwnershipConfig  `json:"ownership"`
	Understand UnderstandConfig `json:"understand"`
	Context    ContextConfig    `json:"context"`
	Outbox     OutboxConfig     `json:"outbox"`
}

// TurnConfig: how incoming messages are gathered into one turn for the agent.
type TurnConfig struct {
	QuietMs          int  `json:"quietMs"`          // the turn is ready after this long without a new message
	TypingGraceMs    int  `json:"typingGraceMs"`    // while the person is typing, wait up to this long more
	MaxWaitMs        int  `json:"maxWaitMs"`        // a turn never waits longer than this after its first message
	ReplyTimeoutSec  int  `json:"replyTimeoutSec"`  // a ready turn with no reply in this time expires (turn.expired)
	Groups           bool `json:"groups"`           // turns also in group chats
	MaxAgeSec        int  `json:"maxAgeSec"`        // older messages (delivered after a long offline spell) start no turn
	SupersedeOnReply bool `json:"supersedeOnReply"` // a new message interrupts the reply being delivered
}

// DeliveryConfig: how a reply is shown to the person, so it reads like someone typing, not a burst from a script.
type DeliveryConfig struct {
	Seen           bool    `json:"seen"`           // mark the turn's messages read before answering
	SeenDelayMs    int     `json:"seenDelayMs"`    // pause before the read receipt
	Typing         bool    `json:"typing"`         // show "typing..." before each part
	CharsPerSecond float64 `json:"charsPerSecond"` // typing speed that sets how long "typing..." shows
	MinTypingMs    int     `json:"minTypingMs"`
	MaxTypingMs    int     `json:"maxTypingMs"`
	PauseMs        int     `json:"pauseMs"` // between parts
	Jitter         float64 `json:"jitter"`  // ±fraction of randomness on every pause
}

// OwnershipConfig: a person writing from the phone (or another device) takes the chat over from the agent.
type OwnershipConfig struct {
	HandbackMinutes int    `json:"handbackMinutes"` // the agent gets the chat back after this long without the person writing
	HumanDevices    string `json:"humanDevices"`    // "any": every other device is a person; "phone": only the main phone
	WhileHuman      string `json:"whileHuman"`      // "pause": no turns while a person owns the chat; "observe": turns still come, marked
}

// UnderstandConfig names the processors (from the server's processors block) that turn media into text.
type UnderstandConfig struct {
	Audio     string `json:"audio"`
	Image     string `json:"image"`
	Video     string `json:"video"`
	Document  string `json:"document"`
	TimeoutMs int    `json:"timeoutMs"` // a turn waits at most this long for its media to be understood
}

// ContextConfig: the compact history that rides with every turn.
type ContextConfig struct {
	Messages int `json:"messages"` // at most this many recent messages
	Chars    int `json:"chars"`    // and at most this many characters in all (oldest dropped first)
}

type OutboxConfig struct {
	MaxAttempts int `json:"maxAttempts"` // transient failures are retried this many times, then the item fails
	Parallel    int `json:"parallel"`    // chats delivered at once (each chat is always in order)
}

func defaultAgent() AgentConfig {
	return AgentConfig{
		Mode:       "live",
		Turn:       TurnConfig{QuietMs: 2500, TypingGraceMs: 8000, MaxWaitMs: 30000, ReplyTimeoutSec: 600, MaxAgeSec: 600, SupersedeOnReply: true},
		Delivery:   DeliveryConfig{Seen: true, SeenDelayMs: 1200, Typing: true, CharsPerSecond: 14, MinTypingMs: 900, MaxTypingMs: 9000, PauseMs: 900, Jitter: 0.3},
		Ownership:  OwnershipConfig{HandbackMinutes: 30, HumanDevices: "any", WhileHuman: "pause"},
		Understand: UnderstandConfig{TimeoutMs: 20000},
		Context:    ContextConfig{Messages: 20, Chars: 4000},
		Outbox:     OutboxConfig{MaxAttempts: 8, Parallel: 8},
	}
}

// decodeAgent lays raw JSON over base: fields it does not name keep base's values.
func decodeAgent(raw json.RawMessage, base AgentConfig) (AgentConfig, error) {
	c := base
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return base, httpError{400, "agent: " + err.Error()}
	}
	return c, c.validate()
}

func (c AgentConfig) validate() error {
	bad := func(f string, a ...any) error { return httpError{400, "agent: " + fmt.Sprintf(f, a...)} }
	if c.Mode != "live" && c.Mode != "shadow" {
		return bad("mode is live or shadow, not %q", c.Mode)
	}
	if c.Ownership.HumanDevices != "any" && c.Ownership.HumanDevices != "phone" {
		return bad("ownership.humanDevices is any or phone")
	}
	if c.Ownership.WhileHuman != "pause" && c.Ownership.WhileHuman != "observe" {
		return bad("ownership.whileHuman is pause or observe")
	}
	if c.Turn.QuietMs < 0 || c.Turn.MaxWaitMs < c.Turn.QuietMs {
		return bad("turn.maxWaitMs must be at least turn.quietMs")
	}
	if c.Delivery.CharsPerSecond <= 0 || c.Delivery.Jitter < 0 || c.Delivery.Jitter >= 1 {
		return bad("delivery.charsPerSecond > 0 and 0 <= delivery.jitter < 1")
	}
	if c.Outbox.MaxAttempts < 1 || c.Outbox.Parallel < 1 {
		return bad("outbox.maxAttempts and outbox.parallel are at least 1")
	}
	return nil
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// ---------------------------------------------------------------- policy

// PolicyConfig protects the number. Off unless enabled; once enabled its defaults are the careful ones.
type PolicyConfig struct {
	Enabled bool `json:"enabled"`

	PerMinute        int `json:"perMinute"` // messages the session sends, all chats together
	PerHour          int `json:"perHour"`
	PerDay           int `json:"perDay"`
	PerChatPerMinute int `json:"perChatPerMinute"`
	NewChatsPerDay   int `json:"newChatsPerDay"` // chats where we write first

	ColdOutreach string `json:"coldOutreach"` // "block": never write first to someone who never wrote to us; "allow"

	QuietHours *QuietHours `json:"quietHours,omitempty"` // no sending in this window (replies may be allowed)

	Allow []string `json:"allow,omitempty"` // when set, only these chats (exact ids or prefixes ending in *)
	Block []string `json:"block,omitempty"` // never these

	KillSwitch bool `json:"killSwitch"` // stop every send at once (also POST /api/{s}/policy/kill)
}

type QuietHours struct {
	From         string `json:"from"` // "22:00"
	To           string `json:"to"`   // "08:00"
	Timezone     string `json:"timezone"`
	AllowReplies bool   `json:"allowReplies"` // a reply to someone who wrote in the last 24 hours still goes out
}

func defaultPolicy() PolicyConfig {
	return PolicyConfig{PerMinute: 12, PerHour: 200, PerDay: 1000, PerChatPerMinute: 6, NewChatsPerDay: 20, ColdOutreach: "block"}
}

func decodePolicy(raw json.RawMessage, base PolicyConfig) (PolicyConfig, error) {
	c := base
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return base, httpError{400, "policy: " + err.Error()}
	}
	if c.ColdOutreach != "block" && c.ColdOutreach != "allow" {
		return base, httpError{400, "policy: coldOutreach is block or allow"}
	}
	if q := c.QuietHours; q != nil {
		if _, err := q.window(time.Now()); err != nil {
			return base, httpError{400, "policy.quietHours: " + err.Error()}
		}
	}
	return c, nil
}

// window says whether t falls in quiet hours and, if so, when they end.
func (q *QuietHours) window(t time.Time) (time.Time, error) {
	loc := time.UTC
	if q.Timezone != "" {
		l, err := time.LoadLocation(q.Timezone)
		if err != nil {
			return time.Time{}, err
		}
		loc = l
	}
	parse := func(s string) (int, error) {
		var h, m int
		if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, fmt.Errorf("time %q is not HH:MM", s)
		}
		return h*60 + m, nil
	}
	from, err := parse(q.From)
	if err != nil {
		return time.Time{}, err
	}
	to, err := parse(q.To)
	if err != nil {
		return time.Time{}, err
	}
	lt := t.In(loc)
	now := lt.Hour()*60 + lt.Minute()
	in := from <= now && now < to
	if from > to { // over midnight
		in = now >= from || now < to
	}
	if !in || from == to {
		return time.Time{}, nil
	}
	end := time.Date(lt.Year(), lt.Month(), lt.Day(), to/60, to%60, 0, 0, loc)
	if !end.After(lt) {
		end = end.AddDate(0, 0, 1)
	}
	return end, nil
}

// matches: exact chat id, or a prefix ending in * ("996*" covers every Kyrgyz number).
func matches(list []string, chat string) bool {
	num := strings.SplitN(chat, "@", 2)[0]
	for _, p := range list {
		if strings.HasSuffix(p, "*") {
			pre := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(chat, pre) || strings.HasPrefix(num, pre) {
				return true
			}
		} else if p == chat || p == num {
			return true
		}
	}
	return false
}
