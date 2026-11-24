#!/usr/bin/env python3
"""WAHA contract conformance: run the behaviour WAHA clients rely on against a running server.

It checks what real WAHA clients were found to depend on: session lifecycle codes, the webhook envelope and its
HMAC, client-chosen message ids with safe retries, WAHA message ids, per-message acks, edit and revoke events.
Point it at WaGo started with -dev-fake (nothing reaches WhatsApp; inbound traffic is played through
/dev/incoming), or at another WAHA-compatible server with --no-dev to run only the checks that need no inbound.

    wago -dev-fake -addr 127.0.0.1:3999 -home /tmp/wago-conf &      # with WAGO_API_KEY=test in the environment
    python3 conformance/waha_conformance.py --url http://127.0.0.1:3999 --key test

Python 3.9+, standard library only. Exit code 0 when every check passes.
"""

import argparse
import hashlib
import hmac
import json
import queue
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

CHAT = "15550000001@c.us"
HMAC_KEY = "conformance-secret"
results = []


class Hooks:
    def __init__(self):
        self.q = queue.Queue()
        hooks = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                hooks.q.put((dict(self.headers), body))
                self.send_response(200)
                self.end_headers()

            def log_message(self, *a):
                pass

        self.srv = HTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_port}/hook"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def wait(self, event, pred=lambda p: True, timeout=5.0):
        end = time.time() + timeout
        while time.time() < end:
            try:
                headers, body = self.q.get(timeout=max(0.05, end - time.time()))
            except queue.Empty:
                break
            e = json.loads(body)
            if e.get("event") == event and pred(e.get("payload") or {}):
                return headers, body, e
        return None, None, None

    def drain(self):
        while not self.q.empty():
            self.q.get_nowait()


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else f"  ({detail})"))


def call(base, key, method, path, body=None):
    req = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("X-Api-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, raw.decode(errors="replace")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[1])
    ap.add_argument("--url", default="http://127.0.0.1:3999")
    ap.add_argument("--key", required=True)
    ap.add_argument("--session", default="conformance")
    ap.add_argument("--no-dev", action="store_true", help="the server has no /dev/incoming (not WaGo -dev-fake)")
    a = ap.parse_args()
    base, key, s = a.url.rstrip("/"), a.key, a.session
    api = lambda m, p, b=None: call(base, key, m, p, b)
    hooks = Hooks()

    # server
    check("GET /ping is open", call(base, "", "GET", "/ping")[0] == 200)
    check("the API needs the key", call(base, "", "GET", "/api/sessions")[0] == 401)
    code, v = api("GET", "/api/version")
    check("GET /api/version", code == 200 and isinstance(v, dict) and "version" in v, v)

    # sessions
    api("DELETE", f"/api/sessions/{s}")
    check("an unknown session is 404", api("GET", f"/api/sessions/{s}")[0] == 404)
    cfg = {"sending": True, "webhooks": [{"url": hooks.url, "events": ["*"], "hmac": {"key": HMAC_KEY},
                                          "retries": {"policy": "constant", "delaySeconds": 1, "attempts": 2}}]}
    code, body = api("POST", "/api/sessions", {"name": s, "start": False, "config": cfg})
    check("POST /api/sessions creates (201)", code == 201, (code, body))
    check("creating it again is 422", api("POST", "/api/sessions", {"name": s})[0] == 422)
    code, info = api("GET", f"/api/sessions/{s}")
    check("a new session is STOPPED", code == 200 and info.get("status") == "STOPPED", info)
    try:
        hk = info["config"]["webhooks"][0]["hmac"]["key"]
    except (KeyError, IndexError, TypeError):
        hk = None
    check("the hook's HMAC key reads back with the API key", hk == HMAC_KEY, hk)
    code, _ = api("PUT", f"/api/sessions/{s}", {"config": {"webhooks": cfg["webhooks"]}})
    check("PUT config without `sending` is accepted", code == 200)
    code, info = api("GET", f"/api/sessions/{s}")
    check("... and keeps `sending`", info.get("config", {}).get("sending") is True, info.get("config"))
    code, _ = api("POST", f"/api/sessions/{s}/start")
    check("start", code in (200, 201))
    status = ""
    for _ in range(50):
        status = api("GET", f"/api/sessions/{s}")[1].get("status")
        if status == "WORKING":
            break
        time.sleep(0.1)
    if status != "WORKING":
        print(f"session is {status}: sending and inbound checks need a WORKING session (WaGo -dev-fake gives one)")
        return finish()

    _, _, ev = hooks.wait("session.status", timeout=1)
    hooks.drain()

    # sending with a client id
    cid = "CONF" + str(int(time.time() * 1000))
    code, msg = api("POST", "/api/sendText", {"session": s, "chatId": CHAT, "text": "conformance", "id": cid})
    check("sendText with an id is 201", code == 201, (code, msg))
    check("the message id is true_<chat>_<id>", isinstance(msg, dict) and msg.get("id") == f"true_{CHAT}_{cid}", msg)
    headers, body, ev = hooks.wait("message.any", lambda p: p.get("id") == f"true_{CHAT}_{cid}")
    check("message.any echoes it", ev is not None)
    if ev:
        need = {"id", "timestamp", "event", "session", "payload", "engine", "environment"}
        check("the webhook envelope has WAHA's fields", need <= set(ev), sorted(need - set(ev)))
        sig = {k.lower(): v for k, v in headers.items()}.get("x-webhook-hmac", "")
        want = hmac.new(HMAC_KEY.encode(), body, hashlib.sha512).hexdigest()
        check("X-Webhook-Hmac is HMAC-SHA512 of the body", hmac.compare_digest(sig, want))
    code, again = api("POST", "/api/sendText", {"session": s, "chatId": CHAT, "text": "conformance", "id": cid})
    check("a repeat with the same id returns the same message", code == 201 and again.get("id") == msg.get("id"), again)
    _, _, dup = hooks.wait("message.any", lambda p: p.get("id") == f"true_{CHAT}_{cid}", timeout=1)
    check("... and sends nothing twice", dup is None)
    code, read = api("GET", f"/api/{s}/chats/{CHAT}/messages/true_{CHAT}_{cid}")
    check("our message reads back by WAHA id with ack 1", code == 200 and read.get("ack") == 1, read)
    check("_data.key carries id, remoteJid and fromMe", (read.get("_data") or {}).get("key", {}).get("id") == cid, read.get("_data"))

    # changes we make raise the same events as changes from the phone
    code, _ = api("PUT", f"/api/{s}/chats/{CHAT}/messages/true_{CHAT}_{cid}", {"text": "conformance, edited"})
    _, _, ev = hooks.wait("message.edited", lambda p: p.get("editedMessageId") == cid)
    check("editing our message raises message.edited", code in (200, 201) and ev is not None and ev["payload"].get("body") == "conformance, edited")
    code, _ = api("DELETE", f"/api/{s}/chats/{CHAT}/messages/true_{CHAT}_{cid}")
    _, _, ev = hooks.wait("message.revoked", lambda p: p.get("revokedMessageId") == cid)
    check("deleting our message raises message.revoked", code in (200, 201) and ev is not None)

    if not a.no_dev:
        dev = lambda b: api("POST", f"/api/{s}/dev/incoming", b)
        iid = "CONFIN" + str(int(time.time() * 1000))
        code, _ = dev({"chatId": CHAT, "text": "hello from the other side", "id": iid, "pushName": "Conf"})
        _, _, ev = hooks.wait("message", lambda p: p.get("id") == f"false_{CHAT}_{iid}")
        check("an incoming message arrives as `message`", code == 200 and ev is not None)
        if ev:
            p = ev["payload"]
            check("... from the chat, fromMe false", p.get("from") == CHAT and p.get("fromMe") is False, p)
            check("... with _data.notifyName", (p.get("_data") or {}).get("notifyName") == "Conf", p.get("_data"))
        code, ct = api("GET", "/api/contacts?" + urllib.parse.urlencode({"session": s, "contactId": CHAT}))
        check("the sender's contact has their pushname", code == 200 and ct.get("pushname") == "Conf", (code, ct))
        dev({"chatId": CHAT, "edit": iid, "text": "hello, edited"})
        _, _, ev = hooks.wait("message.edited", lambda p: p.get("editedMessageId") == iid)
        check("an incoming edit arrives with editedMessageId and body", ev is not None and ev["payload"].get("body") == "hello, edited")
        dev({"chatId": CHAT, "revoke": iid})
        _, _, ev = hooks.wait("message.revoked", lambda p: p.get("revokedMessageId") == iid)
        check("an incoming revoke arrives with revokedMessageId", ev is not None)
        cid2 = cid + "B"
        api("POST", "/api/sendText", {"session": s, "chatId": CHAT, "text": "ack me", "id": cid2})
        dev({"chatId": CHAT, "ack": "read", "ackIds": [cid2]})
        _, _, ev = hooks.wait("message.ack", lambda p: p.get("id") == f"true_{CHAT}_{cid2}")
        check("a read receipt is one message.ack with ack 3 / READ", ev is not None and ev["payload"].get("ack") == 3
              and ev["payload"].get("ackName") == "READ", ev and ev["payload"])

    api("POST", f"/api/sessions/{s}/stop")
    code, _ = api("POST", "/api/sendText", {"session": s, "chatId": CHAT, "text": "x"})
    check("a stopped session refuses to send with 422", code == 422, code)
    code, ct = api("GET", "/api/contacts?" + urllib.parse.urlencode({"session": s, "contactId": "15550000000@c.us"}))
    check("a number nobody saved is still a contact, not 404", code == 200 and ct.get("id") == "15550000000@c.us", (code, ct))
    api("DELETE", f"/api/sessions/{s}")
    return finish()


def finish():
    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
