/**
 * server.ts — local-only admin page (bound to 127.0.0.1).
 *
 * Serves the policy editor + dashboard and the small JSON API behind it:
 *   GET  /api/policy        current policy
 *   PUT  /api/policy        validate, persist to config.yaml, hot-swap (no restart)
 *   GET  /api/status        pool size, stream count, auth + eventsub state, recent grants
 *   POST /api/reset-stream  start a new stream session (resets the per-stream cap)
 *   GET  /connect/:role     begin Twitch OAuth for broadcaster|bot
 *   GET  /callback          OAuth redirect target → exchange code
 */
import { fileURLToPath } from "node:url";
import type { Server } from "node:http";
import express from "express";
import { savePolicy, saveDeliveryConfig, loadDeliveryConfig } from "../config.js";
import type { PolicyEngine } from "../grant/policy.js";
import type { TokenPool } from "../playgate/tokenPool.js";
import type { GrantStore } from "../store/grantStore.js";
import type { Stats } from "./stats.js";
import { type Role, TwitchAuth } from "../twitch/auth.js";
import type { Logger } from "../util.js";

export interface AdminDeps {
  port: number;
  /** Bind address: 127.0.0.1 on bare metal, 0.0.0.0 in a container. */
  bind: string;
  policy: PolicyEngine;
  pool: TokenPool;
  store: GrantStore;
  stats: Stats;
  auth: TwitchAuth;
  eventsubConnected: () => boolean;
  configPath: string;
  log: Logger;
  /** Called after a successful OAuth exchange so sources can start without a restart. */
  onAuthorized?: () => void;
}

const PUBLIC_DIR = fileURLToPath(new URL("./public", import.meta.url));

export function startAdminServer(deps: AdminDeps): Server {
  const app = express();
  app.use(express.json());
  const redirectUri = `http://localhost:${deps.port}/callback`;

  app.get("/api/policy", (_req, res) => res.json(deps.policy.getPolicy()));

  app.put("/api/policy", (req, res) => {
    try {
      const saved = savePolicy(req.body, deps.configPath);
      deps.policy.setPolicy(saved); // hot-swap — takes effect on the next grant
      deps.log.info("policy updated via admin page");
      res.json(saved);
    } catch (e) {
      res.status(400).json({ error: e instanceof Error ? e.message : String(e) });
    }
  });

  app.get("/api/status", (_req, res) => {
    res.json({
      poolSize: deps.pool.size(),
      streamGrants: deps.store.streamCount(),
      streamStartedAt: deps.store.streamStartedAt(),
      totalUsers: deps.store.totalUsers(),
      eventsub: deps.eventsubConnected(),
      delivery: loadDeliveryConfig(deps.configPath),
      auth: {
        broadcaster: deps.auth.getUser("broadcaster") ?? null,
        bot: deps.auth.getUser("bot") ?? null,
      },
      stats: deps.stats.snapshot(),
      recent: deps.store.recent(50),
    });
  });

  app.get("/api/delivery", (_req, res) => {
    res.json(loadDeliveryConfig(deps.configPath));
  });

  app.put("/api/delivery", (req, res) => {
    try {
      const saved = saveDeliveryConfig(req.body, deps.configPath);
      deps.log.info(`delivery mode updated to ${saved.mode}`);
      res.json(saved);
    } catch (e) {
      res.status(400).json({ error: e instanceof Error ? e.message : String(e) });
    }
  });

  app.post("/api/reset-stream", (_req, res) => {
    deps.store.resetStream();
    res.json({ ok: true });
  });

  app.get("/connect/:role", (req, res) => {
    const role = req.params.role;
    if (role !== "broadcaster" && role !== "bot") {
      res.status(400).send("bad role");
      return;
    }
    res.redirect(deps.auth.authorizeUrl(role as Role, redirectUri, role));
  });

  app.get("/callback", async (req, res) => {
    const code = String(req.query.code ?? "");
    const role = String(req.query.state ?? "");
    if (!code || (role !== "broadcaster" && role !== "bot")) {
      res.status(400).send("missing code/state");
      return;
    }
    try {
      await deps.auth.exchangeCode(role as Role, code, redirectUri);
      deps.onAuthorized?.();
      res.redirect(`/?connected=${role}`);
    } catch (e) {
      res.status(500).send(`auth failed: ${e instanceof Error ? e.message : String(e)}`);
    }
  });

  app.use(express.static(PUBLIC_DIR));

  return app.listen(deps.port, deps.bind, () => {
    deps.log.info(`admin page on http://localhost:${deps.port} (bind ${deps.bind})`);
  });
}
