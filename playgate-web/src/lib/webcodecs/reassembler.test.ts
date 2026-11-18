import { describe, it, expect } from "vitest";
import { FLAG_KEY, FLAG_PARAM, FLAG_PARITY, type Fragment } from "./protocol";
import { Reassembler, type AssembledFrame, type LossInfo } from "./reassembler";

const STRIDE = 10;

function frag(
  frameId: number,
  idx: number,
  size: number,
  opts: { key?: boolean; epoch?: number; parity?: boolean; stride?: number; fill?: number } = {},
): Fragment {
  const stride = opts.stride ?? STRIDE;
  const k = Math.ceil(size / stride);
  const len = idx === k - 1 ? size - stride * (k - 1) : stride;
  return {
    header: {
      version: 1,
      flags: (opts.key ? FLAG_KEY | FLAG_PARAM : 0) | (opts.parity ? FLAG_PARITY : 0),
      epoch: opts.epoch ?? 0,
      frameId,
      ptsUs: frameId * 16000,
      frameSize: size,
      fragIdx: idx,
      dataCount: k,
      parityCount: opts.parity ? 1 : 0,
      codec: 0,
      fecScheme: 0,
      sendMs: frameId * 16,
    },
    payload: new Uint8Array(len).fill(opts.fill ?? idx + 1),
  };
}

function setup() {
  const frames: AssembledFrame[] = [];
  const losses: LossInfo[] = [];
  const epochs: number[] = [];
  const r = new Reassembler({
    onFrame: (f) => frames.push(f),
    onLoss: (l) => losses.push(l),
    onEpoch: (e) => epochs.push(e),
  });
  const send = (f: Fragment, t: number) => r.push(f, t);
  const whole = (id: number, size: number, t: number, o: Parameters<typeof frag>[3] = {}) => {
    const k = Math.ceil(size / (o.stride ?? STRIDE));
    for (let i = 0; i < k; i++) send(frag(id, i, size, o), t);
  };
  return { r, frames, losses, epochs, send, whole };
}

describe("Reassembler", () => {
  it("reassembles a keyframe, including out-of-order fragments", () => {
    const { frames, send } = setup();
    send(frag(1, 2, 25, { key: true }), 0);
    send(frag(1, 0, 25, { key: true }), 0);
    expect(frames).toHaveLength(0);
    send(frag(1, 1, 25, { key: true }), 1);
    expect(frames).toHaveLength(1);
    expect(frames[0].key).toBe(true);
    expect([...frames[0].data]).toEqual([...Array(10).fill(1), ...Array(10).fill(2), ...Array(5).fill(3)]);
  });

  it("works when only the last fragment and a middle one arrive first", () => {
    const { frames, send } = setup();
    send(frag(1, 3, 35, { key: true }), 0);
    send(frag(1, 1, 35, { key: true }), 0);
    send(frag(1, 0, 35, { key: true }), 0);
    send(frag(1, 2, 35, { key: true }), 0);
    expect(frames).toHaveLength(1);
    expect(frames[0].data.length).toBe(35);
  });

  it("ignores deltas until a key frame completes", () => {
    const { frames, whole } = setup();
    whole(1, 10, 0);
    whole(2, 10, 1);
    expect(frames).toHaveLength(0);
    whole(3, 10, 2, { key: true });
    whole(4, 10, 3);
    expect(frames.map((f) => f.frameId)).toEqual([3, 4]);
  });

  it("holds a later frame for the reorder window, then delivers in order", () => {
    const { r, frames, losses, send, whole } = setup();
    whole(1, 10, 0, { key: true });
    send(frag(2, 0, 20), 1); // frame 2 incomplete
    whole(3, 10, 2); // frame 3 complete first
    expect(frames.map((f) => f.frameId)).toEqual([1]);
    send(frag(2, 1, 20), 4); // arrives within 5 ms of frame 3 completing
    r.poll(4);
    expect(frames.map((f) => f.frameId)).toEqual([1, 2, 3]);
    expect(losses).toHaveLength(0);
  });

  it("declares a frame lost after the reorder wait and drops deltas until KEY", () => {
    const { r, frames, losses, send, whole } = setup();
    whole(1, 10, 0, { key: true });
    send(frag(2, 0, 20), 1); // never completes
    whole(3, 10, 2);
    expect(r.nextDeadline()).toBe(7);
    r.poll(6);
    expect(losses).toHaveLength(0);
    r.poll(7);
    expect(losses).toEqual([{ epoch: 0, lostId: 2, lastGoodId: 1 }]);
    expect(r.waitingForKey).toBe(true);
    whole(4, 10, 8); // delta after the gap: dropped
    whole(5, 10, 9, { key: true });
    whole(6, 10, 10);
    expect(frames.map((f) => f.frameId)).toEqual([1, 5, 6]);
    expect(losses).toHaveLength(1);
  });

  it("times out an incomplete frame with no later frame", () => {
    const { r, losses, send, whole } = setup();
    whole(1, 10, 0, { key: true });
    send(frag(2, 0, 20), 1);
    r.poll(10);
    expect(losses).toHaveLength(0);
    r.poll(21);
    expect(losses.map((l) => l.lostId)).toEqual([2]);
  });

  it("a lost IDR is handled like any lost frame", () => {
    const { r, frames, losses, send, whole } = setup();
    send(frag(1, 0, 30, { key: true }), 0); // fragment 1 and 2 lost
    r.poll(25);
    expect(losses.map((l) => l.lostId)).toEqual([1]);
    whole(2, 10, 30, { key: true });
    expect(frames.map((f) => f.frameId)).toEqual([2]);
  });

  it("evicts the oldest partial frame beyond 8 in flight", () => {
    const { r, losses, send, whole } = setup();
    whole(1, 10, 0, { key: true });
    for (let id = 2; id <= 10; id++) send(frag(id, 0, 20), 1); // 9 partial frames
    // Eviction counts as a loss: deltas are dropped until the next KEY.
    expect(losses.map((l) => l.lostId)).toEqual([2]);
    expect(r.waitingForKey).toBe(true);
    expect(r.inflight).toBe(0);
  });

  it("ignores duplicates, late frames, bad lengths and parity", () => {
    const { r, frames, send, whole } = setup();
    whole(1, 10, 0, { key: true });
    whole(1, 10, 1, { key: true }); // late duplicate of delivered frame
    send(frag(2, 0, 20), 2);
    send(frag(2, 0, 20), 2); // duplicate fragment
    const bad = frag(2, 1, 20);
    bad.payload = new Uint8Array(3);
    send(bad, 2);
    send(frag(2, 1, 20, { parity: true }), 2);
    expect(frames).toHaveLength(1);
    send(frag(2, 1, 20), 3);
    expect(frames.map((f) => f.frameId)).toEqual([1, 2]);
    expect(r.inflight).toBe(0);
  });

  it("an epoch change resets state and requires a key", () => {
    const { frames, epochs, whole } = setup();
    whole(5, 10, 0, { key: true, epoch: 0 });
    whole(6, 10, 1, { epoch: 1 }); // new epoch, delta: not decodable
    expect(epochs).toEqual([1]);
    whole(7, 10, 2, { epoch: 1, key: true });
    whole(4, 10, 3, { epoch: 0, key: true }); // stale epoch ignored
    expect(frames.map((f) => f.frameId)).toEqual([5, 7]);
  });

  it("handles frame_id wraparound", () => {
    const { frames, whole } = setup();
    whole(0xfffffffe, 10, 0, { key: true });
    whole(0xffffffff, 10, 1);
    whole(0, 10, 2);
    expect(frames.map((f) => f.frameId)).toEqual([0xfffffffe, 0xffffffff, 0]);
  });

  it("counts fragments for FRAME_REPORT", () => {
    const { r, whole, send } = setup();
    whole(1, 25, 0, { key: true }); // 3 fragments
    send(frag(2, 0, 20), 1); // 1 of 2
    const c = r.takeCounters();
    expect(c.fragsRecv).toBe(4);
    expect(c.fragsExpected).toBe(5);
    expect(c.framesComplete).toBe(1);
    expect(c.highestId).toBe(2);
    expect(r.takeCounters().fragsRecv).toBe(0);
  });
});
