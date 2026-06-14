import { describe, it, expect } from "vitest";
import {
  encodeInput,
  decodeInput,
  scaleAxis,
  unscaleAxis,
  BUTTON,
  INPUT_WIRE_SIZE,
  INPUT_WIRE_VERSION,
  AXIS_SCALE,
  emptyInputState,
} from "./input-codec";

describe("input-codec byte layout", () => {
  it("encodes exactly 17 bytes", () => {
    const buf = encodeInput(emptyInputState());
    expect(buf.byteLength).toBe(INPUT_WIRE_SIZE);
    expect(INPUT_WIRE_SIZE).toBe(17);
  });

  it("places version 0x02 at offset 0", () => {
    const v = new DataView(encodeInput(emptyInputState()));
    expect(v.getUint8(0)).toBe(INPUT_WIRE_VERSION);
    expect(v.getUint8(0)).toBe(2);
  });

  it("writes the sequence as little-endian uint32 at offset 13", () => {
    const v = new DataView(encodeInput(emptyInputState(), 0x01020304));
    expect(v.getUint32(13, true)).toBe(0x01020304);
  });

  it("writes buttons as little-endian uint32 at offset 1", () => {
    // 0x00020001 = DPAD_RIGHT | A
    const buttons = BUTTON.DPAD_RIGHT | BUTTON.A;
    const v = new DataView(encodeInput({ ...emptyInputState(), buttons }));
    // Little-endian: LSB first.
    expect(v.getUint8(1)).toBe(0x01);
    expect(v.getUint8(2)).toBe(0x00);
    expect(v.getUint8(3)).toBe(0x02);
    expect(v.getUint8(4)).toBe(0x00);
    expect(v.getUint32(1, true)).toBe(buttons >>> 0);
  });

  it("writes axes as little-endian int16 at offsets 5/7/9/11", () => {
    const v = new DataView(
      encodeInput({ buttons: 0, lx: 1, ly: -1, rx: 0, ry: 0.5 }),
    );
    expect(v.getInt16(5, true)).toBe(AXIS_SCALE); // lx = +1 -> +32767
    expect(v.getInt16(7, true)).toBe(-AXIS_SCALE); // ly = -1 -> -32767
    expect(v.getInt16(9, true)).toBe(0); // rx = 0
    expect(v.getInt16(11, true)).toBe(Math.round(0.5 * AXIS_SCALE)); // ry
  });

  it("uses little-endian for axes (byte order check)", () => {
    // +32767 = 0x7FFF -> LE bytes [0xFF, 0x7F]
    const v = new DataView(encodeInput({ ...emptyInputState(), lx: 1 }));
    expect(v.getUint8(5)).toBe(0xff);
    expect(v.getUint8(6)).toBe(0x7f);
  });
});

describe("axis scaling and clamping", () => {
  it("maps [-1,1] to [-32767, 32767]", () => {
    expect(scaleAxis(0)).toBe(0);
    expect(scaleAxis(1)).toBe(32767);
    expect(scaleAxis(-1)).toBe(-32767);
  });

  it("clamps out-of-range inputs", () => {
    expect(scaleAxis(2)).toBe(32767);
    expect(scaleAxis(-5)).toBe(-32767);
    expect(scaleAxis(1.0001)).toBe(32767);
  });

  it("never produces -32768", () => {
    for (let f = -2; f <= 2; f += 0.013) {
      expect(scaleAxis(f)).toBeGreaterThanOrEqual(-32767);
      expect(scaleAxis(f)).toBeLessThanOrEqual(32767);
    }
  });

  it("treats NaN as 0", () => {
    expect(scaleAxis(NaN)).toBe(0);
  });

  it("unscaleAxis reverses scaleAxis", () => {
    expect(unscaleAxis(32767)).toBeCloseTo(1, 5);
    expect(unscaleAxis(-32767)).toBeCloseTo(-1, 5);
    expect(unscaleAxis(0)).toBe(0);
  });

  it("unscaleAxis clamps -32768 to -1.0", () => {
    expect(unscaleAxis(-32768)).toBe(-1);
  });
});

describe("encode/decode round-trip", () => {
  it("round-trips a full state", () => {
    const state = {
      buttons: BUTTON.A | BUTTON.ZR | BUTTON.HOME | BUTTON.DPAD_LEFT,
      lx: 0.25,
      ly: -0.75,
      rx: -0.5,
      ry: 1,
    };
    const decoded = decodeInput(encodeInput(state));
    expect(decoded).not.toBeNull();
    expect(decoded!.buttons).toBe(state.buttons);
    expect(decoded!.lx).toBeCloseTo(state.lx, 4);
    expect(decoded!.ly).toBeCloseTo(state.ly, 4);
    expect(decoded!.rx).toBeCloseTo(state.rx, 4);
    expect(decoded!.ry).toBeCloseTo(state.ry, 4);
  });

  it("rejects wrong version byte", () => {
    const buf = encodeInput(emptyInputState());
    new DataView(buf).setUint8(0, 0x99);
    expect(decodeInput(buf)).toBeNull();
  });

  it("rejects short buffers", () => {
    expect(decodeInput(new ArrayBuffer(12))).toBeNull();
  });

  it("all 18 button bits round-trip independently", () => {
    for (const mask of Object.values(BUTTON)) {
      const decoded = decodeInput(
        encodeInput({ ...emptyInputState(), buttons: mask }),
      );
      expect(decoded!.buttons).toBe(mask);
    }
  });
});
