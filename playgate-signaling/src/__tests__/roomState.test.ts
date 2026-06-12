/**
 * Unit tests for the pure room/queue logic (roomState.ts).
 *
 * These exercise append / seq assignment / offer-reset / since-filtering
 * directly against a fake in-memory storage, with no DO runtime involved.
 */

import { describe, it, expect, beforeEach } from "vitest";
import {
  RoomState,
  isOfferPayload,
  otherPeer,
  MAX_QUEUE_LENGTH,
} from "../roomState.js";
import { FakeStorage } from "./helpers.js";

describe("isOfferPayload", () => {
  it("is true only for objects with type === 'offer'", () => {
    expect(isOfferPayload({ type: "offer" })).toBe(true);
    expect(isOfferPayload({ type: "offer", sdp: "x" })).toBe(true);
    expect(isOfferPayload({ type: "answer" })).toBe(false);
    expect(isOfferPayload(null)).toBe(false);
    expect(isOfferPayload("offer")).toBe(false);
    expect(isOfferPayload(42)).toBe(false);
    expect(isOfferPayload(["offer"])).toBe(false);
  });
});

describe("otherPeer", () => {
  it("swaps host/viewer", () => {
    expect(otherPeer("host")).toBe("viewer");
    expect(otherPeer("viewer")).toBe("host");
  });
});

describe("RoomState.append + seq assignment", () => {
  let room: RoomState;

  beforeEach(() => {
    room = new RoomState(new FakeStorage());
  });

  it("assigns incrementing seq numbers starting at 0", async () => {
    const a = await room.append("host", { n: 0 });
    const b = await room.append("host", { n: 1 });
    const c = await room.append("host", { n: 2 });
    expect(a.message.seq).toBe(0);
    expect(b.message.seq).toBe(1);
    expect(c.message.seq).toBe(2);
  });

  it("sets an ISO timestamp and records sender/recipient", async () => {
    const r = await room.append("viewer", { type: "answer" });
    expect(typeof r.message.ts).toBe("string");
    expect(new Date(r.message.ts).toISOString()).toBe(r.message.ts);
    expect(r.sender).toBe("viewer");
    expect(r.recipient).toBe("host");
  });

  it("keeps host and viewer queues independent", async () => {
    await room.append("host", { h: 0 });
    await room.append("viewer", { v: 0 });
    // each peer's seq starts independently
    const h1 = await room.append("host", { h: 1 });
    const v1 = await room.append("viewer", { v: 1 });
    expect(h1.message.seq).toBe(1);
    expect(v1.message.seq).toBe(1);
  });
});

describe("RoomState.poll + since-filtering", () => {
  let room: RoomState;

  beforeEach(() => {
    room = new RoomState(new FakeStorage());
  });

  it("returns empty with nextSince=-1 when nothing posted", async () => {
    const r = await room.poll("viewer", -1);
    expect(r.messages).toHaveLength(0);
    expect(r.nextSince).toBe(-1);
  });

  it("viewer reads host messages; host reads viewer messages", async () => {
    await room.append("host", { type: "offer" });
    const v = await room.poll("viewer", -1);
    expect(v.messages).toHaveLength(1);
    expect(v.messages[0]!.payload).toEqual({ type: "offer" });

    await room.append("viewer", { type: "answer" });
    const h = await room.poll("host", -1);
    expect(h.messages).toHaveLength(1);
    expect(h.messages[0]!.payload).toEqual({ type: "answer" });
  });

  it("filters by since and advances nextSince", async () => {
    for (let i = 0; i < 3; i++) await room.append("host", { n: i });
    const r = await room.poll("viewer", 1);
    expect(r.messages).toHaveLength(1);
    expect(r.messages[0]!.seq).toBe(2);
    expect(r.nextSince).toBe(2);
  });

  it("nextSince stays at since when no new messages", async () => {
    await room.append("host", { n: 0 });
    const r = await room.poll("viewer", 0);
    expect(r.messages).toHaveLength(0);
    expect(r.nextSince).toBe(0);
  });
});

describe("RoomState host offer-reset", () => {
  let room: RoomState;

  beforeEach(() => {
    room = new RoomState(new FakeStorage());
  });

  it("replaces the host queue keeping seq continuity", async () => {
    await room.append("host", { type: "offer", sdp: "old" }); // seq 0
    await room.append("host", { candidate: "c" }); // seq 1
    const reset = await room.append("host", { type: "offer", sdp: "new" });
    expect(reset.message.seq).toBe(2); // (last old seq) + 1

    // Fresh viewer sees only the new offer.
    const fresh = await room.poll("viewer", -1);
    expect(fresh.messages).toHaveLength(1);
    expect(fresh.messages[0]!.seq).toBe(2);
    expect(fresh.messages[0]!.payload).toEqual({ type: "offer", sdp: "new" });

    // A viewer mid-poll (since=1) still sees the new offer.
    const resumed = await room.poll("viewer", 1);
    expect(resumed.messages).toHaveLength(1);
    expect(resumed.messages[0]!.seq).toBe(2);
  });

  it("deletes the viewer queue so stale answers do not survive", async () => {
    await room.append("viewer", { type: "answer", sdp: "stale" });
    await room.append("host", { type: "offer", sdp: "new" });
    const h = await room.poll("host", -1);
    expect(h.messages).toHaveLength(0);
    expect(h.nextSince).toBe(-1);
  });

  it("viewer 'offer' payloads still append (only host triggers reset)", async () => {
    await room.append("viewer", { type: "answer" });
    await room.append("viewer", { type: "offer", sdp: "viewer-offer" });
    const h = await room.poll("host", -1);
    expect(h.messages).toHaveLength(2);
    expect(h.messages[1]!.seq).toBe(1);
  });

  it("host non-offer / malformed payloads append without reset", async () => {
    await room.append("host", { type: "offer" }); // seq 0
    for (const weird of ["s", 42, null, ["offer"]]) {
      const r = await room.append("host", weird);
      expect(typeof r.message.seq).toBe("number");
    }
    const v = await room.poll("viewer", -1);
    expect(v.messages).toHaveLength(5); // offer + 4 appended
  });
});

describe("RoomState.backlogFor", () => {
  it("returns the other peer's full queue", async () => {
    const room = new RoomState(new FakeStorage());
    await room.append("host", { a: 1 });
    await room.append("host", { b: 2 });
    const backlog = await room.backlogFor("viewer");
    expect(backlog).toHaveLength(2);
    expect(backlog[0]!.payload).toEqual({ a: 1 });
  });
});

describe("RoomState queue cap", () => {
  it("truncates a queue that reaches MAX_QUEUE_LENGTH", async () => {
    const room = new RoomState(new FakeStorage());
    for (let i = 0; i < MAX_QUEUE_LENGTH + 5; i++) {
      await room.append("host", { n: i });
    }
    const backlog = await room.backlogFor("viewer");
    expect(backlog.length).toBeLessThanOrEqual(MAX_QUEUE_LENGTH);
  });
});
