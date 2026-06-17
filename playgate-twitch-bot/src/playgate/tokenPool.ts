/**
 * tokenPool.ts — a buffer of pre-minted codes.
 *
 * Minting on every grant would be slow and fragile (network blip ⇒ viewer waits
 * with a whisper pending). Instead we keep a small buffer, pre-minting a batch
 * and topping up in the background when it runs low.
 *
 * On startup the pool recovers issued-but-unredeemed codes from the server
 * (GET .../tokens, status === "issued"), so a bot restart never strands codes
 * and never double-counts already-redeemed ones — the server stays the source
 * of truth.
 */
import type { PlayGateClient } from "./client.js";
import type { Logger } from "../util.js";

export interface TokenPoolOptions {
  batchSize: number;
  lowWatermark: number;
}

export class TokenPool {
  private buffer: string[] = [];
  private refilling: Promise<void> | null = null;

  constructor(
    private client: PlayGateClient,
    private roomId: string,
    private opts: TokenPoolOptions,
    private log: Logger,
  ) {}

  /** Recover unredeemed codes from the server, then ensure the buffer is full. */
  async init(): Promise<void> {
    try {
      const tokens = await this.client.listTokens(this.roomId);
      this.buffer = tokens.filter((t) => t.status === "issued").map((t) => t.code);
      this.log.info(`recovered ${this.buffer.length} unredeemed code(s) from server`);
    } catch (e) {
      this.log.warn("recover failed; starting with empty buffer", e);
    }
    if (this.buffer.length <= this.opts.lowWatermark) await this.refill();
  }

  size(): number {
    return this.buffer.length;
  }

  /** Take one code, triggering a background refill when the buffer runs low. */
  async take(): Promise<string> {
    if (this.buffer.length === 0) await this.refill();
    const code = this.buffer.shift();
    if (!code) throw new Error("token pool empty and refill failed");
    if (this.buffer.length <= this.opts.lowWatermark) {
      void this.refill().catch((e) => this.log.warn("background refill failed", e));
    }
    return code;
  }

  /** Return an unused code to the front of the buffer (e.g. delivery failed). */
  giveBack(code: string): void {
    this.buffer.unshift(code);
  }

  /** Mint a fresh batch. Concurrent callers share one in-flight refill. */
  private refill(): Promise<void> {
    if (this.refilling) return this.refilling;
    this.refilling = (async () => {
      const codes = await this.client.issueTokens(this.roomId, this.opts.batchSize);
      this.buffer.push(...codes);
      this.log.info(`refilled; buffer=${this.buffer.length}`);
    })().finally(() => {
      this.refilling = null;
    });
    return this.refilling;
  }
}
