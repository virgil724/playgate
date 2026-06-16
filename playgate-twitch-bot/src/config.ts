/**
 * config.ts — load + validate configuration.
 *
 * Two sources, deliberately split:
 *   - .env       secrets (API keys, OAuth client secret)        — never edited by the UI
 *   - config.yaml non-secret tuning + the `policy` block        — the admin UI rewrites `policy`
 *
 * The `policy` block is the only part mutated at runtime; loadPolicy/savePolicy
 * round-trip it through config.yaml while preserving the rest of the file.
 */
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { parse, stringify } from "yaml";
import { z } from "zod";
import { atomicWriteFile } from "./util.js";

// ---- policy schema (edited by the admin page) ----

export const eligibilitySchema = z.enum(["everyone", "subscribers", "vips", "mods"]);
export type Eligibility = z.infer<typeof eligibilitySchema>;

const cooldownOverride = z.number().int().nonnegative().optional();

export const policySchema = z.object({
  global: z.object({
    perUserCooldownSec: z.number().int().nonnegative(),
    maxPerStream: z.number().int().positive(),
    maxPerMinute: z.number().int().positive(),
  }),
  sources: z.object({
    command: z.object({
      enabled: z.boolean(),
      trigger: z.string().min(1),
      eligibility: eligibilitySchema,
      perUserCooldownSec: cooldownOverride,
    }),
    channel_points: z.object({
      enabled: z.boolean(),
      rewardId: z.string(),
      eligibility: eligibilitySchema,
      perUserCooldownSec: cooldownOverride,
    }),
    subscription: z.object({
      enabled: z.boolean(),
      eligibility: eligibilitySchema,
      perUserCooldownSec: cooldownOverride,
    }),
    cheer: z.object({
      enabled: z.boolean(),
      minBits: z.number().int().nonnegative(),
      perUserCooldownSec: cooldownOverride,
    }),
    raid: z.object({
      enabled: z.boolean(),
      minViewers: z.number().int().nonnegative(),
      perUserCooldownSec: cooldownOverride,
    }),
  }),
});
export type Policy = z.infer<typeof policySchema>;

// ---- full config file schema ----

const configFileSchema = z.object({
  playgate: z.object({
    apiBase: z.string().url(),
    webBase: z.string().url(),
    tokenPool: z.object({
      batchSize: z.number().int().positive().max(100),
      lowWatermark: z.number().int().nonnegative(),
    }),
  }),
  twitch: z.object({
    channelLogin: z.string().min(1),
    botLogin: z.string().min(1),
  }),
  admin: z.object({ port: z.number().int().positive() }),
  policy: policySchema,
});
export type ConfigFile = z.infer<typeof configFileSchema>;

// ---- secrets from .env ----

const envSchema = z.object({
  PLAYGATE_HOST_API_KEY: z.string().min(1, "PLAYGATE_HOST_API_KEY is required"),
  PLAYGATE_ROOM_ID: z.string().min(1, "PLAYGATE_ROOM_ID is required"),
  TWITCH_CLIENT_ID: z.string().min(1, "TWITCH_CLIENT_ID is required"),
  TWITCH_CLIENT_SECRET: z.string().min(1, "TWITCH_CLIENT_SECRET is required"),
});
export type Secrets = z.infer<typeof envSchema>;

export interface Config {
  file: ConfigFile;
  secrets: Secrets;
  /** Address the admin server binds to (ADMIN_BIND, default loopback). */
  adminBind: string;
  paths: { configYaml: string };
}

export const CONFIG_PATH = resolve(process.cwd(), "config.yaml");

/**
 * Optional env overrides for the non-secret identifiers, so a container can be
 * configured entirely through the environment (12-factor) while config.yaml
 * still owns the policy block. Empty/undefined env vars are ignored.
 */
function applyEnvOverrides(file: ConfigFile): ConfigFile {
  const e = process.env;
  if (e.PLAYGATE_API_BASE) file.playgate.apiBase = e.PLAYGATE_API_BASE;
  if (e.PLAYGATE_WEB_BASE) file.playgate.webBase = e.PLAYGATE_WEB_BASE;
  if (e.TWITCH_CHANNEL_LOGIN) file.twitch.channelLogin = e.TWITCH_CHANNEL_LOGIN;
  if (e.TWITCH_BOT_LOGIN) file.twitch.botLogin = e.TWITCH_BOT_LOGIN;
  if (e.ADMIN_PORT) file.admin.port = Number(e.ADMIN_PORT);
  return file;
}

/** Load .env (if present) and config.yaml, validating both. Throws on error. */
export function loadConfig(configPath = CONFIG_PATH): Config {
  // process.loadEnvFile (Node >=20.12) reads .env into process.env — no dotenv dep.
  try {
    process.loadEnvFile();
  } catch {
    // .env is optional if the vars are already exported in the environment.
  }

  if (!existsSync(configPath)) {
    throw new Error(`config.yaml not found at ${configPath}. Copy config.example.yaml to config.yaml.`);
  }
  const raw = parse(readFileSync(configPath, "utf8"));
  const file = applyEnvOverrides(configFileSchema.parse(raw));
  const secrets = envSchema.parse(process.env);
  // Containers publish the admin page through a localhost-pinned port mapping,
  // so they bind 0.0.0.0 inside; bare-metal stays on loopback for safety.
  const adminBind = process.env.ADMIN_BIND || "127.0.0.1";
  return { file, secrets, adminBind, paths: { configYaml: configPath } };
}

/** Read just the policy block from config.yaml (used by the admin GET endpoint). */
export function loadPolicy(configPath = CONFIG_PATH): Policy {
  const raw = parse(readFileSync(configPath, "utf8"));
  return policySchema.parse(raw?.policy);
}

/**
 * Validate and persist a new policy block, preserving every other key in
 * config.yaml. Returns the validated policy. Atomic write — no torn files.
 */
export function savePolicy(next: unknown, configPath = CONFIG_PATH): Policy {
  const policy = policySchema.parse(next);
  const doc = (parse(readFileSync(configPath, "utf8")) ?? {}) as Record<string, unknown>;
  doc.policy = policy;
  atomicWriteFile(configPath, stringify(doc));
  return policy;
}
