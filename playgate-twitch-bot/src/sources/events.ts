/**
 * events.ts — subscription / cheer / raid EventSub topics → GrantRequest.
 *
 * Each event type is its own qualification (a new sub, enough bits, a big-enough
 * raid), so eligibility for these sources is normally "everyone". Anonymous
 * cheers carry no user id and are skipped (nobody to whisper). For raids the
 * grant goes to the raider (the from_broadcaster).
 */
import { NO_BADGES, type GrantRequest } from "../grant/types.js";
import type { SubSpec } from "../twitch/eventsub.js";

export function subscriptionSub(broadcasterUserId: string): SubSpec {
  return { type: "channel.subscribe", version: "1", condition: { broadcaster_user_id: broadcasterUserId } };
}

export function cheerSub(broadcasterUserId: string): SubSpec {
  return { type: "channel.cheer", version: "1", condition: { broadcaster_user_id: broadcasterUserId } };
}

export function raidSub(broadcasterUserId: string): SubSpec {
  return { type: "channel.raid", version: "1", condition: { to_broadcaster_user_id: broadcasterUserId } };
}

export function parseSubscribe(event: Record<string, any>): GrantRequest | null {
  if (!event?.user_id) return null;
  return {
    source: "subscription",
    twitchUserId: String(event.user_id),
    twitchUsername: event.user_name || event.user_login || "viewer",
    badges: { ...NO_BADGES },
  };
}

export function parseCheer(event: Record<string, any>): GrantRequest | null {
  if (event?.is_anonymous || !event?.user_id) return null;
  return {
    source: "cheer",
    twitchUserId: String(event.user_id),
    twitchUsername: event.user_name || event.user_login || "viewer",
    badges: { ...NO_BADGES },
    context: { bits: Number(event.bits) || 0 },
  };
}

export function parseRaid(event: Record<string, any>): GrantRequest | null {
  if (!event?.from_broadcaster_user_id) return null;
  return {
    source: "raid",
    twitchUserId: String(event.from_broadcaster_user_id),
    twitchUsername: event.from_broadcaster_user_name || event.from_broadcaster_user_login || "raider",
    badges: { ...NO_BADGES },
    context: { raidViewers: Number(event.viewers) || 0 },
  };
}
