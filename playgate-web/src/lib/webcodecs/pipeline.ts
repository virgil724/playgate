/**
 * pipeline.ts — glue for the WebCodecs transport (spec section 5):
 *
 *   video DataChannel message -> parseFragment -> Reassembler -> H264Decoder
 *      -> CanvasRenderer, with KEYFRAME_REQUEST / FRAME_REPORT / HELLO on videofb.
 *
 * Framework-free. Transport selection and fallback live in transport.ts.
 */

import { dlog } from "../log";
import { H264Decoder, probeDecoder } from "./decoder";
import {
  CAP_ANNEXB_NO_DESC,
  CAP_HARDWARE,
  CAP_WEBCODECS_H264,
  KEYREASON,
  NO_FRAME_ID,
  encodeFrameReport,
  encodeHello,
  CAP_FEC_RS,
  CAP_FRAG_NACK,
  encodeFragNack,
  CAP_FEC_XOR,
  MAX_FEC_ADVERTISED,
  encodeKeyframeRequest,
  parseFragment,
} from "./protocol";
import { Reassembler, type AssembledFrame } from "./reassembler";
import { CanvasRenderer } from "./renderer";

export const DEFAULT_REPORT_MS = 250;
export const KEYFRAME_MIN_INTERVAL_MS = 250;
export const NO_KEYFRAME_TIMEOUT_MS = 3000;
const DEFAULT_RTT_MS = 250;

/** How long a KEY frame may take to arrive: at least 300ms, or 3 RTTs. */
export function keyFrameTimeoutMs(rttMs: number): number {
  return Math.max(300, 3 * rttMs);
}

/** In-flight frame window: enough slots for every frame that arrives while a
 * KEY is still completing, plus slack; never below the old fixed 8. */
/** Wait before a FRAG_NACK: about two arrival-jitters, 8..30 ms. */
export function nackDelayMs(jitterMs: number): number {
  return Math.max(8, Math.min(30, 2 * jitterMs));
}

/** Delta frame timeout with retransmission: one NACK round trip plus slack. */
export function deltaTimeoutMs(frameIntervalMs: number, nackDelay: number, rttMs: number): number {
  return Math.max(2 * frameIntervalMs, nackDelay + 1.5 * rttMs + 15);
}

export function maxInflightFor(keyTimeoutMs: number, frameIntervalMs: number): number {
  return Math.max(8, Math.ceil(keyTimeoutMs / frameIntervalMs) + 4);
}

/** Rate-limits KEYFRAME_REQUESTs and re-sends while no IDR has arrived. */
export class KeyframeRequester {
  private awaiting = false;
  private pending = false;
  private lastSentMs = -Infinity;
  private reason: number = KEYREASON.unknown;
  private lostId = NO_FRAME_ID;
  sent = 0;

  constructor(
    private readonly send: (reason: number, lostId: number) => void,
    private readonly getRttMs: () => number,
    private readonly minIntervalMs = KEYFRAME_MIN_INTERVAL_MS,
    /** Never re-request sooner than this: an IDR may still be arriving (see
     * the reassembler's key timeout); asking again only adds another IDR to
     * a link that is already busy delivering one. */
    private readonly retryFloorMs: () => number = () => 250,
  ) {}

  get isAwaiting(): boolean {
    return this.awaiting;
  }

  /** Call when HELLO was sent: the host answers with an unsolicited IDR. */
  expectKey(nowMs: number): void {
    this.awaiting = true;
    this.pending = false;
    this.lastSentMs = nowMs;
    this.reason = KEYREASON.initialJoin;
  }

  request(reason: number, lostId: number, nowMs: number): void {
    this.awaiting = true;
    this.reason = reason;
    this.lostId = lostId;
    if (nowMs - this.lastSentMs >= this.minIntervalMs) this.fire(nowMs);
    else this.pending = true; // coalesced: fires at window end (tick)
  }

  keyArrived(): void {
    this.awaiting = false;
    this.pending = false;
  }

  tick(nowMs: number): void {
    if (!this.awaiting) return;
    const sinceLast = nowMs - this.lastSentMs;
    const retryAfter = Math.max(this.retryFloorMs(), 3 * this.getRttMs());
    if ((this.pending && sinceLast >= this.minIntervalMs) || sinceLast >= retryAfter) this.fire(nowMs);
  }

  private fire(nowMs: number): void {
    this.pending = false;
    this.lastSentMs = nowMs;
    this.sent++;
    this.send(this.reason, this.lostId);
  }
}

export interface PipelineOptions {
  canvas: HTMLCanvasElement;
  /** Writes a message on the reliable `videofb` channel. */
  sendFeedback: (msg: Uint8Array) => void;
  /** Current RTT estimate (spec 5.5); <= 0 or NaN means unknown. */
  getRttMs?: () => number;
  /** Unrecoverable: switch to the RTP track. */
  onFallback: (reason: string) => void;
  onFirstKey?: () => void;
  reportMs?: number;
  now?: () => number;
}

export interface PipelineStats {
  fps: number;
  framesComplete: number;
  framesLost: number;
  framesDropped: number;
  keyRequests: number;
  decodeMs: number;
  jitterMs: number;
  queue: number;
  hardware: boolean;
  mode: string;
  codec: string;
  inflight: number;
  decoderErrors: number;
}

export class WebCodecsPipeline {
  private readonly o: PipelineOptions;
  private readonly now: () => number;
  private reassembler: Reassembler;
  private decoder: H264Decoder | null = null;
  private renderer: CanvasRenderer;
  private requester: KeyframeRequester;
  private reportTimer: ReturnType<typeof setInterval> | null = null;
  private houseTimer: ReturnType<typeof setInterval> | null = null;
  private pollTimer: ReturnType<typeof setTimeout> | null = null;
  private keyTimer: ReturnType<typeof setTimeout> | null = null;
  private stopped = false;
  private gotKey = false;
  private totalComplete = 0;
  private totalLost = 0;
  private nacksSent = 0;
  private jitterMs = 0;
  private lastSendMs: number | null = null;
  private lastRecvMs = 0;
  private fpsSample = { t: 0, drawn: 0, fps: 0 };

  constructor(opts: PipelineOptions) {
    this.o = opts;
    this.now = opts.now ?? (() => performance.now());
    this.renderer = new CanvasRenderer(opts.canvas);
    const rtt = () => {
      const r = opts.getRttMs?.() ?? NaN;
      return r > 0 && Number.isFinite(r) ? r : DEFAULT_RTT_MS;
    };
    // A KEY may take many RTTs to drain through SCTP; give it room, and size
    // the in-flight window so the deltas arriving meanwhile are not evicted
    // (evicting the KEY itself, as the oldest slot, restarted the wait).
    const keyTimeout = () => keyFrameTimeoutMs(rtt());
    this.requester = new KeyframeRequester(
      (reason, lostId) => this.sendKeyRequest(reason, lostId),
      rtt,
      KEYFRAME_MIN_INTERVAL_MS,
      keyTimeout,
    );
    // FRAG_NACK: ask again for fragments that have not arrived after a short
    // wait, and give a delta frame long enough for one round of that.
    const nackDelay = () => nackDelayMs(this.jitterMs);
    this.reassembler = new Reassembler({
      maxInflight: () => maxInflightFor(keyTimeout(), this.frameIntervalMs()),
      reorderWaitMs: () => Math.min(5, this.frameIntervalMs()),
      frameTimeoutMs: () => deltaTimeoutMs(this.frameIntervalMs(), nackDelay(), rtt()),
      keyFrameTimeoutMs: keyTimeout,
      nackDelayMs: nackDelay,
      nackRetryMs: () => rtt() + 10,
      onNack: (epoch, frameId, indices) => {
        this.nacksSent++;
        this.o.sendFeedback(encodeFragNack(epoch, frameId, indices));
      },
      onFrame: (f) => this.handleFrame(f),
      onLoss: (l) => {
        dlog("webcodecs", `frame ${l.lostId} lost (epoch ${l.epoch}); requesting keyframe`);
        this.requester.request(KEYREASON.frameLost, l.lostId, this.now());
      },
      onEpoch: (epoch) => {
        dlog("webcodecs", `epoch -> ${epoch}`);
        this.decoder?.reset();
        this.requester.request(KEYREASON.waitingAfterEpoch, NO_FRAME_ID, this.now());
      },
    });
  }

  private frameIntervalMs(): number {
    return this.decoder?.frameIntervalMs ?? 1000 / 60;
  }

  /** Probes the decoder, then sends HELLO. Calls onFallback if unusable. */
  async start(): Promise<void> {
    const probe = await probeDecoder();
    if (this.stopped) return;
    if (!probe.supported) {
      this.o.onFallback("VideoDecoder does not support H.264 configs");
      return;
    }
    this.decoder = new H264Decoder(
      {
        onFrame: (f) => this.renderer.submit(f),
        onNeedKey: (reason, lostId) => {
          this.reassembler.requireKey();
          this.requester.request(
            reason === "queueBacklog" ? KEYREASON.queueBacklog : KEYREASON.decoderError,
            lostId,
            this.now(),
          );
        },
        onFatal: (reason) => this.fallback(reason),
      },
      probe.variant,
    );
    this.renderer.start();

    const reportMs = this.o.reportMs ?? DEFAULT_REPORT_MS;
    const caps =
      CAP_WEBCODECS_H264 | CAP_ANNEXB_NO_DESC | CAP_FEC_XOR | CAP_FEC_RS | CAP_FRAG_NACK | (probe.hardware ? CAP_HARDWARE : 0);
    this.o.sendFeedback(encodeHello(caps, MAX_FEC_ADVERTISED, reportMs === DEFAULT_REPORT_MS ? 0 : reportMs));
    this.requester.expectKey(this.now());
    this.keyTimer = setTimeout(() => {
      if (!this.gotKey) this.fallback(`no complete keyframe within ${NO_KEYFRAME_TIMEOUT_MS}ms of HELLO`);
    }, NO_KEYFRAME_TIMEOUT_MS);
    this.reportTimer = setInterval(() => this.sendReport(), reportMs);
    this.houseTimer = setInterval(() => this.requester.tick(this.now()), 50);
    dlog("webcodecs", `pipeline started hw=${probe.hardware} variant=${probe.variant?.name}`);
  }

  /** Feed one `video` DataChannel message. */
  onMessage(data: ArrayBuffer): void {
    if (this.stopped || !this.decoder) return;
    const frag = parseFragment(data);
    if (!frag) return;
    this.reassembler.push(frag, this.now());
    this.schedulePoll();
  }

  stop(): void {
    this.stopped = true;
    for (const t of [this.reportTimer, this.houseTimer]) if (t) clearInterval(t);
    for (const t of [this.pollTimer, this.keyTimer]) if (t) clearTimeout(t);
    this.reportTimer = this.houseTimer = this.pollTimer = this.keyTimer = null;
    this.renderer.stop();
    this.decoder?.close();
    this.decoder = null;
    this.reassembler.reset();
  }

  getStats(): PipelineStats {
    const now = this.now();
    const s = this.fpsSample;
    if (s.t === 0) {
      s.t = now;
      s.drawn = this.renderer.drawn;
    } else if (now - s.t >= 900) {
      s.fps = ((this.renderer.drawn - s.drawn) * 1000) / (now - s.t);
      s.t = now;
      s.drawn = this.renderer.drawn;
    }
    const d = this.decoder?.getStats();
    return {
      fps: s.fps,
      framesComplete: this.totalComplete,
      framesLost: this.totalLost,
      framesDropped: d?.dropped ?? 0,
      keyRequests: this.requester.sent,
      decodeMs: d?.decodeMsEma ?? 0,
      jitterMs: this.jitterMs,
      queue: d?.queue ?? 0,
      hardware: d?.hardware ?? false,
      mode: d?.mode ?? "-",
      codec: d?.codec ?? "-",
      inflight: this.reassembler.inflight,
      decoderErrors: d?.errors ?? 0,
    };
  }

  // ---- internals ----

  private fallback(reason: string): void {
    if (this.stopped) return;
    dlog("webcodecs", `fallback to RTP track: ${reason}`);
    this.o.onFallback(reason);
  }

  private handleFrame(f: AssembledFrame): void {
    this.totalComplete++;
    // Inter-frame arrival jitter (RFC 3550 style) from send_ms deltas vs local deltas.
    const recv = f.completeMs;
    if (this.lastSendMs !== null) {
      const dSend = (f.sendMs - this.lastSendMs) | 0;
      const d = Math.abs(recv - this.lastRecvMs - dSend);
      this.jitterMs += (d - this.jitterMs) / 16;
    }
    this.lastSendMs = f.sendMs;
    this.lastRecvMs = recv;

    if (f.key) {
      this.requester.keyArrived();
      if (!this.gotKey) {
        this.gotKey = true;
        if (this.keyTimer) clearTimeout(this.keyTimer);
        this.keyTimer = null;
        this.o.onFirstKey?.();
      }
    }
    this.decoder?.decode(f);
  }

  private schedulePoll(): void {
    if (this.pollTimer) clearTimeout(this.pollTimer);
    this.pollTimer = null;
    const deadline = this.reassembler.nextDeadline();
    if (deadline === null) return;
    this.pollTimer = setTimeout(
      () => {
        this.pollTimer = null;
        if (this.stopped) return;
        this.reassembler.poll(this.now());
        this.schedulePoll();
      },
      Math.max(0, deadline - this.now()),
    );
  }

  private sendKeyRequest(reason: number, lostId: number): void {
    const last = this.decoder?.lastGoodId ?? NO_FRAME_ID;
    this.o.sendFeedback(encodeKeyframeRequest(this.reassembler.currentEpoch ?? 0, reason, last, lostId));
  }

  private lastDiagMs = 0;

  /** Debug log of cumulative loss diagnostics, at most every 5 s. */
  private logDiag(): void {
    const now = this.now();
    if (now - this.lastDiagMs < 5000) return;
    this.lastDiagMs = now;
    const d = this.reassembler.diag;
    const fragLoss = d.fragsExpected ? (1 - d.fragsRecv / d.fragsExpected) * 100 : 0;
    dlog(
      "webcodecs",
      `diag frags ${d.fragsRecv}/${d.fragsExpected} (loss ${fragLoss.toFixed(1)}%) recovered ${d.recovered} ` +
        `lost missing=${d.lostMissing} partial=${d.lostPartial} evicted=${d.lostEvicted} ` +
        `partial: missingData=${d.partialMissingData} parity ${d.partialParityHave}/${d.partialParityTotal} ` +
        `nacks ${this.nacksSent}`,
    );
  }

  private sendReport(): void {
    this.logDiag();
    const c = this.reassembler.takeCounters();
    this.totalLost += c.framesLost;
    const d = this.decoder?.getStats();
    this.o.sendFeedback(
      encodeFrameReport({
        highestId: c.highestId,
        framesComplete: c.framesComplete,
        framesLost: c.framesLost,
        fragsRecv: c.fragsRecv,
        fragsExpected: c.fragsExpected,
        recovered: c.recovered,
        decodeQueue: d?.queue ?? 0,
        jitterMsX10: this.jitterMs * 10,
        decMsX10: (d?.decodeMsEma ?? 0) * 10,
      }),
    );
  }
}
