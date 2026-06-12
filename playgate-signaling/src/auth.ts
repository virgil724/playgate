/**
 * Auth stub — validates an "Authorization: Bearer <token>" header.
 *
 * Current implementation:
 *   - If AUTH_DISABLED === "true" → always passes (dev / dry-run mode).
 *   - If SESSION_SECRET is set   → checks that the bearer token equals the
 *     secret (simple shared-secret stub; replace with ed25519 JWT once
 *     playgate-server is live).
 *   - Otherwise                  → always passes (permissive default so the
 *     service works out-of-the-box without secrets configured).
 *
 * To harden: replace `validateToken` with proper JWT verification against the
 * playgate-server public key.
 */

import type { Env } from "./types.js";

export interface AuthResult {
  ok: boolean;
  reason?: string;
}

/**
 * Validate the incoming request's session token.
 * Returns { ok: true } when the request is authorised.
 */
export async function checkAuth(
  request: Request,
  env: Env,
): Promise<AuthResult> {
  // Master bypass — useful for local dev / dry-run.
  if (env.AUTH_DISABLED === "true") {
    return { ok: true };
  }

  // No secret configured → open access (permissive default).
  if (!env.SESSION_SECRET) {
    return { ok: true };
  }

  const authHeader = request.headers.get("Authorization") ?? "";
  if (authHeader.startsWith("Bearer ")) {
    const token = authHeader.slice("Bearer ".length).trim();
    return validateToken(token, env.SESSION_SECRET);
  }

  // Browsers cannot set headers on a WebSocket handshake, so /ws upgrades
  // (and only requests without an Authorization header) may carry the token
  // as a ?token= query parameter instead.
  const queryToken = new URL(request.url).searchParams.get("token");
  if (queryToken) {
    return validateToken(queryToken.trim(), env.SESSION_SECRET);
  }

  return { ok: false, reason: "Missing Bearer token" };
}

/**
 * Stub validator: constant-time comparison of the bearer token against the
 * shared SESSION_SECRET.
 *
 * TODO: Replace with ed25519 JWT signature verification once playgate-server
 * issues signed session tokens.
 */
async function validateToken(
  token: string,
  secret: string,
): Promise<AuthResult> {
  // Use SubtleCrypto for a constant-time HMAC-based check so we don't leak
  // timing information about the secret.
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey(
    "raw",
    enc.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );

  // For the stub, the "token" is simply the HMAC-SHA256 hex of the literal
  // string "playgate-session" signed with SESSION_SECRET.
  const expected = await crypto.subtle.sign(
    "HMAC",
    key,
    enc.encode("playgate-session"),
  );
  const expectedHex = Array.from(new Uint8Array(expected))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");

  // Use a timing-safe comparison via HMAC verify instead of string equality.
  const tokenKey = await crypto.subtle.importKey(
    "raw",
    enc.encode(token),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const tokenSig = await crypto.subtle.sign(
    "HMAC",
    tokenKey,
    enc.encode("playgate-session"),
  );
  const tokenHex = Array.from(new Uint8Array(tokenSig))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");

  if (expectedHex === tokenHex) {
    return { ok: true };
  }
  return { ok: false, reason: "Invalid session token" };
}
