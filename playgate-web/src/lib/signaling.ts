/**
 * signaling.ts — client for the playgate-signaling Cloudflare Worker (T7/T8).
 *
 * Endpoints (see playgate-signaling/README.md):
 *   POST /rooms/{roomId}/{peer}        push a JSON message (we are peer="viewer")
 *   GET  /rooms/{roomId}/{peer}?since  poll the *other* peer's messages
 *   POST /turn/credentials             obtain iceServers
 *
 * Framework-free. No React.
 */

export interface SignalingMessage {
  seq: number;
  ts: string;
  payload: unknown;
}

export interface PollResult {
  messages: SignalingMessage[];
  nextSince: number;
}

export interface TurnCredentials {
  iceServers: RTCIceServer[];
  ttl?: number;
}

/** Public STUN fallback when /turn/credentials is unavailable. */
export const FALLBACK_ICE_SERVERS: RTCIceServer[] = [
  { urls: "stun:stun.l.google.com:19302" },
  { urls: "stun:stun.cloudflare.com:3478" },
];

export interface SignalingOptions {
  baseUrl: string;
  roomId: string;
  /** Our role. Viewer pushes to /viewer and polls /host's messages. */
  peer?: "viewer" | "host";
  /** Optional bearer token (session JWT / HMAC) for auth-enabled deployments. */
  token?: string;
}

export class SignalingClient {
  private baseUrl: string;
  private roomId: string;
  private peer: "viewer" | "host";
  private token?: string;
  private since = -1;
  private polling = false;

  constructor(opts: SignalingOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/$/, "");
    this.roomId = opts.roomId;
    this.peer = opts.peer ?? "viewer";
    this.token = opts.token;
  }

  /** The peer whose messages we poll for (the opposite of our role). */
  private get otherPeer(): "viewer" | "host" {
    return this.peer === "viewer" ? "host" : "viewer";
  }

  private headers(json = true): HeadersInit {
    const h: Record<string, string> = {};
    if (json) h["Content-Type"] = "application/json";
    if (this.token) h["Authorization"] = `Bearer ${this.token}`;
    return h;
  }

  /** Push a JSON payload (SDP offer/answer or ICE candidate) as our peer. */
  async push(payload: unknown): Promise<void> {
    const res = await fetch(`${this.baseUrl}/rooms/${this.roomId}/${this.peer}`, {
      method: "POST",
      headers: this.headers(),
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      throw new Error(`signaling push failed: ${res.status}`);
    }
  }

  /** Poll once for new messages from the other peer. */
  async poll(): Promise<SignalingMessage[]> {
    const url = `${this.baseUrl}/rooms/${this.roomId}/${this.otherPeer}?since=${this.since}`;
    const res = await fetch(url, { headers: this.headers(false) });
    if (!res.ok) {
      throw new Error(`signaling poll failed: ${res.status}`);
    }
    const data = (await res.json()) as PollResult;
    if (typeof data.nextSince === "number") this.since = data.nextSince;
    return data.messages ?? [];
  }

  /**
   * Long-poll loop. Calls onMessage for each new message until stop() is
   * called. Errors are passed to onError (non-fatal — polling continues with
   * back-off) so the UI can show "reconnecting".
   */
  startPolling(
    onMessage: (msg: SignalingMessage) => void,
    onError?: (err: unknown) => void,
    intervalMs = 700,
  ): void {
    this.polling = true;
    const loop = async () => {
      while (this.polling) {
        try {
          const msgs = await this.poll();
          for (const m of msgs) onMessage(m);
          await delay(intervalMs);
        } catch (err) {
          onError?.(err);
          await delay(intervalMs * 2);
        }
      }
    };
    void loop();
  }

  stop(): void {
    this.polling = false;
  }

  /** Fetch TURN/STUN ICE servers; falls back to public STUN on failure. */
  async fetchIceServers(): Promise<RTCIceServer[]> {
    try {
      const res = await fetch(`${this.baseUrl}/turn/credentials`, {
        method: "POST",
        headers: this.headers(),
        body: JSON.stringify({}),
      });
      if (!res.ok) throw new Error(`turn ${res.status}`);
      const data = (await res.json()) as TurnCredentials;
      if (data.iceServers && data.iceServers.length > 0) return data.iceServers;
      return FALLBACK_ICE_SERVERS;
    } catch {
      return FALLBACK_ICE_SERVERS;
    }
  }
}

function delay(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** Classify a signaling payload by shape. */
export function classifySignal(
  payload: unknown,
): "offer" | "answer" | "candidate" | "unknown" {
  if (payload && typeof payload === "object") {
    const p = payload as Record<string, unknown>;
    if (p.type === "offer") return "offer";
    if (p.type === "answer") return "answer";
    if ("candidate" in p) return "candidate";
  }
  return "unknown";
}
