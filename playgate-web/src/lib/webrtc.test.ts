import { describe, expect, it } from "vitest";
import { DISCONNECT_RESTART_MS, newSessionNonce, ViewerConnection } from "./webrtc";

describe("reconnect policy", () => {
  it("backs off 1s, 2s, 4s, 8s, then caps at 10s", () => {
    expect([0, 1, 2, 3, 4, 10].map((a) => ViewerConnection.restartDelayMs(a))).toEqual([
      1000, 2000, 4000, 8000, 10_000, 10_000,
    ]);
  });

  it("waits longer than the host's 5s disconnect grace before rebuilding", () => {
    expect(DISCONNECT_RESTART_MS).toBeGreaterThan(5000);
  });

  it("generates a distinct session nonce per attempt", () => {
    const seen = new Set(Array.from({ length: 100 }, () => newSessionNonce()));
    expect(seen.size).toBe(100);
  });
});
