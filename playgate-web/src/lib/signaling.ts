/**
 * signaling.ts — client for the playgate-signaling Cloudflare Worker (T7/T8).
 *
 * Endpoints (see playgate-signaling/README.md):
 *   POST /rooms/{roomId}/{peer}        push a JSON message (we are peer="viewer")
 *   GET  /rooms/{roomId}/{peer}?since  poll the *other* peer's messages
 *   POST /turn/credentials             obtain iceServers
 *
 * Transport: prefers a per-room WebSocket (GET …/ws, Upgrade) for push delivery.
 * Falls back to HTTP polling (700 ms for the first 15 s, then 3 s) whenever the
 * socket is unavailable.  Reconnects with exponential back-off 1 s → 2 s → 4 s →
 * 8 s (capped at 10 s).
 *
 * Framework-free. No React.
 */

import { dlog } from "./log";

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

/** Factory type that creates a WebSocket; injectable for testing. */
export type WebSocketFactory = (url: string) => WebSocket;

export interface SignalingOptions {
  baseUrl: string;
  roomId: string;
  /** Our role. Both push and poll go to /rooms/{roomId}/{our role};
   * GET under our own name returns the *other* peer's messages. */
  peer?: "viewer" | "host";
  /** Optional bearer token (session JWT / HMAC) for auth-enabled deployments. */
  token?: string;
  /**
   * Injectable WebSocket constructor/factory.  Defaults to globalThis.WebSocket.
   * Pass a fake in tests; pass undefined to force HTTP-only mode.
   */
  wsFactory?: WebSocketFactory | null;
}

// ---------------------------------------------------------------------------
// Internal timing constants
// ---------------------------------------------------------------------------

const WS_BACKOFF_INITIAL_MS = 1_000;
const WS_BACKOFF_CAP_MS = 10_000;
const HTTP_FAST_MS = 700;
const HTTP_SLOW_MS = 3_000;
const HTTP_FAST_WINDOW_MS = 15_000;

export class SignalingClient {
  private baseUrl: string;
  private roomId: string;
  private peer: "viewer" | "host";
  private token?: string;
  private wsFactory: WebSocketFactory | null;

  // Delivery state
  private since = -1;
  /** The highest seq we have delivered; used to deduplicate replay overlap. */
  private maxSeqSeen = -1;

  // Lifecycle flags
  private running = false;

  // WebSocket state
  private ws: WebSocket | null = null;
  private wsReconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private wsBackoffMs = WS_BACKOFF_INITIAL_MS;
  /** True while the WS is fully open and delivering frames. */
  private wsOpen = false;

  // HTTP fallback state
  private httpFallbackTimer: ReturnType<typeof setTimeout> | null = null;
  private httpLoopActive = false;
  private httpFallbackStartedAt = 0;

  // Callbacks registered by startPolling
  private onMessage: ((msg: SignalingMessage) => void) | null = null;
  private onError: ((err: unknown) => void) | null = null;

  constructor(opts: SignalingOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/$/, "");
    this.roomId = opts.roomId;
    this.peer = opts.peer ?? "viewer";
    this.token = opts.token;

    // Resolve wsFactory: explicit null → HTTP only; undefined → use global.
    if (opts.wsFactory === null) {
      this.wsFactory = null;
    } else if (opts.wsFactory !== undefined) {
      this.wsFactory = opts.wsFactory;
    } else if (typeof globalThis.WebSocket !== "undefined") {
      this.wsFactory = (url: string) => new globalThis.WebSocket(url);
    } else {
      this.wsFactory = null;
    }
  }

  // ---------------------------------------------------------------------------
  // HTTP helpers
  // ---------------------------------------------------------------------------

  private headers(json = true): HeadersInit {
    const h: Record<string, string> = {};
    if (json) h["Content-Type"] = "application/json";
    if (this.token) h["Authorization"] = `Bearer ${this.token}`;
    return h;
  }

  /** Push a JSON payload (SDP offer/answer or ICE candidate) as our peer.
   *  When the WS is open the payload is sent over the socket; otherwise HTTP POST. */
  async push(payload: unknown): Promise<void> {
    if (this.wsOpen && this.ws) {
      this.ws.send(JSON.stringify(payload));
      return;
    }
    const res = await fetch(`${this.baseUrl}/rooms/${this.roomId}/${this.peer}`, {
      method: "POST",
      headers: this.headers(),
      body: JSON.stringify(payload),
    });
    if (!res.ok) {
      throw new Error(`signaling push failed: ${res.status}`);
    }
  }

  /**
   * Poll once for new messages from the other peer (HTTP GET).
   * Updates this.since so that a WS→HTTP fallback resumes at the right offset.
   */
  async poll(): Promise<SignalingMessage[]> {
    const url = `${this.baseUrl}/rooms/${this.roomId}/${this.peer}?since=${this.since}`;
    const res = await fetch(url, { headers: this.headers(false) });
    if (!res.ok) {
      throw new Error(`signaling poll failed: ${res.status}`);
    }
    const data = (await res.json()) as PollResult;
    if (typeof data.nextSince === "number") this.since = data.nextSince;
    return data.messages ?? [];
  }

  // ---------------------------------------------------------------------------
  // Public delivery API
  // ---------------------------------------------------------------------------

  /**
   * Start delivery.  Tries WebSocket first; falls back to HTTP polling when the
   * socket is unavailable.  Calls onMessage for every new (not-yet-seen) message.
   *
   * The intervalMs parameter is kept for API compatibility but only governs the
   * initial HTTP polling interval when WS is not available from the start.
   * The actual fast/slow cadence is controlled by the constants above.
   */
  startPolling(
    onMessage: (msg: SignalingMessage) => void,
    onError?: (err: unknown) => void,
    _intervalMs = 700,
  ): void {
    this.running = true;
    this.onMessage = onMessage;
    this.onError = onError ?? null;

    if (this.wsFactory) {
      this.connectWs();
    } else {
      // Pure HTTP environment.
      dlog("signaling", "no WebSocket available; using HTTP polling");
      this.startHttpFallback();
    }
  }

  stop(): void {
    if (!this.running && !this.ws && !this.httpLoopActive) return;
    this.running = false;
    this.wsOpen = false;
    this.httpLoopActive = false;

    if (this.ws) {
      try { this.ws.close(); } catch { /* ignore */ }
      this.ws = null;
    }
    if (this.wsReconnectTimer !== null) {
      clearTimeout(this.wsReconnectTimer);
      this.wsReconnectTimer = null;
    }
    if (this.httpFallbackTimer !== null) {
      clearTimeout(this.httpFallbackTimer);
      this.httpFallbackTimer = null;
    }
  }

  // ---------------------------------------------------------------------------
  // WebSocket transport
  // ---------------------------------------------------------------------------

  private wsUrl(): string {
    // Derive ws/wss from http/https base URL.
    const ws = this.baseUrl.replace(/^http/, "ws");
    let url = `${ws}/rooms/${this.roomId}/${this.peer}/ws`;
    if (this.token) url += `?token=${encodeURIComponent(this.token)}`;
    return url;
  }

  private connectWs(): void {
    if (!this.running) return;
    const url = this.wsUrl();
    dlog("signaling", "ws connecting →", url);

    let socket: WebSocket;
    try {
      socket = this.wsFactory!(url);
    } catch (err) {
      dlog("signaling", "ws construction failed:", err);
      this.scheduleWsReconnect();
      return;
    }
    this.ws = socket;

    socket.onopen = () => {
      if (!this.running || this.ws !== socket) { socket.close(); return; }
      dlog("signaling", "ws open");
      this.wsOpen = true;
      this.wsBackoffMs = WS_BACKOFF_INITIAL_MS; // reset back-off on success
      this.stopHttpFallback();
    };

    socket.onmessage = (ev: MessageEvent) => {
      if (!this.running || this.ws !== socket) return;
      try {
        const frame = JSON.parse(String(ev.data)) as SignalingMessage;
        this.deliverFrame(frame);
      } catch (err) {
        dlog("signaling", "ws message parse error:", err);
      }
    };

    socket.onerror = (ev: Event) => {
      dlog("signaling", "ws error:", (ev as ErrorEvent).message ?? "unknown");
    };

    socket.onclose = (ev: CloseEvent) => {
      if (this.ws !== socket) return; // already replaced; ignore
      dlog("signaling", `ws closed (code=${ev.code})`);
      this.ws = null;
      this.wsOpen = false;
      if (!this.running) return;
      this.startHttpFallback();
      this.scheduleWsReconnect();
    };
  }

  private scheduleWsReconnect(): void {
    if (!this.running) return;
    const delay = this.wsBackoffMs;
    this.wsBackoffMs = Math.min(this.wsBackoffMs * 2, WS_BACKOFF_CAP_MS);
    dlog("signaling", `reconnect scheduled in ${delay}ms`);
    this.wsReconnectTimer = setTimeout(() => {
      this.wsReconnectTimer = null;
      this.connectWs();
    }, delay);
  }

  // ---------------------------------------------------------------------------
  // HTTP fallback polling
  // ---------------------------------------------------------------------------

  private startHttpFallback(): void {
    if (this.httpLoopActive) return; // already running
    dlog("signaling", "fallback polling started");
    this.httpLoopActive = true;
    this.httpFallbackStartedAt = Date.now();
    void this.httpFallbackLoop();
  }

  private stopHttpFallback(): void {
    if (!this.httpLoopActive) return;
    dlog("signaling", "fallback polling stopped");
    this.httpLoopActive = false;
    if (this.httpFallbackTimer !== null) {
      clearTimeout(this.httpFallbackTimer);
      this.httpFallbackTimer = null;
    }
  }

  private httpFallbackLoop(): Promise<void> {
    // The loop is entirely driven by setTimeout so stop() can cancel at any
    // tick boundary without leaving floating promises that reference timers.
    return new Promise<void>((resolve) => {
      const tick = async () => {
        if (!this.httpLoopActive || !this.running) { resolve(); return; }

        // Choose interval based on time since the fallback started.
        const elapsed = Date.now() - this.httpFallbackStartedAt;
        const interval = elapsed < HTTP_FAST_WINDOW_MS ? HTTP_FAST_MS : HTTP_SLOW_MS;

        try {
          const msgs = await this.poll();
          for (const m of msgs) this.deliverFrame(m);
        } catch (err) {
          dlog("signaling", "poll error:", err);
          this.onError?.(err);
        }

        if (!this.httpLoopActive || !this.running) { resolve(); return; }

        this.httpFallbackTimer = setTimeout(() => {
          this.httpFallbackTimer = null;
          void tick();
        }, interval);
      };
      void tick();
    });
  }

  // ---------------------------------------------------------------------------
  // Message deduplication and delivery
  // ---------------------------------------------------------------------------

  private deliverFrame(frame: SignalingMessage): void {
    if (frame.seq <= this.maxSeqSeen) return; // replay overlap – already seen
    this.maxSeqSeen = frame.seq;
    // Keep HTTP since in sync so a fallback resumes at the right offset.
    if (frame.seq >= this.since) this.since = frame.seq;
    this.onMessage?.(frame);
  }

  // ---------------------------------------------------------------------------
  // ICE servers
  // ---------------------------------------------------------------------------

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
      dlog("signaling", "turn response had no iceServers; using fallback STUN");
      return FALLBACK_ICE_SERVERS;
    } catch (err) {
      dlog("signaling", "turn credentials failed; using fallback STUN:", err);
      return FALLBACK_ICE_SERVERS;
    }
  }
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

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
