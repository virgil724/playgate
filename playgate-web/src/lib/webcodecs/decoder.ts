/**
 * decoder.ts — WebCodecs H.264 decoder wrapper (spec section 5.2).
 *
 * Annex-B without `description` first (in-band SPS/PPS); if the browser
 * rejects that before producing any output, falls back to avcC mode.
 * Config probe order: hardware+latency, latency only, plain.
 */

import {
  NAL_PPS,
  NAL_SPS,
  annexBToAvcc,
  buildAvcC,
  bytesEqual,
  codecStringFromSps,
  findNal,
  splitNals,
} from "./h264";
import type { AssembledFrame } from "./reassembler";
import { dlog } from "../log";

const PTS_WRAP = 2 ** 32;
const MAX_DURATION_US = 1_000_000;
const ERROR_WINDOW_MS = 10_000;
const ERROR_LIMIT = 3;
const PROBE_CODEC = "avc1.42e02a";

export interface ConfigVariant {
  name: "hardware+latency" | "latency" | "plain";
  hardwareAcceleration?: "prefer-hardware";
  optimizeForLatency?: boolean;
}

const VARIANTS: ConfigVariant[] = [
  { name: "hardware+latency", hardwareAcceleration: "prefer-hardware", optimizeForLatency: true },
  { name: "latency", optimizeForLatency: true },
  { name: "plain" },
];

export interface ProbeResult {
  supported: boolean;
  hardware: boolean;
  variant: ConfigVariant | null;
}

export function webCodecsAvailable(): boolean {
  return (
    typeof VideoDecoder !== "undefined" &&
    typeof EncodedVideoChunk !== "undefined" &&
    (typeof isSecureContext === "undefined" || isSecureContext)
  );
}

/** Finds the first supported config variant for a generic baseline stream. */
export async function probeDecoder(codec: string = PROBE_CODEC): Promise<ProbeResult> {
  if (!webCodecsAvailable()) return { supported: false, hardware: false, variant: null };
  for (const v of VARIANTS) {
    try {
      const r = await VideoDecoder.isConfigSupported(toConfig(codec, v));
      if (r.supported) return { supported: true, hardware: v.name === "hardware+latency", variant: v };
    } catch {
      /* try next */
    }
  }
  return { supported: false, hardware: false, variant: null };
}

function toConfig(codec: string, v: ConfigVariant, description?: Uint8Array): VideoDecoderConfig {
  const cfg: VideoDecoderConfig = { codec };
  if (v.hardwareAcceleration) cfg.hardwareAcceleration = v.hardwareAcceleration;
  if (v.optimizeForLatency) cfg.optimizeForLatency = true;
  if (description) cfg.description = description;
  return cfg;
}

export interface DecoderEvents {
  onFrame: (frame: VideoFrame) => void;
  /** Stream is damaged/stale: caller must send a KEYFRAME_REQUEST (reason code). */
  onNeedKey: (reason: "decoderError" | "queueBacklog", lostId: number) => void;
  /** Unrecoverable: use the RTP track instead. */
  onFatal: (reason: string) => void;
}

export interface DecoderStats {
  decoded: number;
  /** Frames not submitted (waiting for key, backpressure). */
  dropped: number;
  decodeMsEma: number;
  queue: number;
  hardware: boolean;
  mode: "annexb" | "avcc";
  codec: string;
  errors: number;
}

export class H264Decoder {
  private decoder: VideoDecoder | null = null;
  private variant: ConfigVariant | null;
  private mode: "annexb" | "avcc" = "annexb";
  private triedAvcc = false;
  private configured = false;
  private needsKey = true;
  private codec = "";
  private sps: Uint8Array | null = null;
  private pps: Uint8Array | null = null;
  private outputsSinceConfigure = 0;
  private errorTimes: number[] = [];
  private submitTimes = new Map<number, number>();
  private lastRawPts: number | null = null;
  private ptsWraps = 0;
  private dead = false;
  private lastSubmittedId = 0xffffffff;
  private stats: DecoderStats = {
    decoded: 0,
    dropped: 0,
    decodeMsEma: 0,
    queue: 0,
    hardware: false,
    mode: "annexb",
    codec: "",
    errors: 0,
  };
  /** Smoothed frame interval from pts deltas; feeds backpressure + timeouts. */
  frameIntervalMs = 1000 / 60;

  constructor(
    private readonly ev: DecoderEvents,
    variant: ConfigVariant | null,
  ) {
    this.variant = variant;
  }

  get queueSize(): number {
    return this.decoder?.decodeQueueSize ?? 0;
  }
  get lastGoodId(): number {
    return this.lastSubmittedId;
  }
  getStats(): DecoderStats {
    return { ...this.stats, queue: this.queueSize, mode: this.mode, codec: this.codec };
  }

  /** Epoch change or external reset: drop decoder state, wait for a KEY. */
  reset(): void {
    this.configured = false;
    this.needsKey = true;
    this.lastRawPts = null;
    this.ptsWraps = 0;
    this.submitTimes.clear();
    this.sps = this.pps = null;
    if (this.decoder && this.decoder.state !== "closed") {
      try {
        this.decoder.reset();
      } catch {
        /* recreated on next configure */
      }
    }
  }

  close(): void {
    this.dead = true;
    this.submitTimes.clear();
    if (this.decoder && this.decoder.state !== "closed") {
      try {
        this.decoder.close();
      } catch {
        /* ignore */
      }
    }
    this.decoder = null;
  }

  decode(frame: AssembledFrame): void {
    if (this.dead) return;
    if (this.needsKey && !frame.key) {
      this.stats.dropped++;
      return;
    }

    const nals = splitNals(frame.data);
    if (frame.key) {
      const sps = findNal(nals, NAL_SPS)?.data ?? null;
      const pps = findNal(nals, NAL_PPS)?.data ?? null;
      if (sps && pps) {
        const codec = codecStringFromSps(sps);
        if (!codec) {
          this.stats.dropped++;
          return;
        }
        const changed =
          !this.configured ||
          codec !== this.codec ||
          (this.mode === "avcc" && (!bytesEqual(sps, this.sps) || !bytesEqual(pps, this.pps)));
        if (changed && !this.configure(codec, sps, pps)) return;
      } else if (!this.configured) {
        dlog("webcodecs", "keyframe without in-band SPS/PPS and decoder not configured; dropping");
        this.stats.dropped++;
        this.ev.onNeedKey("decoderError", frame.frameId);
        return;
      }
      this.needsKey = false;
    }
    if (!this.decoder || !this.configured) {
      this.stats.dropped++;
      return;
    }

    // Backpressure (spec 5.2.8).
    const q = this.decoder.decodeQueueSize;
    if (q > 2 && q * this.frameIntervalMs > 200) {
      dlog("webcodecs", `decode backlog q=${q}: reset + keyframe`);
      this.stats.dropped++;
      this.softReset();
      this.ev.onNeedKey("queueBacklog", frame.frameId);
      return;
    }
    if (q > 4) {
      // A dropped delta corrupts everything after it, so also resync.
      this.stats.dropped++;
      this.needsKey = true;
      this.ev.onNeedKey("queueBacklog", frame.frameId);
      return;
    }

    // Timestamps: unwrap u32 pts to a monotonic number; clamp duration.
    const raw = frame.ptsUs >>> 0;
    let duration = 0;
    if (this.lastRawPts !== null) {
      if (raw - this.lastRawPts < -(PTS_WRAP / 2)) this.ptsWraps++;
      const d = (raw - this.lastRawPts + PTS_WRAP) % PTS_WRAP;
      if (d <= MAX_DURATION_US) {
        duration = d;
        if (d > 0) this.frameIntervalMs = this.frameIntervalMs * 0.9 + (d / 1000) * 0.1;
      }
    }
    this.lastRawPts = raw;
    const timestamp = this.ptsWraps * PTS_WRAP + raw;

    let data: Uint8Array = frame.data;
    if (this.mode === "avcc") data = annexBToAvcc(nals);
    try {
      this.submitTimes.set(timestamp, performance.now());
      this.decoder.decode(
        new EncodedVideoChunk({ type: frame.key ? "key" : "delta", timestamp, duration, data }),
      );
      this.lastSubmittedId = frame.frameId;
    } catch (err) {
      dlog("webcodecs", "decode() threw:", err);
      this.submitTimes.delete(timestamp);
      this.handleError(err);
    }
  }

  private softReset(): void {
    this.needsKey = true;
    this.submitTimes.clear();
    this.lastRawPts = null;
    if (this.decoder && this.decoder.state !== "closed") {
      try {
        this.decoder.reset();
      } catch {
        /* ignore */
      }
    }
    this.configured = false;
  }

  /** (Re)configure on a keyframe. Returns false if the frame cannot be decoded. */
  private configure(codec: string, sps: Uint8Array, pps: Uint8Array): boolean {
    if (!this.variant) {
      this.ev.onFatal("no supported decoder config");
      return false;
    }
    if (!this.decoder || this.decoder.state === "closed") {
      this.decoder = new VideoDecoder({
        output: (f) => this.onOutput(f),
        error: (e) => this.handleError(e),
      });
    }
    const description = this.mode === "avcc" ? buildAvcC(sps, pps) : undefined;
    try {
      if (this.decoder.state === "configured") this.decoder.reset();
      this.decoder.configure(toConfig(codec, this.variant, description));
    } catch (err) {
      dlog("webcodecs", "configure threw:", err);
      this.handleError(err);
      return false;
    }
    this.codec = codec;
    this.sps = sps.slice();
    this.pps = pps.slice();
    this.configured = true;
    this.outputsSinceConfigure = 0;
    this.stats.hardware = this.variant.name === "hardware+latency";
    this.submitTimes.clear();
    this.lastRawPts = null;
    dlog("webcodecs", `configured ${codec} mode=${this.mode} variant=${this.variant.name}`);
    return true;
  }

  private onOutput(frame: VideoFrame): void {
    if (this.dead) {
      frame.close();
      return;
    }
    this.outputsSinceConfigure++;
    this.stats.decoded++;
    const t0 = this.submitTimes.get(frame.timestamp);
    if (t0 !== undefined) {
      this.submitTimes.delete(frame.timestamp);
      const ms = performance.now() - t0;
      this.stats.decodeMsEma = this.stats.decodeMsEma === 0 ? ms : this.stats.decodeMsEma * 0.9 + ms * 0.1;
    }
    if (this.submitTimes.size > 64) this.submitTimes.clear();
    this.ev.onFrame(frame);
  }

  private handleError(err: unknown): void {
    if (this.dead) return;
    const now = performance.now();
    this.errorTimes = this.errorTimes.filter((t) => now - t < ERROR_WINDOW_MS);
    this.errorTimes.push(now);
    this.stats.errors++;
    dlog("webcodecs", "decoder error:", err);
    if (this.errorTimes.length >= ERROR_LIMIT) {
      this.ev.onFatal(`${ERROR_LIMIT} decoder errors in ${ERROR_WINDOW_MS / 1000}s`);
      return;
    }
    // Annex-B rejected before any output on a known-good stream: try avcC.
    if (this.mode === "annexb" && !this.triedAvcc && this.outputsSinceConfigure === 0) {
      this.triedAvcc = true;
      this.mode = "avcc";
      dlog("webcodecs", "annex-b rejected; switching to avcC mode");
    }
    // A failed decoder is closed: recreate on the next keyframe.
    if (this.decoder && this.decoder.state === "closed") this.decoder = null;
    this.configured = false;
    this.needsKey = true;
    this.submitTimes.clear();
    this.lastRawPts = null;
    this.ev.onNeedKey("decoderError", this.lastSubmittedId);
  }
}
