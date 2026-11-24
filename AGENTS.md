# WaGo for AI agents

You are an AI agent reading this repository. This file tells you what WaGo is, how to set it up and use it for your
user, how to connect an agent to it, the rules you must follow, and how to work on its code. Read it fully before
you act. The human docs are at <https://nik1t7n.github.io/wago/>; a compact index for models is
[`llms.txt`](llms.txt).

## What WaGo is

WaGo is an agent-first WhatsApp API server: one Go binary (about 21 MB of RAM) that links to a WhatsApp account as a
linked device, keeps every message in a local SQLite mirror, and serves a REST API, signed webhooks and a WebSocket.
Its agent layer does the conversation work for you:

- incoming messages from one chat are gathered into a **turn** and announced once as `turn.ready`, with the recent
  conversation as `context` (voice notes and photos already turned into text);
- you answer with **one call**, `POST /api/{session}/turns/{turn}/reply`, and WaGo delivers it like a person: read
  receipt, "typing…", the answer in parts, pauses, retries with the same ids, per-chat order;
- when a human answers a chat from their phone, the chat becomes theirs and your replies are refused (409) until
  it comes back;
- a policy keeps the number within human limits.

So your job as an agent on WaGo is only to decide **what to say**. Never re-implement batching, typing, retries or
takeover on your side.

## Rules (always)

1. **Sending is the number owner's decision.** A new session has `sending: false`. Do not turn it on, and do not send
   a message to a real person, unless your user explicitly asked for it in this conversation.
2. **Only the number's owner links the phone.** Show them the QR or pairing code; never try to scan or enter it for
   them.
3. **Secrets stay secret.** Read the API key from the environment or a secret reference; send it as the `X-Api-Key`
   header; never print it, log it, commit it or put it in a URL. `device.db` under WaGo's home is the WhatsApp login
   itself: never read it, copy it or show it.
4. **Test without WhatsApp first.** Use `wago -dev-fake` and `POST /api/{s}/dev/incoming` to prove a setup end to
   end. Nothing reaches WhatsApp in that mode.
5. **Answer turns, do not push messages.** Reply to `turn.ready`. Writing first to people who never wrote is blocked
   by default (`coldOutreach: block`) and is how numbers get banned. For marketing or bulk messaging, recommend the
   official WhatsApp Cloud API instead.
6. **A 409 is not an error to fight.** It means the person wrote again (a newer turn is coming) or a human took the
   chat. Drop that reply; do not retry it.

## Set it up for a user

```bash
# build (Go 1.27 and a C compiler), or use the Dockerfile
CGO_ENABLED=1 go install -tags sqlite_fts5 github.com/nik1t7n/wago@latest

# a key for the API (keep it in the user's secret store, not in a file you commit)
export WAGO_API_KEY=$(openssl rand -hex 24)

# a safe first run: no WhatsApp at all
wago -dev-fake &                     # listens on 127.0.0.1:3000
curl -s localhost:3000/health        # {"status":"ok"}
wago config                          # the effective configuration, secrets hidden
```

Configuration is layered: built-in defaults < `wago.yaml` < `WAGO_*` environment < flags < per-session config over
the API. Every field and its default: [`wago.example.yaml`](wago.example.yaml) and [docs/CONFIG.md](docs/CONFIG.md).
Secret fields take references: `env:NAME`, `file:/path`, `keychain:SERVICE` (macOS).

When the user is ready for the real account, run without `-dev-fake` and let **them** link it: `wago open` opens the
dashboard with the QR, or `POST /api/sessions/default/start` then `GET /api/default/auth/qr`, or
`POST /api/default/auth/request-code {"phoneNumber": "..."}` for a code. Wait for `GET /api/sessions/default` to say
`"status": "WORKING"`.

## Connect an agent

All calls carry `-H "X-Api-Key: $WAGO_API_KEY"`. The session is `default` unless the user made others.

```bash
# turn the agent layer on and point turns at your webhook
curl -X PUT localhost:3000/api/sessions/default -H "X-Api-Key: $WAGO_API_KEY" -H 'Content-Type: application/json' \
  -d '{"config": {"agent": {"enabled": true},
       "webhooks": [{"url": "http://127.0.0.1:8080/", "events": ["turn.ready"], "hmac": {"key": "<a secret>"}}]}}'
```

Your webhook receives (verify `X-Webhook-Hmac`, HMAC-SHA512 of the raw body, then answer 200 at once and think in the
background):

```json
{"event": "turn.ready", "session": "default", "payload": {
  "turn": {"id": "t_…", "chatId": "15551234567@c.us", "state": "ready", "owner": "agent"},
  "messages": [{"id": "false_15551234567@c.us_3EB0…", "body": "hi"}],
  "context": [{"role": "user", "name": "Ann", "at": 1795498000, "text": "hi"}]}}
```

`context[].role` is `user` (the person), `agent` (you, earlier) or `human` (the owner answering from a device). Reply:

```bash
curl -X POST localhost:3000/api/default/turns/$TURN/reply -H "X-Api-Key: $WAGO_API_KEY" \
  -H 'Content-Type: application/json' -d '{"parts": [{"text": "Hi Ann!"}, {"text": "Yes, we are open till 8."}]}'
# 202 accepted · 409 superseded or human-owned: drop it · 403 sending off or blocked by policy · 429 a limit: WaGo retries
```

Prefer no webhook? Poll `GET /api/default/turns?state=ready`. Prefer tools? Run the MCP server in
[`examples/mcp-python`](examples/mcp-python/server.py) and use `list_chats`, `chat_context`, `search`,
`waiting_turns`, `reply`, `hand_to_human`. Complete agents in one file: [Python](examples/python-webhook/agent.py),
[TypeScript](examples/typescript-webhook/agent.ts).

### Prove it works without a phone

On `-dev-fake` nothing reaches WhatsApp, so for this test add `"sending": true` to the config above (on a real
account only the owner decides that), then play a message:

```bash
curl -X POST localhost:3000/api/default/dev/incoming -H "X-Api-Key: $WAGO_API_KEY" -H 'Content-Type: application/json' \
  -d '{"chatId": "15551234567@c.us", "text": "hi, open today?", "pushName": "Ann"}'
# ~2.5 s later your webhook gets turn.ready; after you reply:
curl -s "localhost:3000/api/default/outbox?limit=1" -H "X-Api-Key: $WAGO_API_KEY"     # "sending", then "sent", part by part
```

`dev/incoming` also plays voice notes and photos (`file: {mimetype, data}`), typing (`typing: true`) and the owner
answering from the phone (`fromMe: true`), so you can test batching, interruption and takeover. For a whole
conversation in seconds, use the simulator: `POST /api/default/replays {"chatId": "...", "script": [{"afterMs": 0,
"text": "hi"}, {"afterMs": 800, "text": "open today?"}], "speed": 100}`.

## Useful calls

| Need | Call |
|---|---|
| Is it up and linked? | `GET /health`, `GET /api/sessions/default` → `status` |
| Chats, newest first | `GET /api/default/chats/overview?limit=20` |
| A chat's messages | `GET /api/default/chats/{chat}/messages?limit=50` |
| Search everything | `GET /api/default/search?q=invoice` |
| Who is this person | `GET /api/contacts?session=default&contactId={chat}` → `name`, `pushname` |
| Compact history for a prompt | `GET /api/default/chats/{chat}/context?messages=50` |
| Text of a voice note or photo | `GET /api/default/chats/{chat}/messages/{id}/understanding` |
| Hand a chat to a person | `PUT /api/default/chats/{chat}/owner {"owner": "human", "minutes": 120}` |
| Why is a reply waiting | `GET /api/default/outbox?state=held` → `error`, `errorClass` |
| Stop everything now | `POST /api/default/policy/kill {"on": true}` |
| Run silently next to humans | session config `{"agent": {"mode": "shadow"}}` |

Every route, event and error: [docs/API.md](docs/API.md). Behaviour of turns, delivery, ownership, outbox, media
understanding, policy and replays: [docs/AGENT-LAYER.md](docs/AGENT-LAYER.md).

## Troubleshooting

| Symptom | Meaning and fix |
|---|---|
| 401 | missing or wrong `X-Api-Key` |
| 403 on send | `sending` is off (the owner must turn it on), the kill switch is on, or the policy blocked the chat |
| 409 on reply | superseded by a newer turn, or a human owns the chat: drop the reply |
| 409 on turns routes | the agent layer is off: `config.agent.enabled: true` |
| 422 / 503 | the session is not `WORKING`: check `GET /api/sessions/{s}`; `SCAN_QR_CODE` needs the owner |
| no `turn.ready` | agent layer off, the webhook lacks `turn.ready`, the chat is a group (`turn.groups: false`), or a human owns it |
| reply `held` | a policy limit or quiet hours: it goes out when allowed; see `error` on the item |

## Working on WaGo's code

```bash
go vet -tags sqlite_fts5 .
go test -race -tags sqlite_fts5 .                  # the whole suite, on a synthetic mirror; no account needed
go build -tags sqlite_fts5 -o wago .
WAGO_API_KEY=dev ./wago -dev-fake -home /tmp/wago-dev -addr 127.0.0.1:3999 &
python3 conformance/waha_conformance.py --url http://127.0.0.1:3999 --key dev
```

- One Go package in the root. Agent layer: `engine.go` (turns, delivery, outbox), `agent.go` (routes), `agentcfg.go`
  (config and defaults), `understand.go`, `policy.go`, `replay.go`. WhatsApp side: `session.go`, `send.go`,
  `convert.go`, `mirror.go`, `store.go`, `api.go`.
- Test agent-layer behaviour on the engine's clock (see `agent_test.go`), not with sleeps.
- Classify WhatsApp errors with `errors.Is` on whatsmeow's sentinels, never by message text.
- New behaviour gets a working default and a field in the `agent` or `policy` block, documented in
  `wago.example.yaml` and the docs. Within API version 1, add routes and fields; never rename or remove them.
- Keep the WAHA-compatible paths, ids and webhook envelope; the conformance suite must pass.
- Record design decisions with their trade-offs in [docs/DESIGN.md](docs/DESIGN.md), and user-visible changes in
  [CHANGELOG.md](CHANGELOG.md).
- Never commit real phone numbers, keys, session files or mirrors.
