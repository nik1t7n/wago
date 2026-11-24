# Examples

Complete agents in one file each. They use only the standard library (the MCP server needs the official `mcp`
package) and any OpenAI-compatible model; without a model key they answer with a fixed text, enough to see the
whole loop.

| Example | Run |
|---|---|
| [python-webhook](python-webhook/agent.py) | `python3 agent.py` (Python 3.9+) |
| [typescript-webhook](typescript-webhook/agent.ts) | `node agent.ts` (Node 22.18+, runs TypeScript directly) |
| [mcp-python](mcp-python/server.py) | `pip install mcp && python3 server.py`, then add it to Claude, Codex or Cursor |

The webhook agents read `WAGO_URL`, `WAGO_API_KEY`, `WAGO_HOOK_SECRET`, `OPENAI_BASE_URL`, `OPENAI_API_KEY` and
`OPENAI_MODEL`. The header of each file shows how to point a session at it.

To try them without a phone, run `wago -dev-fake` and play a message:

```bash
curl -X POST localhost:3000/api/default/dev/incoming -H "X-Api-Key: $WAGO_API_KEY" \
  -H 'Content-Type: application/json' -d '{"chatId": "15551234567@c.us", "text": "hi, open today?"}'
```

The agent receives the turn about 2.5 s later and its reply shows up in the fake session's outbox
(`GET /api/default/outbox`).
