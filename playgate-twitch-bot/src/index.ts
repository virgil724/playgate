/**
 * index.ts — compose everything and run.
 *
 *   sources (chat + eventsub) → pipeline → policy / tokenPool / delivery / store
 *   admin server is always up so the streamer can authorize and tune policy.
 *
 * The Twitch sources only start once both roles (broadcaster + bot) are
 * authorized; first-time OAuth from the admin page triggers them with no restart.
 */
import { loadConfig } from "./config.js";
import { logger } from "./util.js";
import { PlayGateClient } from "./playgate/client.js";
import { TokenPool } from "./playgate/tokenPool.js";
import { GrantStore } from "./store/grantStore.js";
import { PolicyEngine } from "./grant/policy.js";
import { makePipeline, type GrantHandler } from "./grant/pipeline.js";
import { Stats } from "./admin/stats.js";
import { TwitchAuth } from "./twitch/auth.js";
import { WhisperSender, WhisperDelivery } from "./twitch/whisper.js";
import { EventSubClient } from "./twitch/eventsub.js";
import { ChatSource } from "./sources/chatCommand.js";
import { channelPointsSub, parseRedemption } from "./sources/channelPoints.js";
import {
  cheerSub,
  parseCheer,
  parseRaid,
  parseSubscribe,
  raidSub,
  subscriptionSub,
} from "./sources/events.js";
import { startAdminServer } from "./admin/server.js";
import type { GrantRequest } from "./grant/types.js";

async function main(): Promise<void> {
  const log = logger("bot");
  const cfg = loadConfig();
  const { file, secrets } = cfg;
  const roomId = secrets.PLAYGATE_ROOM_ID;

  // --- PlayGate side ---
  const client = new PlayGateClient(file.playgate.apiBase, secrets.PLAYGATE_HOST_API_KEY, logger("playgate"));
  await client.assertRoom(roomId); // fail fast on a bad key/room
  const pool = new TokenPool(client, roomId, file.playgate.tokenPool, logger("pool"));
  await pool.init();

  // --- shared state ---
  const store = new GrantStore(GrantStore.defaultPath(), logger("store"));
  const policy = new PolicyEngine(file.policy, store);
  const stats = new Stats();
  const auth = new TwitchAuth(
    secrets.TWITCH_CLIENT_ID,
    secrets.TWITCH_CLIENT_SECRET,
    TwitchAuth.defaultPath(),
    logger("auth"),
  );

  let eventsub: EventSubClient | undefined;
  let started = false;

  async function startTwitchIfReady(): Promise<void> {
    if (started) return;
    if (!auth.isAuthorized("bot") || !auth.isAuthorized("broadcaster")) {
      log.warn("waiting for authorization — open the admin page to connect broadcaster + bot");
      return;
    }
    started = true;
    const bot = auth.getUser("bot")!;
    const broadcaster = auth.getUser("broadcaster")!;

    // Wire the chat ⇄ pipeline ⇄ delivery cycle. `pipeline` is referenced
    // lazily by the chat handler, which only fires after chat.start().
    let pipeline: GrantHandler;
    const chat = new ChatSource(
      auth,
      file.twitch.channelLogin,
      file.twitch.botLogin,
      policy,
      (req) => pipeline(req),
      logger("chat"),
    );
    const whisper = new WhisperSender(auth, bot.id, logger("whisper"));
    const delivery = new WhisperDelivery(whisper, (m) => chat.say(m), logger("whisper"));
    pipeline = makePipeline({
      policy,
      pool,
      store,
      delivery,
      stats,
      webBase: file.playgate.webBase,
      roomId,
      log: logger("pipeline"),
    });

    await chat.start();

    // Subscribe to all event topics; policy decides per-grant whether each is
    // enabled, so toggling a source in the admin page needs no re-subscribe.
    const subs = [
      channelPointsSub(broadcaster.id),
      subscriptionSub(broadcaster.id),
      cheerSub(broadcaster.id),
      raidSub(broadcaster.id),
    ];
    eventsub = new EventSubClient(
      auth,
      subs,
      (type, event) => {
        let req: GrantRequest | null = null;
        switch (type) {
          case "channel.channel_points_custom_reward_redemption.add":
            req = parseRedemption(event);
            break;
          case "channel.subscribe":
            req = parseSubscribe(event);
            break;
          case "channel.cheer":
            req = parseCheer(event);
            break;
          case "channel.raid":
            req = parseRaid(event);
            break;
        }
        if (req) void pipeline(req).catch((e) => log.error("handler error (eventsub)", e));
      },
      logger("eventsub"),
    );
    eventsub.start();
    log.info("Twitch sources started");
  }

  startAdminServer({
    port: file.admin.port,
    bind: cfg.adminBind,
    policy,
    pool,
    store,
    stats,
    auth,
    eventsubConnected: () => eventsub?.connected() ?? false,
    configPath: cfg.paths.configYaml,
    log: logger("admin"),
    onAuthorized: () => void startTwitchIfReady(),
  });

  await startTwitchIfReady();

  const shutdown = () => {
    log.info("shutting down");
    store.flush();
    eventsub?.stop();
    process.exit(0);
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

main().catch((e) => {
  logger("bot").error("fatal", e);
  process.exit(1);
});
