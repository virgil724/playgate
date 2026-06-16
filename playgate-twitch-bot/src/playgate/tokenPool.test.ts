import { describe, it, expect, vi } from "vitest";
import { TokenPool } from "./tokenPool.js";
import type { PlayGateClient, TokenInfo } from "./client.js";
import type { Logger } from "../util.js";

const noopLog: Logger = { debug: vi.fn(), info: vi.fn(), warn: vi.fn(), error: vi.fn() };
const tick = () => new Promise((r) => setTimeout(r, 0));

function fakeClient(over: Partial<PlayGateClient> = {}): PlayGateClient {
  let n = 0;
  return {
    listTokens: vi.fn(async (): Promise<TokenInfo[]> => []),
    issueTokens: vi.fn(async (_room: string, count: number) =>
      Array.from({ length: count }, () => `code${n++}`),
    ),
    ...over,
  } as unknown as PlayGateClient;
}

describe("TokenPool", () => {
  it("recovers only issued (unredeemed) codes on init", async () => {
    const client = fakeClient({
      listTokens: vi.fn(async (): Promise<TokenInfo[]> => [
        { code: "a", status: "issued", redeemed: false, revoked: false },
        { code: "b", status: "redeemed", redeemed: true, revoked: false },
        { code: "c", status: "revoked", redeemed: false, revoked: true },
      ]),
    });
    const pool = new TokenPool(client, "room", { batchSize: 5, lowWatermark: 0 }, noopLog);
    await pool.init();
    expect(pool.size()).toBe(1); // only "a"
  });

  it("refills when init finds the buffer at/below the watermark", async () => {
    const client = fakeClient(); // listTokens → []
    const pool = new TokenPool(client, "room", { batchSize: 3, lowWatermark: 0 }, noopLog);
    await pool.init();
    expect(pool.size()).toBe(3);
    expect(client.issueTokens).toHaveBeenCalledWith("room", 3);
  });

  it("take() returns a code and tops up in the background when low", async () => {
    const client = fakeClient({
      listTokens: vi.fn(async (): Promise<TokenInfo[]> => [{ code: "a", status: "issued", redeemed: false, revoked: false }]),
    });
    const pool = new TokenPool(client, "room", { batchSize: 2, lowWatermark: 0 }, noopLog);
    await pool.init();
    const code = await pool.take();
    expect(code).toBe("a");
    await tick(); // let the background refill settle
    expect(pool.size()).toBe(2);
  });

  it("take() mints synchronously when the buffer is empty", async () => {
    const client = fakeClient();
    const pool = new TokenPool(client, "room", { batchSize: 2, lowWatermark: 0 }, noopLog);
    // skip init; buffer starts empty
    const code = await pool.take();
    expect(code).toBeTruthy();
  });

  it("giveBack() returns a code to the front of the buffer", async () => {
    const client = fakeClient({
      listTokens: vi.fn(async (): Promise<TokenInfo[]> => [{ code: "a", status: "issued", redeemed: false, revoked: false }]),
    });
    const pool = new TokenPool(client, "room", { batchSize: 1, lowWatermark: 0 }, noopLog);
    await pool.init();
    pool.giveBack("returned");
    expect(await pool.take()).toBe("returned"); // came back to the front
  });
});
