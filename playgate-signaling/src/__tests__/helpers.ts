/**
 * Test helpers — fakes for the Durable Object layer plus request factories.
 *
 * We cannot run the real DO runtime (no miniflare / vitest-pool-workers here),
 * so we instantiate the real `RoomDO` class against a fake `DurableObjectState`
 * whose storage is an in-memory map. This exercises the actual DO HTTP glue
 * (POST/GET/long-poll routing) without a Workers runtime. The room/queue logic
 * itself is also unit-tested directly via `RoomState` in roomState.test.ts.
 *
 * WebSocket / hibernation glue is intentionally NOT exercised through this fake
 * (the runtime APIs are stubbed to no-ops); per the task that thin layer is
 * left to integration, with the testable logic kept in RoomState.
 */

import type { Env } from "../types.js";
import type { RoomStorage } from "../roomState.js";
import { RoomDO } from "../roomDO.js";

// ---------------------------------------------------------------------------
// In-memory storage shared by the fake DO state and direct RoomState tests
// ---------------------------------------------------------------------------

/** Simple in-memory RoomStorage for unit-testing RoomState directly. */
export class FakeStorage implements RoomStorage {
  private store = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    // Deep-clone on read so callers can't mutate stored arrays in place.
    const v = this.store.get(key);
    return v === undefined ? undefined : (structuredClone(v) as T);
  }

  async put<T>(key: string, value: T): Promise<void> {
    this.store.set(key, structuredClone(value));
  }

  async delete(key: string): Promise<boolean> {
    return this.store.delete(key);
  }

  // --- test helpers ---
  has(key: string): boolean {
    return this.store.has(key);
  }
  raw<T>(key: string): T | undefined {
    return this.store.get(key) as T | undefined;
  }
}

// ---------------------------------------------------------------------------
// Fake DurableObjectState — just enough for RoomDO's HTTP paths
// ---------------------------------------------------------------------------

class FakeDOStorage {
  private store = new Map<string, unknown>();

  async get<T>(key: string): Promise<T | undefined> {
    const v = this.store.get(key);
    return v === undefined ? undefined : (structuredClone(v) as T);
  }
  async put<T>(key: string, value: T): Promise<void> {
    this.store.set(key, structuredClone(value));
  }
  async delete(key: string): Promise<boolean> {
    return this.store.delete(key);
  }
  async deleteAll(): Promise<void> {
    this.store.clear();
  }
  async setAlarm(_when: number): Promise<void> {
    /* no-op in tests */
  }
  async getAlarm(): Promise<number | null> {
    return null;
  }
  async deleteAlarm(): Promise<void> {
    /* no-op */
  }

  // test helper
  has(key: string): boolean {
    return this.store.has(key);
  }
}

class FakeDOState {
  storage = new FakeDOStorage();
  acceptWebSocket(_ws: unknown, _tags?: string[]): void {
    /* no-op: WS not exercised in unit tests */
  }
  getWebSockets(_tag?: string): unknown[] {
    return [];
  }
  getTags(_ws: unknown): string[] {
    return [];
  }
}

// ---------------------------------------------------------------------------
// Fake DurableObjectNamespace backed by real RoomDO instances (one per name)
// ---------------------------------------------------------------------------

class FakeDONamespace {
  private instances = new Map<string, { fetch: (r: Request) => Promise<Response> }>();
  constructor(private readonly env: Env) {}

  idFromName(name: string): { name: string; toString(): string } {
    return { name, toString: () => name };
  }

  get(id: { name: string }): { fetch: (r: Request) => Promise<Response> } {
    let inst = this.instances.get(id.name);
    if (!inst) {
      const state = new FakeDOState();
      const room = new RoomDO(state as unknown as DurableObjectState, this.env);
      inst = { fetch: (r: Request) => room.fetch(r) };
      this.instances.set(id.name, inst);
    }
    return inst;
  }
}

// ---------------------------------------------------------------------------
// Environment factory
// ---------------------------------------------------------------------------

export function makeEnv(overrides: Partial<Env> = {}): Env {
  const env: Env = {
    // ROOMS is replaced below with a namespace bound to this env.
    ROOMS: undefined as unknown as DurableObjectNamespace,
    AUTH_DISABLED: "true",
    ...overrides,
  };
  env.ROOMS = new FakeDONamespace(env) as unknown as DurableObjectNamespace;
  return env;
}

// ---------------------------------------------------------------------------
// Request factory
// ---------------------------------------------------------------------------

export function makeRequest(
  method: string,
  url: string,
  body?: unknown,
  headers?: Record<string, string>,
): Request {
  const init: RequestInit = {
    method,
    headers: {
      "Content-Type": "application/json",
      ...headers,
    },
  };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
  }
  return new Request(url, init);
}
