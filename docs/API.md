# API reference

WaGo speaks WAHA's REST API, webhook envelope and ids, and adds the agent layer (turns, outbox, ownership, context,
policy, replays). This page is the full reference; the agent layer's behaviour is explained in [AGENTS.md](AGENTS.md).

API version: **1** (`GET /api/version` → `{"version": "0.2.0", "api": "1", ...}`). Within a major API version,
fields, routes and events are only added, never removed or renamed. See [CHANGELOG.md](../CHANGELOG.md).

- [Authentication](#authentication)
- [Sessions](#sessions)
- [Reading](#reading)
- [Media](#media)
- [Actions](#actions)
- [Events](#events)
- [Agent layer](#agent-layer)
- [Testing without a phone](#testing-without-a-phone)
- [Errors](#errors)

## Authentication

Every `/api/...` call needs the key: header `X-Api-Key: <key>` (or query `?x-api-key=`). `GET /ping` and
`GET /health` are open. The dashboard uses a cookie instead: `wago open` asks the server for a one-time link
(`POST /api/auth/link` with the key, valid 2 minutes, one use), and the server answers it with an HttpOnly
SameSite=Strict cookie for 30 days. Cookie requests that change something must come from the dashboard's own origin.

## Sessions

A session is one linked WhatsApp account with its own mirror (SQLite history). `default` exists from the start.

| Method and path | What |
|---|---|
| `GET /api/sessions` | all sessions (`?all=true` includes stopped) |
| `POST /api/sessions` | create `{name, start, config}`; 422 if it exists |
| `GET /api/sessions/{s}` | status, `me`, config (HMAC keys shown to API-key callers, `***` on the dashboard) |
| `PUT /api/sessions/{s}` | replace config `{config}`; applied live, the connection is not dropped |
| `DELETE /api/sessions/{s}` | unlink and delete (the mirror file is kept) |
| `POST /api/sessions/{s}/start\|stop\|restart\|logout` | lifecycle; `logout` unlinks the device |
| `GET /api/sessions/{s}/me` | the linked account |
| `GET /api/{s}/auth/qr` | QR as PNG; JSON `{mimetype, data}` with `Accept: application/json`; `?format=raw` gives the code |
| `POST /api/{s}/auth/request-code` | pairing code by phone number `{phoneNumber}` |
| `GET /api/version` (also `/api/server/version`), `/api/server/status` | version and API version; uptime, heap, sessions |

Statuses: `STOPPED`, `STARTING`, `SCAN_QR_CODE`, `WORKING`, `FAILED`. Network-type failures are restarted every 5
minutes; a logout, a ban, a second client or an unscanned QR wait for a person.

Session config:

```json
{
  "sending": false,
  "metadata": {"any": "string"},
  "webhooks": [{
    "url": "http://127.0.0.1:9000/hook",
    "events": ["message", "message.any", "session.status", "turn.ready"],
    "hmac": {"key": "secret"},
    "retries": {"policy": "exponential", "delaySeconds": 2, "attempts": 8},
    "customHeaders": [{"name": "X-Token", "value": "..."}]
  }],
  "agent": {"enabled": true},
  "policy": {"enabled": true},
  "mirror": "/path/only/at/creation.sqlite3"
}
```

- `sending` gates everything that changes something on WhatsApp (see Actions). It is off by default: turning it on is
  the number owner's decision.
- `agent` and `policy` are described in [AGENTS.md](AGENTS.md) and [CONFIG.md](CONFIG.md). Only the fields given
  override the server defaults.
- A config sent without `sending`, `agent` or `policy` (as every WAHA client does on start) keeps their current
  values.
- The mirror path cannot change after creation.

## Reading

All reads come from the mirror: they work while the session is stopped and take milliseconds.

| Method and path | What |
|---|---|
| `GET /api/{s}/chats` | chats, newest first; `limit`, `offset`, `q` (name contains) |
| `GET /api/{s}/chats/overview` | chats with name, picture link and last message; `limit`, `offset` |
| `GET /api/{s}/chats/{chat}/messages` | `limit`, `offset`, `filter.timestamp.gte/lte`, `filter.fromMe`, `sortOrder=asc\|desc` |
| `GET /api/{s}/chats/{chat}/messages/{id}` | one message |
| `GET /api/{s}/search` | full-text search over all chats: `q`, optional `chatId`, `from`, `to` (unix), `rank=true`, `limit` |
| `GET /api/contacts/all?session=`, `/api/contacts?session=&contactId=` | contacts |
| `GET /api/contacts/check-exists?session=&phone=` | is the number on WhatsApp |
| `GET /api/contacts/about`, `/api/contacts/profile-picture` | `?session=&contactId=`; picture cached 24 h, `refresh=true` |
| `GET /api/{s}/chats/{chat}/picture` (link), `/avatar` (image bytes, 204 if none) | |
| `GET /api/{s}/groups`, `/groups/{id}` | groups with participants (5-minute cache, `refresh=true`) |
| `GET /api/{s}/groups/{id}/invite-code` | invite link |
| `GET /api/{s}/lids`, `/lids/{lid}`, `/lids/pn/{pn}` | hidden-address (`@lid`) map |
| `GET /api/files/{s}/{chat}/{id}` | the message's file (see Media) |

Ids:

- Chats are `<number>@c.us` (a person) or `<id>@g.us` (a group). A hidden `@lid` address is stored under the phone
  number once WhatsApp reveals it.
- Messages in responses use WAHA's form `<fromMe>_<chat>_<WhatsApp id>[_<participant>]`; paths accept that or the
  bare WhatsApp id.

A message has WAHA's fields (`id`, `timestamp`, `from`, `fromMe`, `to`, `participant`, `body`, `hasMedia`,
`media{url, mimetype, filename}`, `ack`, `replyTo`, `_data.key{id, remoteJid, remoteJidAlt, fromMe}`,
`_data.notifyName`) plus `chatId`, `kind`, `senderName`, `editedAt`, `revoked`, `reactions{<who>: <emoji>}`.

## Media

The mirror keeps each message's media keys, not the file. `GET /api/files/{s}/{chat}/{id}` downloads and decrypts
it on the first request, then serves it from `<home>/sessions/<s>/files/` (by content hash) with Range support and
a year-long private cache header. WhatsApp keeps files on its servers for a few weeks; for older ones WaGo asks the
phone to upload them again (the phone must be online). Answers: 404 no keys (history that has not synced yet), 503
not connected and not cached, 504 the phone did not answer, 410 the phone no longer has it.

`media.url` in payloads is built on the public URL (`publicUrl`, `WAGO_PUBLIC_URL` or `WAHA_BASE_URL`).

## Actions

All need `sending: true` in the session config (403 otherwise) and a connected session (422 otherwise). Body fields
follow WAHA: `session`, `chatId` (a chat id, a phone number or `+<number>`) and the fields below. Each send answers
201 with the sent message in the same form as reads; it is in the mirror and on the WebSocket at once.

Every send takes `id`, the client's own message id (8-64 letters and digits). WhatsApp uses it, the echo
`message.any` carries it, and a repeat with the same `id` returns the stored message without sending again, so a
client may retry safely after a timeout.

| Method and path | Body |
|---|---|
| `POST /api/sendText`, `POST /api/{s}/chats/{chat}/messages` | `text`, `reply_to` (message id), `mentions` |
| `POST /api/sendImage`, `/sendVideo`, `/sendFile`, `/sendVoice` | `file{mimetype, filename, data (base64) or url}`, `caption`, `reply_to`; voice and non-MP4 video take `convert: true` (any audio becomes an OGG/Opus voice note) |
| `POST /api/sendLocation` | `latitude`, `longitude`, `title`, `address` |
| `POST /api/sendContactVcard` | `contacts[{fullName, phoneNumber, organization}]` or `{vcard}` |
| `POST /api/sendPoll` | `poll{name, options[], multipleAnswers}` |
| `POST /api/forwardMessage` | `messageId` |
| `PUT /api/reaction`, `PUT /api/{s}/chats/{chat}/messages/{id}/reaction` | `messageId`, `reaction` (empty removes) |
| `PUT /api/{s}/chats/{chat}/messages/{id}` | `text`: edit our own message |
| `DELETE /api/{s}/chats/{chat}/messages/{id}` | delete for everyone |
| `POST /api/sendSeen`, `POST /api/{s}/chats/{chat}/messages/read` | `messageIds` or none (all unread since our last message) |
| `POST /api/startTyping`, `/stopTyping`, `POST /api/{s}/presence` | `presence`: `online`, `offline`, `typing`, `recording`, `paused` |
| `POST /api/{s}/chats/{chat}/archive`, `/unarchive` | |
| `POST /api/{s}/groups` | `name`, `participants[{id}]` |
| `POST /api/{s}/groups/{id}/participants/add\|remove\|promote\|demote` | `participants[{id}]` |
| `PUT /api/{s}/groups/{id}/subject`, `/description` | `subject`, `description` |
| `POST /api/{s}/groups/{id}/leave`, `/invite-code/revoke` | |
| `POST /api/contacts/block`, `/unblock` | `contactId` |
| `PUT /api/{s}/profile/status` | `status` |

WaGo never sends presence by itself: the phone stays the online device and keeps its notifications. When the session
has a `policy`, direct sends obey it too (403 for a block, 429 with the time to retry for a limit), and the kill
switch stops every action.

## Events

Webhooks (session config, `wago.yaml`, or WAHA's `WHATSAPP_HOOK_*` variables) and the WebSocket
`GET /ws?session=<s>&events=<a>&events=<b>` carry the same envelope:

```json
{"id": "evt_...", "timestamp": 1795491000000, "event": "message.any", "session": "default",
 "metadata": {}, "me": {"id": "...@c.us", "pushName": "..."}, "payload": {},
 "engine": "GOWS", "environment": {"version": "0.2.0", "engine": "GOWS", "tier": "CORE", "browser": null}}
```

| Event | Payload |
|---|---|
| `message` | an incoming message |
| `message.any` | every message, ours included |
| `message.ack` | one per message: `id`, `from`, `to`, `fromMe`, `ack` (2 delivered, 3 read, 4 played, -1 error), `ackName` |
| `message.reaction`, `message.edited`, `message.revoked` | `reaction{text, messageId}`; `editedMessageId` + `body`; `revokedMessageId`. Raised for changes from the phone and from this API alike |
| `session.status` | `{name, status, reason}` |
| `presence.update`, `group.v2.join`, `group.v2.update`, `call.received` | as in WAHA |
| `turn.ready`, `turn.superseded`, `turn.expired`, `turn.cancelled` | agent layer, below |
| `outbox.sent`, `outbox.held`, `outbox.failed`, `outbox.cancelled`, `outbox.interrupted`, `outbox.shadowed` | agent layer, below |
| `chat.owner`, `understanding.failed`, `policy.warning`, `policy.kill`, `replay.done` | agent layer, below |

Hooks are signed with HMAC-SHA512 of the body in `X-Webhook-Hmac` (`X-Webhook-Hmac-Algorithm: sha512`) when a key is
set, and carry `X-Webhook-Request-Id` and `X-Webhook-Timestamp`. Retries follow the hook's policy, with a queue of
1024 events per hook. The WebSocket accepts only local origins.

## Agent layer

On for a session with `config.agent.enabled: true`. 409 with a message otherwise.

### Turns

| Method and path | What |
|---|---|
| `GET /api/{s}/turns` | newest first; `state`, `chatId`, `limit` |
| `GET /api/{s}/turns/{turn}` | the turn and its replies |
| `POST /api/{s}/turns/{turn}/reply` | answer it: `{parts: [{text} \| {file, kind, caption, convert}], text?, replyTo?, id?}` → 202 (200 for a repeat of the same `id`) |
| `POST /api/{s}/turns/{turn}/skip` | close it without an answer: `{reason?}` |

A turn is `{id, chatId, state, messages[], firstAt, lastAt, readyAt, doneAt, supersededBy, owner, reason}` (times in
unix ms). States: `collecting` → `ready` → `replying` → `done`, or `superseded`, `expired`, `cancelled`.

`turn.ready` carries `{turn, messages: [...the messages, each with "understanding" for media...], context: [...]}`.
A reply to a superseded turn is refused with 409 and `supersededBy`; a reply to a chat a person owns, with 409 and
`owner`.

### Outbox

| Method and path | What |
|---|---|
| `POST /api/{s}/outbox` | queue a message that answers no turn: `{chatId, parts \| text, replyTo?, id?, notBefore? (unix ms)}` → 202 |
| `GET /api/{s}/outbox` | newest first; `state`, `chatId`, `limit` |
| `GET /api/{s}/outbox/{id}` | one item |
| `DELETE /api/{s}/outbox/{id}` | cancel it (or stop it between parts) |

An item is `{id, chatId, turnId, parts, nextPart, state, attempts, nextAt, error, errorClass, sent[], replyTo,
created, updated}`. States: `queued`, `sending`, `held`, `sent`, `shadowed`, `failed`, `cancelled`, `interrupted`.
`errorClass`: `held`, `transient`, `uncertain`, `permanent`, `policy`. `sent` lists the WhatsApp ids of the parts.

### Ownership, context, understanding

| Method and path | What |
|---|---|
| `GET /api/{s}/owners` | chats with an owner record |
| `GET /api/{s}/chats/{chat}/owner` | `{chatId, owner: agent \| human, since, until, reason}` |
| `PUT /api/{s}/chats/{chat}/owner` | `{owner: "human", minutes?, forever?, reason?}` or `{owner: "agent"}` |
| `GET /api/{s}/chats/{chat}/context` | compact history: `messages`, `chars` → `[{id, role, name, at, text}]` |
| `GET /api/{s}/chats/{chat}/messages/{id}/understanding` | the text of a voice note, photo, video or document → `{text}` |

`chat.owner` carries `{chatId, owner, until?, reason}`.

### Policy

| Method and path | What |
|---|---|
| `GET /api/{s}/agent` | effective agent and policy settings, kill switch, counts by state |
| `GET /api/{s}/policy` | policy, kill switch, `signals{sent24h, newChats24h, chats24h, answered, failed24h, risk, why}` |
| `POST /api/{s}/policy/kill` | `{on: true}` stops every send at once, `{on: false}` lets them go |

### Replays and the simulator

| Method and path | What |
|---|---|
| `POST /api/{s}/replays` | `{chatId, since?, until?, limit?, speed?, webhook?}` plays history; `{chatId, script: [{afterMs, text, fromMe?, name?}], speed?}` plays a script → 202 |
| `GET /api/{s}/replays` | replays, newest first |
| `GET /api/{s}/replays/{id}` | `{state, timeline: [{at, kind: inbound \| human \| turn \| agent, text, turnId}], turns, replies}` |
| `DELETE /api/{s}/replays/{id}` | stop it |

A replay's turn and item ids contain the replay id, so the normal reply endpoint routes to it. Its events carry
`"replay": "<id>"` in the payload, and go only to `webhook` when one is given.

## Testing without a phone

Run WaGo with `-dev-fake` (or `devFake: true`): every session is `WORKING` at once on a recording stand-in, sends
reach nobody, webhooks are delivered for real. `POST /api/{s}/dev/incoming` plays inbound traffic through the same
handler as WhatsApp's; it answers 404 outside fake mode.

| Body | Plays |
|---|---|
| `{chatId, text, id?, pushName?, participant?}` | an incoming message (`participant` in a group) |
| `{chatId, file: {mimetype, data, filename?}, text?}` | an incoming voice note, photo, video or document (`text` is the caption) |
| `{chatId, text, fromMe: true, device?}` | a message the owner wrote on their phone (`device` 0) or another linked device |
| `{chatId, edit: <id>, text}` | an edit |
| `{chatId, revoke: <id>}` | a delete for everyone |
| `{chatId, ack: "delivered" \| "read" \| "played", ackIds: [...]}` | receipts for our messages |
| `{chatId, typing: true \| false}` | the person starts or stops typing |

## Errors

Errors are `{"statusCode": <code>, "message": "..."}` (409s may add fields such as `supersededBy`). 400 bad input,
401 no or wrong key, 403 sending off, kill switch or policy, 404 not found, 409 the agent layer's state forbids it,
422 not connected or a WAHA-style conflict, 429 a policy limit, 502 WhatsApp refused, 503 not connected and needed.
