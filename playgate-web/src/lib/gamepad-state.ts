/**
 * gamepad-state.ts — mutable controller state plus keyboard mapping.
 *
 * Framework-free. Holds the current button/axis state and provides helpers to
 * set/clear buttons, set stick axes, and translate keyboard events into state
 * changes. The owner is expected to read .snapshot() at 60 Hz and encode it.
 */

import { BUTTON, type ButtonName, type InputState, emptyInputState } from "./input-codec";

/**
 * Keyboard mapping (desktop). Documented in the UI.
 *
 * Left stick : WASD
 * Right stick: arrow keys
 * Face buttons (ABXY): J K L I
 * Shoulders  : Q=L  E=R  1=ZL  3=ZR
 * +/-        : Enter=PLUS  Backspace/ShiftRight=MINUS
 * D-pad      : T G F H (up/down/left/right)
 * Home/Capture: H? -> use Home=Backquote, Capture=Backslash
 * Stick clicks: Z=L_STICK  C=R_STICK
 */
export interface KeyMapEntry {
  /** Which button this key presses (if any). */
  button?: ButtonName;
  /** Left-stick axis contribution: [axis, value]. */
  leftAxis?: { axis: "lx" | "ly"; value: number };
  /** Right-stick axis contribution. */
  rightAxis?: { axis: "rx" | "ry"; value: number };
}

/** Keyed by KeyboardEvent.code. */
export const KEY_MAP: Record<string, KeyMapEntry> = {
  // Left stick — WASD
  KeyW: { leftAxis: { axis: "ly", value: -1 } },
  KeyS: { leftAxis: { axis: "ly", value: 1 } },
  KeyA: { leftAxis: { axis: "lx", value: -1 } },
  KeyD: { leftAxis: { axis: "lx", value: 1 } },

  // Right stick — arrow keys
  ArrowUp: { rightAxis: { axis: "ry", value: -1 } },
  ArrowDown: { rightAxis: { axis: "ry", value: 1 } },
  ArrowLeft: { rightAxis: { axis: "rx", value: -1 } },
  ArrowRight: { rightAxis: { axis: "rx", value: 1 } },

  // Face buttons — J K L I = A B X Y
  KeyJ: { button: "A" },
  KeyK: { button: "B" },
  KeyL: { button: "X" },
  KeyI: { button: "Y" },

  // Shoulders / triggers
  KeyQ: { button: "L" },
  KeyE: { button: "R" },
  Digit1: { button: "ZL" },
  Digit3: { button: "ZR" },

  // Plus / Minus
  Enter: { button: "PLUS" },
  Backspace: { button: "MINUS" },

  // D-pad — T F G H (up/left/down/right)
  KeyT: { button: "DPAD_UP" },
  KeyG: { button: "DPAD_DOWN" },
  KeyF: { button: "DPAD_LEFT" },
  KeyH: { button: "DPAD_RIGHT" },

  // System buttons
  Backquote: { button: "HOME" },
  Backslash: { button: "CAPTURE" },

  // Stick clicks
  KeyZ: { button: "L_STICK" },
  KeyC: { button: "R_STICK" },
};

/** Human-readable rows for the UI legend. */
export const KEY_LEGEND: { keys: string; action: string }[] = [
  { keys: "W A S D", action: "Left stick" },
  { keys: "Arrow keys", action: "Right stick" },
  { keys: "J K L I", action: "A B X Y" },
  { keys: "Q E", action: "L / R" },
  { keys: "1 3", action: "ZL / ZR" },
  { keys: "T F G H", action: "D-pad up/left/down/right" },
  { keys: "Enter / Backspace", action: "+ / -" },
  { keys: "Z C", action: "Stick click L / R" },
  { keys: "` \\", action: "Home / Capture" },
];

export class GamepadState {
  constructor(public onChange?: () => void) {}

  private buttons = 0;
  // Track per-direction axis intent so opposite keys cancel cleanly.
  private axisKeys: Record<"lx" | "ly" | "rx" | "ry", Set<number>> = {
    lx: new Set(),
    ly: new Set(),
    rx: new Set(),
    ry: new Set(),
  };
  // Touch/analog overrides set the axis directly.
  private analog: Partial<Record<"lx" | "ly" | "rx" | "ry", number>> = {};

  private emitChange(): void {
    this.onChange?.();
  }

  setButton(name: ButtonName, pressed: boolean): void {
    const before = this.buttons;
    if (pressed) this.buttons |= BUTTON[name];
    else this.buttons &= ~BUTTON[name] >>> 0;
    this.buttons = this.buttons >>> 0;
    if (this.buttons !== before) this.emitChange();
  }

  isPressed(name: ButtonName): boolean {
    return (this.buttons & BUTTON[name]) !== 0;
  }

  /** Set an analog axis directly (touch drag / gamepad), value in [-1,1]. */
  setAxis(axis: "lx" | "ly" | "rx" | "ry", value: number): void {
    const next = value === 0 ? undefined : Math.max(-1, Math.min(1, value));
    if (this.analog[axis] === next) return;
    if (next === undefined) delete this.analog[axis];
    else this.analog[axis] = next;
    this.emitChange();
  }

  /** Apply a keyboard key down/up using KEY_MAP. Returns true if mapped. */
  handleKey(code: string, down: boolean): boolean {
    const entry = KEY_MAP[code];
    if (!entry) return false;
    const before = this.snapshot();
    if (entry.button) {
      if (down) this.buttons |= BUTTON[entry.button];
      else this.buttons &= ~BUTTON[entry.button] >>> 0;
      this.buttons = this.buttons >>> 0;
    }
    const dir = entry.leftAxis ?? entry.rightAxis;
    if (dir) {
      const set = this.axisKeys[dir.axis];
      if (down) set.add(dir.value);
      else set.delete(dir.value);
    }
    const after = this.snapshot();
    if (!sameInputState(before, after)) this.emitChange();
    return true;
  }

  private axisValue(axis: "lx" | "ly" | "rx" | "ry"): number {
    if (this.analog[axis] !== undefined) return this.analog[axis]!;
    const set = this.axisKeys[axis];
    let v = 0;
    for (const k of set) v += k;
    return Math.max(-1, Math.min(1, v));
  }

  /** Read the current immutable state for encoding. */
  snapshot(): InputState {
    return {
      buttons: this.buttons >>> 0,
      lx: this.axisValue("lx"),
      ly: this.axisValue("ly"),
      rx: this.axisValue("rx"),
      ry: this.axisValue("ry"),
    };
  }

  /** Reset everything to neutral (e.g. on focus loss or session end). */
  reset(): void {
    const wasActive = this.isActive();
    this.buttons = 0;
    for (const k of Object.keys(this.axisKeys) as (keyof typeof this.axisKeys)[]) {
      this.axisKeys[k].clear();
    }
    this.analog = {};
    if (wasActive) this.emitChange();
  }

  /** True if any input is currently held. */
  isActive(): boolean {
    const s = this.snapshot();
    return s.buttons !== 0 || s.lx !== 0 || s.ly !== 0 || s.rx !== 0 || s.ry !== 0;
  }
}

function sameInputState(a: InputState, b: InputState): boolean {
  return (
    a.buttons === b.buttons &&
    a.lx === b.lx &&
    a.ly === b.ly &&
    a.rx === b.rx &&
    a.ry === b.ry
  );
}

export { emptyInputState };
