/**
 * client.ts — typed client for the playgate-server REST API.
 *
 * Mirrors playgate-web/src/lib/api.ts but for Node (global fetch). The bot only
 * needs the host-authenticated token endpoints; it never redeems codes itself.
 */
import type { Logger } from "../util.js";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

export interface TokenInfo {
  code: string;
  status: "issued" | "redeemed" | "revoked";
  redeemed: boolean;
  revoked: boolean;
}

export class PlayGateClient {
  private baseUrl: string;
  private apiKey: string;

  constructor(baseUrl: string, apiKey: string, private log?: Logger) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
    this.apiKey = apiKey;
  }

  private async req<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.apiKey}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    const data = text ? JSON.parse(text) : {};
    if (!res.ok) {
      throw new ApiError(res.status, (data && data.error) || `HTTP ${res.status}`);
    }
    return data as T;
  }

  /** Mint a batch of redeemable codes for a room (1..100). */
  async issueTokens(roomId: string, count: number): Promise<string[]> {
    const res = await this.req<{ codes: string[] }>(
      "POST",
      `/api/rooms/${encodeURIComponent(roomId)}/tokens`,
      { count },
    );
    this.log?.debug(`minted ${res.codes.length} code(s) for room ${roomId}`);
    return res.codes;
  }

  /** List all codes for a room with their lifecycle status. */
  async listTokens(roomId: string): Promise<TokenInfo[]> {
    const res = await this.req<{ tokens: TokenInfo[] }>(
      "GET",
      `/api/rooms/${encodeURIComponent(roomId)}/tokens`,
    );
    return res.tokens;
  }

  /** Confirm the room exists and the API key owns it (used on startup). */
  async assertRoom(roomId: string): Promise<void> {
    await this.req("GET", `/api/rooms/${encodeURIComponent(roomId)}`);
  }
}
