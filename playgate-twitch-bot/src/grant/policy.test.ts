import { describe, it, expect, beforeEach, vi } from "vitest";
import { PolicyEngine } from "./policy.js";
import type { Policy } from "../config.js";
import type { GrantStore, UserRecord } from "../store/grantStore.js";
import { NO_BADGES, type GrantRequest } from "./types.js";

function basePolicy(): Policy {
  return {
    global: { perUserCooldownSec: 1800, maxPerStream: 200, maxPerMinute: 10 },
    sources: {
      command: { enabled: true, trigger: "!play", eligibility: "subscribers" },
      channel_points: { enabled: true, rewardId: "reward-1", eligibility: "everyone" },
      subscription: { enabled: true, eligibility: "everyone" },
      cheer: { enabled: true, minBits: 100 },
      raid: { enabled: true, minViewers: 5 },
    },
  };
}

/** Minimal in-memory GrantStore stub. */
function fakeStore(over: Partial<Record<keyof GrantStore, unknown>> = {}): GrantStore {
  return {
    getUser: vi.fn((): UserRecord | undefined => undefined),
    countSince: vi.fn(() => 0),
    streamCount: vi.fn(() => 0),
    ...over,
  } as unknown as GrantStore;
}

function req(over: Partial<GrantRequest> = {}): GrantRequest {
  return {
    source: "command",
    twitchUserId: "u1",
    twitchUsername: "alice",
    badges: { ...NO_BADGES },
    ...over,
  };
}

describe("PolicyEngine", () => {
  let policy: Policy;
  beforeEach(() => {
    policy = basePolicy();
  });

  it("denies a disabled source", () => {
    policy.sources.command.enabled = false;
    const e = new PolicyEngine(policy, fakeStore());
    expect(e.evaluate(req()).allowed).toBe(false);
  });

  it("enforces command eligibility (subscribers only)", () => {
    const e = new PolicyEngine(policy, fakeStore());
    expect(e.evaluate(req({ badges: { ...NO_BADGES } })).allowed).toBe(false);
    expect(e.evaluate(req({ badges: { ...NO_BADGES, sub: true } })).allowed).toBe(true);
  });

  it("treats higher roles as satisfying lower eligibility", () => {
    const e = new PolicyEngine(policy, fakeStore());
    expect(e.evaluate(req({ badges: { ...NO_BADGES, mod: true } })).allowed).toBe(true);
  });

  it("matches channel_points reward id", () => {
    const e = new PolicyEngine(policy, fakeStore());
    expect(e.evaluate(req({ source: "channel_points", context: { rewardId: "wrong" } })).allowed).toBe(false);
    expect(e.evaluate(req({ source: "channel_points", context: { rewardId: "reward-1" } })).allowed).toBe(true);
  });

  it("enforces cheer minBits and raid minViewers", () => {
    const e = new PolicyEngine(policy, fakeStore());
    expect(e.evaluate(req({ source: "cheer", context: { bits: 50 } })).allowed).toBe(false);
    expect(e.evaluate(req({ source: "cheer", context: { bits: 100 } })).allowed).toBe(true);
    expect(e.evaluate(req({ source: "raid", context: { raidViewers: 4 } })).allowed).toBe(false);
    expect(e.evaluate(req({ source: "raid", context: { raidViewers: 5 } })).allowed).toBe(true);
  });

  it("blocks a user still within cooldown", () => {
    const recent: UserRecord = { username: "alice", lastGrantedAt: Date.now() - 60_000, count: 1, lastSource: "command" };
    const e = new PolicyEngine(policy, fakeStore({ getUser: vi.fn(() => recent) }));
    expect(e.evaluate(req({ badges: { ...NO_BADGES, sub: true } })).allowed).toBe(false);
  });

  it("allows after cooldown elapses", () => {
    const old: UserRecord = { username: "alice", lastGrantedAt: Date.now() - 2_000_000, count: 1, lastSource: "command" };
    const e = new PolicyEngine(policy, fakeStore({ getUser: vi.fn(() => old) }));
    expect(e.evaluate(req({ badges: { ...NO_BADGES, sub: true } })).allowed).toBe(true);
  });

  it("honors a per-source cooldown override", () => {
    policy.sources.channel_points.perUserCooldownSec = 10; // short override
    const rec: UserRecord = { username: "a", lastGrantedAt: Date.now() - 20_000, count: 1, lastSource: "channel_points" };
    const e = new PolicyEngine(policy, fakeStore({ getUser: vi.fn(() => rec) }));
    // 20s elapsed > 10s override ⇒ allowed, even though global is 1800s
    expect(e.evaluate(req({ source: "channel_points", context: { rewardId: "reward-1" } })).allowed).toBe(true);
  });

  it("enforces per-minute rate limit", () => {
    const e = new PolicyEngine(policy, fakeStore({ countSince: vi.fn(() => 10) }));
    expect(e.evaluate(req({ badges: { ...NO_BADGES, sub: true } })).allowed).toBe(false);
  });

  it("enforces per-stream cap", () => {
    const e = new PolicyEngine(policy, fakeStore({ streamCount: vi.fn(() => 200) }));
    expect(e.evaluate(req({ badges: { ...NO_BADGES, sub: true } })).allowed).toBe(false);
  });
});
