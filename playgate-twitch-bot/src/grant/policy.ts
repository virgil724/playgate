/**
 * policy.ts — layered grant policy with hot-swap.
 *
 * Three layers, evaluated in order, short-circuiting on the first failure:
 *   1. eligibility — is this viewer/event allowed for this source?
 *   2. cooldown    — has this user grabbed a code too recently?
 *   3. rate/cap    — too many grants this minute, or this stream?
 *
 * The current Policy is held in one field and replaced atomically by setPolicy,
 * so the admin page can apply edits without a restart.
 */
import type { Eligibility, Policy } from "../config.js";
import type { GrantRequest, ViewerBadges } from "./types.js";
import type { GrantStore } from "../store/grantStore.js";

export interface Verdict {
  allowed: boolean;
  reason?: string;
}

/** Higher roles satisfy lower thresholds (a mod counts as a subscriber, etc.). */
function meetsEligibility(elig: Eligibility, b: ViewerBadges): boolean {
  switch (elig) {
    case "everyone":
      return true;
    case "subscribers":
      return b.sub || b.vip || b.mod || b.broadcaster;
    case "vips":
      return b.vip || b.mod || b.broadcaster;
    case "mods":
      return b.mod || b.broadcaster;
  }
}

export class PolicyEngine {
  constructor(
    private policy: Policy,
    private store: GrantStore,
  ) {}

  /** Atomically replace the active policy (called by the admin page). */
  setPolicy(next: Policy): void {
    this.policy = next;
  }

  getPolicy(): Policy {
    return this.policy;
  }

  /** Is a source currently enabled? (used to decide which adapters to wire). */
  isEnabled(source: GrantRequest["source"]): boolean {
    return this.policy.sources[source].enabled;
  }

  evaluate(req: GrantRequest): Verdict {
    const { sources, global } = this.policy;

    // --- layer 1: eligibility (source-specific) ---
    const elig = this.checkEligibility(req);
    if (!elig.allowed) return elig;

    // --- layer 2: per-user cooldown (source override falls back to global) ---
    const src = sources[req.source];
    const cooldownSec = src.perUserCooldownSec ?? global.perUserCooldownSec;
    if (cooldownSec > 0) {
      const user = this.store.getUser(req.twitchUserId);
      if (user) {
        const elapsedSec = (Date.now() - user.lastGrantedAt) / 1000;
        if (elapsedSec < cooldownSec) {
          const wait = Math.ceil(cooldownSec - elapsedSec);
          return { allowed: false, reason: `cooldown: ${wait}s remaining` };
        }
      }
    }

    // --- layer 3: rate + per-stream cap ---
    if (this.store.countSince(Date.now() - 60_000) >= global.maxPerMinute) {
      return { allowed: false, reason: "rate limit: too many grants this minute" };
    }
    if (this.store.streamCount() >= global.maxPerStream) {
      return { allowed: false, reason: "per-stream cap reached" };
    }

    return { allowed: true };
  }

  private checkEligibility(req: GrantRequest): Verdict {
    const s = this.policy.sources;
    switch (req.source) {
      case "command":
        return meetsEligibility(s.command.eligibility, req.badges)
          ? { allowed: true }
          : { allowed: false, reason: `not eligible (${s.command.eligibility} only)` };

      case "channel_points": {
        // If a specific reward is configured, the redemption must match it.
        if (s.channel_points.rewardId && req.context?.rewardId !== s.channel_points.rewardId) {
          return { allowed: false, reason: "reward id mismatch" };
        }
        return meetsEligibility(s.channel_points.eligibility, req.badges)
          ? { allowed: true }
          : { allowed: false, reason: `not eligible (${s.channel_points.eligibility} only)` };
      }

      case "subscription":
        // The subscribe event itself is the qualification.
        return meetsEligibility(s.subscription.eligibility, req.badges)
          ? { allowed: true }
          : { allowed: false, reason: `not eligible (${s.subscription.eligibility} only)` };

      case "cheer": {
        const bits = req.context?.bits ?? 0;
        return bits >= s.cheer.minBits
          ? { allowed: true }
          : { allowed: false, reason: `below minBits (${bits} < ${s.cheer.minBits})` };
      }

      case "raid": {
        const viewers = req.context?.raidViewers ?? 0;
        return viewers >= s.raid.minViewers
          ? { allowed: true }
          : { allowed: false, reason: `below minViewers (${viewers} < ${s.raid.minViewers})` };
      }
    }
  }
}
