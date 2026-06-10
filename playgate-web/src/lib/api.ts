/**
 * api.ts — typed client for the playgate-server REST API.
 *
 * See playgate-server/README.md. Host-authenticated calls take an API key
 * (Bearer). Viewer redeem needs no auth.
 */

export interface RedeemResult {
  session_token: string;
  queue_position: number;
  expires_at: number;
  room_id: string;
  viewer_id: string;
}

export interface RoomStatus {
  id: string;
  name: string;
  session_seconds: number;
  online: boolean;
  current_viewer: string | null;
  queue_depth: number;
}

export interface TokenInfo {
  code: string;
  status: "issued" | "redeemed" | "revoked";
  redeemed: boolean;
  revoked: boolean;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

export class ApiClient {
  private baseUrl: string;
  private apiKey?: string;

  constructor(baseUrl: string, apiKey?: string) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
    this.apiKey = apiKey;
  }

  setApiKey(key: string | undefined) {
    this.apiKey = key;
  }

  private async req<T>(method: string, path: string, body?: unknown, auth = false): Promise<T> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (auth) {
      if (!this.apiKey) throw new ApiError(401, "missing API key");
      headers["Authorization"] = `Bearer ${this.apiKey}`;
    }
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

  // ---- viewer ----
  redeem(code: string): Promise<RedeemResult> {
    return this.req<RedeemResult>("POST", `/api/tokens/${encodeURIComponent(code)}/redeem`, {});
  }

  getRoom(id: string): Promise<RoomStatus> {
    return this.req<RoomStatus>("GET", `/api/rooms/${encodeURIComponent(id)}`);
  }

  // ---- host (auth) ----
  registerHost(name: string): Promise<{ host_id: string; api_key: string }> {
    return this.req("POST", "/api/hosts/register", { name });
  }

  listRooms(): Promise<{ rooms: RoomStatus[] }> {
    return this.req("GET", "/api/rooms?host=me", undefined, true);
  }

  createRoom(name: string, sessionSeconds?: number): Promise<RoomStatus> {
    return this.req("POST", "/api/rooms", { name, session_seconds: sessionSeconds ?? 60 }, true);
  }

  issueTokens(roomId: string, count: number): Promise<{ codes: string[] }> {
    return this.req("POST", `/api/rooms/${encodeURIComponent(roomId)}/tokens`, { count }, true);
  }

  listTokens(roomId: string): Promise<{ tokens: TokenInfo[] }> {
    return this.req("GET", `/api/rooms/${encodeURIComponent(roomId)}/tokens`, undefined, true);
  }

  revokeToken(code: string): Promise<void> {
    return this.req("DELETE", `/api/tokens/${encodeURIComponent(code)}`, undefined, true);
  }

  kick(roomId: string): Promise<{ status: string }> {
    return this.req("POST", `/api/rooms/${encodeURIComponent(roomId)}/kick`, {}, true);
  }
}

/** Read configured base URLs from Vite env. */
export const API_BASE_URL: string =
  (import.meta.env.VITE_API_BASE_URL as string | undefined) ?? "http://localhost:8080";
export const SIGNALING_BASE_URL: string =
  (import.meta.env.VITE_SIGNALING_BASE_URL as string | undefined) ?? "http://localhost:8787";
