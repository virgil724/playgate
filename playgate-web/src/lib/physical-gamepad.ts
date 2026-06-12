/**
 * physical-gamepad.ts — read a connected physical gamepad (browser Gamepad
 * API) and convert it to an InputState.
 *
 * Mapping is POSITIONAL against the browser "standard" layout
 * (https://w3c.github.io/gamepad/#remapping): the bottom face button acts as
 * the Switch's B, the right one as A, and so on — muscle memory follows
 * button positions on Xbox/PS pads, not their labels.
 *
 * Axes pass through directly: the Gamepad API reports up as -1, which is
 * also this app's convention (KeyW → ly: -1; VirtualGamepad drag-up → -1).
 *
 * Framework-free. No React imports.
 */

import { BUTTON, type InputState } from "./input-codec";

/** Stick values below this magnitude are treated as 0 (drift/noise). */
export const STICK_DEADZONE = 0.15;

/** standard-mapping button index → wire button mask (positional). */
const STD_BUTTON_TO_MASK: readonly number[] = [
  BUTTON.B, //          0  bottom face   → Switch B (bottom)
  BUTTON.A, //          1  right face    → Switch A (right)
  BUTTON.Y, //          2  left face     → Switch Y (left)
  BUTTON.X, //          3  top face      → Switch X (top)
  BUTTON.L, //          4  L1
  BUTTON.R, //          5  R1
  BUTTON.ZL, //         6  L2
  BUTTON.ZR, //         7  R2
  BUTTON.MINUS, //      8  select/back
  BUTTON.PLUS, //       9  start
  BUTTON.L_STICK, //   10  L3
  BUTTON.R_STICK, //   11  R3
  BUTTON.DPAD_UP, //   12
  BUTTON.DPAD_DOWN, // 13
  BUTTON.DPAD_LEFT, // 14
  BUTTON.DPAD_RIGHT, //15
  BUTTON.HOME, //      16  guide/home
  BUTTON.CAPTURE, //   17  extra (PS touchpad / Switch capture)
];

/** Apply a radial deadzone, rescaled so output ramps from 0 at the edge. */
export function applyDeadzone(v: number, dz = STICK_DEADZONE): number {
  const a = Math.abs(v);
  if (a < dz) return 0;
  const scaled = (a - dz) / (1 - dz);
  return Math.sign(v) * Math.min(1, scaled);
}

/** The parts of Gamepad we read (kept narrow so tests can fake it). */
export interface GamepadLike {
  buttons: ReadonlyArray<{ pressed: boolean }>;
  axes: ReadonlyArray<number>;
}

/** Convert one (standard-mapping) gamepad to an InputState. */
export function gamepadToInputState(gp: GamepadLike): InputState {
  let buttons = 0;
  const n = Math.min(gp.buttons.length, STD_BUTTON_TO_MASK.length);
  for (let i = 0; i < n; i++) {
    if (gp.buttons[i]?.pressed) buttons |= STD_BUTTON_TO_MASK[i];
  }
  return {
    buttons: buttons >>> 0,
    lx: applyDeadzone(gp.axes[0] ?? 0),
    ly: applyDeadzone(gp.axes[1] ?? 0),
    rx: applyDeadzone(gp.axes[2] ?? 0),
    ry: applyDeadzone(gp.axes[3] ?? 0),
  };
}

/**
 * Read the current state of the first usable connected gamepad, or null when
 * none. Prefers a "standard"-mapping pad; falls back to the first connected
 * one (best effort — indices may be scrambled on exotic pads).
 */
export function pollPhysicalGamepad(): InputState | null {
  if (typeof navigator === "undefined" || !navigator.getGamepads) return null;
  const pads = navigator.getGamepads();
  let fallback: Gamepad | null = null;
  for (const gp of pads) {
    if (!gp || !gp.connected) continue;
    if (gp.mapping === "standard") return gamepadToInputState(gp);
    fallback = fallback ?? gp;
  }
  return fallback ? gamepadToInputState(fallback) : null;
}

/**
 * Merge keyboard/virtual state with the physical pad: buttons are OR'd,
 * a non-zero physical axis wins over the keyboard axis.
 */
export function mergeInput(base: InputState, pad: InputState | null): InputState {
  if (!pad) return base;
  return {
    buttons: (base.buttons | pad.buttons) >>> 0,
    lx: pad.lx !== 0 ? pad.lx : base.lx,
    ly: pad.ly !== 0 ? pad.ly : base.ly,
    rx: pad.rx !== 0 ? pad.rx : base.rx,
    ry: pad.ry !== 0 ? pad.ry : base.ry,
  };
}
