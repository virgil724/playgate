/**
 * pipeline.ts — the single entry point every trigger source funnels into.
 *
 *   policy.evaluate → pool.take → delivery.deliver → store.record
 *
 * Sources only build a GrantRequest and call handle(); they know nothing about
 * codes, whispers, cooldowns or stats.
 */
import type { GrantRequest } from "./types.js";
import type { PolicyEngine } from "./policy.js";
import type { TokenPool } from "../playgate/tokenPool.js";
import type { GrantStore } from "../store/grantStore.js";
import type { Stats } from "../admin/stats.js";
import type { Logger } from "../util.js";
import { buildRedeemUrl } from "./message.js";

/** Outcome of trying to hand a code to a viewer. */
export type DeliveryResult = "whisper" | "fallback" | "chat" | "failed";

/** Delivers a code to a viewer (whisper, with public chat fallback). */
export interface Delivery {
  deliver(req: GrantRequest, code: string, redeemUrl: string): Promise<DeliveryResult>;
}

export interface PipelineDeps {
  policy: PolicyEngine;
  pool: TokenPool;
  store: GrantStore;
  delivery: Delivery;
  stats: Stats;
  webBase: string;
  roomId: string;
  log: Logger;
}

export type GrantHandler = (req: GrantRequest) => Promise<void>;

export function makePipeline(deps: PipelineDeps): GrantHandler {
  const { policy, pool, store, delivery, stats, webBase, roomId, log } = deps;

  return async function handle(req: GrantRequest): Promise<void> {
    // 1. policy
    const verdict = policy.evaluate(req);
    if (!verdict.allowed) {
      stats.denied();
      log.info(`denied ${req.source} ${req.twitchUsername}: ${verdict.reason}`);
      return;
    }

    // 2. take a code
    let code: string;
    try {
      code = await pool.take();
    } catch (e) {
      stats.error(String(e));
      log.error(`pool empty for ${req.twitchUsername}; dropping grant`, e);
      return;
    }

    // 3. deliver
    const url = buildRedeemUrl(webBase, roomId, code);
    let result: DeliveryResult;
    try {
      result = await delivery.deliver(req, code, url);
    } catch (e) {
      result = "failed";
      stats.error(String(e));
      log.error(`delivery threw for ${req.twitchUsername}`, e);
    }

    if (result === "failed") {
      // Nobody received it — return the code so it isn't wasted, and don't
      // start the user's cooldown (they never got anything).
      pool.giveBack(code);
      stats.deliveryFailed();
      log.warn(`delivery failed for ${req.twitchUsername}; code returned to pool`);
      return;
    }

    // 4. record (starts cooldown, feeds rate limit + dashboard)
    store.record(req, code, result);
    stats.delivered(result);
    log.info(`granted ${req.source} → ${req.twitchUsername} via ${result}`);
  };
}
