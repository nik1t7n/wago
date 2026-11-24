<h1 align="center">WaGo</h1>

<p align="center"><b>WhatsApp for AI agents, in one 21 MB binary.</b></p>

<p align="center">
A WAHA-compatible WhatsApp API server written in Go, with the conversation layer agents actually need built in:<br>
turns, human-like delivery, human takeover, a durable outbox, voice-to-text, number protection and replays.
</p>

<p align="center">
<a href="#quickstart">Quickstart</a> ·
<a href="docs/AGENTS.md">Agent layer</a> ·
<a href="docs/API.md">API</a> ·
<a href="docs/CONFIG.md">Configuration</a> ·
<a href="docs/MIGRATION.md">From WAHA</a> ·
<a href="examples">Examples</a>
</p>

<p align="center">
<a href="https://github.com/nik1t7n/wago/actions/workflows/ci.yml"><img src="https://github.com/nik1t7n/wago/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
<img src="https://img.shields.io/badge/go-1.27-00ADD8" alt="Go 1.27">
<img src="https://img.shields.io/badge/API-v1-0ea5e9" alt="API v1">
<img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="Apache-2.0">
</p>

<p align="center"><img src="docs/assets/hero.svg" alt="WhatsApp on a phone linked to WaGo, which serves a WAHA-compatible API, an agent layer and a SQLite mirror to your agent" width="900"></p>

## Why WaGo

Connecting a language model to WhatsApp takes an afternoon. Making it behave well in real chats is where the months
go: people send three short messages instead of one, record voice notes, keep typing while the bot answers, and the
owner jumps into some chats from their own phone. WaGo solves that once, inside the connector, so your agent gets
**one clean event per turn** and answers with **one call**, in any language and with any model.

| | |
|---|---|
| 🪶 **Tiny and native** | One Go process over [whatsmeow](https://github.com/tulir/whatsmeow). About **21 MB of RAM** on a paired account where a WAHA container took **394 MiB**. No Node, no browser, no Docker required. |
| 🔌 **WAHA-compatible** | Same REST paths, webhook envelope, ids and environment variables. Existing WAHA clients move by changing a URL, and a black-box [conformance suite](conformance) proves it. |
| 💬 **Turns, not messages** | Bursts of messages, voice notes and typing are gathered into one `turn.ready`. If the person writes again, the turn is superseded and the agent answers everything at once. |
| ✍️ **Delivery that reads like a person** | Marked read, "typing…" for as long as the text would take, the answer in parts with natural pauses, and it stops mid-answer if the person writes again. |
| 🙋 **Humans first** | Answer a chat from your phone and the bot steps back on its own, then returns after a quiet period. No flags to set, no keywords. |
| 📮 **Durable outbox** | Every reply survives restarts, retries with the same WhatsApp id (no duplicates), keeps per-chat order, and tells you why anything waits. |
| 🎙️ **Voice and photos as text** | Plug in Whisper, a vision model, your own HTTP service or a local command. Agents get text, cached once per message. |
| 🛡️ **Number protection** | Rate limits, new-chat limits, cold-outreach block, quiet hours, allow and block lists, a kill switch, and early warnings when people start blocking you. |
| 🎬 **Shadow mode and replays** | Run a new agent silently on live traffic, replay a real day of chats at 100x, or script a conversation for your agent's CI. |
| 🔎 **Full history, instantly** | Every message lands in a local SQLite mirror with full-text search. Reads take milliseconds and work while offline. |
| 🧰 **Yours to shape** | Every behaviour has a working default and can be overridden per server, per session and live over the API. |

## Quickstart

### 1. Run it

**Docker** (any Linux server):

```bash
git clone https://github.com/nik1t7n/wago && cd wago
docker build -t wago .
export WAGO_API_KEY=$(openssl rand -hex 24)
docker run -d --name wago -p 127.0.0.1:3000:3000 -e WAGO_API_KEY -v wago-data:/data wago
```

**Binary** (macOS or Linux, Go 1.27 and a C compiler):

```bash
CGO_ENABLED=1 go install -tags sqlite_fts5 github.com/nik1t7n/wago@latest
export WAGO_API_KEY=$(openssl rand -hex 24)
wago
```

WaGo listens on `127.0.0.1:3000` and keeps its state in the platform's data directory. `wago config` shows the
effective configuration; [CONFIG.md](docs/CONFIG.md) covers the file, environment variables, flags and secret
references (`env:`, `file:`, macOS `keychain:`).

### 2. Link your phone

```bash
wago open                                   # opens the dashboard, signed in; scan the QR there
```

or over the API: `POST /api/sessions/default/start`, then `GET /api/default/auth/qr` (a PNG) or
`POST /api/default/auth/request-code` for a pairing code. In WhatsApp: **Settings → Linked devices → Link a device**.
History starts flowing into the mirror right away.

### 3. Your first agent

```bash
python3 examples/python-webhook/agent.py    # listens on :8080; set OPENAI_API_KEY for a real model
```

```bash
curl -X PUT localhost:3000/api/sessions/default -H "X-Api-Key: $WAGO_API_KEY" -H 'Content-Type: application/json' \
  -d '{"config": {"sending": true, "agent": {"enabled": true},
       "webhooks": [{"url": "http://127.0.0.1:8080/", "events": ["turn.ready"]}]}}'
```

Write to the number from another phone: you will see it read, "typing…", and the answer arrive in parts. Sending
is off until you turn it on, so nothing leaves the number by accident.

> **No phone at hand?** Start with `wago -dev-fake`: every session runs on a recording stand-in and
> `POST /api/{session}/dev/incoming` plays messages, voice notes and typing. The whole loop works, nothing reaches
> WhatsApp.

## How a turn works

<p align="center"><img src="docs/assets/turn.svg" alt="Timeline of one turn: messages gather, the turn is ready after a quiet window, the agent replies, WaGo marks read, types and sends the parts" width="900"></p>

Your agent receives:

```json
{
  "event": "turn.ready",
  "payload": {
    "turn": {"id": "t_3f1c…", "chatId": "15551234567@c.us", "owner": "agent"},
    "context": [
      {"role": "user",  "name": "Ann", "text": "do you deliver on sunday?"},
      {"role": "agent", "text": "Yes, from 10 to 18."},
      {"role": "user",  "text": "hi"},
      {"role": "user",  "text": "[audio] can I pay by card?"}
    ]
  }
}
```

and answers:

```http
POST /api/default/turns/t_3f1c…/reply
{"parts": [{"text": "Hi Ann!"}, {"text": "Yes, card or cash on delivery."}]}
```

Everything else (timing, read receipts, typing, retries, ordering, takeover, limits) is WaGo's job. A `409` means
the person wrote again or a human took over: do nothing, the next turn is on its way. The full guide is
[AGENTS.md](docs/AGENTS.md).

## Connect any agent

| Path | For | Example |
|---|---|---|
| Webhook + REST | bots and backends in any language | [Python](examples/python-webhook/agent.py) · [TypeScript](examples/typescript-webhook/agent.ts) |
| MCP | Claude, Codex, Cursor and any MCP client | [MCP server](examples/mcp-python/server.py) |
| WAHA clients | anything already written for WAHA | change the base URL, see [MIGRATION.md](docs/MIGRATION.md) |
| WebSocket | dashboards and live views | `GET /ws` |

Each example is one file with no dependencies beyond the standard library (the MCP one needs the official `mcp`
package) and works with any OpenAI-compatible model.

## Make it yours

Every behaviour has a working default, and each layer overrides only what it names:

```
built-in defaults  <  wago.yaml  <  environment  <  flags  <  per-session config (live, over the API)
```

```yaml
# wago.yaml
processors:
  whisper: {type: transcribe, url: https://api.openai.com/v1, model: whisper-1, apiKey: env:OPENAI_API_KEY}
agent:
  turn: {quietMs: 4000}
  delivery: {charsPerSecond: 18}
  ownership: {handbackMinutes: 60, humanDevices: phone}
  understand: {audio: whisper}
policy:
  enabled: true
  quietHours: {from: "22:00", to: "08:00", timezone: Europe/Berlin, allowReplies: true}
```

[`wago.example.yaml`](wago.example.yaml) lists every field. Media processors can be any OpenAI-compatible endpoint,
your own HTTP service or a local command such as `whisper.cpp`, so audio and images can stay on your machine.

## Compared

| | WaGo | WAHA | WhatsApp Cloud API |
|---|---|---|---|
| What it connects | your WhatsApp or WhatsApp Business app number, as a linked device | the same, as a linked device | a number registered on Meta's Business Platform |
| Runs as | one Go binary, ~21 MB RAM | Node.js server in Docker, 394 MiB measured | hosted by Meta |
| Message history | full local mirror with full-text search | depends on the engine | not kept, store webhooks yourself |
| Agent layer | built in | none | none |
| First message to a new contact | allowed, limited by your policy | allowed | approved templates only |
| Official | no | no | yes |

## Risks, stated plainly

WaGo, like WAHA and every library of this kind, is an **unofficial client**: it links to an account the way WhatsApp
Web does, but WhatsApp's terms do not permit automated or bulk use, and accounts that look like spam get banned. WaGo
is built to behave like a careful person and its policy defaults are conservative, but no software can guarantee an
account. Use it for your own number and for people who write to you. **For mass messaging, marketing or anything at
scale, use the official [WhatsApp Business Platform (Cloud API)](https://developers.facebook.com/docs/whatsapp/cloud-api).**

## Security

- Loopback by default. Put WaGo behind a reverse proxy or a private network before exposing it.
- Every `/api` call needs the key. Keys are read from references (`env:`, `file:`, `keychain:`), never written to
  logs or config dumps.
- Webhooks are signed with HMAC-SHA512, as in WAHA.
- State is created with mode 0700/0600. `device.db` is the WhatsApp login: keep it private and backed up.
- Sending is off for every new session until the number's owner turns it on.

Report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## Documentation

| | |
|---|---|
| [AGENTS.md](docs/AGENTS.md) | the agent layer: turns, delivery, ownership, outbox, understanding, policy, replays |
| [API.md](docs/API.md) | every route, event and error |
| [CONFIG.md](docs/CONFIG.md) | file, environment, flags, secrets, files on disk |
| [MIGRATION.md](docs/MIGRATION.md) | moving a service from WAHA |
| [DESIGN.md](docs/DESIGN.md) | why it is built this way, with measurements |
| [CHANGELOG.md](CHANGELOG.md) | versions and API compatibility |

## Development

```bash
go test -race -tags sqlite_fts5 .                        # the whole suite, on a synthetic mirror, no account needed
go build -tags sqlite_fts5 -o wago . && ./wago -dev-fake &
python3 conformance/waha_conformance.py --url http://127.0.0.1:3000 --key "$WAGO_API_KEY"
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Roadmap

- MCP server built into WaGo, no separate process.
- WhatsApp Cloud API as a second transport behind the same agent layer, so an agent can move to the official API
  without a rewrite.

## License

[Apache-2.0](LICENSE). WaGo is not affiliated with, endorsed by or connected to WhatsApp or Meta. "WhatsApp" is a
trademark of its owner.
