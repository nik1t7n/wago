# Configuration

WaGo runs with no configuration at all: `WAGO_API_KEY=... wago` listens on `127.0.0.1:3000` and keeps its state in
the platform's data directory. Everything else is a layer on top, and later layers win:

```
built-in defaults  <  wago.yaml  <  environment  <  command-line flags
```

`wago config` prints the effective result, with secrets shown only as their references.

## The file

WaGo reads the first of: `-config PATH` (or `WAGO_CONFIG`), `./wago.yaml`, `<user config dir>/wago/wago.yaml`
(`~/.config/wago/wago.yaml` on Linux, `~/Library/Application Support/wago/wago.yaml` on macOS). A misspelt key is an
error, not a silently ignored setting. [`wago.example.yaml`](../wago.example.yaml) lists every field with its
default. The file and the REST API use the same keys, so an `agent` block copied from one works in the other.

## Environment and flags

| Setting | File | Environment | Flag | Default |
|---|---|---|---|---|
| listen address | `listen` | `WAGO_LISTEN` | `-addr` | `127.0.0.1:3000` |
| state directory | `home` | `WAGO_HOME` | `-home` | `~/.local/share/wago`, macOS `~/Library/Application Support/wago` |
| base of media links | `publicUrl` | `WAGO_PUBLIC_URL`, `WAHA_BASE_URL` | `-public-url` | `http://<listen>` |
| API key | `apiKey` | `WAGO_API_KEY`, `WAHA_API_KEY` | `-api-key` | `env:WAGO_API_KEY` |
| linked device name | `deviceName` | `WAGO_DEVICE_NAME` | `-device-name` | `WaGo` |
| whatsmeow log level | `logLevel` | `WAGO_LOG_LEVEL` | `-log` | `WARN` |
| Go heap soft limit, MiB | `memoryMb` | `WAGO_MEMORY_MB` | `-mem` | `96` |
| fake WhatsApp (development) | `devFake` | `WAGO_DEV_FAKE` | `-dev-fake` | `false` |
| mirror of the `default` session | `mirror` | `WAGO_MIRROR` | `-mirror` | inside `home` |
| server-wide webhook | `webhooks` | `WHATSAPP_HOOK_URL`, `_EVENTS`, `_HMAC_KEY`, `_RETRIES_POLICY`, `_RETRIES_DELAY_SECONDS`, `_RETRIES_ATTEMPTS`, `_CUSTOM_HEADERS` | | none |
| media processors | `processors` | | | none |
| agent defaults | `agent` | | | see [AGENT-LAYER.md](AGENT-LAYER.md) |
| policy defaults | `policy` | | | off |

The `WAHA_*` and `WHATSAPP_HOOK_*` names are WAHA's own, so a WAHA deployment's environment works unchanged.

## Secrets

A secret field holds a reference, never the value itself in a file you might commit:

| Reference | Reads |
|---|---|
| `env:NAME` | an environment variable |
| `file:/run/secrets/wago_key` | a file, trailing newline trimmed (Docker and Kubernetes secrets) |
| `keychain:SERVICE` or `keychain:SERVICE/ACCOUNT` | the macOS Keychain; the account defaults to the current user |

A plain string is taken as the value, which is fine for tests and discouraged elsewhere. The same references work
for `processors.*.apiKey`.

## Per session

Each session's config (`PUT /api/sessions/{s}`) can carry its own `agent` and `policy` blocks. They are stored as
given and laid over the server defaults field by field, so `{"agent": {"enabled": true, "turn": {"quietMs": 4000}}}`
changes two fields and keeps the rest. Invalid values are refused with 400 and the reason. Changes apply live,
without reconnecting.

## Files WaGo keeps

```
<home>/
  sessions.json                 the sessions and their configs
  sessions/<name>/
    device.db                   the linked device's keys (keep private: it is the WhatsApp login)
    mirror.sqlite3              history: chats, messages, full-text index, media keys
    agent.sqlite3               turns, outbox, owners, what this server sent, understanding cache
    files/                      downloaded media, by content hash
  web-sessions.json             dashboard sign-in tokens
```

All of it is created with mode 0700/0600. Back up `device.db` to keep the link, `mirror.sqlite3` to keep history
(SQLite's `.backup` works while WaGo runs).
