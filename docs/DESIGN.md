# Design notes

Why WaGo is built the way it is: the problem each part solves, what was measured or read, the alternatives, and the
trade-offs. Read this before changing a core part.

## One native process over whatsmeow

**Problem.** The common way to give a program WhatsApp is WAHA or a similar wrapper: a Node.js server around a
WhatsApp engine, usually in Docker. Measured on a paired, idle account, WAHA's container held 394 MiB, about 300 MiB
of it the Node server itself (an unpaired container settles near 300 MiB; capping Node's heap at 128 MB crashes it).
The GOWS engine inside WAHA is a 42 MB Go process; its Node wrapper took 466 MB.

**Decision.** One Go binary over [whatsmeow](https://github.com/tulir/whatsmeow), the library behind WAHA's GOWS
engine and mautrix-whatsapp. It keeps WAHA's REST paths, webhook envelope and ids, so WAHA clients move by changing
a URL. On the same account WaGo runs in about 21 MB.

**Rejected.** Tuning WAHA (the floor is the Node server), WAHA with GOWS (the same Node server around it), lighter
container runtimes (they remove VM overhead, not the process), reading WAHA's own store (its docs warn it can corrupt
history while running).

## The mirror is the store

Every message goes straight into a per-session SQLite file (WAL, FTS5) and WAHA's read endpoints are served from it.

- One writer goroutine groups operations into transactions (up to 5000 rows per commit), so a history sync of tens
  of thousands of messages costs one commit per batch; readers use a separate pool and never wait.
- Reads take milliseconds and work while the session is stopped; full-text search covers all chats (case and
  diacritics folded).
- Media keys are kept per message (a few hundred bytes). The file is downloaded and decrypted on first request, cached
  by content hash, and re-requested from the phone when WhatsApp's servers have dropped it.
- `repair()` runs on every open and is idempotent: it fixes rows older writers got wrong (envelope kinds, control
  messages stored as rows, hidden `@lid` senders), so a mirror inherited from another tool converges.

## Passive by default

WaGo never sends presence on its own, so the phone stays the online device and keeps its notifications. Every action
that changes WhatsApp is gated by the session's `sending` switch, off until the number's owner turns it on. A WAHA
client that re-sends its config on every start does not know the switch, so a config without it keeps the current
value.

## The agent layer lives in the connector

Turns, delivery, ownership, the outbox and the policy could live in each agent. They do not, because each one needs
something only the connector has: the typing indicator (turns), the send path and its errors (outbox, delivery),
messages from other devices (ownership), every message this number sends (policy). An agent that re-implements them
sees half the picture. In the connector they are written and tested once, and every agent, in any language, gets
them through one event and one call.

### A separate store for the agent layer

Turns, the outbox, owners and the record of what this server sent live in `agent.sqlite3`, not in the mirror. The
mirror is history as WhatsApp tells it and can be rebuilt from the phone; the agent store is what this server
decided and promised, and cannot. Keeping them apart means a mirror can be replaced, shared read-only or repaired
without touching promises in flight, and a replay can run its own in-memory agent store against the real mirror.

### One loop over due times

The engine schedules everything (a turn's quiet window, expiry, hand-back, retries, deliveries) as due times checked
by one loop, not as timers. There is nothing to cancel or leak, a restart only needs the stored due times, and the
same code runs on the wall clock, on a replay's scaled clock and in tests. The cost is a resolution of the loop's
period (100 ms), far below anything a person notices.

### Derived message ids

A reply part's WhatsApp id is derived from the outbox item id and the part's index. A retry after a timeout, or after
a crash between "sent" and "recorded", sends the same id again; WhatsApp apps key messages by id, so the repeat is the same message, and
WaGo first checks the mirror for it. This is what makes "uncertain" failures safe to retry and the outbox
exactly-once in practice.

### Errors by what to do about them

WhatsApp errors are sorted by the library's own error values (`errors.Is` on whatsmeow's sentinels and WaGo's HTTP
codes), not by matching message text: `held` (wait for a switch), `transient` (back off and retry), `uncertain`
(retry with the same id), `permanent` (stop). The class is stored on the item, so an operator sees why a message
waits.

### Ownership from devices, not from guesses

A message from our own account that this server did not send was written on another device: the phone, the desktop
app, or another bot. That is a reliable signal (whatsmeow does not echo our own sends back), so the chat goes to a
person without heuristics on content. `humanDevices: phone` narrows it to the main phone for setups with another bot
on a linked device.

### Configuration as layers over defaults

Every agent and policy field has a working default; a block in `wago.yaml` or in a session's config is decoded on
top of the defaults, so it names only what it changes. Unknown fields are errors. The file and the REST API share
the same keys.

### Scaled clocks for replays

A replay runs the real engine on a clock that runs N times faster. The alternative, a discrete-event simulator that
jumps to the next due time, would be exact but cannot include the agent's real latency, which is part of what a
replay should show. The scaled clock keeps the agent real; the trade-off is that scheduling noise is magnified by
the speed, documented in [AGENTS.md](AGENTS.md).

## Testing

- `go test -race -tags sqlite_fts5 .` runs everything on a synthetic mirror (no account needed): the WAHA contract,
  the agent layer stage by stage, config layering, webhooks and signatures, the mirror writer.
- `WAGO_TEST_MIRROR=<copy of a real mirror>` adds checks that need real-world data (repairs, paging and search
  against a real account).
- [`conformance/waha_conformance.py`](../conformance/waha_conformance.py) checks the WAHA contract black-box against
  a running server, WaGo or WAHA itself.
- `-dev-fake` replaces WhatsApp with a recording stand-in for every session, and `/dev/incoming` plays inbound
  traffic, voice notes and typing included, so a service can be tested end to end without a phone.
