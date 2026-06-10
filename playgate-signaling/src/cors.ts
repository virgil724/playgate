/**
 * CORS helpers.
 *
 * PlayGate viewers call this Worker from the browser, which may be hosted on a
 * different origin (e.g. the game-viewer page).  We therefore need permissive
 * CORS headers on every response.
 */

export const CORS_HEADERS: HeadersInit = {
  "Access-Control-Allow-Origin": "*",
  "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
  "Access-Control-Allow-Headers": "Content-Type, Authorization",
  "Access-Control-Max-Age": "86400",
};

/** Wrap a Response with CORS headers. */
export function withCors(response: Response): Response {
  const headers = new Headers(response.headers);
  for (const [k, v] of Object.entries(CORS_HEADERS)) {
    headers.set(k, v);
  }
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

/** Handle CORS preflight (OPTIONS) requests. */
export function handleOptions(): Response {
  return new Response(null, { status: 204, headers: CORS_HEADERS });
}

/** Return a JSON error response with CORS headers. */
export function jsonError(message: string, status: number): Response {
  return withCors(
    new Response(JSON.stringify({ error: message }), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
}

/** Return a JSON success response with CORS headers. */
export function jsonOk(body: unknown, status = 200): Response {
  return withCors(
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
}
