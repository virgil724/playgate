/** Shows only the first 6 and last 4 characters of an API key; short keys are fully masked. */
export function maskApiKey(key: string): string {
  if (key.length <= 12) return "•".repeat(key.length);
  return `${key.slice(0, 6)}${"•".repeat(8)}${key.slice(-4)}`;
}
