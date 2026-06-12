/**
 * T8 — TURN credential endpoint:
 *
 *   POST /turn/credentials
 *
 * Calls the Cloudflare Realtime TURN credential generation API and returns
 * short-lived TURN/STUN credentials ready to be used as `iceServers` in
 * RTCPeerConnection.
 *
 * Required Worker secrets:
 *   TURN_KEY_ID        — Cloudflare Realtime TURN key ID
 *   TURN_KEY_API_TOKEN — Cloudflare Realtime TURN API token
 *
 * Cloudflare API reference:
 *   POST https://rtc.live.cloudflare.com/v1/turn/keys/{key_id}/credentials/generate
 *
 * The API responds with a bare ICE server object (no success/result envelope):
 *   { "iceServers": { "urls": [...], "username": "...", "credential": "..." } }
 */

import type {
  Env,
  TurnCredentialsResponse,
  CloudflareCredentialResponse,
  RTCIceServer,
} from "./types.js";
import { jsonError, jsonOk } from "./cors.js";

const CF_TURN_BASE = "https://rtc.live.cloudflare.com/v1/turn/keys";

/**
 * POST /turn/credentials
 *
 * Optional request body: { ttl?: number }  (default 86 400 s / 24 h)
 * Response: TurnCredentialsResponse
 */
export async function handleTurnCredentials(
  request: Request,
  env: Env,
): Promise<Response> {
  if (!env.TURN_KEY_ID || !env.TURN_KEY_API_TOKEN) {
    return jsonError(
      "TURN credentials not configured — set TURN_KEY_ID and TURN_KEY_API_TOKEN secrets",
      503,
    );
  }

  // Optional TTL override from caller (capped at 24 h).
  let requestedTtl = 86_400;
  try {
    const body = await request.json() as Record<string, unknown>;
    if (typeof body.ttl === "number" && body.ttl > 0) {
      requestedTtl = Math.min(body.ttl, 86_400);
    }
  } catch {
    // Body is optional; ignore parse failures.
  }

  const url = `${CF_TURN_BASE}/${env.TURN_KEY_ID}/credentials/generate`;

  let cfResponse: Response;
  try {
    cfResponse = await fetch(url, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${env.TURN_KEY_API_TOKEN}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ ttl: requestedTtl }),
    });
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return jsonError(`Failed to reach Cloudflare TURN API: ${msg}`, 502);
  }

  if (!cfResponse.ok) {
    const text = await cfResponse.text();
    return jsonError(
      `Cloudflare TURN API returned ${cfResponse.status}: ${text}`,
      502,
    );
  }

  const data = (await cfResponse.json()) as CloudflareCredentialResponse;

  const ice = data.iceServers;
  if (!ice?.username || !ice.credential || !ice.urls?.length) {
    return jsonError("Cloudflare TURN API returned an unexpected response shape", 502);
  }

  // Build iceServers array:
  //   1. STUN entry (no credentials needed; harmless if urls already has one)
  //   2. TURN entry (carries username + credential)
  const iceServers: RTCIceServer[] = [
    // Cloudflare free STUN
    { urls: "stun:stun.cloudflare.com:3478" },
    // Authenticated TURN
    { urls: ice.urls, username: ice.username, credential: ice.credential },
  ];

  // The API does not echo a TTL back; report the one we requested.
  const responseBody: TurnCredentialsResponse = { iceServers, ttl: requestedTtl };
  return jsonOk(responseBody);
}
