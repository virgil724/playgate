/**
 * Pure room/queue logic, extracted so it can be unit-tested without a Durable
 * Object runtime (we cannot install miniflare / vitest-pool-workers here).
 *
 * `RoomState` owns the per-peer message queues for a single room and implements:
 *   - append + seq assignment
 *   - the host offer-reset rule (replace host queue, delete viewer queue,
 *     preserving seq continuity as lastSeq + 1)
 *   - since-filtering for polls
 *
 * It persists through an injectable {@link RoomStorage} interface so the real
 * DO can back it with `ctx.storage` (SQLite KV) and tests can fake it with a
 * plain in-memory map. Every mutation persists immediately so the DO is safe to
 * hibernate / evict at any point.
 */

import type { SignalingMessage } from "./types.js";

/** The two valid peers. */
export type Peer = "host" | "viewer";

export const VALID_PEERS: ReadonlySet<string> = new Set(["host", "viewer"]);

/** Maximum messages retained per peer per room (abuse protection). */
export const MAX_QUEUE_LENGTH = 200;

/**
 * Minimal async key/value storage the room logic needs. Mirrors the subset of
 * `DurableObjectStorage` we use, so the DO can pass `ctx.storage` straight in
 * and tests can supply a trivial fake.
 */
export interface RoomStorage {
  get<T>(key: string): Promise<T | undefined>;
  put<T>(key: string, value: T): Promise<void>;
  delete(key: string): Promise<boolean>;
}

/** Result of appending a message: the stored message + which peer owns it. */
export interface AppendResult {
  /** The stored {seq,ts,payload} message. */
  message: SignalingMessage;
  /** The peer whose queue the message was stored in (the sender). */
  sender: Peer;
  /** The peer that should be notified / polled (the other peer). */
  recipient: Peer;
}

export function otherPeer(peer: Peer): Peer {
  return peer === "host" ? "viewer" : "host";
}

/** Storage key for a peer's queue. */
function queueKey(peer: Peer): string {
  return `queue:${peer}`;
}

/**
 * True when the payload is a JSON object with type === "offer".
 * Defensive: a malformed / non-object payload must never crash append and falls
 * back to the plain append path.
 */
export function isOfferPayload(payload: unknown): boolean {
  return (
    typeof payload === "object" &&
    payload !== null &&
    (payload as { type?: unknown }).type === "offer"
  );
}

export class RoomState {
  constructor(private readonly storage: RoomStorage) {}

  private async loadQueue(peer: Peer): Promise<SignalingMessage[]> {
    const stored = await this.storage.get<SignalingMessage[]>(queueKey(peer));
    return stored ?? [];
  }

  /**
   * Append a payload posted by `peer`.
   *
   * Normal case: the payload is appended to the sender's queue with seq =
   * lastSeq + 1.
   *
   * Host offer-reset: when `peer === "host"` and the payload is an SDP offer,
   * the host queue is REPLACED with just this offer (seq continuity preserved
   * as lastSeq + 1) and the viewer queue is DELETED. Stale ICE/answers from a
   * dead peer must not survive a fresh offer; seq continuity keeps a viewer
   * that is already polling with a large `since` from missing the new offer.
   */
  async append(peer: Peer, payload: unknown): Promise<AppendResult> {
    const key = queueKey(peer);
    let queue = await this.loadQueue(peer);

    // Truncate if suspiciously long (abuse protection).
    if (queue.length >= MAX_QUEUE_LENGTH) {
      queue = queue.slice(-MAX_QUEUE_LENGTH + 1);
    }

    const msg: SignalingMessage = {
      seq: (queue[queue.length - 1]?.seq ?? -1) + 1,
      ts: new Date().toISOString(),
      payload,
    };

    if (peer === "host" && isOfferPayload(payload)) {
      // Fresh signaling session: keep only the new offer, drop the viewer queue.
      await this.storage.put(key, [msg]);
      await this.storage.delete(queueKey("viewer"));
    } else {
      queue.push(msg);
      await this.storage.put(key, queue);
    }

    return { message: msg, sender: peer, recipient: otherPeer(peer) };
  }

  /**
   * Return messages posted by the OTHER peer that `peer` has not yet seen
   * (seq > since). `since === -1` returns everything.
   */
  async poll(
    peer: Peer,
    since: number,
  ): Promise<{ messages: SignalingMessage[]; nextSince: number }> {
    const all = await this.loadQueue(otherPeer(peer));
    const messages = all.filter((m) => m.seq > since);
    const nextSince =
      messages.length > 0
        ? (messages[messages.length - 1]?.seq ?? since)
        : since;
    return { messages, nextSince };
  }

  /** Backlog of the other peer's messages for an initial WebSocket push. */
  async backlogFor(peer: Peer): Promise<SignalingMessage[]> {
    return this.loadQueue(otherPeer(peer));
  }
}
