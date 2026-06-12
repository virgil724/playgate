/**
 * RoomDO — one Durable Object instance per signaling room.
 *
 * Replaces the old Workers KV polling design. Holds the per-peer message queues
 * in DO storage (SQLite-backed KV API) and serves three transports over the
 * same room state:
 *
 *   - HTTP POST  /rooms/{roomId}/{peer}              → append to sender queue
 *   - HTTP GET   /rooms/{roomId}/{peer}?since&wait   → poll other peer (+ long-poll)
 *   - WS  GET    /rooms/{roomId}/{peer}/ws           → push other peer's messages
 *
 * The pure queue logic lives in {@link RoomState}; this class is the thin glue
 * that wires it to `ctx.storage`, WebSocket hibernation, long-poll resolvers,
 * and an idle-cleanup alarm. Keep it thin: anything testable belongs in
 * RoomState.
 *
 * Storage layout (all under this DO's keyspace):
 *   "queue:host"   → SignalingMessage[]   (host's outbound queue)
 *   "queue:viewer" → SignalingMessage[]   (viewer's outbound queue)
 * Plus DO-managed alarm state. RoomState persists on every mutation, so the DO
 * is safe to hibernate or evict at any time.
 *
 * Long-poll resolvers are kept in memory only (`#waiters`): a held GET request
 * keeps the DO awake while waiting, which is fine. They are resolved when a new
 * message arrives for that peer, or on timeout (empty messages).
 */

import type { Env, SignalingMessage, MessagesResponse } from "./types.js";
import { CORS_HEADERS, jsonError, jsonOk } from "./cors.js";
import {
  RoomState,
  VALID_PEERS,
  otherPeer,
  type Peer,
  type RoomStorage,
} from "./roomState.js";

/** Cap the server-side long-poll hold at 25 s (under CF's request limits). */
const MAX_WAIT_SECONDS = 25;

/** Wipe idle rooms ~10 min after the last activity (KV-TTL spirit). */
const IDLE_CLEANUP_MS = 10 * 60 * 1000;

/** A pending long-poll request waiting for the other peer to send something. */
interface Waiter {
  peer: Peer;
  since: number;
  resolve: (body: MessagesResponse) => void;
  timer: ReturnType<typeof setTimeout>;
}

export class RoomDO {
  private readonly state: DurableObjectState;
  private readonly room: RoomState;
  private readonly waiters = new Set<Waiter>();

  constructor(state: DurableObjectState, _env: Env) {
    this.state = state;
    // Adapt DurableObjectStorage to the minimal RoomStorage interface. The
    // SQLite-backed KV API (get/put/delete) is what RoomState uses.
    const storage: RoomStorage = {
      get: <T>(key: string) =>
        state.storage.get<T>(key) as Promise<T | undefined>,
      put: <T>(key: string, value: T) => state.storage.put<T>(key, value),
      delete: (key: string) => state.storage.delete(key),
    };
    this.room = new RoomState(storage);
  }

  /** Push the idle-cleanup alarm out to "now + IDLE_CLEANUP_MS". */
  private async refreshAlarm(): Promise<void> {
    await this.state.storage.setAlarm(Date.now() + IDLE_CLEANUP_MS);
  }

  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    // Path shape: /rooms/{roomId}/{peer}[/ws]. The peer (and optional /ws) is
    // all this DO needs; the roomId already selected this instance.
    const parts = url.pathname.split("/").filter(Boolean); // ["rooms", id, peer, "ws"?]
    const peer = parts[2];
    const isWs = parts[3] === "ws";

    if (peer === undefined || !VALID_PEERS.has(peer)) {
      return jsonError(`peer must be "host" or "viewer", got "${peer}"`, 400);
    }
    const typedPeer = peer as Peer;

    if (isWs) {
      return this.handleWebSocket(request, typedPeer);
    }

    const method = request.method.toUpperCase();
    if (method === "POST") {
      return this.handlePost(request, typedPeer);
    }
    if (method === "GET") {
      return this.handleGet(url, typedPeer);
    }
    return jsonError("Method not allowed", 405);
  }

  // ---- HTTP POST -----------------------------------------------------------

  private async handlePost(request: Request, peer: Peer): Promise<Response> {
    let payload: unknown;
    try {
      payload = await request.json();
    } catch {
      return jsonError("Request body must be valid JSON", 400);
    }

    const { message, recipient } = await this.room.append(peer, payload);
    await this.refreshAlarm();
    this.notify(recipient, message);

    return jsonOk({ seq: message.seq, ts: message.ts }, 201);
  }

  // ---- HTTP GET (with optional long-poll) ----------------------------------

  private async handleGet(url: URL, peer: Peer): Promise<Response> {
    const sinceParam = url.searchParams.get("since");
    const since = sinceParam !== null ? parseInt(sinceParam, 10) : -1;
    if (sinceParam !== null && (isNaN(since) || since < -1)) {
      return jsonError("`since` must be a non-negative integer", 400);
    }

    const waitParam = url.searchParams.get("wait");
    let wait = waitParam !== null ? parseInt(waitParam, 10) : 0;
    if (isNaN(wait) || wait < 0) wait = 0;
    wait = Math.min(wait, MAX_WAIT_SECONDS);

    await this.refreshAlarm();

    const result = await this.room.poll(peer, since);

    // Immediate response when we have data or the caller didn't ask to wait.
    if (result.messages.length > 0 || wait === 0) {
      return jsonOk(result);
    }

    // Long-poll: hold the request until a message arrives or `wait` elapses.
    const body = await new Promise<MessagesResponse>((resolve) => {
      const waiter: Waiter = {
        peer,
        since,
        resolve,
        timer: setTimeout(() => {
          this.waiters.delete(waiter);
          resolve({ messages: [], nextSince: since });
        }, wait * 1000),
      };
      this.waiters.add(waiter);
    });
    return jsonOk(body);
  }

  // ---- WebSocket (hibernation API) -----------------------------------------

  private async handleWebSocket(
    request: Request,
    peer: Peer,
  ): Promise<Response> {
    if (request.headers.get("Upgrade") !== "websocket") {
      return jsonError("Expected Upgrade: websocket", 426);
    }

    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];

    // Tag the socket with its peer so webSocketMessage can route without state.
    this.state.acceptWebSocket(server, [peer]);
    await this.refreshAlarm();

    // Immediately push the other peer's current backlog, one frame per message.
    const backlog = await this.room.backlogFor(peer);
    for (const msg of backlog) {
      server.send(JSON.stringify(msg));
    }

    return new Response(null, { status: 101, webSocket: client });
  }

  async webSocketMessage(
    ws: WebSocket,
    message: string | ArrayBuffer,
  ): Promise<void> {
    const peer = this.peerOf(ws);
    if (peer === undefined) return;

    let payload: unknown;
    try {
      payload = JSON.parse(
        typeof message === "string" ? message : new TextDecoder().decode(message),
      );
    } catch {
      // Malformed frames are ignored (do not close the socket).
      try {
        ws.send(JSON.stringify({ error: "bad json" }));
      } catch {
        /* socket may be gone; ignore */
      }
      return;
    }

    const { message: stored, recipient } = await this.room.append(peer, payload);
    await this.refreshAlarm();
    this.notify(recipient, stored);
  }

  async webSocketClose(
    ws: WebSocket,
    code: number,
    _reason: string,
  ): Promise<void> {
    // Best-effort close of our side; cleanup of in-memory state is automatic.
    try {
      ws.close(code, "closing");
    } catch {
      /* already closed */
    }
  }

  async webSocketError(_ws: WebSocket, _error: unknown): Promise<void> {
    // Nothing to clean up; socket is removed from the DO automatically.
  }

  // ---- Notification fan-out ------------------------------------------------

  /**
   * Deliver a freshly stored message to everyone waiting on `recipient`:
   *   - every connected WebSocket tagged with `recipient`
   *   - every pending long-poll whose `since` precedes this message's seq
   */
  private notify(recipient: Peer, message: SignalingMessage): void {
    const frame = JSON.stringify(message);
    for (const ws of this.state.getWebSockets(recipient)) {
      try {
        ws.send(frame);
      } catch {
        /* drop send errors; socket teardown is handled by the runtime */
      }
    }

    for (const waiter of [...this.waiters]) {
      if (waiter.peer === recipient && message.seq > waiter.since) {
        clearTimeout(waiter.timer);
        this.waiters.delete(waiter);
        waiter.resolve({ messages: [message], nextSince: message.seq });
      }
    }
  }

  /** Recover the peer tag attached to a hibernation socket. */
  private peerOf(ws: WebSocket): Peer | undefined {
    const tags = this.state.getTags(ws);
    const tag = tags.find((t) => VALID_PEERS.has(t));
    return tag as Peer | undefined;
  }

  // ---- Idle cleanup --------------------------------------------------------

  async alarm(): Promise<void> {
    // If sockets are still connected the room is active — reschedule instead.
    if (this.state.getWebSockets().length > 0) {
      await this.refreshAlarm();
      return;
    }
    // Otherwise the room has been idle ~10 min: wipe all room storage.
    await this.state.storage.deleteAll();
  }
}

// `otherPeer` is part of the shared room logic; re-export so callers importing
// the DO module have it without reaching into roomState.
export { otherPeer };
