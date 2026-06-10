/**
 * Test helpers — lightweight KV mock and request factory.
 */

import type { Env, PeerQueue } from "../types.js";

// ---------------------------------------------------------------------------
// In-memory KV mock
// We avoid implementing the full KVNamespace interface (it has complex
// overloads that are painful to satisfy exactly), so we cast at the end.
// ---------------------------------------------------------------------------

interface KVEntry {
  value: string;
  expiresAt?: number;
}

export class MockKVNamespace {
  private store = new Map<string, KVEntry>();

  async put(
    key: string,
    value: string,
    options?: { expirationTtl?: number },
  ): Promise<void> {
    const ttl = options?.expirationTtl;
    const expiresAt =
      ttl !== undefined ? Date.now() + ttl * 1000 : undefined;
    this.store.set(key, { value, expiresAt });
  }

  // Single implementation that covers all overloads used in the Worker code.
  // The Worker only calls: .get(key, "json")
  async get(key: string, type?: string): Promise<unknown> {
    const entry = this.store.get(key);
    if (!entry) return null;
    if (entry.expiresAt && Date.now() > entry.expiresAt) {
      this.store.delete(key);
      return null;
    }
    if (type === "json") return JSON.parse(entry.value) as unknown;
    return entry.value;
  }

  async delete(key: string): Promise<void> {
    this.store.delete(key);
  }

  async list(): Promise<{ keys: Array<{ name: string }> }> {
    return { keys: [...this.store.keys()].map((name) => ({ name })) };
  }

  // --- test helpers ---

  /** Read raw stored string. */
  getRaw(key: string): string | undefined {
    return this.store.get(key)?.value;
  }

  /** Parse stored PeerQueue. */
  getQueue(key: string): PeerQueue | null {
    const raw = this.getRaw(key);
    return raw ? (JSON.parse(raw) as PeerQueue) : null;
  }

  /** Clear all entries. */
  clear(): void {
    this.store.clear();
  }
}

// ---------------------------------------------------------------------------
// Environment factory
// ---------------------------------------------------------------------------

export function makeEnv(overrides: Partial<Env> = {}): Env {
  return {
    // Cast to KVNamespace — the mock satisfies all methods the Worker calls.
    SIGNALING_KV: new MockKVNamespace() as unknown as KVNamespace,
    AUTH_DISABLED: "true",
    ...overrides,
  };
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
