import { describe, it, expect } from "vitest";
import { FLAG_KEY, FLAG_PARAM, type Fragment } from "./protocol";
import { Reassembler, type AssembledFrame, type LossInfo } from "./reassembler";

const STRIDE = 10;

function frag(frameId: number, idx: number, size: number, key = false): Fragment {
  const k = Math.ceil(size / STRIDE);
  const len = idx === k - 1 ? size - STRIDE * (k - 1) : STRIDE;
  return {
    header: {
      version: 1,
      flags: key ? FLAG_KEY | FLAG_PARAM : 0,
      epoch: 0,
      frameId,
      ptsUs: frameId * 16000,
      frameSize: size,
      fragIdx: idx,
      dataCount: k,
      parityCount: 0,
      codec: 0,
      fecScheme: 0,
      sendMs: frameId * 16,
    },
    payload: new Uint8Array(len).fill(idx + 1),
  };
}

function setup() {
  const frames: AssembledFrame[] = [];
  const losses: LossInfo[] = [];
  const nacks: { frameId: number; indices: number[] }[] = [];
  const r = new Reassembler({
    frameTimeoutMs: () => 100,
    reorderWaitMs: () => 5,
    nackDelayMs: () => 10,
    nackRetryMs: () => 40,
    maxNacks: 2,
    onNack: (_e, frameId, indices) => nacks.push({ frameId, indices }),
    onFrame: (f) => frames.push(f),
    onLoss: (l) => losses.push(l),
    onEpoch: () => {},
  });
  const whole = (id: number, size: number, t: number, key = false) => {
    for (let i = 0; i < Math.ceil(size / STRIDE); i++) r.push(frag(id, i, size, key), t);
  };
  return { r, frames, losses, nacks, whole };
}

describe("Reassembler FRAG_NACK", () => {
  it("asks for the missing fragments and completes on the retransmission", () => {
    const { r, frames, losses, nacks, whole } = setup();
    whole(1, 25, 0, true);
    r.push(frag(2, 0, 25), 16);
    r.push(frag(2, 2, 25), 16); // fragment 1 lost
    r.poll(20);
    expect(nacks).toEqual([]);
    r.poll(27);
    expect(nacks).toEqual([{ frameId: 2, indices: [1] }]);
    r.push(frag(2, 1, 25), 60); // retransmission, past the old 33 ms timeout
    expect(frames.map((f) => f.frameId)).toEqual([1, 2]);
    expect(losses).toEqual([]);
  });

  it("asks for a frame that never arrived and delivers it in order", () => {
    const { r, frames, losses, nacks, whole } = setup();
    whole(1, 15, 0, true);
    whole(3, 15, 32); // frame 2 missing entirely
    r.poll(40);
    expect(frames.map((f) => f.frameId)).toEqual([1]);
    r.poll(43);
    expect(nacks).toEqual([{ frameId: 2, indices: [] }]);
    whole(2, 15, 70);
    expect(frames.map((f) => f.frameId)).toEqual([1, 2, 3]);
    expect(losses).toEqual([]);
  });

  it("retries, then declares the frame lost at the frame timeout", () => {
    const { r, losses, nacks, whole } = setup();
    whole(1, 15, 0, true);
    r.push(frag(2, 0, 25), 16);
    for (let t = 17; t <= 200; t++) r.poll(t);
    expect(nacks.map((n) => n.frameId)).toEqual([2, 2]);
    expect(losses.map((l) => l.lostId)).toEqual([2]);
  });

  it("never schedules a deadline that is already past while waiting on a gap", () => {
    const { r, whole } = setup();
    whole(1, 15, 0, true);
    whole(3, 15, 32); // gap at 2, noticed at 32
    for (const t of [40, 42, 60, 82, 100]) {
      r.poll(t);
      const d = r.nextDeadline();
      expect(d === null || d > t).toBe(true);
    }
    expect(r.nextDeadline()).toBe(132); // the gap's loss deadline
  });

  it("stops NACKing gaps once a keyframe is required", () => {
    const { r, nacks, whole } = setup();
    whole(1, 15, 0, true);
    whole(3, 15, 32);
    r.requireKey();
    for (let t = 33; t <= 200; t++) r.poll(t);
    expect(nacks).toEqual([]);
    expect(r.nextDeadline()).toBeNull();
  });

  it("schedules a poll for the NACK deadline", () => {
    const { r, whole } = setup();
    whole(1, 15, 0, true);
    r.push(frag(2, 0, 25), 16);
    expect(r.nextDeadline()).toBe(26);
  });
});

describe("Reassembler counters with retransmission", () => {
  it("does not count a duplicate fragment as received twice", () => {
    const { r, whole } = setup();
    whole(1, 15, 0, true);
    r.push(frag(2, 0, 25), 16);
    r.push(frag(2, 0, 25), 30); // retransmitted duplicate
    r.push(frag(2, 1, 25), 30);
    r.push(frag(2, 2, 25), 30);
    whole(2, 25, 40); // late duplicates of a delivered frame
    const c = r.takeCounters();
    expect(c.fragsRecv).toBe(5);
    expect(c.fragsExpected).toBe(5);
  });

  it("counts parity that arrives after its frame was delivered", () => {
    const { r } = setup();
    const withParity = (f: Fragment): Fragment => ({ ...f, header: { ...f.header, parityCount: 1, fecScheme: 1 } });
    for (let i = 0; i < 2; i++) r.push(withParity(frag(1, i, 15, true)), 0);
    const parity = withParity(frag(1, 0, 15, true));
    parity.header = { ...parity.header, fragIdx: 2, flags: parity.header.flags | 0x04 };
    r.push(parity, 1); // frame 1 already complete and delivered
    const c = r.takeCounters();
    expect(c.fragsExpected).toBe(3);
    expect(c.fragsRecv).toBe(3);
  });
});
