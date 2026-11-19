/**
 * transport.ts — per-viewer video transport selection and fallback
 * (spec sections 6 and 7).
 *
 * Wanted transport: `webcodecs` unless `?transport=track`, WebCodecs is
 * unavailable, the browser is WebKit (Safari is not supported; only
 * `?transport=webcodecs` forces it, for testing), or we already fell back
 * this session. Firefox is allowed but not officially supported. The choice is announced
 * to the host on the control channel; any failure sends `use:"track"` and
 * hands rendering back to the always-wired RTP <video>.
 */

import { dlog } from "../log";
import { webCodecsAvailable } from "./decoder";
import { WebCodecsPipeline, type PipelineStats } from "./pipeline";

export type TransportMode = "pending" | "webcodecs" | "track";
export type TransportOverride = "track" | "webcodecs" | null;

export const VIDEO_LABEL_TIMEOUT_MS = 3000;
export const VIDEO_OPEN_TIMEOUT_MS = 5000;

/** Normalises the `?transport=` URL value (testing override). */
export function parseTransportOverride(v: string | null): TransportOverride {
  return v === "track" || v === "webcodecs" ? v : null;
}

/**
 * Reports whether the user agent is a WebKit engine (desktop Safari, or any
 * browser on iOS/iPadOS, which all run WebKit). WebCodecs mode is not
 * supported there; those viewers stay on the RTP track.
 */
export function isWebKit(ua: string = typeof navigator !== "undefined" ? navigator.userAgent : ""): boolean {
  if (!/AppleWebKit/.test(ua)) return false;
  // Chromium (desktop, Android, Edge, Opera) also says AppleWebKit; iOS
  // Chrome (CriOS) and Firefox (FxiOS) do not say "Chrome", so they count as WebKit.
  return !/Chrome|Chromium|Edg\/|OPR\/|Android/.test(ua);
}

/** Reports whether the user agent is Firefox (allowed, not officially supported). */
export function isFirefox(ua: string = typeof navigator !== "undefined" ? navigator.userAgent : ""): boolean {
  return /Firefox\//.test(ua) && !/Seamonkey\//.test(ua);
}

export interface TransportOptions {
  getCanvas: () => HTMLCanvasElement | null;
  sendControl: (msg: string) => void;
  getPeer: () => RTCPeerConnection | null;
  onMode: (mode: TransportMode, reason?: string) => void;
  override?: TransportOverride;
  /** User agent string; defaults to navigator.userAgent (injectable for tests). */
  userAgent?: string;
}

export class VideoTransportController {
  private mode: TransportMode = "pending";
  private fellBack = false;
  private videoCh: RTCDataChannel | null = null;
  private fbCh: RTCDataChannel | null = null;
  private pipeline: WebCodecsPipeline | null = null;
  private labelTimer: ReturnType<typeof setTimeout> | null = null;
  private openTimer: ReturnType<typeof setTimeout> | null = null;
  private rttTimer: ReturnType<typeof setInterval> | null = null;
  private rttMs = NaN;
  private generation = 0;

  constructor(private readonly o: TransportOptions) {}

  get currentMode(): TransportMode {
    return this.mode;
  }

  private wantWebCodecs(): boolean {
    if (this.o.override === "track" || this.fellBack || !webCodecsAvailable()) return false;
    const ua = this.o.userAgent;
    if (this.o.override !== "webcodecs" && isWebKit(ua)) return false;
    return true;
  }

  /** The reason WebCodecs is not wanted, for logging; null when it is wanted. */
  private notWantedReason(): string | null {
    if (this.wantWebCodecs()) return null;
    if (this.o.override === "track") return "?transport=track";
    if (this.fellBack) return "fell back earlier this session";
    if (!webCodecsAvailable()) return "WebCodecs unavailable or not a secure context";
    return "Safari/WebKit is not supported";
  }

  private setMode(m: TransportMode, reason?: string): void {
    if (this.mode === m) return;
    this.mode = m;
    dlog("webcodecs", `transport -> ${m}`, reason ?? "");
    this.o.onMode(m, reason);
  }

  /** A fresh RTCPeerConnection was created (initial or host recycle). */
  peerReset(): void {
    this.teardown();
    this.mode = this.wantWebCodecs() ? "pending" : "track";
    this.o.onMode(this.mode);
  }

  onControlOpen(): void {
    if (!this.wantWebCodecs()) {
      this.o.sendControl(JSON.stringify({ kind: "video_transport", use: "track" }));
      this.setMode("track", this.notWantedReason() ?? undefined);
      return;
    }
    if (isFirefox(this.o.userAgent)) {
      dlog("webcodecs", "Firefox: WebCodecs mode is not officially supported; please report problems");
    }
    this.o.sendControl(JSON.stringify({ kind: "video_transport", use: "webcodecs" }));
    this.clearTimer("labelTimer");
    this.labelTimer = setTimeout(() => {
      if (!this.videoCh) this.fallback("host offered no video channel");
    }, VIDEO_LABEL_TIMEOUT_MS);
  }

  onControlMessage(data: string): void {
    let m: { kind?: string; active?: string };
    try {
      m = JSON.parse(data);
    } catch {
      return;
    }
    if (m.kind !== "video_transport" || typeof m.active !== "string") return;
    dlog("webcodecs", `host video_transport active=${m.active}`);
    if (m.active === "track" && this.wantWebCodecs()) this.fallback("host kept the RTP track");
  }

  onVideoChannel(ch: RTCDataChannel): void {
    this.videoCh = ch;
    ch.binaryType = "arraybuffer";
    this.clearTimer("labelTimer");
    if (!this.wantWebCodecs()) return;
    this.openTimer = setTimeout(() => {
      if (ch.readyState !== "open") this.fallback("video channel did not open");
    }, VIDEO_OPEN_TIMEOUT_MS);
    ch.onopen = () => {
      this.clearTimer("openTimer");
      this.maybeStart();
    };
    ch.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) this.pipeline?.onMessage(ev.data);
    };
    if (ch.readyState === "open") ch.onopen?.(new Event("open"));
  }

  onVideoFbChannel(ch: RTCDataChannel): void {
    this.fbCh = ch;
    ch.binaryType = "arraybuffer";
    if (!this.wantWebCodecs()) return;
    ch.onopen = () => this.maybeStart();
    if (ch.readyState === "open") this.maybeStart();
  }

  getStats(): PipelineStats | null {
    return this.pipeline?.getStats() ?? null;
  }

  close(): void {
    this.teardown();
  }

  // ---- internals ----

  private maybeStart(): void {
    if (this.pipeline || !this.videoCh || !this.fbCh) return;
    if (this.videoCh.readyState !== "open" || this.fbCh.readyState !== "open") return;
    const canvas = this.o.getCanvas();
    if (!canvas) {
      this.fallback("no canvas");
      return;
    }
    const gen = this.generation;
    const fb = this.fbCh;
    const pipeline = new WebCodecsPipeline({
      canvas,
      sendFeedback: (msg) => {
        if (fb.readyState === "open") fb.send(msg as unknown as ArrayBuffer);
      },
      getRttMs: () => this.rttMs,
      onFallback: (reason) => {
        if (gen === this.generation) this.fallback(reason);
      },
      onFirstKey: () => {
        if (gen === this.generation) this.setMode("webcodecs");
      },
    });
    this.pipeline = pipeline;
    this.startRttPoll();
    void pipeline.start().catch((err) => {
      if (gen === this.generation) this.fallback(`pipeline start failed: ${String(err)}`);
    });
  }

  private startRttPoll(): void {
    const poll = async () => {
      const pc = this.o.getPeer();
      if (!pc) return;
      try {
        const stats = await pc.getStats();
        stats.forEach((s) => {
          if (s.type === "candidate-pair" && s.state === "succeeded" && s.nominated) {
            if (typeof s.currentRoundTripTime === "number") this.rttMs = s.currentRoundTripTime * 1000;
          }
        });
      } catch {
        /* unavailable: pipeline assumes a default */
      }
    };
    void poll();
    this.rttTimer = setInterval(() => void poll(), 1000);
  }

  private fallback(reason: string): void {
    if (this.fellBack) return;
    this.fellBack = true; // not automatic to return within a session
    dlog("webcodecs", `falling back to RTP track: ${reason}`);
    this.o.sendControl(JSON.stringify({ kind: "video_transport", use: "track" }));
    this.teardown();
    this.setMode("track", reason);
  }

  private clearTimer(k: "labelTimer" | "openTimer"): void {
    if (this[k]) clearTimeout(this[k]!);
    this[k] = null;
  }

  private teardown(): void {
    this.generation++;
    this.clearTimer("labelTimer");
    this.clearTimer("openTimer");
    if (this.rttTimer) clearInterval(this.rttTimer);
    this.rttTimer = null;
    this.pipeline?.stop();
    this.pipeline = null;
    if (this.videoCh) {
      this.videoCh.onopen = null;
      this.videoCh.onmessage = null;
    }
    if (this.fbCh) this.fbCh.onopen = null;
    this.videoCh = null;
    this.fbCh = null;
  }
}
