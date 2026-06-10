import { useCallback, useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { ApiClient, API_BASE_URL, SIGNALING_BASE_URL, ApiError } from "../lib/api";
import { SignalingClient } from "../lib/signaling";
import { ViewerConnection, type ConnectionState } from "../lib/webrtc";
import { GamepadState } from "../lib/gamepad-state";
import { encodeInput } from "../lib/input-codec";
import {
  type ControlEvent,
  grantsControl,
  revokesControl,
  describeEvent,
} from "../lib/control-events";
import { VirtualGamepad } from "../components/VirtualGamepad";

const CONN_LABEL: Record<ConnectionState, string> = {
  idle: "Idle",
  "fetching-ice": "Preparing connection…",
  "waiting-offer": "Waiting for stream…",
  connecting: "Connecting…",
  connected: "Connected",
  failed: "Connection failed",
  closed: "Disconnected",
};

export function RoomPage() {
  const { roomId = "" } = useParams();
  const videoRef = useRef<HTMLVideoElement>(null);
  const connRef = useRef<ViewerConnection | null>(null);
  const gamepadRef = useRef(new GamepadState());
  const grantedRef = useRef(false);

  const [conn, setConn] = useState<ConnectionState>("idle");
  const [connDetail, setConnDetail] = useState("");
  const [granted, setGranted] = useState(false);
  const [remaining, setRemaining] = useState<number | null>(null);
  const [queuePos, setQueuePos] = useState<number | null>(null);
  const [statusMsg, setStatusMsg] = useState("");
  const [code, setCode] = useState("");
  const [redeeming, setRedeeming] = useState(false);
  const [redeemError, setRedeemError] = useState("");
  const [session, setSession] = useState<{ token: string; viewerId: string } | null>(null);
  const [, forceRender] = useState(0);

  const onChange = useCallback(() => forceRender((n) => n + 1), []);

  const handleControlEvent = useCallback((ev: ControlEvent) => {
    setStatusMsg(describeEvent(ev));
    const viewerId = session?.viewerId ?? "";
    if (ev.kind === "granted" && grantsControl(ev, viewerId)) {
      grantedRef.current = true;
      setGranted(true);
      setQueuePos(null);
      setRemaining(ev.remainingSeconds);
    } else if (ev.kind === "tick") {
      setRemaining(ev.remainingSeconds);
    } else if (ev.kind === "queued") {
      setQueuePos(ev.queuePosition);
      setGranted(false);
      grantedRef.current = false;
    } else if (revokesControl(ev, viewerId)) {
      grantedRef.current = false;
      setGranted(false);
      setRemaining(0);
      gamepadRef.current.reset();
    }
  }, [session]);

  // Start the WebRTC connection once we have a session (or immediately for view-only).
  useEffect(() => {
    if (!roomId) return;
    const signaling = new SignalingClient({
      baseUrl: SIGNALING_BASE_URL,
      roomId,
      peer: "viewer",
      token: session?.token,
    });
    const connection = new ViewerConnection({
      signaling,
      authPayload: session ? { token: session.token } : undefined,
      callbacks: {
        onState: (s, d) => {
          setConn(s);
          if (d) setConnDetail(d);
        },
        onTrack: (stream) => {
          if (videoRef.current) videoRef.current.srcObject = stream;
        },
        onControlEvent: handleControlEvent,
      },
    });
    connRef.current = connection;
    void connection.start();
    return () => {
      connection.close();
      connRef.current = null;
    };
  }, [roomId, session, handleControlEvent]);

  // 60 Hz input loop: sample gamepad, encode, send — only while granted.
  useEffect(() => {
    let raf = 0;
    let last = 0;
    const interval = 1000 / 60;
    const loop = (t: number) => {
      raf = requestAnimationFrame(loop);
      if (t - last < interval) return;
      last = t;
      if (!grantedRef.current) return;
      const conn = connRef.current;
      if (!conn || !conn.inputReady) return;
      conn.sendInput(encodeInput(gamepadRef.current.snapshot()));
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, []);

  // Keyboard mapping.
  useEffect(() => {
    const isTextField = (el: EventTarget | null) =>
      el instanceof HTMLElement && (el.tagName === "INPUT" || el.tagName === "TEXTAREA");
    const down = (e: KeyboardEvent) => {
      if (e.repeat || isTextField(e.target)) return;
      if (gamepadRef.current.handleKey(e.code, true)) {
        e.preventDefault();
        onChange();
      }
    };
    const up = (e: KeyboardEvent) => {
      if (gamepadRef.current.handleKey(e.code, false)) {
        e.preventDefault();
        onChange();
      }
    };
    const blur = () => gamepadRef.current.reset();
    window.addEventListener("keydown", down);
    window.addEventListener("keyup", up);
    window.addEventListener("blur", blur);
    return () => {
      window.removeEventListener("keydown", down);
      window.removeEventListener("keyup", up);
      window.removeEventListener("blur", blur);
    };
  }, [onChange]);

  const redeem = async () => {
    if (!code.trim()) return;
    setRedeeming(true);
    setRedeemError("");
    try {
      const api = new ApiClient(API_BASE_URL);
      const res = await api.redeem(code.trim());
      setSession({ token: res.session_token, viewerId: res.viewer_id });
      setQueuePos(res.queue_position);
      setStatusMsg(`Redeemed — queue position ${res.queue_position}`);
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? err.status === 404
            ? "Code not found"
            : err.status === 409
              ? "Code already used"
              : err.status === 410
                ? "Code was revoked"
                : err.message
          : "Network error";
      setRedeemError(msg);
    } finally {
      setRedeeming(false);
    }
  };

  const connClass =
    conn === "connected" ? "ok" : conn === "failed" ? "bad" : "warn";

  return (
    <div className="room">
      <div className="topbar">
        <span className={`dot ${connClass}`} />
        <span>{CONN_LABEL[conn]}</span>
        <span className="muted mono">room {roomId}</span>
        {granted && remaining !== null && (
          <span className="countdown" style={{ marginLeft: "auto", color: "var(--ok)" }}>
            ⏱ {remaining}s
          </span>
        )}
        {!granted && queuePos !== null && queuePos > 0 && (
          <span className="countdown" style={{ marginLeft: "auto", color: "var(--warn)" }}>
            Queue #{queuePos}
          </span>
        )}
      </div>

      <div className="video-wrap">
        <video ref={videoRef} playsInline autoPlay muted />
        {conn !== "connected" && (
          <div className="overlay-status">
            <div className="status-pill">{CONN_LABEL[conn]}</div>
            {connDetail && <div className="muted">{connDetail}</div>}
          </div>
        )}
      </div>

      {!session && (
        <div className="panel" style={{ margin: 12 }}>
          <strong>Have a token code?</strong>
          <div className="row" style={{ marginTop: 8 }}>
            <input
              placeholder="enter code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && void redeem()}
              className="mono"
            />
            <button className="btn-primary" onClick={() => void redeem()} disabled={redeeming}>
              {redeeming ? "Redeeming…" : "Redeem"}
            </button>
          </div>
          {redeemError && <div className="error" style={{ marginTop: 6 }}>{redeemError}</div>}
          <div className="muted" style={{ marginTop: 6, fontSize: 13 }}>
            Tokens are given out by the streamer. Watch above without a code.
          </div>
        </div>
      )}

      {statusMsg && (
        <div className="muted" style={{ padding: "4px 12px", textAlign: "center" }}>
          {statusMsg}
        </div>
      )}

      <VirtualGamepad state={gamepadRef.current} enabled={granted} onChange={onChange} />
    </div>
  );
}
