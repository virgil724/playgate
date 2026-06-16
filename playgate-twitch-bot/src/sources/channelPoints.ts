/**
 * channelPoints.ts — Channel Points custom reward redemption → GrantRequest.
 *
 * EventSub carries no chat badges, so badges are all-false; channel_points
 * eligibility is expected to be "everyone" (the viewer already spent points).
 * The reward id is passed through for the policy's reward-match check.
 */
import { NO_BADGES, type GrantRequest } from "../grant/types.js";
import type { SubSpec } from "../twitch/eventsub.js";

export function channelPointsSub(broadcasterUserId: string): SubSpec {
  return {
    type: "channel.channel_points_custom_reward_redemption.add",
    version: "1",
    condition: { broadcaster_user_id: broadcasterUserId },
  };
}

export function parseRedemption(event: Record<string, any>): GrantRequest | null {
  if (!event?.user_id) return null;
  return {
    source: "channel_points",
    twitchUserId: String(event.user_id),
    twitchUsername: event.user_name || event.user_login || "viewer",
    badges: { ...NO_BADGES },
    context: { rewardId: event.reward?.id },
  };
}
