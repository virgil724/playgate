/**
 * chatCommand.ts — IRC chat source (tmi.js).
 *
 * Listens for the configured trigger (e.g. "!play") and turns each invocation
 * into a GrantRequest(command). The same connection doubles as the whisper
 * fallback channel, so this also exposes say().
 */
import tmi from "tmi.js";
import type { TwitchAuth } from "../twitch/auth.js";
import type { PolicyEngine } from "../grant/policy.js";
import type { GrantHandler } from "../grant/pipeline.js";
import type { GrantRequest, ViewerBadges } from "../grant/types.js";
import type { Logger } from "../util.js";

/** Derive eligibility badges from tmi message tags (defensive — tags vary). */
function badgesFromTags(tags: tmi.ChatUserstate): ViewerBadges {
  const b = (tags.badges ?? {}) as Record<string, string | undefined>;
  return {
    sub: tags.subscriber === true || !!b.subscriber || !!b.founder,
    vip: (tags as Record<string, unknown>).vip === true || !!b.vip,
    mod: tags.mod === true || !!b.moderator,
    broadcaster: !!b.broadcaster,
  };
}

export class ChatSource {
  private client: tmi.Client;
  private channel: string;

  constructor(
    auth: TwitchAuth,
    channelLogin: string,
    botLogin: string,
    private policy: PolicyEngine,
    private handler: GrantHandler,
    private log: Logger,
  ) {
    this.channel = channelLogin.toLowerCase();
    // password as a function so reconnects pick up a refreshed token.
    const opts = {
      options: { skipUpdatingEmotesets: true },
      identity: {
        username: botLogin,
        password: async () => `oauth:${await auth.getValidToken("bot")}`,
      },
      channels: [this.channel],
    } as unknown as tmi.Options;
    this.client = new tmi.Client(opts);

    this.client.on("message", (_channel, tags, message, self) => {
      if (self) return;
      this.onMessage(tags, message);
    });
  }

  async start(): Promise<void> {
    await this.client.connect();
    this.log.info(`chat connected to #${this.channel}`);
  }

  /** Public chat send — used by the whisper fallback. */
  async say(message: string): Promise<void> {
    await this.client.say(this.channel, message);
  }

  private onMessage(tags: tmi.ChatUserstate, message: string): void {
    if (!this.policy.isEnabled("command")) return;
    const trigger = this.policy.getPolicy().sources.command.trigger;
    const text = message.trim();
    if (text !== trigger && !text.startsWith(`${trigger} `)) return;

    const userId = tags["user-id"];
    if (!userId) return; // can't dedupe or whisper without an id
    const req: GrantRequest = {
      source: "command",
      twitchUserId: userId,
      twitchUsername: tags["display-name"] || tags.username || "viewer",
      badges: badgesFromTags(tags),
    };
    void this.handler(req).catch((e) => this.log.error("handler error (command)", e));
  }
}
