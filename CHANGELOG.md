# Changelog

WaGo follows [semantic versioning](https://semver.org). The REST API has its own major version (`GET /api/version`
returns both): within API version 1, routes, fields and events are only added, never removed or renamed. A change
that breaks a client gets a new API version and a note here.

## 0.2.0 - 2026-11-24 (API 1)

The agent layer.

- **Turns.** Incoming messages are gathered per chat into one `turn.ready`, with a quiet window, typing grace, a
  maximum wait, supersede on a new message (unanswered messages carry over), expiry and skip.
- **Human-like delivery.** Read receipts, "typing…" sized to the text, replies in parts with jittered pauses, and a
  stop when the person writes again (`turn.interrupted`).
- **Chat ownership.** A message written on the phone or another device hands the chat to a person (`chat.owner`),
  with hand-back after a quiet period, phone-only detection and an observe mode. Manual takeover and release.
- **Durable outbox.** Every reply is stored before it is sent, retried with the same WhatsApp ids, kept in per-chat
  order, and classified as held, transient, uncertain, permanent or policy. Cancel, retry and inspect over the API.
- **Media understanding.** Voice notes and images become text through named processors: OpenAI-compatible
  transcription or vision, your own HTTP service, or a local command. Cached once per message.
- **Context.** Each turn carries the recent conversation with roles (`user`, `agent`, `human`) within message and
  character budgets.
- **Number policy.** Per-minute, hour, day and per-chat limits, new-chat limits, cold-outreach block, quiet hours,
  allow and block lists, a kill switch, and `policy.warning` on risk signals.
- **Shadow mode, replays and the simulator.** Run the whole layer without sending, replay real history on a scaled
  clock, or play a scripted conversation for an agent's CI.
- **Configuration.** `wago.yaml`, `WAGO_*` environment variables and flags in layers over defaults; secret references
  `env:`, `file:` and `keychain:`; `wago config` prints the effective result. Server-wide webhooks and WAHA's
  `WHATSAPP_HOOK_*` variables.
- `GET /api/contacts` answers for any number, saved or not (WAHA does too), and both contact routes return
  `pushname`, the name a person set for themselves, from the live address book or their latest message.
- Edits, deletes, reactions and other changes made from our own account now emit their events.
- `-dev-fake` gives every session a recording stand-in; `/dev/incoming` plays voice notes, photos, typing and
  messages from the owner's devices.
- WAHA conformance suite and agent examples in Python, TypeScript and MCP.

**Changed:** `-key-service NAME` is replaced by `-api-key keychain:NAME` (or any other secret reference).

## 0.1.0 - 2026-11-24 (API 1)

First release: a WAHA-compatible WhatsApp API server over whatsmeow.

- Sessions, pairing by QR or code, status and lifecycle, multiple accounts.
- A per-session SQLite mirror with full-text search as the store for every read: chats, messages, contacts,
  groups, `@lid` resolution, media on request with a content-addressed cache.
- Sending and actions behind a per-session switch: text, media with previews and voice conversion, location,
  contacts, polls, forward, reactions, edit, delete, read, typing, presence, archive, groups, block, profile.
- WAHA's webhook envelope with HMAC signatures and retries, a WebSocket stream, and a dashboard.
