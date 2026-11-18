import { describe, expect, it } from "vitest";
import { maskApiKey } from "./mask";

describe("maskApiKey", () => {
  it("keeps only the first 6 and last 4 characters of a real key", () => {
    const key = "39d5429b181d38858cfde89d6b8d43431d03b128ff54f7fdba44f4d96b9f8b85";
    const masked = maskApiKey(key);
    expect(masked).toBe("39d542••••••••8b85");
    expect(masked).not.toContain(key.slice(6, -4));
  });
  it("fully masks short keys", () => {
    expect(maskApiKey("abc")).toBe("•••");
    expect(maskApiKey("")).toBe("");
  });
});
