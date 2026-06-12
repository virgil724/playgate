import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { SignalingClient, classifySignal, FALLBACK_ICE_SERVERS } from "./signaling";

describe("classifySignal", () => {
  it("detects offers, answers, candidates", () => {
    expect(classifySignal({ type: "offer", sdp: "x" })).toBe("offer");
    expect(classifySignal({ type: "answer", sdp: "x" })).toBe("answer");
    expect(classifySignal({ candidate: "candidate:..." })).toBe("candidate");
    expect(classifySignal({ foo: 1 })).toBe("unknown");
    expect(classifySignal(null)).toBe("unknown");
  });
});

describe("SignalingClient", () => {
  const baseUrl = "http://signal.test";
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("viewer pushes to /viewer", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({}) });
    const c = new SignalingClient({ baseUrl, roomId: "r1", peer: "viewer" });
    await c.push({ type: "answer", sdp: "x" });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("http://signal.test/rooms/r1/viewer");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ type: "answer", sdp: "x" });
  });

  it("viewer polls the host's messages and advances since", async () => {
    fetchMock.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        messages: [{ seq: 0, ts: "t", payload: { type: "offer", sdp: "s" } }],
        nextSince: 0,
      }),
    });
    const c = new SignalingClient({ baseUrl, roomId: "r1", peer: "viewer" });
    const msgs = await c.poll();
    expect(msgs).toHaveLength(1);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "http://signal.test/rooms/r1/viewer?since=-1",
    );

    // Second poll uses advanced since.
    fetchMock.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ messages: [], nextSince: 0 }),
    });
    await c.poll();
    expect(fetchMock.mock.calls[1][0]).toBe(
      "http://signal.test/rooms/r1/viewer?since=0",
    );
  });

  it("includes Bearer token when provided", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({}) });
    const c = new SignalingClient({ baseUrl, roomId: "r1", token: "jwt123" });
    await c.push({ type: "answer" });
    const opts = fetchMock.mock.calls[0][1];
    expect((opts.headers as Record<string, string>)["Authorization"]).toBe(
      "Bearer jwt123",
    );
  });

  it("returns TURN iceServers on success", async () => {
    const iceServers = [{ urls: "turn:turn.test" }];
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ iceServers }) });
    const c = new SignalingClient({ baseUrl, roomId: "r1" });
    expect(await c.fetchIceServers()).toEqual(iceServers);
  });

  it("falls back to STUN on TURN failure", async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 500 });
    const c = new SignalingClient({ baseUrl, roomId: "r1" });
    expect(await c.fetchIceServers()).toEqual(FALLBACK_ICE_SERVERS);
  });

  it("falls back to STUN when iceServers empty", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ iceServers: [] }) });
    const c = new SignalingClient({ baseUrl, roomId: "r1" });
    expect(await c.fetchIceServers()).toEqual(FALLBACK_ICE_SERVERS);
  });

  it("throws on push failure", async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 403 });
    const c = new SignalingClient({ baseUrl, roomId: "r1" });
    await expect(c.push({})).rejects.toThrow(/403/);
  });
});
