import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // Run in Node environment — we mock Workers-specific globals (KV, fetch)
    // ourselves.  The @cloudflare/vitest-pool-workers pool is the proper approach
    // for full Workers runtime parity, but requires a wrangler.toml with a valid
    // KV namespace ID which is not available without a Cloudflare account.
    // All Workers APIs used in the code (Request, Response, URL, Headers,
    // crypto.subtle) are available in Node 22 natively.
    environment: "node",
    include: ["src/__tests__/**/*.test.ts"],
    globals: false,
  },
  resolve: {
    // Allow importing .ts files without extension in tests
    extensions: [".ts", ".js"],
  },
});
