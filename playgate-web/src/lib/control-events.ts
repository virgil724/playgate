/**
 * control-events.ts — parse SessionEvent JSON from the "control" DataChannel.
 *
 * Spec: playgate-host/docs/protocols.md §4.
 *
 *   { kind, viewer_id, remaining_seconds, queue_position, ts }
 *
 * kinds: granted | expired | idle_kicked | queued | tick
 */

export type ControlEventKind =
  | "granted"
  | "expired"
  | "idle_kicked"
  | "queued"
  | "tick";

export interface ControlEvent {
  kind: ControlEventKind;
  viewerId: string;
  remainingSeconds: number;
  queuePosition: number;
  ts: number;
}

const KNOWN_KINDS: ReadonlySet<string> = new Set([
  "granted",
  "expired",
  "idle_kicked",
  "queued",
  "tick",
]);

/**
 * Parse a control-channel text message into a ControlEvent. Returns null if the
 * message is not valid JSON or has an unknown `kind`.
 */
export function parseControlEvent(data: unknown): ControlEvent | null {
  let obj: unknown = data;
  if (typeof data === "string") {
    try {
      obj = JSON.parse(data);
    } catch {
      return null;
    }
  }
  if (!obj || typeof obj !== "object") return null;
  const o = obj as Record<string, unknown>;
  const kind = o.kind;
  if (typeof kind !== "string" || !KNOWN_KINDS.has(kind)) return null;

  return {
    kind: kind as ControlEventKind,
    viewerId: typeof o.viewer_id === "string" ? o.viewer_id : "",
    remainingSeconds: numOr(o.remaining_seconds, 0),
    queuePosition: numOr(o.queue_position, 0),
    ts: numOr(o.ts, 0),
  };
}

function numOr(v: unknown, def: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : def;
}

/** Does this event mean the local viewer currently holds control? */
export function grantsControl(event: ControlEvent, localViewerId: string): boolean {
  if (event.kind !== "granted") return false;
  // If we don't know our viewer id, accept any grant (single-viewer dev case).
  return localViewerId === "" || event.viewerId === localViewerId;
}

/** Does this event revoke control for the local viewer? */
export function revokesControl(event: ControlEvent, localViewerId: string): boolean {
  if (event.kind !== "expired" && event.kind !== "idle_kicked") return false;
  return localViewerId === "" || event.viewerId === localViewerId;
}

/** Human-readable status line for the UI. */
export function describeEvent(event: ControlEvent): string {
  switch (event.kind) {
    case "granted":
      return `You're in control — ${event.remainingSeconds}s`;
    case "tick":
      return `${event.remainingSeconds}s remaining`;
    case "queued":
      return `You're #${event.queuePosition} in the queue`;
    case "expired":
      return "Your turn has ended";
    case "idle_kicked":
      return "Removed for inactivity";
  }
}
