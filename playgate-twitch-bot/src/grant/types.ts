/**
 * types.ts — the unified GrantRequest.
 *
 * Every trigger source (chat command, channel points, sub/cheer/raid) is
 * normalised into this single shape before entering the pipeline, so policy,
 * token issuance and delivery never need to know which source fired.
 */

export type GrantSource = "command" | "channel_points" | "subscription" | "cheer" | "raid";

export interface ViewerBadges {
  sub: boolean;
  vip: boolean;
  mod: boolean;
  broadcaster: boolean;
}

export interface GrantRequest {
  source: GrantSource;
  /** Numeric Twitch user id — the dedupe key (usernames change, ids don't). */
  twitchUserId: string;
  /** Login/display name, for whispers and the activity log. */
  twitchUsername: string;
  badges: ViewerBadges;
  /** Source-specific extras used by eligibility checks. */
  context?: {
    rewardId?: string;
    bits?: number;
    raidViewers?: number;
  };
}

export const NO_BADGES: ViewerBadges = { sub: false, vip: false, mod: false, broadcaster: false };
