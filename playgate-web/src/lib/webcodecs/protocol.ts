/**
 * protocol.ts — wire format for the WebCodecs video transport
 * (playgate-host/docs/webcodecs-transport.md sections 3 and 4).
 * All integers are big-endian. Framework-free.
 */

export const VIDEO_CHANNEL_LABEL = "video";
export const VIDEOFB_CHANNEL_LABEL = "videofb";

export const HEADER_SIZE = 28;
export const WIRE_VERSION = 1;

export const FLAG_KEY = 0x01;
export const FLAG_PARAM = 0x02;
export const FLAG_PARITY = 0x04;

export interface FragmentHeader {
  version: number;
  flags: number;
  epoch: number;
  frameId: number;
  ptsUs: number;
  frameSize: number;
  fragIdx: number;
  dataCount: number;
  parityCount: number;
  codec: number;
  fecScheme: number;
  sendMs: number;
}

export interface Fragment {
  header: FragmentHeader;
  payload: Uint8Array;
}

/** Parses one DataChannel message. Returns null for anything invalid or of an
 * unknown version (the receiver must drop those, never throw). */
export function parseFragment(buf: ArrayBuffer | Uint8Array): Fragment | null {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (bytes.byteLength < HEADER_SIZE) return null;
  const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const version = v.getUint8(0);
  if (version !== WIRE_VERSION) return null;
  const header: FragmentHeader = {
    version,
    flags: v.getUint8(1),
    epoch: v.getUint16(2),
    frameId: v.getUint32(4),
    ptsUs: v.getUint32(8),
    frameSize: v.getUint32(12),
    fragIdx: v.getUint16(16),
    dataCount: v.getUint16(18),
    parityCount: v.getUint16(20),
    codec: v.getUint8(22),
    fecScheme: v.getUint8(23),
    sendMs: v.getUint32(24),
  };
  if (header.dataCount < 1) return null;
  if (header.fragIdx >= header.dataCount + header.parityCount) return null;
  if (header.frameSize < 1) return null;
  return { header, payload: bytes.subarray(HEADER_SIZE) };
}

/** Inverse of the header read in parseFragment; used by tests. */
export function encodeHeader(h: FragmentHeader): Uint8Array {
  const out = new Uint8Array(HEADER_SIZE);
  const v = new DataView(out.buffer);
  v.setUint8(0, h.version);
  v.setUint8(1, h.flags);
  v.setUint16(2, h.epoch);
  v.setUint32(4, h.frameId);
  v.setUint32(8, h.ptsUs);
  v.setUint32(12, h.frameSize);
  v.setUint16(16, h.fragIdx);
  v.setUint16(18, h.dataCount);
  v.setUint16(20, h.parityCount);
  v.setUint8(22, h.codec);
  v.setUint8(23, h.fecScheme);
  v.setUint32(24, h.sendMs);
  return out;
}

/** Serial-number comparison (RFC 1982 style): >0 if a is after b. */
export function serialDiff32(a: number, b: number): number {
  return (a - b) | 0;
}
export function serialDiff16(a: number, b: number): number {
  return ((a - b) << 16) >> 16;
}

// ---- viewer -> host feedback (channel "videofb", reliable) ----

export const FB_KEYFRAME_REQUEST = 0x01;
export const FB_FRAME_REPORT = 0x02;
export const FB_HELLO = 0x03;

export const KEYREASON = {
  unknown: 0,
  frameLost: 1,
  decoderError: 2,
  queueBacklog: 3,
  initialJoin: 4,
  waitingAfterEpoch: 5,
} as const;

export const CAP_WEBCODECS_H264 = 0x01;
export const CAP_HARDWARE = 0x02;
export const CAP_ANNEXB_NO_DESC = 0x04;
export const CAP_FEC_XOR = 0x08;
export const CAP_FEC_RS = 0x10;
/** Max parity_count this viewer will process (HELLO max_fec). */
export const MAX_FEC_ADVERTISED = 255;

export const NO_FRAME_ID = 0xffffffff;

function prefix(len: number, type: number): { out: Uint8Array; v: DataView } {
  const out = new Uint8Array(len);
  const v = new DataView(out.buffer);
  v.setUint8(0, 1); // version
  v.setUint8(1, type);
  // flags (u16 at offset 2) reserved = 0
  return { out, v };
}

const sat16 = (n: number) => Math.max(0, Math.min(0xffff, Math.round(n)));

export function encodeHello(caps: number, maxFec: number, reportMs: number): Uint8Array {
  const { out, v } = prefix(12, FB_HELLO);
  v.setUint8(4, caps);
  v.setUint8(5, maxFec);
  v.setUint16(6, reportMs);
  return out;
}

export function encodeKeyframeRequest(
  epoch: number,
  reason: number,
  lastGoodId: number,
  lostId: number,
): Uint8Array {
  const { out, v } = prefix(16, FB_KEYFRAME_REQUEST);
  v.setUint16(4, epoch & 0xffff);
  v.setUint8(6, reason);
  v.setUint32(8, lastGoodId >>> 0);
  v.setUint32(12, lostId >>> 0);
  return out;
}

export interface FrameReport {
  highestId: number;
  framesComplete: number;
  framesLost: number;
  fragsRecv: number;
  fragsExpected: number;
  recovered: number;
  decodeQueue: number;
  jitterMsX10: number;
  decMsX10: number;
}

export function encodeFrameReport(r: FrameReport): Uint8Array {
  const { out, v } = prefix(24, FB_FRAME_REPORT);
  v.setUint32(4, r.highestId >>> 0);
  v.setUint16(8, sat16(r.framesComplete));
  v.setUint16(10, sat16(r.framesLost));
  v.setUint16(12, sat16(r.fragsRecv));
  v.setUint16(14, sat16(r.fragsExpected));
  v.setUint16(16, sat16(r.recovered));
  v.setUint16(18, sat16(r.decodeQueue));
  v.setUint16(20, sat16(r.jitterMsX10));
  v.setUint16(22, sat16(r.decMsX10));
  return out;
}
