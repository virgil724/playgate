import { describe, it, expect } from "vitest";
import {
  applyDeadzone,
  gamepadToInputState,
  mergeInput,
  STICK_DEADZONE,
  type GamepadLike,
} from "./physical-gamepad";
import { BUTTON, emptyInputState } from "./input-codec";

function fakePad(pressed: number[] = [], axes: number[] = [0, 0, 0, 0]): GamepadLike {
  const buttons = Array.from({ length: 18 }, (_, i) => ({
    pressed: pressed.includes(i),
  }));
  return { buttons, axes };
}

describe("applyDeadzone", () => {
  it("zeroes values inside the deadzone", () => {
    expect(applyDeadzone(0)).toBe(0);
    expect(applyDeadzone(STICK_DEADZONE - 0.01)).toBe(0);
    expect(applyDeadzone(-(STICK_DEADZONE - 0.01))).toBe(0);
  });

  it("ramps from 0 at the edge to ±1 at full deflection", () => {
    expect(applyDeadzone(STICK_DEADZONE)).toBeCloseTo(0, 5);
    expect(applyDeadzone(1)).toBe(1);
    expect(applyDeadzone(-1)).toBe(-1);
    const mid = applyDeadzone((1 + STICK_DEADZONE) / 2);
    expect(mid).toBeCloseTo(0.5, 5);
  });
});

describe("gamepadToInputState", () => {
  it("maps face buttons positionally (bottom=B, right=A, left=Y, top=X)", () => {
    const s = gamepadToInputState(fakePad([0, 1, 2, 3]));
    expect(s.buttons).toBe(BUTTON.B | BUTTON.A | BUTTON.Y | BUTTON.X);
  });

  it("maps shoulders, triggers, sticks, start/select, dpad, home/capture", () => {
    const s = gamepadToInputState(
      fakePad([4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17]),
    );
    expect(s.buttons).toBe(
      BUTTON.L | BUTTON.R | BUTTON.ZL | BUTTON.ZR |
      BUTTON.MINUS | BUTTON.PLUS |
      BUTTON.L_STICK | BUTTON.R_STICK |
      BUTTON.DPAD_UP | BUTTON.DPAD_DOWN | BUTTON.DPAD_LEFT | BUTTON.DPAD_RIGHT |
      BUTTON.HOME | BUTTON.CAPTURE,
    );
  });

  it("passes axes through with deadzone (up = -1 convention)", () => {
    const s = gamepadToInputState(fakePad([], [0.05, -1, 1, -0.05]));
    expect(s.lx).toBe(0); // inside deadzone
    expect(s.ly).toBe(-1);
    expect(s.rx).toBe(1);
    expect(s.ry).toBe(0);
  });

  it("tolerates pads with fewer buttons/axes", () => {
    const s = gamepadToInputState({ buttons: [{ pressed: true }], axes: [] });
    expect(s.buttons).toBe(BUTTON.B);
    expect(s.lx).toBe(0);
  });
});

describe("mergeInput", () => {
  it("returns base unchanged when no pad is present", () => {
    const base = { buttons: BUTTON.A, lx: 0.5, ly: 0, rx: 0, ry: 0 };
    expect(mergeInput(base, null)).toEqual(base);
  });

  it("ORs buttons and lets a non-zero pad axis win", () => {
    const base = { buttons: BUTTON.A, lx: 0.5, ly: -1, rx: 0, ry: 0 };
    const pad = { buttons: BUTTON.ZR, lx: -0.25, ly: 0, rx: 1, ry: 0 };
    expect(mergeInput(base, pad)).toEqual({
      buttons: BUTTON.A | BUTTON.ZR,
      lx: -0.25, // pad wins
      ly: -1, // pad neutral → keyboard kept
      rx: 1,
      ry: 0,
    });
  });

  it("merging two empty states stays neutral", () => {
    expect(mergeInput(emptyInputState(), emptyInputState())).toEqual(emptyInputState());
  });
});
