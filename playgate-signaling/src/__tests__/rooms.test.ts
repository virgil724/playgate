/**
 * Unit tests for T7 signaling endpoints (rooms.ts + index.ts routing).
 *
 * Tests cover the full offer/answer / ICE candidate exchange flow:
 *   1. Host pushes SDP offer
 *   2. Viewer polls and receives it
 *   3. Viewer pushes SDP answer
 *   4. Host polls and receives it
 *   5. ICE candidate exchange in both directions
 *   6. `since` filtering works correctly
 *   7. Invalid inputs return appropriate errors
 */

import { describe, it, expect, beforeEach } from "vitest";
import worker from "../index.js";
import { makeEnv, makeRequest, MockKVNamespace } from "./helpers.js";
import type { MessagesResponse } from "../types.js";

const BASE = "https://signaling.example.com";

describe("POST /rooms/:roomId/:peer", () => {
  let env: ReturnType<typeof makeEnv>;

  beforeEach(() => {
    env = makeEnv();
  });

  it("accepts a valid SDP offer from host", async () => {
    const offer = { type: "offer", sdp: "v=0\r\no=- 123 ...\r\n" };
    const req = makeRequest("POST", `${BASE}/rooms/room1/host`, offer);
    const res = await worker.fetch(req, env, {} as ExecutionContext);

    expect(res.status).toBe(201);
    const body = await res.json() as { seq: number; ts: string };
    expect(body.seq).toBe(0);
    expect(typeof body.ts).toBe("string");
  });

  it("accepts an ICE candidate from viewer", async () => {
    const candidate = { candidate: "candidate:1 1 UDP ...", sdpMid: "0" };
    const req = makeRequest("POST", `${BASE}/rooms/room1/viewer`, candidate);
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(201);
  });

  it("rejects unknown peer name with 404 (unmatched route)", async () => {
    // The route regex only accepts /host|viewer/, so /spectator routes return 404.
    const req = makeRequest("POST", `${BASE}/rooms/room1/spectator`, { sdp: "..." });
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(404);
  });

  it("rejects non-JSON body", async () => {
    const req = new Request(`${BASE}/rooms/room1/host`, {
      method: "POST",
      headers: { "Content-Type": "text/plain" },
      body: "not json",
    });
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(400);
  });

  it("assigns incrementing seq numbers", async () => {
    const post = (payload: unknown) =>
      worker.fetch(
        makeRequest("POST", `${BASE}/rooms/seqroom/host`, payload),
        env,
        {} as ExecutionContext,
      );

    const r0 = await (await post({ n: 0 })).json() as { seq: number };
    const r1 = await (await post({ n: 1 })).json() as { seq: number };
    const r2 = await (await post({ n: 2 })).json() as { seq: number };

    expect(r0.seq).toBe(0);
    expect(r1.seq).toBe(1);
    expect(r2.seq).toBe(2);
  });
});

describe("GET /rooms/:roomId/:peer", () => {
  let env: ReturnType<typeof makeEnv>;

  beforeEach(() => {
    env = makeEnv();
  });

  it("returns empty list when no messages have been posted", async () => {
    const req = makeRequest("GET", `${BASE}/rooms/empty/viewer`);
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(200);
    const body = await res.json() as MessagesResponse;
    expect(body.messages).toHaveLength(0);
    expect(body.nextSince).toBe(-1);
  });

  it("viewer can read messages posted by host", async () => {
    const offer = { type: "offer", sdp: "v=0\r\n" };

    // Host posts
    await worker.fetch(
      makeRequest("POST", `${BASE}/rooms/r2/host`, offer),
      env,
      {} as ExecutionContext,
    );

    // Viewer polls
    const res = await worker.fetch(
      makeRequest("GET", `${BASE}/rooms/r2/viewer`),
      env,
      {} as ExecutionContext,
    );
    expect(res.status).toBe(200);
    const body = await res.json() as MessagesResponse;
    expect(body.messages).toHaveLength(1);
    expect(body.messages[0]!.payload).toEqual(offer);
    expect(body.nextSince).toBe(0);
  });

  it("host can read messages posted by viewer", async () => {
    const answer = { type: "answer", sdp: "v=0\r\n" };

    // Viewer posts answer
    await worker.fetch(
      makeRequest("POST", `${BASE}/rooms/r3/viewer`, answer),
      env,
      {} as ExecutionContext,
    );

    // Host polls
    const res = await worker.fetch(
      makeRequest("GET", `${BASE}/rooms/r3/host`),
      env,
      {} as ExecutionContext,
    );
    const body = await res.json() as MessagesResponse;
    expect(body.messages).toHaveLength(1);
    expect(body.messages[0]!.payload).toEqual(answer);
  });

  it("since param filters out already-seen messages", async () => {
    // Host posts three messages
    for (let i = 0; i < 3; i++) {
      await worker.fetch(
        makeRequest("POST", `${BASE}/rooms/r4/host`, { n: i }),
        env,
        {} as ExecutionContext,
      );
    }

    // Viewer polls with since=1 — should only get seq 2
    const res = await worker.fetch(
      makeRequest("GET", `${BASE}/rooms/r4/viewer?since=1`),
      env,
      {} as ExecutionContext,
    );
    const body = await res.json() as MessagesResponse;
    expect(body.messages).toHaveLength(1);
    expect(body.messages[0]!.seq).toBe(2);
    expect(body.nextSince).toBe(2);
  });

  it("nextSince advances correctly across multiple polls", async () => {
    const post = (payload: unknown) =>
      worker.fetch(
        makeRequest("POST", `${BASE}/rooms/r5/host`, payload),
        env,
        {} as ExecutionContext,
      );
    const poll = (since?: number) =>
      worker.fetch(
        makeRequest(
          "GET",
          `${BASE}/rooms/r5/viewer${since !== undefined ? `?since=${since}` : ""}`,
        ),
        env,
        {} as ExecutionContext,
      );

    // Post 2 messages
    await post({ m: "offer" });
    await post({ m: "ice1" });

    const first = await (await poll()).json() as MessagesResponse;
    expect(first.messages).toHaveLength(2);
    expect(first.nextSince).toBe(1);

    // Post 1 more
    await post({ m: "ice2" });

    const second = await (
      await poll(first.nextSince)
    ).json() as MessagesResponse;
    expect(second.messages).toHaveLength(1);
    expect(second.messages[0]!.payload).toEqual({ m: "ice2" });
  });

  it("rejects unknown peer name with 404 (unmatched route)", async () => {
    // Route regex only accepts /host|viewer/, so /badpeer returns 404.
    const res = await worker.fetch(
      makeRequest("GET", `${BASE}/rooms/r/badpeer`),
      env,
      {} as ExecutionContext,
    );
    expect(res.status).toBe(404);
  });

  it("rejects invalid since param", async () => {
    const res = await worker.fetch(
      makeRequest("GET", `${BASE}/rooms/r6/viewer?since=abc`),
      env,
      {} as ExecutionContext,
    );
    expect(res.status).toBe(400);
  });
});

describe("Full offer/answer + ICE exchange flow", () => {
  it("completes a WebRTC pairing round-trip", async () => {
    const env = makeEnv();
    const roomId = "pairing-room";

    // ── Step 1: Host posts SDP offer ──────────────────────────────────────
    const offer = { type: "offer", sdp: "v=0\r\no=host 1 1 IN IP4 0.0.0.0\r\n" };
    const postOffer = await worker.fetch(
      makeRequest("POST", `${BASE}/rooms/${roomId}/host`, offer),
      env,
      {} as ExecutionContext,
    );
    expect(postOffer.status).toBe(201);

    // ── Step 2: Viewer polls and receives the offer ───────────────────────
    const getOffer = await (
      await worker.fetch(
        makeRequest("GET", `${BASE}/rooms/${roomId}/viewer`),
        env,
        {} as ExecutionContext,
      )
    ).json() as MessagesResponse;

    expect(getOffer.messages).toHaveLength(1);
    expect(getOffer.messages[0]!.payload).toEqual(offer);
    const viewerSince = getOffer.nextSince; // = 0

    // ── Step 3: Viewer posts SDP answer ───────────────────────────────────
    const answer = { type: "answer", sdp: "v=0\r\no=viewer 2 2 IN IP4 0.0.0.0\r\n" };
    await worker.fetch(
      makeRequest("POST", `${BASE}/rooms/${roomId}/viewer`, answer),
      env,
      {} as ExecutionContext,
    );

    // ── Step 4: Host polls and receives the answer ────────────────────────
    const getAnswer = await (
      await worker.fetch(
        makeRequest("GET", `${BASE}/rooms/${roomId}/host`),
        env,
        {} as ExecutionContext,
      )
    ).json() as MessagesResponse;

    expect(getAnswer.messages).toHaveLength(1);
    expect(getAnswer.messages[0]!.payload).toEqual(answer);
    const hostSince = getAnswer.nextSince; // = 0

    // ── Step 5: ICE candidates ─────────────────────────────────────────────
    const hostIce = { candidate: "candidate:1 1 UDP 2130706431 192.168.1.1 54321 typ host" };
    const viewerIce = { candidate: "candidate:1 1 UDP 2130706431 10.0.0.1 12345 typ host" };

    await worker.fetch(
      makeRequest("POST", `${BASE}/rooms/${roomId}/host`, hostIce),
      env,
      {} as ExecutionContext,
    );
    await worker.fetch(
      makeRequest("POST", `${BASE}/rooms/${roomId}/viewer`, viewerIce),
      env,
      {} as ExecutionContext,
    );

    // Viewer polls for new host messages (since=0, should get hostIce at seq=1)
    const viewerIcePoll = await (
      await worker.fetch(
        makeRequest("GET", `${BASE}/rooms/${roomId}/viewer?since=${viewerSince}`),
        env,
        {} as ExecutionContext,
      )
    ).json() as MessagesResponse;

    expect(viewerIcePoll.messages).toHaveLength(1);
    expect(viewerIcePoll.messages[0]!.payload).toEqual(hostIce);

    // Host polls for new viewer messages (since=0, should get viewerIce at seq=1)
    const hostIcePoll = await (
      await worker.fetch(
        makeRequest("GET", `${BASE}/rooms/${roomId}/host?since=${hostSince}`),
        env,
        {} as ExecutionContext,
      )
    ).json() as MessagesResponse;

    expect(hostIcePoll.messages).toHaveLength(1);
    expect(hostIcePoll.messages[0]!.payload).toEqual(viewerIce);
  });
});

describe("CORS", () => {
  it("responds to OPTIONS preflight with 204", async () => {
    const env = makeEnv();
    const req = new Request(`${BASE}/rooms/r/host`, { method: "OPTIONS" });
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(204);
    expect(res.headers.get("Access-Control-Allow-Origin")).toBe("*");
  });

  it("attaches CORS headers to POST responses", async () => {
    const env = makeEnv();
    const req = makeRequest("POST", `${BASE}/rooms/cors-room/host`, { sdp: "v=0" });
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.headers.get("Access-Control-Allow-Origin")).toBe("*");
  });
});

describe("Auth stub", () => {
  it("returns 200 when AUTH_DISABLED=true", async () => {
    const env = makeEnv({ AUTH_DISABLED: "true" });
    const req = makeRequest("GET", `${BASE}/rooms/r/viewer`);
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(200);
  });

  it("returns 401 when SESSION_SECRET set but no token provided", async () => {
    const env = makeEnv({ AUTH_DISABLED: undefined, SESSION_SECRET: "s3cr3t" });
    const req = makeRequest("GET", `${BASE}/rooms/r/viewer`);
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(401);
  });

  it("healthz is always reachable without auth", async () => {
    const env = makeEnv({ AUTH_DISABLED: undefined, SESSION_SECRET: "s3cr3t" });
    const req = makeRequest("GET", `${BASE}/healthz`);
    const res = await worker.fetch(req, env, {} as ExecutionContext);
    expect(res.status).toBe(200);
  });
});
