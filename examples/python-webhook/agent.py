"""A complete WhatsApp agent on WaGo in one file, Python standard library only.

WaGo does the WhatsApp side: it gathers a person's messages into a turn, transcribes voice notes, sends the
recent history as context, marks messages read, shows "typing...", splits the answer into parts, stops if the
person writes again, and hands the chat to a human who answers from the phone. This file only decides what to say.

    export WAGO_URL=http://127.0.0.1:3000 WAGO_API_KEY=...       # your WaGo
    export WAGO_HOOK_SECRET=...                                  # the hook's hmac key (optional but recommended)
    export OPENAI_BASE_URL=https://api.openai.com/v1 OPENAI_API_KEY=... OPENAI_MODEL=gpt-4o-mini   # any compatible API
    python3 agent.py                                             # listens on :8080 for WaGo's webhook

Then point the session at it (once):

    curl -X PUT $WAGO_URL/api/sessions/default -H "X-Api-Key: $WAGO_API_KEY" -H 'Content-Type: application/json' \
      -d '{"config": {"agent": {"enabled": true}, "webhooks": [{"url": "http://127.0.0.1:8080/",
           "events": ["turn.ready"], "hmac": {"key": "'$WAGO_HOOK_SECRET'"}}]}}'

Without OPENAI_API_KEY the agent answers with a fixed text, which is enough to see the whole loop work.
"""

import hashlib
import hmac
import json
import os
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

WAGO = os.environ.get("WAGO_URL", "http://127.0.0.1:3000").rstrip("/")
KEY = os.environ.get("WAGO_API_KEY", "")
SECRET = os.environ.get("WAGO_HOOK_SECRET", "")
LLM_URL = os.environ.get("OPENAI_BASE_URL", "https://api.openai.com/v1").rstrip("/")
LLM_KEY = os.environ.get("OPENAI_API_KEY", "")
LLM_MODEL = os.environ.get("OPENAI_MODEL", "gpt-4o-mini")
PORT = int(os.environ.get("PORT", "8080"))

SYSTEM = (
    "You are the assistant of a small business on WhatsApp. Answer briefly and warmly, like a person typing on a "
    "phone. Use the language of the customer. If you cannot help, say a colleague will answer soon."
)


def call(method, url, body=None, headers=None):
    req = urllib.request.Request(url, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=60) as resp:
        return resp.status, json.loads(resp.read() or b"null")


def think(context):
    """The agent's brain: the chat so far in, the answer's parts out. Replace with your own."""
    if not LLM_KEY:
        return ["Thanks for your message!", "We'll get back to you shortly."]
    roles = {"user": "user", "agent": "assistant", "human": "assistant"}
    messages = [{"role": "system", "content": SYSTEM}]
    messages += [{"role": roles[line["role"]], "content": line["text"]} for line in context]
    _, out = call("POST", LLM_URL + "/chat/completions", {"model": LLM_MODEL, "messages": messages},
                  {"Authorization": "Bearer " + LLM_KEY})
    text = out["choices"][0]["message"]["content"].strip()
    # one paragraph per WhatsApp message reads more naturally than one long block
    return [p.strip() for p in text.split("\n\n") if p.strip()][:4]


def on_turn(session, payload):
    turn = payload["turn"]
    parts = think(payload["context"])
    try:
        call("POST", f"{WAGO}/api/{session}/turns/{turn['id']}/reply",
             {"parts": [{"text": p} for p in parts]}, {"X-Api-Key": KEY})
    except urllib.error.HTTPError as e:
        if e.code == 409:  # the person wrote again (a newer turn is coming) or a human took the chat
            return
        raise


class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if SECRET:
            want = hmac.new(SECRET.encode(), body, hashlib.sha512).hexdigest()
            if not hmac.compare_digest(want, self.headers.get("X-Webhook-Hmac", "")):
                self.send_response(401)
                self.end_headers()
                return
        self.send_response(200)
        self.end_headers()
        event = json.loads(body)
        if event["event"] == "turn.ready":
            # answer in the background: WaGo's webhook must not wait for the model
            threading.Thread(target=on_turn, args=(event["session"], event["payload"]), daemon=True).start()

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print(f"agent listening on :{PORT}, WaGo at {WAGO}")
    HTTPServer(("127.0.0.1", PORT), Hook).serve_forever()
