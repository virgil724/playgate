/**
 * h264.ts — minimal Annex-B helpers: NAL scanning, SPS codec string, avcC
 * description and Annex-B -> length-prefixed rewrite (spec section 5.2).
 */

export interface Nal {
  /** NAL unit type (header byte & 0x1f). */
  type: number;
  /** NAL bytes including the header byte, excluding the start code. */
  data: Uint8Array;
}

/** Splits an Annex-B buffer on 00 00 01 / 00 00 00 01 start codes. */
export function splitNals(buf: Uint8Array): Nal[] {
  const starts: number[] = []; // index of first byte AFTER a start code
  const codeLens: number[] = [];
  const n = buf.length;
  for (let i = 0; i + 2 < n; ) {
    if (buf[i + 2] > 1) {
      i += 3;
    } else if (buf[i] === 0 && buf[i + 1] === 0 && buf[i + 2] === 1) {
      starts.push(i + 3);
      codeLens.push(i > 0 && buf[i - 1] === 0 ? 4 : 3);
      i += 3;
    } else {
      i++;
    }
  }
  const nals: Nal[] = [];
  for (let s = 0; s < starts.length; s++) {
    const begin = starts[s];
    let end = s + 1 < starts.length ? starts[s + 1] - codeLens[s + 1] : n;
    // trailing_zero_8bits belong to no NAL
    while (end > begin && buf[end - 1] === 0) end--;
    if (end <= begin) continue;
    nals.push({ type: buf[begin] & 0x1f, data: buf.subarray(begin, end) });
  }
  return nals;
}

export const NAL_SLICE_IDR = 5;
export const NAL_SEI = 6;
export const NAL_SPS = 7;
export const NAL_PPS = 8;
export const NAL_AUD = 9;
export const NAL_FILLER = 12;

const hex2 = (b: number) => b.toString(16).padStart(2, "0");

/** `avc1.PPCCLL` from SPS bytes 1..3 (profile_idc, constraint flags, level_idc). */
export function codecStringFromSps(sps: Uint8Array): string | null {
  if (sps.length < 4) return null;
  return "avc1." + hex2(sps[1]) + hex2(sps[2]) + hex2(sps[3]);
}

export function findNal(nals: Nal[], type: number): Nal | undefined {
  return nals.find((n) => n.type === type);
}

/** avcC box payload: version 1, 4-byte lengths, one SPS and one PPS. */
export function buildAvcC(sps: Uint8Array, pps: Uint8Array): Uint8Array {
  const out = new Uint8Array(8 + sps.length + 3 + pps.length);
  const v = new DataView(out.buffer);
  out[0] = 1;
  out[1] = sps[1];
  out[2] = sps[2];
  out[3] = sps[3];
  out[4] = 0xff; // reserved 111111 + lengthSizeMinusOne = 3
  out[5] = 0xe1; // reserved 111 + numSPS = 1
  v.setUint16(6, sps.length);
  out.set(sps, 8);
  let o = 8 + sps.length;
  out[o++] = 1; // numPPS
  v.setUint16(o, pps.length);
  o += 2;
  out.set(pps, o);
  return out;
}

const DROP_IN_AVCC = new Set([NAL_SPS, NAL_PPS, NAL_AUD, NAL_SEI, NAL_FILLER]);

/** Rewrites NALs to 4-byte length prefixes, dropping SPS/PPS/AUD/SEI/filler. */
export function annexBToAvcc(nals: Nal[]): Uint8Array {
  const keep = nals.filter((n) => !DROP_IN_AVCC.has(n.type));
  let total = 0;
  for (const n of keep) total += 4 + n.data.length;
  const out = new Uint8Array(total);
  const v = new DataView(out.buffer);
  let o = 0;
  for (const n of keep) {
    v.setUint32(o, n.data.length);
    o += 4;
    out.set(n.data, o);
    o += n.data.length;
  }
  return out;
}

export function bytesEqual(a: Uint8Array | null, b: Uint8Array | null): boolean {
  if (!a || !b || a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}
