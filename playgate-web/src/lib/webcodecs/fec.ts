/**
 * fec.ts — application-level FEC for the WebCodecs transport
 * (playgate-host/docs/webcodecs-transport.md section 8). Mirror of
 * playgate-host/internal/rtc/videofec.go; both are checked against the same
 * golden vectors. Framework-free.
 *
 * Every shard has the same length (`stride`). Scheme 1 (XOR): m parity shards,
 * data shard i belongs to group i mod m, parity j = XOR of group j.
 * Scheme 2 (RS): systematic Reed-Solomon over GF(256) (poly 0x11d, generator 2)
 * with a Cauchy parity matrix C[j][i] = 1 / ((k + j) ^ i).
 */

export const FEC_NONE = 0;
export const FEC_XOR = 1;
export const FEC_RS = 2;
export const MAX_FEC_SHARDS = 255;

export const gfExp = new Uint8Array(512);
export const gfLog = new Int32Array(256);
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    gfExp[i] = x;
    gfLog[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
  for (let i = 255; i < 512; i++) gfExp[i] = gfExp[i - 255];
}

export function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0;
  return gfExp[gfLog[a] + gfLog[b]];
}

export function gfInv(a: number): number {
  return gfExp[255 - gfLog[a]];
}

/** RS coefficient of parity row j, data column i. */
export function cauchy(k: number, j: number, i: number): number {
  return gfInv((k + j) ^ i);
}

/** dst ^= c * src */
function mulAdd(dst: Uint8Array, src: Uint8Array, c: number): void {
  if (c === 0) return;
  if (c === 1) {
    for (let i = 0; i < src.length; i++) dst[i] ^= src[i];
    return;
  }
  const lc = gfLog[c];
  for (let i = 0; i < src.length; i++) {
    const v = src[i];
    if (v !== 0) dst[i] ^= gfExp[lc + gfLog[v]];
  }
}

function scale(b: Uint8Array, c: number): void {
  for (let i = 0; i < b.length; i++) b[i] = gfMul(b[i], c);
}

/** Computes m parity shards for k equal-length data shards. */
export function fecEncode(scheme: number, data: Uint8Array[], m: number): Uint8Array[] {
  const k = data.length;
  if (k === 0 || m <= 0) throw new Error("fec needs k>0 and m>0");
  if (k + m > MAX_FEC_SHARDS) throw new Error("fec k+m exceeds 255");
  const sz = data[0].length;
  const par = Array.from({ length: m }, () => new Uint8Array(sz));
  if (scheme === FEC_XOR) {
    if (m > k) throw new Error("xor fec needs m <= k");
    for (let i = 0; i < k; i++) mulAdd(par[i % m], data[i], 1);
  } else if (scheme === FEC_RS) {
    for (let j = 0; j < m; j++) for (let i = 0; i < k; i++) mulAdd(par[j], data[i], cauchy(k, j, i));
  } else {
    throw new Error(`unknown fec scheme ${scheme}`);
  }
  return par;
}

/**
 * Fills the null entries among shards[0..k) in place. `shards` has k+m
 * entries (null = missing; all present ones have the same length). Returns the
 * number of data shards recovered, or -1 if recovery is impossible (shards are
 * left untouched).
 */
export function fecRecover(scheme: number, k: number, m: number, shards: (Uint8Array | null)[]): number {
  if (shards.length !== k + m) return -1;
  const missing: number[] = [];
  for (let i = 0; i < k; i++) if (shards[i] === null) missing.push(i);
  if (missing.length === 0) return 0;
  let sz = 0;
  for (const s of shards) {
    if (s) {
      sz = s.length;
      break;
    }
  }
  if (sz === 0) return -1;

  if (scheme === FEC_XOR) {
    if (m < 1 || m > k) return -1;
    const seen = new Set<number>();
    for (const i of missing) {
      const g = i % m;
      if (seen.has(g) || shards[k + g] === null) return -1;
      seen.add(g);
    }
    for (const i of missing) {
      const g = i % m;
      const out = new Uint8Array(shards[k + g] as Uint8Array);
      for (let d = g; d < k; d += m) if (d !== i) mulAdd(out, shards[d] as Uint8Array, 1);
      shards[i] = out;
    }
    return missing.length;
  }

  if (scheme === FEC_RS) {
    const e = missing.length;
    const rows: number[] = [];
    for (let j = 0; j < m && rows.length < e; j++) if (shards[k + j] !== null) rows.push(j);
    if (rows.length < e) return -1;
    const rhs: Uint8Array[] = rows.map((j) => {
      const v = new Uint8Array(shards[k + j] as Uint8Array);
      for (let i = 0; i < k; i++) {
        const d = shards[i];
        if (d) mulAdd(v, d, cauchy(k, j, i));
      }
      return v;
    });
    const a: number[][] = rows.map((j) => missing.map((i) => cauchy(k, j, i)));
    for (let col = 0; col < e; col++) {
      let p = -1;
      for (let r = col; r < e; r++) {
        if (a[r][col] !== 0) {
          p = r;
          break;
        }
      }
      if (p < 0) return -1;
      [a[col], a[p]] = [a[p], a[col]];
      [rhs[col], rhs[p]] = [rhs[p], rhs[col]];
      const inv = gfInv(a[col][col]);
      for (let c = 0; c < e; c++) a[col][c] = gfMul(a[col][c], inv);
      scale(rhs[col], inv);
      for (let r = 0; r < e; r++) {
        if (r === col || a[r][col] === 0) continue;
        const f = a[r][col];
        for (let c = 0; c < e; c++) a[r][c] ^= gfMul(f, a[col][c]);
        mulAdd(rhs[r], rhs[col], f);
      }
    }
    for (let c = 0; c < e; c++) shards[missing[c]] = rhs[c];
    return e;
  }
  return -1;
}
