/**
 * log.ts — lightweight debug logging shared by the signaling/WebRTC layers.
 *
 * Every entry goes to the browser console and to any subscribed UI panel
 * (RoomPage renders one) so connection issues can be diagnosed without
 * opening devtools.
 *
 * Framework-free. No React imports.
 */

export interface LogEntry {
  /** HH:MM:SS.mmm local time */
  ts: string;
  /** Subsystem, e.g. "signaling" | "webrtc" | "control" */
  tag: string;
  text: string;
}

type Listener = (e: LogEntry) => void;

const listeners = new Set<Listener>();
const history: LogEntry[] = [];
const HISTORY_MAX = 300;

function fmt(v: unknown): string {
  if (typeof v === "string") return v;
  if (v instanceof Error) return v.message;
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

export function dlog(tag: string, ...parts: unknown[]): void {
  const now = new Date();
  const ts =
    now.toTimeString().slice(0, 8) +
    "." +
    String(now.getMilliseconds()).padStart(3, "0");
  const entry: LogEntry = { ts, tag, text: parts.map(fmt).join(" ") };
  history.push(entry);
  if (history.length > HISTORY_MAX) history.shift();
  // eslint-disable-next-line no-console
  console.log(`[${entry.ts}] ${entry.tag}: ${entry.text}`);
  for (const l of listeners) l(entry);
}

/** Subscribe to new entries; returns an unsubscribe function. */
export function subscribeLog(fn: Listener): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/** Snapshot of entries logged so far (capped at HISTORY_MAX). */
export function logHistory(): LogEntry[] {
  return history.slice();
}
