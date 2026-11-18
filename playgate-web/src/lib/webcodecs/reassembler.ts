/**
 * reassembler.ts — fragment reassembly keyed by (epoch, frame_id)
 * (spec section 5.1). Pure logic: time is passed in, no timers or DOM.
 *
 * Policy:
 *  - at most `maxInflight` partial frames; the oldest is evicted (lost)
 *  - frames are emitted strictly in increasing frame_id
 *  - a later complete frame waits `reorderWaitMs` for the missing one, then the
 *    missing one is declared lost; an incomplete frame is lost after `frameTimeoutMs`
 *  - after a loss every delta is dropped until a KEY frame completes
 *  - no FEC (parity fragments are counted but ignored) and no concealment
 */

import { FLAG_KEY, FLAG_PARITY, type Fragment, serialDiff16, serialDiff32 } from "./protocol";

export interface AssembledFrame {
  epoch: number;
  frameId: number;
  key: boolean;
  ptsUs: number;
  sendMs: number;
  data: Uint8Array;
  firstArrivalMs: number;
  completeMs: number;
}

export interface LossInfo {
  epoch: number;
  lostId: number;
  /** Last frame_id emitted to the consumer, or null if none in this epoch. */
  lastGoodId: number | null;
}

export interface ReassemblerOptions {
  maxInflight?: number;
  reorderWaitMs?: () => number;
  frameTimeoutMs?: () => number;
  onFrame: (f: AssembledFrame) => void;
  /** A loss that requires a keyframe (a KEYFRAME_REQUEST should follow). */
  onLoss: (l: LossInfo) => void;
  /** New epoch seen: consumer must reset its decoder and wait for a KEY. */
  onEpoch: (epoch: number) => void;
}

export interface ReassemblerCounters {
  fragsRecv: number;
  fragsExpected: number;
  framesComplete: number;
  framesLost: number;
  highestId: number;
}

/** Sanity cap so a corrupt header cannot make us allocate gigabytes. */
const MAX_FRAME_BYTES = 32 * 1024 * 1024;
const SEEN_MEMORY = 64;

interface Slot {
  epoch: number;
  frameId: number;
  key: boolean;
  k: number;
  n: number;
  frameSize: number;
  /** 0 until known (see place()). */
  stride: number;
  buf: Uint8Array;
  have: Uint8Array;
  count: number;
  ptsUs: number;
  sendMs: number;
  firstMs: number;
  completeMs: number | null;
}

export class Reassembler {
  private slots = new Map<number, Slot>();
  private epoch: number | null = null;
  /** Last frame_id emitted or abandoned-through; null = nothing yet in this epoch. */
  private lastId: number | null = null;
  private needKey = true;
  private seen: number[] = [];
  private counters: ReassemblerCounters = {
    fragsRecv: 0,
    fragsExpected: 0,
    framesComplete: 0,
    framesLost: 0,
    highestId: 0,
  };
  private haveHighest = false;
  private readonly maxInflight: number;
  private readonly reorderWaitMs: () => number;
  private readonly frameTimeoutMs: () => number;

  constructor(private readonly o: ReassemblerOptions) {
    this.maxInflight = o.maxInflight ?? 8;
    this.reorderWaitMs = o.reorderWaitMs ?? (() => 5);
    this.frameTimeoutMs = o.frameTimeoutMs ?? (() => 20);
  }

  get waitingForKey(): boolean {
    return this.needKey;
  }
  get currentEpoch(): number | null {
    return this.epoch;
  }
  get inflight(): number {
    return this.slots.size;
  }

  /** Counters since the last call (highestId is absolute). */
  takeCounters(): ReassemblerCounters {
    const c = this.counters;
    this.counters = {
      fragsRecv: 0,
      fragsExpected: 0,
      framesComplete: 0,
      framesLost: 0,
      highestId: c.highestId,
    };
    return c;
  }

  reset(): void {
    this.slots.clear();
    this.lastId = null;
    this.needKey = true;
  }

  /** Forces "drop deltas until KEY" (decoder error, backlog reset). */
  requireKey(): void {
    this.needKey = true;
    for (const [id, s] of this.slots) if (!s.key) this.slots.delete(id);
  }

  push(frag: Fragment, nowMs: number): void {
    const h = frag.header;
    if (h.codec !== 0) return;
    if (h.frameSize > MAX_FRAME_BYTES) return;

    if (this.epoch === null || serialDiff16(h.epoch, this.epoch) > 0) {
      const first = this.epoch === null;
      this.epoch = h.epoch;
      this.slots.clear();
      this.lastId = null;
      this.needKey = true;
      this.seen = [];
      if (!first) this.o.onEpoch(h.epoch);
    } else if (serialDiff16(h.epoch, this.epoch) < 0) {
      return; // stale epoch
    }

    const n = h.dataCount + h.parityCount;
    this.counters.fragsRecv++;
    if (!this.haveHighest || serialDiff32(h.frameId, this.counters.highestId) > 0) {
      this.counters.highestId = h.frameId;
      this.haveHighest = true;
    }
    if (!this.seen.includes(h.frameId)) {
      this.seen.push(h.frameId);
      if (this.seen.length > SEEN_MEMORY) this.seen.shift();
      this.counters.fragsExpected += n;
    }

    // Parity is not implemented (no FEC): receivers treat it as absent.
    if ((h.flags & FLAG_PARITY) !== 0 || h.fragIdx >= h.dataCount) return;
    if (this.lastId !== null && serialDiff32(h.frameId, this.lastId) <= 0) return; // late
    const key = (h.flags & FLAG_KEY) !== 0;
    if (this.needKey && !key && !this.keySlotBefore(h.frameId)) return;

    let slot = this.slots.get(h.frameId);
    if (!slot) {
      if (h.dataCount > h.frameSize) return;
      slot = {
        epoch: h.epoch,
        frameId: h.frameId,
        key,
        k: h.dataCount,
        n,
        frameSize: h.frameSize,
        stride: h.dataCount === 1 ? h.frameSize : 0,
        buf: new Uint8Array(h.frameSize),
        have: new Uint8Array(h.dataCount),
        count: 0,
        ptsUs: h.ptsUs,
        sendMs: h.sendMs,
        firstMs: nowMs,
        completeMs: null,
      };
      this.slots.set(h.frameId, slot);
      this.evictIfNeeded();
      if (!this.slots.has(h.frameId)) return;
    } else if (slot.k !== h.dataCount || slot.frameSize !== h.frameSize || slot.key !== key) {
      return;
    }

    if (!this.place(slot, h.fragIdx, frag.payload)) return;
    if (slot.count === slot.k && slot.completeMs === null) slot.completeMs = nowMs;
    this.process(nowMs);
  }

  /** Time-driven part: reorder waits and frame timeouts. */
  poll(nowMs: number): void {
    this.process(nowMs);
  }

  /** Absolute time (same clock as push/poll) at which poll() should next run. */
  nextDeadline(): number | null {
    let best: number | null = null;
    const next = this.nextId();
    for (const s of this.slots.values()) {
      let t: number;
      if (s.completeMs === null) t = s.firstMs + this.frameTimeoutMs();
      else if (this.needKey) {
        if (s.key) continue;
        t = s.completeMs + this.frameTimeoutMs();
      } else if (next !== null && s.frameId === next) continue;
      else t = s.completeMs + this.reorderWaitMs();
      if (best === null || t < best) best = t;
    }
    return best;
  }

  // ---- internals ----

  /** True if a KEY slot with a smaller frame_id is in flight (deltas that
   * overtake it must be kept; they become deliverable once it completes). */
  private keySlotBefore(id: number): boolean {
    for (const s of this.slots.values()) {
      if (s.key && serialDiff32(s.frameId, id) < 0) return true;
    }
    return false;
  }

  private nextId(): number | null {
    return this.lastId === null ? null : (this.lastId + 1) >>> 0;
  }

  /** Copies a data fragment into the slot. Returns false if invalid/duplicate. */
  private place(s: Slot, idx: number, payload: Uint8Array): boolean {
    if (idx >= s.k || s.have[idx] || payload.length === 0) return false;
    const last = idx === s.k - 1;
    let offset: number;
    if (s.stride === 0 && !last) {
      // First non-last fragment fixes the stride (fragment 0 length by spec).
      const stride = payload.length;
      if ((s.k - 1) * stride >= s.frameSize || s.k * stride < s.frameSize) return false;
      s.stride = stride;
    }
    if (last) {
      offset = s.frameSize - payload.length;
      if (s.stride !== 0 && offset !== (s.k - 1) * s.stride) return false;
      if (offset < 0 || payload.length > s.frameSize) return false;
    } else {
      if (payload.length !== s.stride) return false;
      offset = idx * s.stride;
    }
    if (offset + payload.length > s.frameSize) return false;
    s.buf.set(payload, offset);
    s.have[idx] = 1;
    s.count++;
    return true;
  }

  private evictIfNeeded(): void {
    while (this.slots.size > this.maxInflight) {
      let oldest: Slot | null = null;
      for (const s of this.slots.values()) {
        if (!oldest || serialDiff32(s.frameId, oldest.frameId) < 0) oldest = s;
      }
      if (!oldest) return;
      this.slots.delete(oldest.frameId);
      this.lose(oldest.frameId);
    }
  }

  private sortedSlots(): Slot[] {
    return [...this.slots.values()].sort((a, b) => serialDiff32(a.frameId, b.frameId));
  }

  /** Declares `id` lost: drop deltas, wait for KEY, notify. */
  private lose(id: number): void {
    this.counters.framesLost++;
    this.needKey = true;
    for (const [sid, s] of this.slots) if (!s.key) this.slots.delete(sid);
    this.o.onLoss({ epoch: this.epoch ?? 0, lostId: id, lastGoodId: this.lastId });
  }

  private emit(s: Slot): void {
    this.slots.delete(s.frameId);
    this.lastId = s.frameId;
    this.counters.framesComplete++;
    this.o.onFrame({
      epoch: s.epoch,
      frameId: s.frameId,
      key: s.key,
      ptsUs: s.ptsUs,
      sendMs: s.sendMs,
      data: s.buf,
      firstArrivalMs: s.firstMs,
      completeMs: s.completeMs ?? s.firstMs,
    });
  }

  private process(now: number): void {
    for (let guard = 0; guard < 1024; guard++) {
      const sorted = this.sortedSlots();

      // 1. A complete KEY frame supersedes everything older (and is the only
      //    way out of needKey).
      let key: Slot | null = null;
      for (const s of sorted) {
        if (s.key && s.completeMs !== null) key = s; // newest complete key wins
      }
      const next = this.nextId();
      if (key && (this.needKey || next === null || key.frameId !== next)) {
        for (const s of sorted) {
          if (serialDiff32(s.frameId, key.frameId) < 0) {
            this.slots.delete(s.frameId);
            if (s.completeMs === null) this.counters.framesLost++;
          }
        }
        this.needKey = false;
        this.emit(key);
        continue;
      }
      if (this.needKey || next === null) break;

      // 2. In-order delivery.
      const ns = this.slots.get(next);
      if (ns && ns.completeMs !== null) {
        this.emit(ns);
        continue;
      }

      // 3. `next` is missing or incomplete: has a later frame been waiting
      //    longer than the reorder window, or has `next` itself timed out?
      let lose = false;
      if (ns && now - ns.firstMs >= this.frameTimeoutMs()) lose = true;
      if (!lose) {
        for (const s of sorted) {
          if (s.completeMs !== null && s.frameId !== next && now - s.completeMs >= this.reorderWaitMs()) {
            lose = true;
            break;
          }
        }
      }
      if (lose) {
        this.lose(next);
        continue;
      }
      break;
    }

    // 4. Any other incomplete frame past its timeout is lost.
    const timeout = this.frameTimeoutMs();
    for (const s of this.sortedSlots()) {
      if (this.needKey && !s.key) {
        // Deltas parked behind an in-flight KEY: silently expire.
        if (now - (s.completeMs ?? s.firstMs) >= timeout) this.slots.delete(s.frameId);
      } else if (s.completeMs === null && now - s.firstMs >= timeout) {
        this.slots.delete(s.frameId);
        this.lose(s.frameId);
      }
    }
  }
}
