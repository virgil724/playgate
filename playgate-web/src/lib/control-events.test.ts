import { describe, it, expect } from "vitest";
import {
  parseControlEvent,
  grantsControl,
  revokesControl,
  describeEvent,
} from "./control-events";

describe("parseControlEvent", () => {
  it("parses a granted event from JSON string", () => {
    const ev = parseControlEvent(
      JSON.stringify({
        kind: "granted",
        viewer_id: "cafe1234",
        remaining_seconds: 120,
        queue_position: 0,
        ts: 1718000000,
      }),
    );
    expect(ev).toEqual({
      kind: "granted",
      viewerId: "cafe1234",
      remainingSeconds: 120,
      queuePosition: 0,
      ts: 1718000000,
    });
  });

  it("parses an already-parsed object", () => {
    const ev = parseControlEvent({ kind: "tick", remaining_seconds: 59 });
    expect(ev?.kind).toBe("tick");
    expect(ev?.remainingSeconds).toBe(59);
  });

  it("parses queued with queue_position", () => {
    const ev = parseControlEvent({ kind: "queued", queue_position: 3 });
    expect(ev?.queuePosition).toBe(3);
  });

  it("defaults missing numeric fields to 0", () => {
    const ev = parseControlEvent({ kind: "expired" });
    expect(ev).toEqual({
      kind: "expired",
      viewerId: "",
      remainingSeconds: 0,
      queuePosition: 0,
      ts: 0,
    });
  });

  it("rejects unknown kinds", () => {
    expect(parseControlEvent({ kind: "bogus" })).toBeNull();
  });

  it("rejects invalid JSON", () => {
    expect(parseControlEvent("{not json")).toBeNull();
  });

  it("rejects non-objects", () => {
    expect(parseControlEvent(null)).toBeNull();
    expect(parseControlEvent(42)).toBeNull();
  });
});

describe("grantsControl / revokesControl", () => {
  it("grants for matching viewer", () => {
    const ev = parseControlEvent({ kind: "granted", viewer_id: "me" })!;
    expect(grantsControl(ev, "me")).toBe(true);
    expect(grantsControl(ev, "other")).toBe(false);
  });

  it("grants for any viewer when local id unknown", () => {
    const ev = parseControlEvent({ kind: "granted", viewer_id: "anyone" })!;
    expect(grantsControl(ev, "")).toBe(true);
  });

  it("tick does not grant control", () => {
    const ev = parseControlEvent({ kind: "tick" })!;
    expect(grantsControl(ev, "")).toBe(false);
  });

  it("revokes on expired / idle_kicked for matching viewer", () => {
    const exp = parseControlEvent({ kind: "expired", viewer_id: "me" })!;
    const idle = parseControlEvent({ kind: "idle_kicked", viewer_id: "me" })!;
    expect(revokesControl(exp, "me")).toBe(true);
    expect(revokesControl(idle, "me")).toBe(true);
    expect(revokesControl(exp, "other")).toBe(false);
  });
});

describe("describeEvent", () => {
  it("produces readable strings", () => {
    expect(describeEvent(parseControlEvent({ kind: "granted", remaining_seconds: 90 })!)).toContain("90");
    expect(describeEvent(parseControlEvent({ kind: "queued", queue_position: 2 })!)).toContain("#2");
    expect(describeEvent(parseControlEvent({ kind: "expired" })!)).toMatch(/ended/i);
    expect(describeEvent(parseControlEvent({ kind: "idle_kicked" })!)).toMatch(/inactivity/i);
  });
});
