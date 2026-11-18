import { describe, it, expect } from "vitest";
import { KeyframeRequester, keyFrameTimeoutMs, maxInflightFor } from "./pipeline";
import { KEYREASON } from "./protocol";
import { annexBToAvcc, buildAvcC, codecStringFromSps, splitNals } from "./h264";

const SPS = [0x67, 0x42, 0xe0, 0x2a, 0xda, 0x01];
const PPS = [0x68, 0xce, 0x3c, 0x80];
const IDR = [0x65, 0x88, 0x84, 0x00, 0x33];

describe("h264 helpers", () => {
  const annexB = Uint8Array.from([0, 0, 0, 1, ...SPS, 0, 0, 1, ...PPS, 0, 0, 0, 1, ...IDR]);

  it("splits 3 and 4 byte start codes", () => {
    const nals = splitNals(annexB);
    expect(nals.map((n) => n.type)).toEqual([7, 8, 5]);
    expect([...nals[0].data]).toEqual(SPS);
    expect([...nals[1].data]).toEqual(PPS);
    expect([...nals[2].data]).toEqual(IDR);
  });

  it("builds the avc1 codec string from SPS bytes 1..3", () => {
    expect(codecStringFromSps(Uint8Array.from(SPS))).toBe("avc1.42e02a");
  });

  it("builds avcC and length-prefixed data", () => {
    const nals = splitNals(annexB);
    const avcc = buildAvcC(Uint8Array.from(SPS), Uint8Array.from(PPS));
    expect([...avcc.slice(0, 8)]).toEqual([1, 0x42, 0xe0, 0x2a, 0xff, 0xe1, 0, SPS.length]);
    expect(avcc.length).toBe(8 + SPS.length + 3 + PPS.length);
    const body = annexBToAvcc(nals);
    expect([...body]).toEqual([0, 0, 0, IDR.length, ...IDR]); // SPS/PPS stripped
  });
});

describe("KeyframeRequester", () => {
  function make() {
    const sent: Array<[number, number]> = [];
    const r = new KeyframeRequester((reason, id) => sent.push([reason, id]), () => 100);
    return { r, sent };
  }

  it("sends once per loss and rate-limits to 250ms", () => {
    const { r, sent } = make();
    r.request(KEYREASON.frameLost, 7, 1000);
    r.request(KEYREASON.frameLost, 8, 1100); // inside window: coalesced
    expect(sent).toHaveLength(1);
    r.tick(1200);
    expect(sent).toHaveLength(1);
    r.tick(1250);
    expect(sent).toHaveLength(2);
    expect(sent[1]).toEqual([KEYREASON.frameLost, 8]);
  });

  it("re-sends while no IDR arrives, stops after one does", () => {
    const { r, sent } = make();
    r.request(KEYREASON.frameLost, 1, 0);
    r.tick(200);
    expect(sent).toHaveLength(1);
    r.tick(300); // max(250, 3*100)
    expect(sent).toHaveLength(2);
    r.keyArrived();
    r.tick(5000);
    expect(sent).toHaveLength(2);
  });
});

describe("key frame timing", () => {
  it("waits at least 300ms or 3 RTTs for a KEY", () => {
    expect(keyFrameTimeoutMs(10)).toBe(300);
    expect(keyFrameTimeoutMs(200)).toBe(600);
  });

  it("sizes the in-flight window to the frames arriving during a KEY", () => {
    expect(maxInflightFor(300, 1000 / 30)).toBe(13); // ceil(9) + 4
    expect(maxInflightFor(300, 1000 / 60)).toBe(22); // ceil(18) + 4
    expect(maxInflightFor(20, 1000 / 60)).toBe(8); // never below the old fixed 8
  });

  it("does not re-request while a KEY may still be arriving", () => {
    const sent: number[] = [];
    const r = new KeyframeRequester(() => sent.push(1), () => 10, 250, () => 300);
    r.request(KEYREASON.frameLost, 1, 0);
    r.tick(260); // past the 250ms rate limit but inside the 300ms key timeout
    expect(sent).toHaveLength(1);
    r.tick(300);
    expect(sent).toHaveLength(2);
  });
});
