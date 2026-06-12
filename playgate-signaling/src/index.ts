/**
 * PlayGate Signaling Worker — entry point.
 *
 * Routes:
 *   OPTIONS *                       → CORS preflight
 *   POST /rooms/:roomId/:peer       → push signaling message      (DO-backed)
 *   GET  /rooms/:roomId/:peer       → poll messages (+ ?wait long-poll)
 *   GET  /rooms/:roomId/:peer/ws    → WebSocket push (Upgrade: websocket)
 *   POST /turn/credentials          → issue TURN credentials
 *   GET  /healthz                   → liveness probe
 *
 * Room state lives in a per-room Durable Object (see RoomDO); the Worker only
 * authenticates, handles CORS/turn/healthz, and forwards /rooms/* to the DO
 * selected by `env.ROOMS.idFromName(roomId)`.
 */

import type { Env } from "./types.js";
import { handleOptions, jsonError, jsonOk, withCors } from "./cors.js";
import { checkAuth } from "./auth.js";
import { handleTurnCredentials } from "./turn.js";

// Route pattern: /rooms/<roomId>/<peer>[/ws]
const ROOMS_RE = /^\/rooms\/([^/]+)\/(host|viewer)(?:\/ws)?$/;

export { RoomDO } from "./roomDO.js";

export default {
  async fetch(
    request: Request,
    env: Env,
    _ctx: ExecutionContext,
  ): Promise<Response> {
    const method = request.method.toUpperCase();

    // CORS preflight
    if (method === "OPTIONS") {
      return handleOptions();
    }

    const url = new URL(request.url);
    const path = url.pathname;

    // Liveness probe — no auth required
    if (method === "GET" && path === "/healthz") {
      return jsonOk({ status: "ok" });
    }

    const isWsUpgrade =
      method === "GET" && request.headers.get("Upgrade") === "websocket";

    // Auth check (all other routes). WebSocket upgrades cannot carry headers
    // from a browser, so checkAuth also accepts a ?token= query parameter.
    const auth = await checkAuth(request, env);
    if (!auth.ok) {
      return jsonError(auth.reason ?? "Unauthorized", 401);
    }

    // POST /turn/credentials
    if (method === "POST" && path === "/turn/credentials") {
      return handleTurnCredentials(request, env);
    }

    // /rooms/:roomId/:peer[/ws] → forward to the per-room Durable Object.
    const roomsMatch = ROOMS_RE.exec(path);
    if (roomsMatch) {
      if (method !== "GET" && method !== "POST") {
        return jsonError("Method not allowed", 405);
      }
      const roomId = roomsMatch[1]!;
      const id = env.ROOMS.idFromName(roomId);
      const stub = env.ROOMS.get(id);
      const doResponse = await stub.fetch(request);

      // WebSocket upgrade (101) responses must be returned as-is; CORS headers
      // and body rewrapping are not valid on a switching-protocols response.
      if (isWsUpgrade || doResponse.status === 101) {
        return doResponse;
      }
      return withCors(doResponse);
    }

    return jsonError("Not found", 404);
  },
} satisfies ExportedHandler<Env>;
