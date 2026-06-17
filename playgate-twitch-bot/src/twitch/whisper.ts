/**
 * whisper.ts — deliver codes by Twitch whisper, with a public chat fallback.
 *
 * Whispers are how we keep a code private (it redeems without auth, so posting
 * it openly invites theft). But Helix whispers are heavily rate-limited
 * (~1/sec, and the sending account must have a verified phone number), so:
 *   - WhisperSender serializes sends and spaces them out.
 *   - WhisperDelivery falls back to a public @mention + redeem link when a
 *     whisper can't be delivered (rate limit / recipient blocks strangers).
 */
import { sleep, type Logger } from "../util.js";
import type { TwitchAuth } from "./auth.js";
import type { GrantRequest } from "../grant/types.js";
import type { Delivery, DeliveryResult } from "../grant/pipeline.js";
import { buildPublicFallback, buildWhisper } from "../grant/message.js";

/** Serializes whispers and spaces them to respect Twitch's ~1/sec limit. */
export class WhisperSender {
  private chain: Promise<void> = Promise.resolve();
  private lastSentAt = 0;

  constructor(
    private auth: TwitchAuth,
    readonly fromUserId: string,
    private log: Logger,
    private minIntervalMs = 1100,
  ) {}

  /** Queue a whisper; resolves when sent, rejects if Twitch rejects it. */
  send(toUserId: string, message: string): Promise<void> {
    const prev = this.chain;
    let release!: () => void;
    const done = new Promise<void>((r) => (release = r));
    this.chain = prev.then(() => done); // keep the chain serial

    return (async () => {
      await prev.catch(() => {}); // wait our turn regardless of prior failures
      try {
        const wait = this.minIntervalMs - (Date.now() - this.lastSentAt);
        if (wait > 0) await sleep(wait);
        await this.auth.helix(
          "bot",
          "POST",
          `/whispers?from_user_id=${encodeURIComponent(this.fromUserId)}&to_user_id=${encodeURIComponent(toUserId)}`,
          { message },
        );
        this.lastSentAt = Date.now();
        this.log.debug(`whispered to ${toUserId}`);
      } finally {
        release();
      }
    })();
  }
}

export type ChatSay = (message: string) => Promise<void>;

export type DeliveryMode = "whisper" | "chat" | "dual";

/** Delivery strategy: try a whisper, fall back to public chat. */
export class WhisperDelivery implements Delivery {
  constructor(
    private whisper: WhisperSender,
    private chatSay: ChatSay,
    private mode: DeliveryMode,
    private log: Logger,
  ) {}

  async deliver(req: GrantRequest, code: string, redeemUrl: string): Promise<DeliveryResult> {
    // Chat-only mode: skip whisper entirely.
    if (this.mode === "chat") {
      try {
        await this.chatSay(buildPublicFallback(req.twitchUsername, redeemUrl));
        return "chat";
      } catch (e) {
        this.log.error(`chat delivery failed for ${req.twitchUsername}`, e);
        return "failed";
      }
    }

    // Dual mode: fire whisper + chat simultaneously; chat guarantees delivery.
    if (this.mode === "dual") {
      if (req.twitchUserId !== this.whisper.fromUserId) {
        this.whisper
          .send(req.twitchUserId, buildWhisper(redeemUrl, code))
          .catch((e) => this.log.warn(`whisper to ${req.twitchUsername} failed`, e));
      }
      try {
        await this.chatSay(buildPublicFallback(req.twitchUsername, redeemUrl));
        return "whisper";
      } catch (e) {
        this.log.error(`chat delivery failed for ${req.twitchUsername}`, e);
        return "failed";
      }
    }

    // Whisper-first mode: try whisper, fall back to public chat.
    if (req.twitchUserId !== this.whisper.fromUserId) {
      try {
        await this.whisper.send(req.twitchUserId, buildWhisper(redeemUrl, code));
        return "whisper";
      } catch (e) {
        this.log.warn(`whisper to ${req.twitchUsername} failed; trying chat fallback`, e);
      }
    } else {
      this.log.info(`${req.twitchUsername} is the bot's own account; can't whisper self, using chat`);
    }
    try {
      await this.chatSay(buildPublicFallback(req.twitchUsername, redeemUrl));
      return "fallback";
    } catch (e) {
      this.log.error(`chat fallback failed for ${req.twitchUsername}`, e);
      return "failed";
    }
  }
}
