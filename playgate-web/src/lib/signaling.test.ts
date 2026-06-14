import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { SignalingClient, classifySignal, FALLBACK_ICE_SERVERS } from "./signaling";
import type { SignalingMessage } from "./signaling";

// ---------------------------------------------------------------------------
// FakeWebSocket — minimal WebSocket test double
//
// The test environment is Node (not jsdom), so DOM event constructors like
// CloseEvent / MessageEvent are not available.  We use plain objects that
// satisfy the handler signatures instead.
// ---------------------------------------------------------------------------

class FakeWebSocket {
  url: string;
  readyState: number = 0; // CONNECTING

  onopen: ((ev: { type: string }) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: ((ev: { message?: string }) => void) | null = null;
  onclose: ((ev: { code: number; wasClean: boolean }) => void) | null = null;

  /** Messages the SUT sent to the server. */
  sent: string[] = [];

  /** All FakeWebSocket instances created in the current test. */
  static instances: FakeWebSocket[] = [];

  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }

  send(data: string) {
    this.sent.push(data);
  }

  close(code?: number, _reason?: string) {
    if (this.readyState === 3 /* CLOSED */) return;
    this.readyState = 3;
    this.onclose?.({ code: code ?? 1000, wasClean: true });
  }

  // Test helpers — fire events from the "server" side.

  serverOpen() {
    this.readyState = 1; // OPEN
    this.onopen?.({ type: "open" });
  }

  serverSend(f: SignalingMessage) {
    this.onmessage?.({ data: JSON.stringify(f) });
  }

  serverError() {
    this.onerror?.({ message: "connection refused" });
  }

  serverClose(code = 1006) {
    this.readyState = 3;
    this.onclose?.({ code, wasClean: false });
  }
}

// Factory that hands back FakeWebSocket instances in order.
function makeFakeFactory(): (url: string) => WebSocket {
  return (url: string) => new FakeWebSocket(url) as unknown as WebSocket;
}

// Shorthand for a fake message frame.
function frame(seq: number, payload: unknown = { type: "offer" }): SignalingMessage {
  return { seq, ts: new Date().toISOString(), payload };
}

// ---------------------------------------------------------------------------
// classifySignal
// ---------------------------------------------------------------------------

describe("classifySignal", () => {
  it("detects offers, answers, candidates", () => {
    expect(classifySignal({ type: "offer", sdp: "x" })).toBe("offer");
    expect(classifySignal({ type: "answer", sdp: "x" })).toBe("answer");
    expect(classifySignal({ candidate: "candidate:..." })).toBe("candidate");
    expect(classifySignal({ foo: 1 })).toBe("unknown");
    expect(classifySignal(null)).toBe("unknown");
  });
});

// ---------------------------------------------------------------------------
// HTTP-only tests (existing coverage, kept intact)
// ---------------------------------------------------------------------------

describe("SignalingClient (HTTP)", () => {
  const baseUrl = "http://signal.test";
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    FakeWebSocket.instances = [];
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("viewer pushes to /viewer", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({}) });
    const c = new SignalingClient({ baseUrl, roomId: "r1", peer: "viewer", wsFactory: null });
    await c.push({ type: "answer", sdp: "x" });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("http://signal.test/rooms/r1/viewer");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body as string)).toEqual({ type: "answer", sdp: "x", viewerId: c.viewerId });
  });

  it("viewer polls the host's messages and advances since", async () => {
    fetchMock.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        messages: [{ seq: 0, ts: "t", payload: { type: "offer", sdp: "s" } }],
        nextSince: 0,
      }),
    });
    const c = new SignalingClient({ baseUrl, roomId: "r1", peer: "viewer", wsFactory: null });
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
    const c = new SignalingClient({ baseUrl, roomId: "r1", token: "jwt123", wsFactory: null });
    await c.push({ type: "answer" });
    const opts = fetchMock.mock.calls[0][1];
    expect((opts.headers as Record<string, string>)["Authorization"]).toBe(
      "Bearer jwt123",
    );
  });

  it("returns TURN iceServers on success", async () => {
    const iceServers = [{ urls: "turn:turn.test" }];
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ iceServers }) });
    const c = new SignalingClient({ baseUrl, roomId: "r1", wsFactory: null });
    expect(await c.fetchIceServers()).toEqual(iceServers);
  });

  it("falls back to STUN on TURN failure", async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 500 });
    const c = new SignalingClient({ baseUrl, roomId: "r1", wsFactory: null });
    expect(await c.fetchIceServers()).toEqual(FALLBACK_ICE_SERVERS);
  });

  it("falls back to STUN when iceServers empty", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ iceServers: [] }) });
    const c = new SignalingClient({ baseUrl, roomId: "r1", wsFactory: null });
    expect(await c.fetchIceServers()).toEqual(FALLBACK_ICE_SERVERS);
  });

  it("throws on push failure", async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 403 });
    const c = new SignalingClient({ baseUrl, roomId: "r1", wsFactory: null });
    await expect(c.push({})).rejects.toThrow(/403/);
  });
});

// ---------------------------------------------------------------------------
// WebSocket transport tests
// ---------------------------------------------------------------------------

describe("SignalingClient (WebSocket)", () => {
  const baseUrl = "http://signal.test";
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    FakeWebSocket.instances = [];
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  // Helper: build a client wired to FakeWebSocket.
  function makeClient(token?: string) {
    return new SignalingClient({
      baseUrl,
      roomId: "room1",
      peer: "viewer",
      token,
      wsFactory: makeFakeFactory(),
    });
  }

  // -------------------------------------------------------------------------

  it("WS frames are delivered to onMessage", () => {
    const received: SignalingMessage[] = [];
    const c = makeClient();
    c.startPolling((m) => received.push(m));

    const ws = FakeWebSocket.instances[0];
    ws.serverOpen();
    ws.serverSend(frame(0, { type: "offer", sdp: "a" }));
    ws.serverSend(frame(1, { candidate: "cand1" }));

    expect(received).toHaveLength(2);
    expect(received[0].seq).toBe(0);
    expect(received[1].seq).toBe(1);
    c.stop();
  });

  it("WS frames are deduped by seq (replay overlap with prior HTTP since)", () => {
    const received: SignalingMessage[] = [];
    const c = makeClient();
    c.startPolling((m) => received.push(m));

    const ws = FakeWebSocket.instances[0];
    ws.serverOpen();
    // Frames 0 and 1 arrive, then the server replays 0 again (overlap).
    ws.serverSend(frame(0));
    ws.serverSend(frame(1));
    ws.serverSend(frame(0)); // duplicate — must be ignored
    ws.serverSend(frame(1)); // duplicate — must be ignored
    ws.serverSend(frame(2)); // new

    expect(received).toHaveLength(3);
    expect(received.map((m) => m.seq)).toEqual([0, 1, 2]);
    c.stop();
  });

  it("push() goes over the open socket", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({}) });
    const c = makeClient();
    c.startPolling(() => {});

    const ws = FakeWebSocket.instances[0];
    ws.serverOpen();

    await c.push({ type: "answer", sdp: "y" });

    // Sent over WS, not via fetch.
    expect(fetchMock).not.toHaveBeenCalled();
    expect(ws.sent).toHaveLength(1);
    expect(JSON.parse(ws.sent[0])).toEqual({ type: "answer", sdp: "y", viewerId: c.viewerId });
    c.stop();
  });

  it("push() falls back to fetch POST when socket is not open", async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({}) });
    const c = makeClient();
    c.startPolling(() => {});

    // WS created but NOT yet opened (readyState = CONNECTING).
    await c.push({ type: "answer", sdp: "z" });

    const ws = FakeWebSocket.instances[0];
    expect(ws.sent).toHaveLength(0); // nothing sent over socket
    expect(fetchMock).toHaveBeenCalledOnce();
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("http://signal.test/rooms/room1/viewer");
    expect(opts.method).toBe("POST");
    c.stop();
  });

  it("WS close triggers HTTP fallback AND a scheduled reconnect", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [], nextSince: 0 }),
    });

    const received: SignalingMessage[] = [];
    const c = makeClient();
    c.startPolling((m) => received.push(m));

    const ws1 = FakeWebSocket.instances[0];
    ws1.serverOpen();
    ws1.serverClose(1006);

    // The HTTP fallback loop runs its first tick immediately (no initial delay).
    // Drain the microtask queue so the pending poll() resolves.
    await vi.advanceTimersByTimeAsync(0);

    expect(fetchMock).toHaveBeenCalledOnce();
    const [url] = fetchMock.mock.calls[0];
    expect(url).toContain("/rooms/room1/viewer?since=");

    // A reconnect timer should be scheduled at 1s back-off.
    // After advancing past it a new WS should appear.
    await vi.advanceTimersByTimeAsync(1_100);
    expect(FakeWebSocket.instances).toHaveLength(2);

    c.stop();
  });

  it("WS reopen stops the HTTP fallback loop", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [], nextSince: 0 }),
    });

    const c = makeClient();
    c.startPolling(() => {});

    const ws1 = FakeWebSocket.instances[0];
    ws1.serverOpen();
    ws1.serverClose(1006);

    // Advance past the reconnect back-off (1 s) so the second WS is created.
    // The HTTP fallback may fire one or more polls during this window — that's fine.
    await vi.advanceTimersByTimeAsync(1_100);
    const ws2 = FakeWebSocket.instances[1];
    ws2.serverOpen(); // WS reconnects — fallback should stop

    // Snapshot fetch count right after WS opens.
    const fetchCallsWhenWsOpen = fetchMock.mock.calls.length;

    // Advance time further; NO additional HTTP fetches should happen while WS is up.
    await vi.advanceTimersByTimeAsync(5_000);

    expect(fetchMock.mock.calls.length).toBe(fetchCallsWhenWsOpen);
    c.stop();
  });

  it("stop() closes the socket and cancels all timers/loops", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [], nextSince: 0 }),
    });

    const received: SignalingMessage[] = [];
    const c = makeClient();
    c.startPolling((m) => received.push(m));

    const ws = FakeWebSocket.instances[0];
    ws.serverOpen();
    ws.serverClose(1006); // triggers fallback + reconnect timer

    // Let the first fallback poll fire.
    await vi.advanceTimersByTimeAsync(0);

    c.stop(); // must cancel everything

    // After stop() no further fetch calls should arrive.
    const fetchCallsAtStop = fetchMock.mock.calls.length;
    await vi.advanceTimersByTimeAsync(30_000);

    expect(fetchMock.mock.calls.length).toBe(fetchCallsAtStop);
    // No second WS was ever opened (reconnect was cancelled).
    expect(FakeWebSocket.instances).toHaveLength(1);
  });

  it("stop() is idempotent and safe mid-handshake", () => {
    const c = makeClient();
    c.startPolling(() => {});
    // WS in CONNECTING state — stop before server opens it.
    expect(() => { c.stop(); c.stop(); }).not.toThrow();
    expect(FakeWebSocket.instances[0].readyState).toBe(3); // CLOSED
  });

  it("token is appended as ?token= on the WS URL", () => {
    const c = makeClient("mytoken");
    c.startPolling(() => {});
    const ws = FakeWebSocket.instances[0];
    expect(ws.url).toContain("?token=mytoken");
    c.stop();
  });

  it("derives wss:// from https:// base URL", () => {
    const c = new SignalingClient({
      baseUrl: "https://signal.prod",
      roomId: "r99",
      wsFactory: makeFakeFactory(),
    });
    c.startPolling(() => {});
    const ws = FakeWebSocket.instances[0];
    expect(ws.url).toMatch(/^wss:\/\//);
    c.stop();
  });
});

// ---------------------------------------------------------------------------
// No-WebSocket environment → pure HTTP fallback
// ---------------------------------------------------------------------------

describe("SignalingClient (no WebSocket environment)", () => {
  const baseUrl = "http://signal.test";
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    FakeWebSocket.instances = [];
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("uses 700ms polling initially then 3s after 15s", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => ({ messages: [], nextSince: 0 }),
    });

    // Pass wsFactory: null to simulate an environment without WebSocket.
    const c = new SignalingClient({
      baseUrl,
      roomId: "room1",
      wsFactory: null,
    });
    c.startPolling(() => {});

    // ---- Phase 1: verify fast cadence (700 ms) ----
    // Drain the first immediate poll.
    await vi.advanceTimersByTimeAsync(0);
    const afterFirst = fetchMock.mock.calls.length;
    expect(afterFirst).toBeGreaterThanOrEqual(1);

    // One 700ms tick → exactly one more poll.
    await vi.advanceTimersByTimeAsync(700);
    const after700 = fetchMock.mock.calls.length;
    expect(after700 - afterFirst).toBe(1);

    // Two more 700ms ticks → two more polls.
    await vi.advanceTimersByTimeAsync(700);
    await vi.advanceTimersByTimeAsync(700);
    const after2100 = fetchMock.mock.calls.length;
    expect(after2100 - after700).toBe(2);

    // ---- Phase 2: advance into slow mode ----
    // Advance the remaining ~12.6s of the fast window (each 700ms fires one poll,
    // ~18 more polls — well under the 10 000 timer limit).
    await vi.advanceTimersByTimeAsync(12_600);
    // Total elapsed: 2100 + 12600 = 14700ms — still in fast window.
    // One more tick crosses 15 000ms.
    await vi.advanceTimersByTimeAsync(700); // cross 15 400ms
    // Now in slow mode. Drain any in-flight fetch.
    await vi.advanceTimersByTimeAsync(0);

    const slowBase = fetchMock.mock.calls.length;

    // In slow mode the next poll should NOT fire within 2.9 s.
    await vi.advanceTimersByTimeAsync(2_900);
    expect(fetchMock.mock.calls.length).toBe(slowBase);

    // After 3 s total elapsed (from slowBase snapshot) it fires exactly once.
    await vi.advanceTimersByTimeAsync(200);
    expect(fetchMock.mock.calls.length).toBe(slowBase + 1);

    // Another 3 s fires once more (not multiple fast-cadence polls).
    await vi.advanceTimersByTimeAsync(3_000);
    expect(fetchMock.mock.calls.length).toBe(slowBase + 2);

    c.stop();

    // Confirm no WebSocket was ever created.
    expect(FakeWebSocket.instances).toHaveLength(0);
  });
});
