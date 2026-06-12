/**
 * Unit tests for T8 TURN credential endpoint (turn.ts).
 *
 * The Cloudflare TURN API call is intercepted via vi.stubGlobal("fetch", …)
 * so no real network requests are made.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import worker from "../index.js";
import { makeEnv, makeRequest } from "./helpers.js";
import type { TurnCredentialsResponse } from "../types.js";

const BASE = "https://signaling.example.com";

/**
 * Build a mock Cloudflare TURN API success response — a bare ICE server
 * object (no success/result envelope), matching the live API.
 */
function mockCfTurnResponse(overrides?: Partial<{
  username: string;
  credential: string;
  urls: string[];
}>): Response {
  const iceServers = {
    username: "test-user",
    credential: "test-cred",
    urls: [
      "stun:stun.cloudflare.com:3478",
      "turn:turn.cloudflare.com:3478?transport=udp",
      "turn:turn.cloudflare.com:3478?transport=tcp",
      "turns:turn.cloudflare.com:5349?transport=tcp",
    ],
    ...overrides,
  };
  return new Response(
    JSON.stringify({ iceServers }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

describe("POST /turn/credentials", () => {
  let originalFetch: typeof globalThis.fetch;

  beforeEach(() => {
    originalFetch = globalThis.fetch;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    vi.restoreAllMocks();
  });

  it("returns 503 when TURN secrets are not configured", async () => {
    const env = makeEnv({ TURN_KEY_ID: undefined, TURN_KEY_API_TOKEN: undefined });
    const req = makeRequest("POST", `${BASE}/turn/credentials`);
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(503);
  });

  it("returns valid iceServers on success", async () => {
    globalThis.fetch = vi.fn().mockResolvedValueOnce(mockCfTurnResponse());

    const env = makeEnv({
      TURN_KEY_ID: "test-key-id",
      TURN_KEY_API_TOKEN: "test-token",
    });
    const req = makeRequest("POST", `${BASE}/turn/credentials`);
    const res = await worker.fetch(req, env, {} as ExecutionContext);

    expect(res.status).toBe(200);
    const body = await res.json() as TurnCredentialsResponse;

    expect(body.ttl).toBe(86400);
    expect(Array.isArray(body.iceServers)).toBe(true);

    // Should include STUN entry
    const stunEntry = body.iceServers.find(
      (s) =>
        (typeof s.urls === "string" && s.urls.startsWith("stun:")) ||
        (Array.isArray(s.urls) && (s.urls as string[]).some((u) => u.startsWith("stun:"))),
    );
    expect(stunEntry).toBeDefined();

    // Should include TURN entry with credentials
    const turnEntry = body.iceServers.find((s) => s.username && s.credential);
    expect(turnEntry).toBeDefined();
    expect(turnEntry!.username).toBe("test-user");
    expect(turnEntry!.credential).toBe("test-cred");
  });

  it("forwards correct Authorization header to Cloudflare API", async () => {
    const mockFetch = vi.fn().mockResolvedValueOnce(mockCfTurnResponse());
    globalThis.fetch = mockFetch;

    const env = makeEnv({
      TURN_KEY_ID: "my-key-id",
      TURN_KEY_API_TOKEN: "my-api-token",
    });
    await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`),
      env,
      {} as ExecutionContext,
    );

    expect(mockFetch).toHaveBeenCalledOnce();
    const [calledUrl, calledInit] = mockFetch.mock.calls[0] as [string, RequestInit];
    expect(calledUrl).toContain("my-key-id");
    expect((calledInit.headers as Record<string, string>)["Authorization"]).toBe(
      "Bearer my-api-token",
    );
  });

  it("returns 502 when Cloudflare API returns error status", async () => {
    globalThis.fetch = vi.fn().mockResolvedValueOnce(
      new Response("Internal Server Error", { status: 500 }),
    );

    const env = makeEnv({
      TURN_KEY_ID: "key",
      TURN_KEY_API_TOKEN: "token",
    });
    const res = await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`),
      env,
      {} as ExecutionContext,
    );
    expect(res.status).toBe(502);
  });

  it("returns 502 when fetch throws a network error", async () => {
    globalThis.fetch = vi.fn().mockRejectedValueOnce(new Error("network failure"));

    const env = makeEnv({ TURN_KEY_ID: "k", TURN_KEY_API_TOKEN: "t" });
    const res = await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`),
      env,
      {} as ExecutionContext,
    );
    expect(res.status).toBe(502);
  });

  it("respects optional TTL in request body (capped at 86400)", async () => {
    const mockFetch = vi.fn().mockResolvedValueOnce(mockCfTurnResponse());
    globalThis.fetch = mockFetch;

    const env = makeEnv({ TURN_KEY_ID: "k", TURN_KEY_API_TOKEN: "t" });
    await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`, { ttl: 3600 }),
      env,
      {} as ExecutionContext,
    );

    const sentBody = JSON.parse(
      (mockFetch.mock.calls[0] as [string, RequestInit])[1].body as string,
    ) as { ttl: number };
    expect(sentBody.ttl).toBe(3600);
  });

  it("caps TTL at 86400 even if caller requests more", async () => {
    const mockFetch = vi.fn().mockResolvedValueOnce(mockCfTurnResponse());
    globalThis.fetch = mockFetch;

    const env = makeEnv({ TURN_KEY_ID: "k", TURN_KEY_API_TOKEN: "t" });
    await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`, { ttl: 999_999 }),
      env,
      {} as ExecutionContext,
    );

    const sentBody = JSON.parse(
      (mockFetch.mock.calls[0] as [string, RequestInit])[1].body as string,
    ) as { ttl: number };
    expect(sentBody.ttl).toBe(86400);
  });

  it("attaches CORS headers to response", async () => {
    globalThis.fetch = vi.fn().mockResolvedValueOnce(mockCfTurnResponse());

    const env = makeEnv({ TURN_KEY_ID: "k", TURN_KEY_API_TOKEN: "t" });
    const res = await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`),
      env,
      {} as ExecutionContext,
    );
    expect(res.headers.get("Access-Control-Allow-Origin")).toBe("*");
  });

  it("handles an unexpected Cloudflare API response shape", async () => {
    globalThis.fetch = vi.fn().mockResolvedValueOnce(
      new Response(
        JSON.stringify({ success: false, result: null, errors: ["bad"], messages: [] }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    const env = makeEnv({ TURN_KEY_ID: "k", TURN_KEY_API_TOKEN: "t" });
    const res = await worker.fetch(
      makeRequest("POST", `${BASE}/turn/credentials`),
      env,
      {} as ExecutionContext,
    );
    expect(res.status).toBe(502);
  });
});
