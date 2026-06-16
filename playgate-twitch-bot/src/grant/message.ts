/**
 * message.ts — viewer-facing text: the whisper, its redeem link, and the
 * public chat fallback used when a whisper can't be delivered.
 */

/** Redeem link that pre-fills the code on the playgate-web room page. */
export function buildRedeemUrl(webBase: string, roomId: string, code: string): string {
  const base = webBase.replace(/\/$/, "");
  return `${base}/room/${encodeURIComponent(roomId)}?code=${encodeURIComponent(code)}`;
}

/** Private whisper containing the redeem link (and raw code as a backup). */
export function buildWhisper(redeemUrl: string, code: string): string {
  return `🎮 You're up! Redeem your PlayGate control code here: ${redeemUrl} (code: ${code})`;
}

/**
 * Public chat fallback when the whisper fails. Note: the link embeds the code,
 * so anyone in chat could redeem it first — pair with a per-user cooldown.
 */
export function buildPublicFallback(username: string, redeemUrl: string): string {
  return `@${username} couldn't whisper you — redeem your PlayGate code here: ${redeemUrl}`;
}
