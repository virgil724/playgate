// Usage: node cdp-eval.mjs <targetId> <file-with-js-expression>
// Evaluates an (async) expression in one phone Chrome tab via the forwarded
// devtools port and prints the JSON result. Only touches the given target.
import { readFileSync } from "node:fs";

const [, , targetId, exprFile] = process.argv;
const port = process.env.CDP_PORT ?? "9333";
const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
const t = list.find((x) => x.id === targetId);
if (!t) {
  console.error("target not found:", targetId);
  process.exit(2);
}
const expression = readFileSync(exprFile, "utf8");
const ws = new WebSocket(t.webSocketDebuggerUrl);
const timeout = setTimeout(() => {
  console.error("timeout");
  process.exit(3);
}, Number(process.env.CDP_TIMEOUT_MS ?? 60000));
ws.onopen = () =>
  ws.send(
    JSON.stringify({
      id: 1,
      method: "Runtime.evaluate",
      params: { expression, awaitPromise: true, returnByValue: true, userGesture: true },
    }),
  );
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id !== 1) return;
  clearTimeout(timeout);
  if (m.result?.exceptionDetails) {
    console.error("exception:", JSON.stringify(m.result.exceptionDetails).slice(0, 800));
  } else {
    console.log(JSON.stringify(m.result?.result?.value, null, 2));
  }
  ws.close();
  process.exit(0);
};
ws.onerror = (e) => {
  console.error("ws error", e.message ?? e);
  process.exit(4);
};
