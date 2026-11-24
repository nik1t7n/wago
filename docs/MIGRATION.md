# Moving a service from WAHA to WaGo

WaGo speaks WAHA's REST API, webhook envelope and ids, so a WAHA client usually needs only a new base URL, a key and
one decision about sending. This guide is what to change, what to check and how to roll back. The full API is in
the [README](../README.md).

## What stays the same

| | |
|---|---|
| Paths | `/api/sessions/...`, `/api/{session}/...`, `/api/sendText` and the other send routes, `/api/files/...`, `/api/server/status`, `/ping`, `/health` (a trailing slash is accepted) |
| Auth | `X-Api-Key` header |
| Session lifecycle | `POST /api/sessions` answers 422 when the session exists, then `PUT` + `POST .../start` work as in WAHA |
| Statuses | `STOPPED`, `STARTING`, `SCAN_QR_CODE`, `WORKING`, `FAILED` |
| QR | `GET /api/{s}/auth/qr?format=raw` gives `{"value": ...}`; 422 outside `SCAN_QR_CODE` |
| Ids | chats `<n>@c.us`, `<id>@g.us`, `@lid`; message ids `<fromMe>_<chat>_<id>[_<participant>]` |
| Webhooks | `{id, timestamp, event, session, metadata, me, payload, engine, environment}`; `customHeaders`, `hmac` (`X-Webhook-Hmac`, sha512), `retries` |
| Message payload | `id`, `from`, `fromMe`, `to`, `participant`, `body`, `timestamp`, `hasMedia`, `media{url, mimetype, filename}`, `ack`, `replyTo` |
| Media | `media.url` points to `/api/files/{s}/{chat}/{id}` and is fetched with the key |

## What is different

1. **Sending is off until the owner turns it on.** Each session has `config.sending`; while it is false every
   action that changes WhatsApp (send, seen, typing, reactions, edits, groups...) answers 403 `sending is off`. A
   config without the field (every WAHA client) keeps the current value, so a service that re-sends its config on
   each start does not switch it off. The owner turns it on in the dashboard settings, or the service's own config
   carries `"sending": true` if the owner decided it should.
2. **A key is always required.** There is no `WAHA_NO_API_KEY`. Give the service the key; a client that adds
   `X-Api-Key` only "when set" must have it set.
3. **One engine.** `engine` in requests is ignored; responses say `GOWS` (WaGo is built on whatsmeow, the library
   behind WAHA's GOWS engine). Code that branches on `NOWEB` must not.
4. **Hidden addresses are resolved more often.** WaGo keeps a `@lid` to phone map and reports a person as
   `<phone>@c.us` once the phone is known, so fewer `@lid` chats reach the service. Keep handling both.
5. **New devices start with a new link.** WAHA's session files cannot be moved. The number's owner links WaGo once
   (QR or pairing code); WaGo then receives the history the phone shares with a new desktop device (about thirty
   thousand messages took about a minute on a real account) into its mirror.
6. **The dashboard has no password.** Sign-in is a one-time link created with the key (`wago open`). On a server,
   reach it through an SSH tunnel; do not expose WaGo to the internet.
7. **Extras the service may use:** reads work while the session is stopped (`/chats`, `/messages`, `/search`),
   full-text search (`GET /api/{s}/search`), old media re-requested from the phone, config changes applied without
   reconnecting, presence never sent by itself.

Not implemented (WAHA Plus or rarely used): channels/newsletters, status (stories) posting, labels, calls control,
apps/chatwoot integrations, S3 media storage, several engines per server.

## Steps for one service

1. **Inventory.** List every WAHA call the service makes (`grep -rn "/api/" <service>`), the webhook events it
   subscribes to, and where it reads `media.url`. Compare with the README tables; anything missing is either
   unused or a WaGo feature to add before the move.
2. **Rehearse against a copy, with nothing sent.** Run a throwaway WaGo in fake mode on another port with a copy of a
   mirror, point the service at it and replay its flow:
   ```bash
   sqlite3 /path/to/mirror.sqlite3 ".backup /tmp/wago-rehearsal/mirror.sqlite3"   # optional: real history to read
   WAGO_API_KEY=rehearsal-key wago -dev-fake -addr 127.0.0.1:3409 -home /tmp/wago-rehearsal/home \
     -mirror /tmp/wago-rehearsal/mirror.sqlite3
   python3 conformance/waha_conformance.py --url http://127.0.0.1:3409 --key rehearsal-key
   ```
   In fake mode every session is `WORKING` at once, sends are recorded locally and reach nobody, webhooks are
   delivered for real (sent messages raise `message.any`), and `POST /api/{s}/dev/incoming` plays inbound messages,
   edits, deletes, receipts, voice notes and typing. Check: session create/update/start codes, 403 before the owner
   turns sending on and 201 after, the webhook headers and payload the service validates, a media download.
3. **Configure the service.** Base URL, the key (an env var or secret store), session name, its webhook URL.
4. **Link.** The owner links the WaGo session (dashboard QR or `POST /api/{s}/auth/request-code`). Only the owner
   scans or types the code.
5. **Turn sending on** if the service sends, as the owner's decision, in the dashboard.
6. **Watch the first real messages**: the webhook arrives, the service answers, the mirror has both sides
   (`GET /api/{s}/chats/{chat}/messages`, or search with `GET /api/{s}/search`).
7. **Retire WAHA** after a few quiet days: stop and remove the container and its volume; the owner removes the old
   linked device on the phone (Settings, Linked devices).

Rollback: point the service back at WAHA's URL and key and start its container; WAHA's own session is still linked
until the owner removes it, so nothing is re-paired.

## A live number without downtime

WhatsApp lets one number have several linked devices, so WaGo can be linked next to WAHA instead of replacing it in
one step. While both are linked, both receive every message; only the one the service points at sends.

1. **Contract on the test environment.** WaGo in `-dev-fake` next to the service's tests: replay every call from the
   inventory and play inbound traffic with `/dev/incoming`. Nothing reaches WhatsApp.
2. **Shadow on the server.** WaGo container beside WAHA on the same private network, sending off, its webhook going
   to a comparison receiver (or nowhere). The owner of the number links it as an extra device (QR from the WaGo
   dashboard through an SSH tunnel, or `request-code`). For a day or more, compare: every message WAHA delivered has
   a WaGo twin with the same id, body, media and ack.
3. **Switch.** Point the service's base URL at WaGo, turn sending on there, keep WAHA linked and running with its
   webhook off. Rollback is pointing the URL back.
4. **Retire** after quiet days: stop WAHA, the number's owner removes its linked device, delete its volume.

Steps 2-4 change production: each needs the owner's word.

## Running on a Linux server

The `Dockerfile` builds on the target (cgo is not cross-compiled): alpine with ffmpeg, user 10001, listens on 3000,
state in the `/data` volume. Settings come from WAHA's own variables where they exist: `WAHA_API_KEY`,
`WAHA_BASE_URL` (base of `media.url`, set it to the address the service uses, e.g. `http://wago:3000`) and
`WHATSAPP_HOOK_*` (server-wide webhook); the rest from `WAGO_*` or a mounted `wago.yaml` ([CONFIG.md](CONFIG.md)).

```yaml
services:
  wago:
    build: ./wago                 # or an image you built from this repository
    environment:
      WAHA_API_KEY: ${WAHA_API_KEY}
      WAHA_BASE_URL: http://wago:3000
      WHATSAPP_HOOK_URL: http://app:8000/webhooks/waha
      WHATSAPP_HOOK_EVENTS: message,message.any,message.ack,session.status
      WHATSAPP_HOOK_HMAC_KEY: ${WAHA_HOOK_HMAC}
    volumes: [wago-data:/data]
    mem_limit: 256m
    networks: [private, egress]  # internet for WhatsApp, the private network for webhooks and file URLs
volumes:
  wago-data:
```

Never publish its port beyond `127.0.0.1` or a private network. The dashboard is reached through an SSH tunnel with
`wago open -addr 127.0.0.1:<tunnel port>` and the server's key in `WAGO_API_KEY`.
