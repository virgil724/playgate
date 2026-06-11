/**
 * T7 — Signaling endpoints:
 *
 *   POST /rooms/:roomId/:peer   — push a message (SDP offer/answer or ICE candidate)
 *   GET  /rooms/:roomId/:peer   — poll messages posted by the *other* peer
 *                                 optional ?since=<seq> to skip already-seen messages
 *
 * Storage layout (Workers KV):
 *   key  = "room:{roomId}:{peer}"
 *   value = JSON PeerQueue
 *   TTL  = MESSAGE_TTL_SECONDS  (default 300 s / 5 min)
 *
 * Peers:
 *   "host"   — the Go+Pion host process
 *   "viewer" — the browser viewer
 */

import type { Env, PeerQueue, SignalingMessage, MessagesResponse } from "./types.js";
import { jsonError, jsonOk } from "./cors.js";

export const MESSAGE_TTL_SECONDS = 300; // 5 minutes
const MAX_QUEUE_LENGTH = 200; // safety cap per peer per room
const VALID_PEERS = new Set(["host", "viewer"]);

function kvKey(roomId: string, peer: string): string {
  return `room:${roomId}:${peer}`;
}

function otherPeer(peer: string): string {
  return peer === "host" ? "viewer" : "host";
}

/**
 * True when the payload is a JSON object with type === "offer".
 * Defensive: a malformed / non-object payload must never crash handlePost and
 * falls back to the plain append path.
 */
function isOfferPayload(payload: unknown): boolean {
  return (
    typeof payload === "object" &&
    payload !== null &&
    (payload as { type?: unknown }).type === "offer"
  );
}

/**
 * POST /rooms/:roomId/:peer
 * Body: any JSON object (SDP offer/answer or ICE candidate)
 * Appends the message to the sending peer's KV queue. A host SDP offer instead
 * starts a fresh signaling session: it replaces the host queue (seq-continuous)
 * and deletes the viewer queue — see inline comments below.
 */
export async function handlePost(
  request: Request,
  env: Env,
  roomId: string,
  peer: string,
): Promise<Response> {
  if (!VALID_PEERS.has(peer)) {
    return jsonError(`peer must be "host" or "viewer", got "${peer}"`, 400);
  }

  let payload: unknown;
  try {
    payload = await request.json();
  } catch {
    return jsonError("Request body must be valid JSON", 400);
  }

  const key = kvKey(roomId, peer);

  // Read-modify-write (Workers KV is eventually consistent; for signaling this
  // is fine — messages are short-lived and ordering is best-effort).
  const existing = await env.SIGNALING_KV.get<PeerQueue>(key, "json");
  const queue: PeerQueue = existing ?? { messages: [] };

  // Truncate if queue is suspiciously long (protection against abuse).
  if (queue.messages.length >= MAX_QUEUE_LENGTH) {
    queue.messages = queue.messages.slice(-MAX_QUEUE_LENGTH + 1);
  }

  const msg: SignalingMessage = {
    seq: (queue.messages[queue.messages.length - 1]?.seq ?? -1) + 1,
    ts: new Date().toISOString(),
    payload,
  };

  if (peer === "host" && isOfferPayload(payload)) {
    // A host offer starts a NEW signaling session: every previously queued
    // host message belongs to a dead peer (stale ICE ufrag/pwd + DTLS
    // fingerprint), so keep only the new offer. Seq continuity is preserved
    // (msg.seq above is last-old-seq + 1): a browser that was already polling
    // holds a large `since`, and if seq restarted at 0 the `seq > since`
    // filter in handleGet would hide the new offer from it forever.
    queue.messages = [msg];
    // Stale viewer answers must not survive a new offer either — they answer
    // a dead peer. Delete the viewer queue entirely; the host always starts
    // polling from since=-1 per connection, so its seq restarting at 0 is fine.
    await env.SIGNALING_KV.delete(kvKey(roomId, otherPeer(peer)));
  } else {
    queue.messages.push(msg);
  }

  await env.SIGNALING_KV.put(key, JSON.stringify(queue), {
    expirationTtl: MESSAGE_TTL_SECONDS,
  });

  return jsonOk({ seq: msg.seq, ts: msg.ts }, 201);
}

/**
 * GET /rooms/:roomId/:peer?since=n
 *
 * Returns messages posted by the *other* peer that the calling peer hasn't
 * seen yet.  `since` is the last seq number the caller has processed; the
 * response contains messages with seq > since.
 *
 * When `since` is omitted all messages are returned.
 */
export async function handleGet(
  request: Request,
  env: Env,
  roomId: string,
  peer: string,
): Promise<Response> {
  if (!VALID_PEERS.has(peer)) {
    return jsonError(`peer must be "host" or "viewer", got "${peer}"`, 400);
  }

  const url = new URL(request.url);
  const sinceParam = url.searchParams.get("since");
  const since = sinceParam !== null ? parseInt(sinceParam, 10) : -1;

  if (sinceParam !== null && (isNaN(since) || since < -1)) {
    return jsonError("`since` must be a non-negative integer", 400);
  }

  // A peer polls for messages posted by the *other* peer.
  const senderPeer = otherPeer(peer);
  const key = kvKey(roomId, senderPeer);

  const stored = await env.SIGNALING_KV.get<PeerQueue>(key, "json");
  const allMessages: SignalingMessage[] = stored?.messages ?? [];
  const filtered = allMessages.filter((m) => m.seq > since);

  const nextSince =
    filtered.length > 0
      ? (filtered[filtered.length - 1]?.seq ?? since)
      : since;

  const body: MessagesResponse = { messages: filtered, nextSince };
  return jsonOk(body);
}
