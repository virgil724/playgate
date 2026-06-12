/**
 * Cloudflare Worker environment bindings.
 */
export interface Env {
  /** Workers KV namespace for transient signaling messages (TTL 5 min) */
  SIGNALING_KV: KVNamespace;

  /** Cloudflare Realtime TURN key ID (wrangler secret) */
  TURN_KEY_ID?: string;

  /** Cloudflare Realtime TURN API token (wrangler secret) */
  TURN_KEY_API_TOKEN?: string;

  /**
   * Shared HMAC secret used by the stub session-token validator.
   * Leave unset to skip signature verification (dev-only).
   */
  SESSION_SECRET?: string;

  /**
   * Set to "true" to bypass all auth checks entirely.
   * Useful for local development / dry-run.
   */
  AUTH_DISABLED?: string;
}

/** A single signaling message stored in KV (one SDP or ICE payload). */
export interface SignalingMessage {
  /** Sequence index within the peer's queue */
  seq: number;
  /** ISO-8601 timestamp */
  ts: string;
  /** Arbitrary JSON payload (SDP offer/answer or ICE candidate) */
  payload: unknown;
}

/** The envelope stored in KV for one peer's queue. */
export interface PeerQueue {
  messages: SignalingMessage[];
}

/** Response body for GET /rooms/:roomId/:peer */
export interface MessagesResponse {
  messages: SignalingMessage[];
  nextSince: number;
}

/** Response body for POST /turn/credentials */
export interface TurnCredentialsResponse {
  iceServers: RTCIceServer[];
  ttl: number;
}

/** Minimal RTCIceServer shape (matches browser RTCIceServer) */
export interface RTCIceServer {
  urls: string | string[];
  username?: string;
  credential?: string;
}

/**
 * Cloudflare Realtime TURN credential API response — a bare ICE server
 * object, not the usual success/result API envelope.
 */
export interface CloudflareCredentialResponse {
  iceServers?: {
    urls?: string[];
    username?: string;
    credential?: string;
  };
}
