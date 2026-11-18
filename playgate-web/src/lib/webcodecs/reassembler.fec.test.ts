import { describe, it, expect } from "vitest";
import { FEC_RS, FEC_XOR, fecEncode } from "./fec";
import { FLAG_KEY, FLAG_PARAM, FLAG_PARITY, type Fragment } from "./protocol";
import { Reassembler, type AssembledFrame, type LossInfo } from "./reassembler";

const STRIDE = 10;

/** Builds all n fragments of one frame the way the Go sender does. */
function frameFrags(
  frameId: number,
  au: Uint8Array,
  scheme: number,
  m: number,
  opts: { key?: boolean } = {},
): Fragment[] {
  const k = Math.ceil(au.length / STRIDE);
  const shards: Uint8Array[] = [];
  const out: Fragment[] = [];
  const base = {
    version: 1,
    epoch: 0,
    frameId,
    ptsUs: frameId * 16000,
    frameSize: au.length,
    dataCount: k,
    parityCount: m,
    codec: 0,
    fecScheme: scheme,
    sendMs: frameId,
  };
  const flags = opts.key ? FLAG_KEY | FLAG_PARAM : 0;
  for (let i = 0; i < k; i++) {
    const payload = au.slice(i * STRIDE, Math.min(au.length, (i + 1) * STRIDE));
    const sh = new Uint8Array(STRIDE);
    sh.set(payload);
    shards.push(sh);
    out.push({ header: { ...base, flags, fragIdx: i }, payload });
  }
  fecEncode(scheme, shards, m).forEach((p, j) =>
    out.push({ header: { ...base, flags: flags | FLAG_PARITY, fragIdx: k + j }, payload: p }),
  );
  return out;
}

function setup() {
  const frames: AssembledFrame[] = [];
  const losses: LossInfo[] = [];
  const r = new Reassembler({
    onFrame: (f) => frames.push(f),
    onLoss: (l) => losses.push(l),
    onEpoch: () => {},
  });
  return { r, frames, losses };
}

function au(size: number, seed: number): Uint8Array {
  return Uint8Array.from({ length: size }, (_, i) => (i * 7 + seed) & 0xff);
}

describe("Reassembler FEC", () => {
  for (const scheme of [FEC_XOR, FEC_RS]) {
    it(`scheme ${scheme}: recovers lost fragments (incl. short last) instead of losing the frame`, () => {
      const { r, frames, losses } = setup();
      const data = au(47, 3); // k=5, last fragment has 7 bytes
      const frags = frameFrags(1, data, scheme, 2, { key: true });
      // drop data 4 (short last) and data 1 -> different XOR groups (4%2=0, 1%2=1)
      frags.filter((f) => f.header.fragIdx !== 4 && f.header.fragIdx !== 1).forEach((f) => r.push(f, 0));
      expect(losses).toHaveLength(0);
      expect(frames).toHaveLength(1);
      expect(frames[0].data).toEqual(data);
      expect(r.takeCounters().recovered).toBe(1);
    });

    it(`scheme ${scheme}: parity arriving before the data still works`, () => {
      const { r, frames } = setup();
      const data = au(30, 9); // k=3
      const frags = frameFrags(1, data, scheme, 1, { key: true });
      const parity = frags.filter((f) => f.header.fragIdx >= 3);
      const rest = frags.filter((f) => f.header.fragIdx < 3 && f.header.fragIdx !== 2);
      parity.forEach((f) => r.push(f, 0));
      rest.forEach((f) => r.push(f, 0));
      expect(frames).toHaveLength(1);
      expect(frames[0].data).toEqual(data);
    });

    it(`scheme ${scheme}: unrecoverable loss still declares the frame lost`, () => {
      const { r, frames, losses } = setup();
      // key frame first so deltas are accepted
      frameFrags(1, au(30, 1), scheme, 1, { key: true }).forEach((f) => r.push(f, 0));
      expect(frames).toHaveLength(1);
      const frags = frameFrags(2, au(50, 2), scheme, 1); // k=5, m=1
      frags.filter((f) => f.header.fragIdx !== 0 && f.header.fragIdx !== 2).forEach((f) => r.push(f, 1));
      r.poll(1000);
      expect(frames).toHaveLength(1);
      expect(losses).toHaveLength(1);
      expect(losses[0].lostId).toBe(2);
      expect(r.takeCounters().recovered).toBe(0);
    });
  }

  it("ignores parity with an unknown scheme", () => {
    const { r, frames } = setup();
    const frags = frameFrags(1, au(30, 4), FEC_RS, 1, { key: true });
    frags.forEach((f) => (f.header.fecScheme = 9));
    frags.filter((f) => f.header.fragIdx !== 1).forEach((f) => r.push(f, 0));
    expect(frames).toHaveLength(0);
  });
});
