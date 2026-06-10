/**
 * input-codec.ts — InputCommand 13-byte little-endian wire format.
 *
 * Mirrors playgate-host `internal/rtc/input_codec.go` and the spec in
 * playgate-host/docs/protocols.md §1.
 *
 * Layout (little-endian throughout):
 *   offset 0  uint8  version  (must equal 0x01)
 *   offset 1  uint32 buttons  (bitmask, see BUTTON below)
 *   offset 5  int16  lx       (left  stick X)
 *   offset 7  int16  ly       (left  stick Y)
 *   offset 9  int16  rx       (right stick X)
 *   offset 11 int16  ry       (right stick Y)
 *   total 13 bytes
 */

export const INPUT_WIRE_VERSION = 0x01;
export const INPUT_WIRE_SIZE = 13;
export const AXIS_SCALE = 32767;

/** Button bit positions / masks (uint32). Matches protocols.md §2. */
export const BUTTON = {
  A: 0x00000001,
  B: 0x00000002,
  X: 0x00000004,
  Y: 0x00000008,
  L: 0x00000010,
  R: 0x00000020,
  ZL: 0x00000040,
  ZR: 0x00000080,
  PLUS: 0x00000100,
  MINUS: 0x00000200,
  HOME: 0x00000400,
  CAPTURE: 0x00000800,
  L_STICK: 0x00001000,
  R_STICK: 0x00002000,
  DPAD_UP: 0x00004000,
  DPAD_DOWN: 0x00008000,
  DPAD_LEFT: 0x00010000,
  DPAD_RIGHT: 0x00020000,
} as const;

export type ButtonName = keyof typeof BUTTON;

/** All button names in bit order, useful for UIs and iteration. */
export const BUTTON_NAMES = Object.keys(BUTTON) as ButtonName[];

/** Normalised controller state. Axes are in [-1, 1]. */
export interface InputState {
  buttons: number; // uint32 bitmask
  lx: number;
  ly: number;
  rx: number;
  ry: number;
}

export function emptyInputState(): InputState {
  return { buttons: 0, lx: 0, ly: 0, rx: 0, ry: 0 };
}

/**
 * Scale a normalised axis value in [-1, 1] to a signed 16-bit integer:
 *   int16 = clamp(round(value * 32767), -32767, +32767)
 * Note: -32768 (0x8000) is intentionally never produced.
 */
export function scaleAxis(value: number): number {
  if (Number.isNaN(value)) value = 0;
  const scaled = Math.round(value * AXIS_SCALE);
  if (scaled > AXIS_SCALE) return AXIS_SCALE;
  if (scaled < -AXIS_SCALE) return -AXIS_SCALE;
  return scaled;
}

/** Reverse of scaleAxis. -32768 decodes to -1.0 (matches host clamp). */
export function unscaleAxis(int16: number): number {
  if (int16 <= -32768) return -1;
  return int16 / AXIS_SCALE;
}

/** Encode an InputState into a 13-byte ArrayBuffer. */
export function encodeInput(state: InputState): ArrayBuffer {
  const buf = new ArrayBuffer(INPUT_WIRE_SIZE);
  const v = new DataView(buf);
  v.setUint8(0, INPUT_WIRE_VERSION);
  v.setUint32(1, state.buttons >>> 0, true);
  v.setInt16(5, scaleAxis(state.lx), true);
  v.setInt16(7, scaleAxis(state.ly), true);
  v.setInt16(9, scaleAxis(state.rx), true);
  v.setInt16(11, scaleAxis(state.ry), true);
  return buf;
}

/**
 * Decode a 13-byte buffer back into an InputState. Returns null if the
 * length or version byte is wrong (matching the host's drop behaviour).
 */
export function decodeInput(buf: ArrayBuffer | DataView): InputState | null {
  const v = buf instanceof DataView ? buf : new DataView(buf);
  if (v.byteLength < INPUT_WIRE_SIZE) return null;
  if (v.getUint8(0) !== INPUT_WIRE_VERSION) return null;
  return {
    buttons: v.getUint32(1, true) >>> 0,
    lx: unscaleAxis(v.getInt16(5, true)),
    ly: unscaleAxis(v.getInt16(7, true)),
    rx: unscaleAxis(v.getInt16(9, true)),
    ry: unscaleAxis(v.getInt16(11, true)),
  };
}
