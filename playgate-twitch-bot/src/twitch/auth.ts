/**
 * auth.ts — Twitch OAuth (Authorization Code) for two roles.
 *
 *   broadcaster — authorizes EventSub topics (redemptions/subs/bits) on the channel
 *   bot         — reads chat (tmi.js) and sends whispers
 *
 * Tokens (+ refresh tokens) persist to a JSON file so the streamer authorizes
 * once. getValidToken transparently refreshes before expiry. A thin helix()
 * helper injects the client-id + bearer that every Helix call needs.
 */
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { atomicWriteFile, type Logger } from "../util.js";

export type Role = "broadcaster" | "bot";

/** Scopes each role must consent to. */
export const SCOPES: Record<Role, string[]> = {
  broadcaster: ["channel:read:redemptions", "channel:read:subscriptions", "bits:read"],
  bot: ["chat:read", "chat:edit", "user:manage:whispers"],
};

interface StoredToken {
  access: string;
  refresh: string;
  expiresAt: number; // epoch ms
  userId: string;
  login: string;
  scopes: string[];
}

type Store = Partial<Record<Role, StoredToken>>;

const ID_BASE = "https://id.twitch.tv";
const HELIX_BASE = "https://api.twitch.tv/helix";

export class TwitchAuth {
  private store: Store = {};

  constructor(
    private clientId: string,
    private clientSecret: string,
    private tokenPath: string,
    private log: Logger,
  ) {
    if (existsSync(tokenPath)) {
      try {
        this.store = JSON.parse(readFileSync(tokenPath, "utf8"));
      } catch (e) {
        this.log.warn("could not parse token file; re-auth required", e);
      }
    }
  }

  static defaultPath(): string {
    return resolve(process.cwd(), "twitch.tokens.json");
  }

  isAuthorized(role: Role): boolean {
    return !!this.store[role];
  }

  getUser(role: Role): { id: string; login: string } | undefined {
    const t = this.store[role];
    return t ? { id: t.userId, login: t.login } : undefined;
  }

  /** Build the consent URL to send the streamer/bot through. */
  authorizeUrl(role: Role, redirectUri: string, state: string): string {
    const p = new URLSearchParams({
      client_id: this.clientId,
      redirect_uri: redirectUri,
      response_type: "code",
      scope: SCOPES[role].join(" "),
      state,
      force_verify: "true",
    });
    return `${ID_BASE}/oauth2/authorize?${p.toString()}`;
  }

  /** Exchange an authorization code for tokens and persist them. */
  async exchangeCode(role: Role, code: string, redirectUri: string): Promise<void> {
    const body = new URLSearchParams({
      client_id: this.clientId,
      client_secret: this.clientSecret,
      code,
      grant_type: "authorization_code",
      redirect_uri: redirectUri,
    });
    const res = await fetch(`${ID_BASE}/oauth2/token`, { method: "POST", body });
    if (!res.ok) throw new Error(`token exchange failed: ${res.status} ${await res.text()}`);
    const data = (await res.json()) as { access_token: string; refresh_token: string; expires_in: number };
    await this.persistFromTokenResponse(role, data);
    this.log.info(`authorized ${role} as ${this.store[role]?.login}`);
  }

  /** Access token for a role, refreshing if it's near expiry. */
  async getValidToken(role: Role): Promise<string> {
    const t = this.store[role];
    if (!t) throw new Error(`${role} is not authorized — visit the admin page to connect`);
    if (Date.now() < t.expiresAt - 60_000) return t.access;
    return this.refresh(role);
  }

  private async refresh(role: Role): Promise<string> {
    const t = this.store[role]!;
    const body = new URLSearchParams({
      client_id: this.clientId,
      client_secret: this.clientSecret,
      grant_type: "refresh_token",
      refresh_token: t.refresh,
    });
    const res = await fetch(`${ID_BASE}/oauth2/token`, { method: "POST", body });
    if (!res.ok) throw new Error(`token refresh failed for ${role}: ${res.status} ${await res.text()}`);
    const data = (await res.json()) as { access_token: string; refresh_token: string; expires_in: number };
    await this.persistFromTokenResponse(role, data);
    this.log.debug(`refreshed ${role} token`);
    return this.store[role]!.access;
  }

  /** Validate a fresh token to learn its user id/login + scopes, then save. */
  private async persistFromTokenResponse(
    role: Role,
    data: { access_token: string; refresh_token: string; expires_in: number },
  ): Promise<void> {
    const v = await fetch(`${ID_BASE}/oauth2/validate`, {
      headers: { Authorization: `OAuth ${data.access_token}` },
    });
    if (!v.ok) throw new Error(`token validate failed: ${v.status}`);
    const info = (await v.json()) as { user_id: string; login: string; scopes: string[] };
    this.store[role] = {
      access: data.access_token,
      refresh: data.refresh_token,
      expiresAt: Date.now() + data.expires_in * 1000,
      userId: info.user_id,
      login: info.login,
      scopes: info.scopes,
    };
    atomicWriteFile(this.tokenPath, JSON.stringify(this.store, null, 2));
  }

  /** Authenticated Helix request for a role. Returns parsed JSON (or null on 204). */
  async helix<T>(role: Role, method: string, path: string, body?: unknown): Promise<T> {
    const token = await this.getValidToken(role);
    const headers: Record<string, string> = {
      "Client-Id": this.clientId,
      Authorization: `Bearer ${token}`,
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetch(`${HELIX_BASE}${path}`, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (res.status === 204) return null as T;
    const text = await res.text();
    const json = text ? JSON.parse(text) : null;
    if (!res.ok) {
      const msg = (json && (json.message || json.error)) || `HTTP ${res.status}`;
      const err = new Error(`helix ${method} ${path}: ${msg}`) as Error & { status: number };
      err.status = res.status;
      throw err;
    }
    return json as T;
  }
}
