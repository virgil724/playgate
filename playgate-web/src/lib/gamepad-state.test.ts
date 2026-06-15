import { describe, it, expect } from "vitest";
import { GamepadState, KEY_MAP } from "./gamepad-state";
import { BUTTON } from "./input-codec";

describe("GamepadState buttons", () => {
  it("sets and clears buttons", () => {
    const g = new GamepadState();
    g.setButton("A", true);
    expect(g.isPressed("A")).toBe(true);
    expect(g.snapshot().buttons & BUTTON.A).toBe(BUTTON.A);
    g.setButton("A", false);
    expect(g.isPressed("A")).toBe(false);
    expect(g.snapshot().buttons).toBe(0);
  });

  it("combines multiple buttons", () => {
    const g = new GamepadState();
    g.setButton("A", true);
    g.setButton("ZR", true);
    expect(g.snapshot().buttons).toBe((BUTTON.A | BUTTON.ZR) >>> 0);
  });

  it("keeps buttons as unsigned uint32 for high bits", () => {
    const g = new GamepadState();
    g.setButton("DPAD_RIGHT", true); // bit 17 = 0x20000
    expect(g.snapshot().buttons).toBe(BUTTON.DPAD_RIGHT);
    expect(g.snapshot().buttons).toBeGreaterThan(0);
  });
});

describe("GamepadState axes", () => {
  it("sets analog axis with clamping", () => {
    const g = new GamepadState();
    g.setAxis("lx", 0.5);
    expect(g.snapshot().lx).toBe(0.5);
    g.setAxis("lx", 5);
    expect(g.snapshot().lx).toBe(1);
    g.setAxis("lx", 0);
    expect(g.snapshot().lx).toBe(0);
  });

  it("cancels opposite direction keys on the same axis", () => {
    const g = new GamepadState();
    g.handleKey("KeyA", true); // lx -1
    expect(g.snapshot().lx).toBe(-1);
    g.handleKey("KeyD", true); // lx +1 -> cancels
    expect(g.snapshot().lx).toBe(0);
    g.handleKey("KeyA", false); // release left -> only +1 remains
    expect(g.snapshot().lx).toBe(1);
  });
});

describe("GamepadState keyboard mapping", () => {
  it("maps WASD to left stick", () => {
    const g = new GamepadState();
    g.handleKey("KeyW", true);
    expect(g.snapshot().ly).toBe(-1);
    g.handleKey("KeyS", true);
    expect(g.snapshot().ly).toBe(0);
  });

  it("maps arrow keys to right stick", () => {
    const g = new GamepadState();
    g.handleKey("ArrowRight", true);
    expect(g.snapshot().rx).toBe(1);
  });

  it("maps JKLI to ABXY", () => {
    const g = new GamepadState();
    g.handleKey("KeyJ", true);
    g.handleKey("KeyK", true);
    g.handleKey("KeyL", true);
    g.handleKey("KeyI", true);
    const b = g.snapshot().buttons;
    expect(b & BUTTON.A).toBe(BUTTON.A);
    expect(b & BUTTON.B).toBe(BUTTON.B);
    expect(b & BUTTON.X).toBe(BUTTON.X);
    expect(b & BUTTON.Y).toBe(BUTTON.Y);
  });

  it("returns false for unmapped keys", () => {
    const g = new GamepadState();
    expect(g.handleKey("KeyP", true)).toBe(false);
  });

  it("every KEY_MAP entry references a valid button or axis", () => {
    for (const entry of Object.values(KEY_MAP)) {
      const hasAction = !!entry.button || !!entry.leftAxis || !!entry.rightAxis;
      expect(hasAction).toBe(true);
      if (entry.button) expect(BUTTON[entry.button]).toBeDefined();
    }
  });
});

describe("GamepadState lifecycle", () => {
  it("reset clears all state", () => {
    const g = new GamepadState();
    g.setButton("A", true);
    g.handleKey("KeyW", true);
    g.setAxis("rx", 0.5);
    expect(g.isActive()).toBe(true);
    g.reset();
    expect(g.isActive()).toBe(false);
    expect(g.snapshot()).toEqual({ buttons: 0, lx: 0, ly: 0, rx: 0, ry: 0 });
  });

  it("isActive reflects held inputs", () => {
    const g = new GamepadState();
    expect(g.isActive()).toBe(false);
    g.setButton("HOME", true);
    expect(g.isActive()).toBe(true);
  });
});

describe("GamepadState onChange", () => {
  it("fires for button, axis, keyboard, and reset changes", () => {
    let calls = 0;
    const g = new GamepadState(() => calls++);

    g.setButton("A", true);
    g.setAxis("lx", 0.5);
    g.handleKey("KeyW", true);
    g.reset();

    expect(calls).toBe(4);
  });

  it("does not fire when state is unchanged", () => {
    let calls = 0;
    const g = new GamepadState(() => calls++);

    g.setButton("A", false);
    g.setAxis("lx", 0);
    g.handleKey("KeyW", false);
    g.reset();

    expect(calls).toBe(0);
  });
});
