import { describe, it, expect } from "vitest";
import { FEC_RS, FEC_XOR, cauchy, fecEncode, fecRecover, gfExp, gfInv, gfMul } from "./fec";

// Golden vectors shared with playgate-host/internal/rtc/videofec_test.go.
// data[i][b] = (i*37 + b*11 + 5) & 0xff, k=4, stride=8, m=2.
const GOLD = {
  xor: ["4a4a7e564abad6ce", "5e4acadef6cadab6"],
  rs: ["b84d6d2bb562d3ff", "8a99c8d08dfeb396"],
};

const hex = (b: Uint8Array) => [...b].map((x) => x.toString(16).padStart(2, "0")).join("");

function goldData(): Uint8Array[] {
  return Array.from({ length: 4 }, (_, i) => Uint8Array.from({ length: 8 }, (_, b) => (i * 37 + b * 11 + 5) & 0xff));
}

/** Small deterministic PRNG (mulberry32). */
function rng(seed: number): () => number {
  let a = seed;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

describe("fec golden vectors", () => {
  it("GF(256) tables match the Go side", () => {
    expect(gfMul(0x53, 0xca)).toBe(143);
    expect(gfInv(0x53)).toBe(140);
    expect(gfExp[8]).toBe(0x1d);
    expect(gfExp[254]).toBe(142);
    expect(cauchy(4, 0, 0)).toBe(71);
    expect(cauchy(4, 1, 3)).toBe(122);
  });
  it("parity matches the Go side", () => {
    const x = fecEncode(FEC_XOR, goldData(), 2).map(hex);
    expect(x).toEqual(GOLD.xor);
    const r = fecEncode(FEC_RS, goldData(), 2).map(hex);
    expect(r).toEqual(GOLD.rs);
  });
});

describe("fecRecover", () => {
  it("RS recovers any <= m losses", () => {
    const rand = rng(7);
    for (const [k, m] of [
      [2, 1],
      [5, 2],
      [10, 3],
      [30, 6],
      [200, 55],
    ]) {
      const data = Array.from({ length: k }, () => Uint8Array.from({ length: 48 }, () => (rand() * 256) | 0));
      const par = fecEncode(FEC_RS, data, m);
      for (let trial = 0; trial < 20; trial++) {
        const shards: (Uint8Array | null)[] = [...data, ...par];
        const lose = Math.floor(rand() * (m + 1));
        const idx = [...shards.keys()].sort(() => rand() - 0.5).slice(0, lose);
        for (const i of idx) shards[i] = null;
        expect(fecRecover(FEC_RS, k, m, shards)).toBeGreaterThanOrEqual(0);
        for (let i = 0; i < k; i++) expect(shards[i]).toEqual(data[i]);
      }
    }
  });
  it("RS fails cleanly with more than m losses and leaves shards untouched", () => {
    const data = goldData();
    const par = fecEncode(FEC_RS, data, 2);
    const shards: (Uint8Array | null)[] = [...data, ...par];
    shards[0] = shards[2] = shards[3] = null;
    expect(fecRecover(FEC_RS, 4, 2, shards)).toBe(-1);
    expect(shards[0]).toBeNull();
    expect(shards[2]).toBeNull();
    expect(shards[3]).toBeNull();
  });
  it("XOR recovers one loss per interleaved group", () => {
    const data = Array.from({ length: 9 }, (_, i) => Uint8Array.from({ length: 16 }, (_, b) => (i * 13 + b) & 0xff));
    const par = fecEncode(FEC_XOR, data, 3);
    const shards: (Uint8Array | null)[] = [...data, ...par];
    shards[3] = shards[4] = shards[5] = null;
    expect(fecRecover(FEC_XOR, 9, 3, shards)).toBe(3);
    for (let i = 0; i < 9; i++) expect(shards[i]).toEqual(data[i]);
    const bad: (Uint8Array | null)[] = [...data, ...par];
    bad[0] = bad[3] = null;
    expect(fecRecover(FEC_XOR, 9, 3, bad)).toBe(-1);
  });
});
