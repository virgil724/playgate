import { describe, it, expect } from "vitest";
import {
  HEADER_SIZE,
  encodeFrameReport,
  encodeHello,
  encodeKeyframeRequest,
  encodeHeader,
  parseFragment,
  serialDiff16,
  serialDiff32,
} from "./protocol";

// Golden vector from playgate-host/docs/webcodecs-transport.md section 9.
const GOLDEN = Uint8Array.from([
  0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x10, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x09, 0x60, 0x00, 0x01, 0x00,
  0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64,
]);

const GOLDEN_HEADER = {
  version: 1,
  flags: 0x03,
  epoch: 1,
  frameId: 0x10,
  ptsUs: 0x10000,
  frameSize: 2400,
  fragIdx: 1,
  dataCount: 3,
  parityCount: 0,
  codec: 0,
  fecScheme: 0,
  sendMs: 0x64,
};

describe("fragment header", () => {
  it("parses the doc's golden vector", () => {
    expect(GOLDEN.length).toBe(HEADER_SIZE);
    const msg = new Uint8Array(HEADER_SIZE + 3);
    msg.set(GOLDEN);
    msg.set([7, 8, 9], HEADER_SIZE);
    const f = parseFragment(msg.buffer);
    expect(f).not.toBeNull();
    expect(f!.header).toEqual(GOLDEN_HEADER);
    expect([...f!.payload]).toEqual([7, 8, 9]);
  });

  it("encodes the golden header byte for byte", () => {
    expect([...encodeHeader(GOLDEN_HEADER)]).toEqual([...GOLDEN]);
  });

  it("drops unknown versions, short messages and impossible counts", () => {
    const bad = GOLDEN.slice();
    bad[0] = 2;
    expect(parseFragment(bad)).toBeNull();
    expect(parseFragment(GOLDEN.slice(0, 27))).toBeNull();
    expect(parseFragment(encodeHeader({ ...GOLDEN_HEADER, fragIdx: 3 }))).toBeNull();
    expect(parseFragment(encodeHeader({ ...GOLDEN_HEADER, dataCount: 0 }))).toBeNull();
  });

  it("ignores reserved flag bits", () => {
    const f = parseFragment(encodeHeader({ ...GOLDEN_HEADER, flags: 0xf3 }));
    expect(f!.header.flags & 0x03).toBe(0x03);
  });
});

describe("serial arithmetic", () => {
  it("handles wrap", () => {
    expect(serialDiff32(2, 0xfffffffe)).toBe(4);
    expect(serialDiff32(0xfffffffe, 2)).toBe(-4);
    expect(serialDiff16(1, 0xffff)).toBe(2);
  });
});

describe("feedback encoders", () => {
  it("HELLO is 12 bytes", () => {
    const b = encodeHello(0x07, 0, 0);
    expect(b.length).toBe(12);
    expect([...b.slice(0, 5)]).toEqual([1, 0x03, 0, 0, 0x07]);
  });

  it("KEYFRAME_REQUEST is 16 bytes, big-endian", () => {
    const b = encodeKeyframeRequest(0x0102, 1, 0xffffffff, 0x10);
    expect(b.length).toBe(16);
    expect([...b]).toEqual([1, 1, 0, 0, 1, 2, 1, 0, 255, 255, 255, 255, 0, 0, 0, 0x10]);
  });

  it("FRAME_REPORT is 24 bytes and saturates", () => {
    const b = encodeFrameReport({
      highestId: 5,
      framesComplete: 70000,
      framesLost: 1,
      fragsRecv: 2,
      fragsExpected: 3,
      recovered: 0,
      decodeQueue: 4,
      jitterMsX10: 5,
      decMsX10: 6,
    });
    expect(b.length).toBe(24);
    const v = new DataView(b.buffer);
    expect(v.getUint8(1)).toBe(2);
    expect(v.getUint32(4)).toBe(5);
    expect(v.getUint16(8)).toBe(0xffff);
    expect(v.getUint16(22)).toBe(6);
  });
});
