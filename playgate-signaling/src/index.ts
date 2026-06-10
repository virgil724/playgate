/**
 * PlayGate Signaling Worker — entry point.
 *
 * Routes:
 *   OPTIONS *                    → CORS preflight
 *   POST /rooms/:roomId/:peer    → push signaling message  (T7)
 *   GET  /rooms/:roomId/:peer    → poll signaling messages (T7)
 *   POST /turn/credentials       → issue TURN credentials  (T8)
 *   GET  /healthz                → liveness probe
 */

import type { Env } from "./types.js";
import { handleOptions, jsonError, jsonOk } from "./cors.js";
import { checkAuth } from "./auth.js";
import { handlePost, handleGet } from "./rooms.js";
import { handleTurnCredentials } from "./turn.js";

// Route pattern: /rooms/<roomId>/<peer>
const ROOMS_RE = /^\/rooms\/([^/]+)\/(host|viewer)$/;

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

    // Auth check (all other routes)
    const auth = await checkAuth(request, env);
    if (!auth.ok) {
      return jsonError(auth.reason ?? "Unauthorized", 401);
    }

    // POST /turn/credentials
    if (method === "POST" && path === "/turn/credentials") {
      return handleTurnCredentials(request, env);
    }

    // /rooms/:roomId/:peer
    const roomsMatch = ROOMS_RE.exec(path);
    if (roomsMatch) {
      const roomId = roomsMatch[1]!;
      const peer = roomsMatch[2]!;

      if (method === "POST") {
        return handlePost(request, env, roomId, peer);
      }
      if (method === "GET") {
        return handleGet(request, env, roomId, peer);
      }
      return jsonError("Method not allowed", 405);
    }

    return jsonError("Not found", 404);
  },
} satisfies ExportedHandler<Env>;
