import { useCallback, useRef } from "react";
import type { GamepadState } from "../lib/gamepad-state";
import { KEY_LEGEND } from "../lib/gamepad-state";
import type { ButtonName } from "../lib/input-codec";

interface Props {
  state: GamepadState;
  /** Whether input is currently active (granted). Disables visually when not. */
  enabled: boolean;
  /** Called whenever the held set changes, so the parent can re-render highlights. */
  onChange?: () => void;
}

/**
 * Touch + pointer virtual gamepad overlay. Mutates the shared GamepadState.
 * The parent samples GamepadState at 60 Hz for sending; this component only
 * manages the held set and analog sticks.
 */
export function VirtualGamepad({ state, enabled, onChange }: Props) {
  const press = useCallback(
    (name: ButtonName, down: boolean) => {
      state.setButton(name, down);
      onChange?.();
    },
    [state, onChange],
  );

  return (
    <div className="gamepad" aria-disabled={!enabled}>
      <div className="pad-cluster">
        <Stick state={state} side="left" onChange={onChange} />
        <div className="dpad-grid">
          <HoldBtn name="DPAD_UP" label="▲" cls="up" press={press} state={state} />
          <HoldBtn name="DPAD_LEFT" label="◀" cls="left" press={press} state={state} />
          <HoldBtn name="DPAD_RIGHT" label="▶" cls="right" press={press} state={state} />
          <HoldBtn name="DPAD_DOWN" label="▼" cls="down" press={press} state={state} />
        </div>
        <div className="shoulders">
          <HoldBtn name="L" label="L" press={press} state={state} small />
          <HoldBtn name="ZL" label="ZL" press={press} state={state} small />
        </div>
        <div className="center-row">
          <HoldBtn name="MINUS" label="−" press={press} state={state} small />
          <HoldBtn name="CAPTURE" label="◉" press={press} state={state} small />
        </div>
      </div>

      <div className="pad-cluster">
        <Stick state={state} side="right" onChange={onChange} />
        <div className="face-grid">
          <HoldBtn name="X" label="X" cls="x" press={press} state={state} />
          <HoldBtn name="Y" label="Y" cls="y" press={press} state={state} />
          <HoldBtn name="A" label="A" cls="a" press={press} state={state} />
          <HoldBtn name="B" label="B" cls="b" press={press} state={state} />
        </div>
        <div className="shoulders">
          <HoldBtn name="ZR" label="ZR" press={press} state={state} small />
          <HoldBtn name="R" label="R" press={press} state={state} small />
        </div>
        <div className="center-row">
          <HoldBtn name="PLUS" label="+" press={press} state={state} small />
          <HoldBtn name="HOME" label="⌂" press={press} state={state} small />
        </div>
      </div>

      <div className="panel legend" style={{ gridColumn: "1 / -1" }}>
        <strong>Keyboard map</strong>
        <table>
          <tbody>
            {KEY_LEGEND.map((row) => (
              <tr key={row.action}>
                <td>{row.keys}</td>
                <td>{row.action}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function HoldBtn({
  name,
  label,
  cls = "",
  press,
  state,
  small,
}: {
  name: ButtonName;
  label: string;
  cls?: string;
  press: (n: ButtonName, d: boolean) => void;
  state: GamepadState;
  small?: boolean;
}) {
  const down = (e: React.PointerEvent) => {
    e.preventDefault();
    (e.target as HTMLElement).setPointerCapture?.(e.pointerId);
    press(name, true);
  };
  const up = (e: React.PointerEvent) => {
    e.preventDefault();
    press(name, false);
  };
  return (
    <div
      className={`gp-btn ${small ? "gp-small" : ""} ${cls} ${state.isPressed(name) ? "held" : ""}`}
      onPointerDown={down}
      onPointerUp={up}
      onPointerCancel={up}
      onPointerLeave={(e) => {
        if (e.buttons) up(e);
      }}
      role="button"
      aria-label={name}
    >
      {label}
    </div>
  );
}

function Stick({
  state,
  side,
  onChange,
}: {
  state: GamepadState;
  side: "left" | "right";
  onChange?: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const nubRef = useRef<HTMLDivElement>(null);
  const axX = side === "left" ? "lx" : "rx";
  const axY = side === "left" ? "ly" : "ry";

  const move = (clientX: number, clientY: number) => {
    const el = ref.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const cx = r.left + r.width / 2;
    const cy = r.top + r.height / 2;
    let dx = (clientX - cx) / (r.width / 2);
    let dy = (clientY - cy) / (r.height / 2);
    const mag = Math.hypot(dx, dy);
    if (mag > 1) {
      dx /= mag;
      dy /= mag;
    }
    state.setAxis(axX, dx);
    state.setAxis(axY, dy);
    if (nubRef.current) {
      nubRef.current.style.transform = `translate(calc(-50% + ${dx * 36}px), calc(-50% + ${dy * 36}px))`;
    }
    onChange?.();
  };

  const reset = () => {
    state.setAxis(axX, 0);
    state.setAxis(axY, 0);
    if (nubRef.current) nubRef.current.style.transform = "translate(-50%, -50%)";
    onChange?.();
  };

  return (
    <div
      className="stick"
      ref={ref}
      onPointerDown={(e) => {
        e.preventDefault();
        (e.target as HTMLElement).setPointerCapture?.(e.pointerId);
        move(e.clientX, e.clientY);
      }}
      onPointerMove={(e) => {
        if (e.buttons) move(e.clientX, e.clientY);
      }}
      onPointerUp={reset}
      onPointerCancel={reset}
      onPointerLeave={(e) => {
        if (e.buttons) reset();
      }}
      aria-label={`${side} stick`}
    >
      <div className="nub" ref={nubRef} />
    </div>
  );
}
