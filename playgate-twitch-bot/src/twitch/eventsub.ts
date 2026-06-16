/**
 * eventsub.ts — Twitch EventSub over WebSocket.
 *
 * WebSocket transport suits a single-machine bot: no public callback URL needed.
 * Flow: connect → session_welcome (carries session id) → create subscriptions
 * via Helix bound to that session → notifications stream in. Handles keepalive,
 * server-initiated reconnect, and reconnect-with-backoff on unexpected drops.
 *
 * Docs: https://dev.twitch.tv/docs/eventsub/handling-websocket-events/
 */
import WebSocket from "ws";
import type { TwitchAuth } from "./auth.js";
import type { Logger } from "../util.js";

export interface SubSpec {
  type: string;
  version: string;
  condition: Record<string, string>;
}

export type NotificationHandler = (subType: string, event: Record<string, unknown>) => void;

const DEFAULT_URL = "wss://eventsub.ws.twitch.tv/ws";

export class EventSubClient {
  private ws?: WebSocket;
  private sessionId?: string;
  private closedByUs = false;
  private expectingReconnect = false;
  private backoffMs = 1000;

  constructor(
    private auth: TwitchAuth,
    private subs: SubSpec[],
    private onNotification: NotificationHandler,
    private log: Logger,
  ) {}

  start(): void {
    this.closedByUs = false;
    this.open(DEFAULT_URL);
  }

  stop(): void {
    this.closedByUs = true;
    this.ws?.close();
  }

  connected(): boolean {
    return !!this.sessionId;
  }

  private open(url: string): void {
    const ws = new WebSocket(url);
    this.ws = ws;
    ws.on("message", (data: WebSocket.RawData) => void this.onMessage(data.toString()));
    ws.on("close", (code: number) => this.onClose(code));
    ws.on("error", (err: Error) => this.log.warn("eventsub ws error", err.message));
  }

  private async onMessage(raw: string): Promise<void> {
    let msg: any;
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    switch (msg.metadata?.message_type) {
      case "session_welcome": {
        this.sessionId = msg.payload.session.id;
        this.backoffMs = 1000;
        if (this.expectingReconnect) {
          // Reconnect carries existing subscriptions — don't recreate them.
          this.expectingReconnect = false;
          this.log.info("eventsub reconnected");
        } else {
          this.log.info(`eventsub connected (session ${this.sessionId})`);
          await this.subscribeAll();
        }
        break;
      }
      case "session_keepalive":
        break;
      case "notification":
        this.onNotification(msg.payload.subscription.type, msg.payload.event);
        break;
      case "session_reconnect": {
        // Twitch asks us to migrate; open the new URL before the old closes.
        this.expectingReconnect = true;
        this.log.info("eventsub reconnect requested by Twitch");
        this.open(msg.payload.session.reconnect_url);
        break;
      }
      case "revocation":
        this.log.warn("eventsub subscription revoked", msg.payload.subscription);
        break;
    }
  }

  private async subscribeAll(): Promise<void> {
    for (const sub of this.subs) {
      try {
        await this.auth.helix("broadcaster", "POST", "/eventsub/subscriptions", {
          type: sub.type,
          version: sub.version,
          condition: sub.condition,
          transport: { method: "websocket", session_id: this.sessionId },
        });
        this.log.info(`subscribed ${sub.type}`);
      } catch (e) {
        this.log.error(`subscribe ${sub.type} failed`, e);
      }
    }
  }

  private onClose(code: number): void {
    this.sessionId = undefined;
    if (this.closedByUs || this.expectingReconnect) return;
    const delay = this.backoffMs;
    this.backoffMs = Math.min(this.backoffMs * 2, 30_000);
    this.log.warn(`eventsub closed (${code}); reconnecting in ${delay}ms`);
    setTimeout(() => {
      if (!this.closedByUs) this.open(DEFAULT_URL);
    }, delay);
  }
}
