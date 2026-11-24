// A complete WhatsApp agent on WaGo in one file, no dependencies (Node 22.18+ runs TypeScript directly).
//
// WaGo does the WhatsApp side: turns, transcribed voice notes, context, read receipts, "typing...", parts,
// interruption, handing the chat to a human. This file only decides what to say.
//
//   WAGO_URL=http://127.0.0.1:3000 WAGO_API_KEY=... WAGO_HOOK_SECRET=... node agent.ts
//   OPENAI_BASE_URL / OPENAI_API_KEY / OPENAI_MODEL: any OpenAI-compatible API (without a key: a fixed answer)
//
// Point the session at it once:
//   PUT /api/sessions/default {"config": {"agent": {"enabled": true},
//     "webhooks": [{"url": "http://127.0.0.1:8080/", "events": ["turn.ready"], "hmac": {"key": "<secret>"}}]}}

import { createHmac, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";

const WAGO = (process.env.WAGO_URL ?? "http://127.0.0.1:3000").replace(/\/$/, "");
const KEY = process.env.WAGO_API_KEY ?? "";
const SECRET = process.env.WAGO_HOOK_SECRET ?? "";
const LLM_URL = (process.env.OPENAI_BASE_URL ?? "https://api.openai.com/v1").replace(/\/$/, "");
const LLM_KEY = process.env.OPENAI_API_KEY ?? "";
const LLM_MODEL = process.env.OPENAI_MODEL ?? "gpt-4o-mini";
const PORT = Number(process.env.PORT ?? 8080);

type ContextLine = { id: string; role: "user" | "agent" | "human"; name?: string; at: number; text: string };
type TurnReady = { turn: { id: string; chatId: string }; messages: unknown[]; context: ContextLine[] };

const SYSTEM =
  "You are the assistant of a small business on WhatsApp. Answer briefly and warmly, like a person typing on a " +
  "phone. Use the language of the customer. If you cannot help, say a colleague will answer soon.";

// The agent's brain: the chat so far in, the answer's parts out. Replace with your own.
async function think(context: ContextLine[]): Promise<string[]> {
  if (!LLM_KEY) return ["Thanks for your message!", "We'll get back to you shortly."];
  const messages = [
    { role: "system", content: SYSTEM },
    ...context.map((l) => ({ role: l.role === "user" ? "user" : "assistant", content: l.text })),
  ];
  const res = await fetch(`${LLM_URL}/chat/completions`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${LLM_KEY}` },
    body: JSON.stringify({ model: LLM_MODEL, messages }),
  });
  const out = (await res.json()) as { choices: { message: { content: string } }[] };
  return out.choices[0].message.content.split("\n\n").map((p) => p.trim()).filter(Boolean).slice(0, 4);
}

async function onTurn(session: string, p: TurnReady): Promise<void> {
  const parts = await think(p.context);
  const res = await fetch(`${WAGO}/api/${session}/turns/${p.turn.id}/reply`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Api-Key": KEY },
    body: JSON.stringify({ parts: parts.map((text) => ({ text })) }),
  });
  // 409: the person wrote again (a newer turn is coming) or a human took the chat; nothing to do
  if (!res.ok && res.status !== 409) console.error("reply failed", res.status, await res.text());
}

function signed(body: Buffer, header: string | undefined): boolean {
  if (!SECRET) return true;
  const want = Buffer.from(createHmac("sha512", SECRET).update(body).digest("hex"));
  const got = Buffer.from(header ?? "");
  return want.length === got.length && timingSafeEqual(want, got);
}

createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on("data", (c: Buffer) => chunks.push(c));
  req.on("end", () => {
    const body = Buffer.concat(chunks);
    if (!signed(body, req.headers["x-webhook-hmac"] as string | undefined)) {
      res.writeHead(401).end();
      return;
    }
    res.writeHead(200).end(); // answer the hook at once, think afterwards
    const event = JSON.parse(body.toString());
    if (event.event === "turn.ready") onTurn(event.session, event.payload).catch((e) => console.error(e));
  });
}).listen(PORT, "127.0.0.1", () => console.log(`agent listening on :${PORT}, WaGo at ${WAGO}`));
