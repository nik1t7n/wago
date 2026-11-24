"""WaGo as MCP tools: let Claude, Codex, Cursor or any MCP client read and answer WhatsApp through your WaGo.

    pip install mcp            # the official MCP Python SDK
    WAGO_URL=http://127.0.0.1:3000 WAGO_API_KEY=... python3 server.py      # stdio server

Claude Code:   claude mcp add wago -e WAGO_URL=... -e WAGO_API_KEY=... -- python3 /path/to/server.py
Claude Desktop / Cursor: {"mcpServers": {"wago": {"command": "python3", "args": ["/path/to/server.py"],
                          "env": {"WAGO_URL": "...", "WAGO_API_KEY": "..."}}}}

Reading tools work on any session. Answering goes through WaGo's agent layer (turns and the outbox), so the
number policy, human-like delivery and human takeover apply to the model's messages too. Set WAGO_READ_ONLY=1 to
expose only the reading tools.
"""

import json
import os
import urllib.parse
import urllib.request

from mcp.server.fastmcp import FastMCP

WAGO = os.environ.get("WAGO_URL", "http://127.0.0.1:3000").rstrip("/")
KEY = os.environ.get("WAGO_API_KEY", "")
SESSION = os.environ.get("WAGO_SESSION", "default")
READ_ONLY = os.environ.get("WAGO_READ_ONLY") == "1"

mcp = FastMCP("wago")


def api(method: str, path: str, body=None, **query):
    q = {k: v for k, v in query.items() if v not in (None, "")}
    url = WAGO + path + ("?" + urllib.parse.urlencode(q) if q else "")
    req = urllib.request.Request(url, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("X-Api-Key", KEY)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return json.loads(resp.read() or b"null")
    except urllib.error.HTTPError as e:
        return {"error": e.code, "message": json.loads(e.read() or b"{}").get("message", "")}


@mcp.tool()
def list_chats(query: str = "", limit: int = 20) -> list:
    """Recent WhatsApp chats, newest first, with the last message. `query` filters by name."""
    chats = api("GET", f"/api/{SESSION}/chats/overview", limit=limit)
    if query and isinstance(chats, list):
        chats = [c for c in chats if query.lower() in (c.get("name") or "").lower()]
    return chats


@mcp.tool()
def chat_context(chat_id: str, messages: int = 30) -> list:
    """The chat's recent messages, oldest first, as compact lines (voice notes and photos already as text)."""
    return api("GET", f"/api/{SESSION}/chats/{urllib.parse.quote(chat_id)}/context", messages=messages, chars=12000)


@mcp.tool()
def search(query: str, chat_id: str = "", limit: int = 20) -> list:
    """Full-text search over every chat's history."""
    return api("GET", f"/api/{SESSION}/search", q=query, chatId=chat_id, limit=limit)


@mcp.tool()
def waiting_turns(limit: int = 20) -> list:
    """Turns waiting for an answer: a person wrote and nobody has replied yet."""
    return api("GET", f"/api/{SESSION}/turns", state="ready", limit=limit)


if not READ_ONLY:

    @mcp.tool()
    def reply(turn_id: str, parts: list[str]) -> dict:
        """Answer a waiting turn. Each part is one WhatsApp message, delivered with typing pauses like a person.
        Refused (409) when the person has written again since; answer the newer turn instead."""
        return api("POST", f"/api/{SESSION}/turns/{turn_id}/reply", {"parts": [{"text": p} for p in parts]})

    @mcp.tool()
    def hand_to_human(chat_id: str, reason: str = "") -> dict:
        """Give the chat to a person: the agent stops answering it until they hand it back or go quiet."""
        return api("PUT", f"/api/{SESSION}/chats/{urllib.parse.quote(chat_id)}/owner", {"owner": "human", "reason": reason})


if __name__ == "__main__":
    mcp.run()
