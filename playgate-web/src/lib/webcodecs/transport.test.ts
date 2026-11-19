import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { isFirefox, isWebKit, VideoTransportController, type TransportOverride } from "./transport";

const UA = {
  safariMac:
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
  safariIOS:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
  chromeIOS:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0.6478.54 Mobile/15E148 Safari/604.1",
  firefoxIOS:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/127.0 Mobile/15E148 Safari/605.1.15",
  chromeWin:
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
  chromeAndroid:
    "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
  edge:
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
  firefoxWin: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0",
};

describe("isWebKit", () => {
  it("flags Safari and every iOS browser", () => {
    for (const ua of [UA.safariMac, UA.safariIOS, UA.chromeIOS, UA.firefoxIOS]) {
      expect(isWebKit(ua), ua).toBe(true);
    }
  });
  it("does not flag Chromium or Firefox on desktop/Android", () => {
    for (const ua of [UA.chromeWin, UA.chromeAndroid, UA.edge, UA.firefoxWin]) {
      expect(isWebKit(ua), ua).toBe(false);
    }
  });
});

describe("isFirefox", () => {
  it("detects desktop Firefox only", () => {
    expect(isFirefox(UA.firefoxWin)).toBe(true);
    expect(isFirefox(UA.chromeWin)).toBe(false);
    expect(isFirefox(UA.safariMac)).toBe(false);
  });
});

describe("VideoTransportController browser policy", () => {
  beforeEach(() => {
    // Pretend WebCodecs exists in a secure context so only the UA decides.
    vi.stubGlobal("VideoDecoder", class {});
    vi.stubGlobal("EncodedVideoChunk", class {});
    vi.stubGlobal("isSecureContext", true);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function requested(userAgent: string, override: TransportOverride = null): string {
    const sent: string[] = [];
    const c = new VideoTransportController({
      getCanvas: () => null,
      sendControl: (m) => sent.push(m),
      getPeer: () => null,
      onMode: () => {},
      override,
      userAgent,
    });
    c.onControlOpen();
    c.peerReset(); // clears timers started by onControlOpen
    return JSON.parse(sent[0]).use;
  }

  it("keeps Safari on the RTP track", () => {
    expect(requested(UA.safariMac)).toBe("track");
    expect(requested(UA.safariIOS)).toBe("track");
  });
  it("lets ?transport=webcodecs force Safari for testing", () => {
    expect(requested(UA.safariMac, "webcodecs")).toBe("webcodecs");
  });
  it("asks for WebCodecs on Chrome and (unofficially) Firefox", () => {
    expect(requested(UA.chromeWin)).toBe("webcodecs");
    expect(requested(UA.firefoxWin)).toBe("webcodecs");
  });

  it("retries WebCodecs after a transient fallback on peerReset", () => {
    vi.useFakeTimers();
    try {
      const sent: string[] = [];
      const c = new VideoTransportController({
        getCanvas: () => null,
        sendControl: (m) => sent.push(m),
        getPeer: () => null,
        onMode: () => {},
        userAgent: UA.chromeWin,
      });
      c.onControlOpen();
      expect(JSON.parse(sent[sent.length - 1]).use).toBe("webcodecs");
      // Host offered no video channel -> transient fallback to track.
      vi.advanceTimersByTime(3500);
      expect(c.currentMode).toBe("track");
      // New host / reconnect resets transient fallback and retries WebCodecs.
      c.peerReset();
      c.onControlOpen();
      expect(JSON.parse(sent[sent.length - 1]).use).toBe("webcodecs");
      c.close();
    } finally {
      vi.useRealTimers();
    }
  });
});
