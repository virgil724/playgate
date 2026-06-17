/**
 * grantStore.ts — persistent record of who got a code and when.
 *
 * Backs three policy checks (per-user cooldown, per-minute rate, per-stream cap)
 * and the admin dashboard's activity log. Persisted to a JSON file via atomic
 * writes (debounced) so cooldowns survive a bot restart. State is tiny
 * (username → last grant + a capped event log), so a file beats a native DB.
 */
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { atomicWriteFile, type Logger } from "../util.js";
import type { GrantRequest, GrantSource } from "../grant/types.js";

const MAX_EVENTS = 500; // keep the most recent N grant events (rate + dashboard)

export interface UserRecord {
  username: string;
  lastGrantedAt: number; // epoch ms
  count: number;
  lastSource: GrantSource;
}

export interface GrantEvent {
  userId: string;
  username: string;
  source: GrantSource;
  code: string;
  delivery: "whisper" | "fallback" | "chat";
  at: number;
}

interface StoreShape {
  users: Record<string, UserRecord>;
  events: GrantEvent[];
  streamStartedAt: number;
}

export class GrantStore {
  private data: StoreShape;
  private flushTimer: NodeJS.Timeout | null = null;

  constructor(
    private path: string,
    private log: Logger,
  ) {
    this.data = { users: {}, events: [], streamStartedAt: Date.now() };
    if (existsSync(path)) {
      try {
        this.data = { ...this.data, ...JSON.parse(readFileSync(path, "utf8")) };
      } catch (e) {
        this.log.warn("could not parse grant store; starting fresh", e);
      }
    }
  }

  static defaultPath(): string {
    return resolve(process.cwd(), "grants.state.json");
  }

  getUser(userId: string): UserRecord | undefined {
    return this.data.users[userId];
  }

  /** Number of grants since a given epoch-ms timestamp (for per-minute rate). */
  countSince(sinceMs: number): number {
    return this.data.events.reduce((n, e) => (e.at >= sinceMs ? n + 1 : n), 0);
  }

  /** Grants issued in the current stream session (for per-stream cap). */
  streamCount(): number {
    return this.countSince(this.data.streamStartedAt);
  }

  /** Begin a new stream session: per-stream cap counts from now. */
  resetStream(): void {
    this.data.streamStartedAt = Date.now();
    this.scheduleFlush();
  }

  streamStartedAt(): number {
    return this.data.streamStartedAt;
  }

  /** Record a successful (delivered) grant. */
  record(req: GrantRequest, code: string, delivery: "whisper" | "fallback" | "chat"): void {
    const now = Date.now();
    const prev = this.data.users[req.twitchUserId];
    this.data.users[req.twitchUserId] = {
      username: req.twitchUsername,
      lastGrantedAt: now,
      count: (prev?.count ?? 0) + 1,
      lastSource: req.source,
    };
    this.data.events.push({
      userId: req.twitchUserId,
      username: req.twitchUsername,
      source: req.source,
      code,
      delivery,
      at: now,
    });
    if (this.data.events.length > MAX_EVENTS) {
      this.data.events.splice(0, this.data.events.length - MAX_EVENTS);
    }
    this.scheduleFlush();
  }

  /** Most recent grant events, newest first (for the dashboard). */
  recent(limit = 50): GrantEvent[] {
    return this.data.events.slice(-limit).reverse();
  }

  totalUsers(): number {
    return Object.keys(this.data.users).length;
  }

  /** Debounced atomic flush — coalesces bursts of grants into one write. */
  private scheduleFlush(): void {
    if (this.flushTimer) return;
    this.flushTimer = setTimeout(() => {
      this.flushTimer = null;
      this.flush();
    }, 500);
  }

  flush(): void {
    try {
      atomicWriteFile(this.path, JSON.stringify(this.data));
    } catch (e) {
      this.log.error("flush failed", e);
    }
  }
}
