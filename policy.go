package main

// Number policy. WhatsApp bans numbers that behave like spam: many messages to people who never wrote, bursts no
// person could type, messages that nobody answers. The policy keeps a number inside human-looking limits and
// shows the signals WhatsApp itself is likely to read. It is a guard rail, not a licence: for bulk or marketing
// traffic use the official WhatsApp Business Platform (Cloud API).

import (
	"context"
	"time"
)

// policyGate decides whether a message to chat may go now. hold: when to try again (rate limits, quiet hours);
// err: never (kill switch, block list, cold outreach). Both zero: go.
func (s *Session) policyGate(ctx context.Context, db *agentDB, h history, chat string, now time.Time) (hold time.Time, why string, err error) {
	pol := s.policyCfg()
	if killed(db, pol) {
		return time.Time{}, "", httpError{403, "the kill switch is on: nothing is sent"}
	}
	if !pol.Enabled {
		return time.Time{}, "", nil
	}
	if len(pol.Block) > 0 && matches(pol.Block, chat) {
		return time.Time{}, "", httpError{403, "policy: " + chat + " is on the block list"}
	}
	if len(pol.Allow) > 0 && !matches(pol.Allow, chat) {
		return time.Time{}, "", httpError{403, "policy: " + chat + " is not on the allow list"}
	}
	last := h.lastInbound(ctx, chat, now)
	if last.IsZero() && pol.ColdOutreach == "block" {
		return time.Time{}, "", httpError{403, "policy: " + chat + " never wrote to this number, and coldOutreach is block"}
	}
	if q := pol.QuietHours; q != nil {
		if end, _ := q.window(now); !end.IsZero() && !(q.AllowReplies && !last.IsZero() && now.Sub(last) < 24*time.Hour) {
			return end, "quiet hours until " + end.Format("15:04 MST"), nil
		}
	}
	type limit struct {
		n      int
		window time.Duration
		chat   string
		name   string
	}
	for _, l := range []limit{{pol.PerMinute, time.Minute, "", "perMinute"}, {pol.PerHour, time.Hour, "", "perHour"},
		{pol.PerDay, 24 * time.Hour, "", "perDay"}, {pol.PerChatPerMinute, time.Minute, chat, "perChatPerMinute"}} {
		if l.n > 0 && db.countSent(now.Add(-l.window), l.chat, false) >= l.n {
			return now.Add(l.window / time.Duration(l.n)), "rate limit " + l.name, nil
		}
	}
	if pol.NewChatsPerDay > 0 && h.empty(ctx, chat) && db.countSent(now.Add(-24*time.Hour), "", true) >= pol.NewChatsPerDay {
		return now.Add(time.Hour), "rate limit newChatsPerDay", nil
	}
	return time.Time{}, "", nil
}

func killed(db *agentDB, pol PolicyConfig) bool {
	return pol.KillSwitch || (db != nil && db.get("kill") == "1")
}

// Signals are the numbers that predict trouble with WhatsApp, over the last 24 hours of this server's sends.
type Signals struct {
	Sent24h     int     `json:"sent24h"`
	NewChats24h int     `json:"newChats24h"` // chats where we wrote first
	Chats24h    int     `json:"chats24h"`
	Answered    float64 `json:"answered"` // share of those chats where the person wrote back after our first message
	Failed24h   int     `json:"failed24h"`
	Risk        string  `json:"risk"` // low, elevated, high
	Why         string  `json:"why,omitempty"`
}

func (e *Engine) signals(ctx context.Context, now time.Time) Signals {
	db := e.db
	since := now.Add(-24 * time.Hour)
	sg := Signals{Sent24h: db.countSent(since, "", false), NewChats24h: db.countSent(since, "", true), Risk: "low"}
	rows, err := db.db.Query("SELECT chat, MIN(at) FROM sent WHERE at>=? GROUP BY chat", since.UnixMilli())
	if err == nil {
		answered := 0
		for rows.Next() {
			var chat string
			var first int64
			rows.Scan(&chat, &first)
			sg.Chats24h++
			if e.hist.lastInbound(ctx, chat, now).UnixMilli() > first {
				answered++
			}
		}
		rows.Close()
		if sg.Chats24h > 0 {
			sg.Answered = float64(answered) / float64(sg.Chats24h)
		}
	}
	db.db.QueryRow("SELECT COUNT(*) FROM outbox WHERE state='failed' AND updated>=?", since.UnixMilli()).Scan(&sg.Failed24h)
	pol := e.policy()
	newLimit := max(pol.NewChatsPerDay, 1)
	switch {
	case sg.Chats24h >= 20 && sg.Answered < 0.15:
		sg.Risk, sg.Why = "high", "most people never answer"
	case sg.NewChats24h > 3*newLimit:
		sg.Risk, sg.Why = "high", "many chats started by this number"
	case sg.Chats24h >= 10 && sg.Answered < 0.35:
		sg.Risk, sg.Why = "elevated", "few people answer"
	case sg.NewChats24h > newLimit:
		sg.Risk, sg.Why = "elevated", "more new chats than newChatsPerDay"
	case sg.Failed24h >= 5:
		sg.Risk, sg.Why = "elevated", "repeated failed sends"
	}
	return sg
}

var riskRank = map[string]int{"low": 0, "elevated": 1, "high": 2}

// watchRisk raises policy.warning when the risk goes up (checked at most once a minute).
func (e *Engine) watchRisk(now time.Time) {
	if e.key != "" || now.Sub(e.lastRisk) < time.Minute {
		return
	}
	e.lastRisk = now
	sg := e.signals(context.Background(), now)
	if e.risk != "" && riskRank[sg.Risk] > riskRank[e.risk] {
		e.emit("policy.warning", map[string]any{"signals": sg})
	}
	e.risk = sg.Risk
}
