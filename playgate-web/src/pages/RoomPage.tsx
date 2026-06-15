import { useCallback, useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { ApiClient, API_BASE_URL, SIGNALING_BASE_URL, ApiError } from "../lib/api";
import { SignalingClient } from "../lib/signaling";
import { ViewerConnection, type ConnectionState } from "../lib/webrtc";
import { GamepadState } from "../lib/gamepad-state";
import { encodeInput, type InputState } from "../lib/input-codec";
import { pollPhysicalGamepad, mergeInput } from "../lib/physical-gamepad";
import {
  type ControlEvent,
  grantsControl,
  revokesControl,
  describeEvent,
} from "../lib/control-events";
import { VirtualGamepad } from "../components/VirtualGamepad";
import { dlog, subscribeLog, logHistory, type LogEntry } from "../lib/log";

const CONN_LABEL: Record<ConnectionState, string> = {
  idle: "Idle",
  "fetching-ice": "Preparing connection…",
  "waiting-offer": "Waiting for stream…",
  connecting: "Connecting…",
  connected: "Connected",
  failed: "Connection failed",
  closed: "Disconnected",
};

const INPUT_BACKPRESSURE_BYTES = 2048;
const ACTIVE_INPUT_REFRESH_MS = 1000 / 30;
const NEUTRAL_RESEND_COUNT = 4;

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
  // The <video> starts muted so the browser allows autoplay; the host now sends
  // an Opus audio track, so expose a toggle to unmute after a user gesture.
  const [audioMuted, setAudioMuted] = useState(true);
  const [code, setCode] = useState("");
  const [redeeming, setRedeeming] = useState(false);
  const [redeemError, setRedeemError] = useState("");
  const [session, setSession] = useState<{ token: string; viewerId: string } | null>(null);
  // Ref mirror so stable callbacks can read the current session without
  // retriggering the connection effect (reconnecting on redeem races the
  // host, which is still serving the connection we'd be tearing down).
  const sessionRef = useRef(session);
  sessionRef.current = session;
  const [logs, setLogs] = useState<LogEntry[]>(() => logHistory());
  const [rtt, setRtt] = useState<string>("-");
  const [videoStats, setVideoStats] = useState<string>("-");
  const [audioStats, setAudioStats] = useState<string>("-");
  // Previous cumulative counters, so jitter/decode are shown as the CURRENT
  // per-interval value (delta) rather than a session average that hides drift.
  const statsPrevRef = useRef({ jbDelay: 0, jbCount: 0, decTime: 0, decFrames: 0 });
  // Monotonic input sequence so the host can drop stale/reordered frames.
  const inputSeqRef = useRef(0);
  const inputFramesSentRef = useRef(0);
  const inputFramesDroppedRef = useRef(0);
  const [inputStats, setInputStats] = useState({ sent: 0, dropped: 0 });
  const lastSentInputRef = useRef<InputState | null>(null);
  const neutralResendsRemainingRef = useRef(0);
  const inputSendQueuedRef = useRef(false);
  const sendNowRef = useRef<(force?: boolean) => boolean>(() => false);
  const [showGamepad, setShowGamepad] = useState(() => {
    if (typeof window !== "undefined") {
      return "ontouchstart" in window || navigator.maxTouchPoints > 0;
    }
    return false;
  });
  const [showLogs, setShowLogs] = useState(false);
  const logBoxRef = useRef<HTMLDivElement>(null);
  const [, forceRender] = useState(0);

  // Mirror the shared debug log into the on-page panel.
  useEffect(() => {
    const unsub = subscribeLog((e) => setLogs((prev) => [...prev.slice(-299), e]));
    return unsub;
  }, []);
  useEffect(() => {
    const box = logBoxRef.current;
    if (box) box.scrollTop = box.scrollHeight;
  }, [logs]);

  const currentInputState = useCallback(
    () => mergeInput(gamepadRef.current.snapshot(), pollPhysicalGamepad()),
    [],
  );

  const sendNow = useCallback(
    (force = false): boolean => {
      if (!grantedRef.current) return false;
      const conn = connRef.current;
      if (!conn || !conn.inputReady) return false;
      if (conn.inputBufferedAmount > INPUT_BACKPRESSURE_BYTES) {
        inputFramesDroppedRef.current++;
        return false;
      }

      const state = currentInputState();
      const previous = lastSentInputRef.current;
      if (!force && previous && sameInputState(previous, state)) return false;

      conn.sendInput(encodeInput(state, ++inputSeqRef.current));
      inputFramesSentRef.current++;
      lastSentInputRef.current = state;

      if (isNeutralInput(state)) {
        if (!previous || !isNeutralInput(previous)) {
          neutralResendsRemainingRef.current = NEUTRAL_RESEND_COUNT;
        }
      } else {
        neutralResendsRemainingRef.current = 0;
      }
      return true;
    },
    [currentInputState],
  );
  sendNowRef.current = sendNow;

  const queueInputSend = useCallback(() => {
    if (inputSendQueuedRef.current) return;
    inputSendQueuedRef.current = true;
    queueMicrotask(() => {
      inputSendQueuedRef.current = false;
      sendNowRef.current();
    });
  }, []);

  useEffect(() => {
    gamepadRef.current.onChange = queueInputSend;
    return () => {
      gamepadRef.current.onChange = undefined;
    };
  }, [queueInputSend]);

  const onChange = useCallback(() => forceRender((n) => n + 1), []);

  const handleControlEvent = useCallback((ev: ControlEvent) => {
    const viewerId = sessionRef.current?.viewerId ?? "";
    // Events are broadcast to every viewer on the control channel; only react to
    // the ones addressed to our own session. Without this, all clients would
    // share the controller's countdown / queue position.
    if (viewerId === "" || ev.viewerId !== viewerId) return;
    setStatusMsg(describeEvent(ev));
    if (ev.kind === "granted" && grantsControl(ev, viewerId)) {
      lastSentInputRef.current = null;
      neutralResendsRemainingRef.current = 0;
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
      gamepadRef.current.reset();
      sendNowRef.current(true);
      grantedRef.current = false;
      setGranted(false);
      setRemaining(0);
      setQueuePos(null);
      // The session is over and the JWT is spent — drop it so the redeem
      // panel reappears and the viewer can enter a fresh code without
      // reloading the page. The connection itself stays up (view-only).
      setSession(null);
    }
  }, []);

  // Start the WebRTC connection once per room. Redeeming later does NOT
  // reconnect — auth is sent over the live control channel (see below);
  // tearing down and re-answering races the host, which is still serving
  // the connection we just closed.
  useEffect(() => {
    if (!roomId) return;
    const sess = sessionRef.current;
    dlog("page", `starting connection room=${roomId} ${sess ? "with session token" : "view-only (no token)"}`);
    const signaling = new SignalingClient({
      baseUrl: SIGNALING_BASE_URL,
      roomId,
      peer: "viewer",
      token: sess?.token,
    });
    const connection = new ViewerConnection({
      signaling,
      authPayload: sess ? { token: sess.token } : undefined,
      callbacks: {
        onState: (s, d) => {
          setConn(s);
          if (d) setConnDetail(d);
          if (s !== "connected") {
            setRtt("-");
            setVideoStats("-");
            setAudioStats("-");
          }
        },
        onTrack: (stream) => {
          if (videoRef.current) videoRef.current.srcObject = stream;
        },
        onControlEvent: handleControlEvent,
        onControlMessage: (data) => {
          try {
            const m = JSON.parse(data);
            if (m.kind === "pong" && typeof m.ts === "number") {
              setRtt((performance.now() - m.ts).toFixed(1) + " ms");
            }
          } catch {
            // ignore
          }
        },
      },
    });
    connRef.current = connection;
    void connection.start();

    const pingInterval = setInterval(() => {
      const conn = connRef.current;
      if (conn) {
        conn.sendControl(JSON.stringify({ kind: "ping", ts: performance.now() }));
      }
    }, 2000);

    const statsInterval = setInterval(async () => {
      setInputStats({
        sent: inputFramesSentRef.current,
        dropped: inputFramesDroppedRef.current,
      });
      inputFramesSentRef.current = 0;
      inputFramesDroppedRef.current = 0;

      const conn = connRef.current;
      if (!conn) return;
      const pc = conn.peerConnection;
      if (!pc || pc.connectionState !== "connected") return;
      try {
        const stats = await pc.getStats();
        let inb: any = null, codec: any = null, ainb: any = null, acodec: any = null;
        stats.forEach((s) => {
          if (s.type === "inbound-rtp" && s.kind === "video") inb = s;
          if (s.type === "inbound-rtp" && s.kind === "audio") ainb = s;
        });
        if (inb && inb.codecId) codec = stats.get(inb.codecId);
        if (inb) {
          const fmt = codec?.sdpFmtpLine?.match(/profile-level-id=([0-9a-fA-F]{6})/)?.[1];
          // Browser-side latency: average jitter-buffer delay and decode time
          // (cumulative counters → per-frame average). These are the streaming-
          // added latency the host pipeline can't see; the dominant chunk vs a
          // local capture preview.
          const jbCount = inb.jitterBufferEmittedCount ?? 0;
          const jbDelay = inb.jitterBufferDelay ?? 0;
          const decFrames = inb.framesDecoded ?? 0;
          const decTime = inb.totalDecodeTime ?? 0;
          const prev = statsPrevRef.current;
          const dCount = jbCount - prev.jbCount;
          const dFrames = decFrames - prev.decFrames;
          // Per-interval (current) averages; fall back to cumulative on first sample.
          const jbMs =
            dCount > 0 ? ((jbDelay - prev.jbDelay) / dCount) * 1000
            : jbCount > 0 ? (jbDelay / jbCount) * 1000 : 0;
          const decMs =
            dFrames > 0 ? ((decTime - prev.decTime) / dFrames) * 1000
            : decFrames > 0 ? (decTime / decFrames) * 1000 : 0;
          statsPrevRef.current = { jbDelay, jbCount, decTime, decFrames };
          setVideoStats(
            `${codec?.mimeType || "?"} ${fmt || ""} | ${inb.framesPerSecond ?? 0} fps | ` +
              `jitter ${jbMs.toFixed(1)}ms | decode ${decMs.toFixed(1)}ms`
          );
        }
        if (ainb && ainb.codecId) acodec = stats.get(ainb.codecId);
        if (ainb) {
          setAudioStats(
            `${acodec?.mimeType || "?"} | pkts ${ainb.packetsReceived ?? 0} | ${Math.round(
              (ainb.bytesReceived ?? 0) / 1024
            )} KB`
          );
        }
      } catch {
        // ignore transient getStats errors
      }
    }, 1000);

    return () => {
      clearInterval(pingInterval);
      clearInterval(statsInterval);
      connection.close();
      connRef.current = null;
    };
  }, [roomId, handleControlEvent]);

  // Authorize on the live connection when a token is redeemed.
  useEffect(() => {
    if (session) connRef.current?.sendAuth(session.token);
  }, [session]);

  // Event-driven input sends happen from GamepadState.onChange. This RAF loop
  // is only the fallback: poll physical gamepads, refresh held states at a lower
  // cadence, and resend neutral a few times so releases survive packet loss.
  useEffect(() => {
    let raf = 0;
    let lastActiveRefresh = 0;
    let lastNeutralResend = 0;
    const loop = (t: number) => {
      raf = requestAnimationFrame(loop);
      if (!grantedRef.current) return;
      const conn = connRef.current;
      if (!conn || !conn.inputReady) return;

      const state = currentInputState();
      const lastSent = lastSentInputRef.current;
      if (!lastSent || !sameInputState(lastSent, state)) {
        sendNowRef.current();
        if (!isNeutralInput(state)) lastActiveRefresh = t;
        return;
      }

      if (!isNeutralInput(state)) {
        if (t - lastActiveRefresh >= ACTIVE_INPUT_REFRESH_MS) {
          sendNowRef.current(true);
          lastActiveRefresh = t;
        }
        return;
      }

      if (neutralResendsRemainingRef.current > 0 && t - lastNeutralResend >= ACTIVE_INPUT_REFRESH_MS) {
        if (sendNowRef.current(true)) {
          neutralResendsRemainingRef.current--;
          lastNeutralResend = t;
        }
      }
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, [currentInputState]);

  // Physical gamepad connect/disconnect indicator.
  const [padName, setPadName] = useState<string | null>(null);
  useEffect(() => {
    const refresh = () => {
      const pads = navigator.getGamepads?.() ?? [];
      const gp = pads.find((p) => p?.connected) ?? null;
      setPadName(gp ? gp.id.slice(0, 32) : null);
      dlog("page", gp ? `gamepad connected: ${gp.id}` : "gamepad disconnected");
    };
    window.addEventListener("gamepadconnected", refresh);
    window.addEventListener("gamepaddisconnected", refresh);
    return () => {
      window.removeEventListener("gamepadconnected", refresh);
      window.removeEventListener("gamepaddisconnected", refresh);
    };
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
    const blur = () => {
      gamepadRef.current.reset();
      sendNowRef.current(true);
      onChange();
    };
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
        {padName && <span className="muted">🎮 {padName}</span>}
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

      <div className="stats-bar mono" style={{
        display: "flex",
        gap: 16,
        padding: "6px 12px",
        background: "var(--panel-2)",
        borderBottom: "1px solid var(--border)",
        fontSize: 12,
        color: "var(--muted)",
        flexWrap: "wrap",
        alignItems: "center"
      }}>
        <span>RTT: <strong style={{ color: "var(--text)" }}>{rtt}</strong></span>
        <span>Video: <strong style={{ color: "var(--text)" }}>{videoStats}</strong></span>
        <span>Audio: <strong style={{ color: "var(--text)" }}>{audioStats}</strong></span>
        <span>Input: <strong style={{ color: "var(--text)" }}>{inputStats.sent} sent / {inputStats.dropped} dropped</strong></span>
        <div style={{ marginLeft: "auto", display: "flex", gap: 12 }}>
          <label style={{ display: "flex", alignItems: "center", gap: 4, cursor: "pointer", userSelect: "none" }}>
            <input
              type="checkbox"
              checked={showGamepad}
              onChange={(e) => setShowGamepad(e.target.checked)}
              style={{ margin: 0, width: "auto", height: "auto", padding: 0 }}
            />
            <span>虛擬手把</span>
          </label>
          <label style={{ display: "flex", alignItems: "center", gap: 4, cursor: "pointer", userSelect: "none" }}>
            <input
              type="checkbox"
              checked={showLogs}
              onChange={(e) => setShowLogs(e.target.checked)}
              style={{ margin: 0, width: "auto", height: "auto", padding: 0 }}
            />
            <span>偵錯日誌</span>
          </label>
        </div>
      </div>

      <div className="video-wrap">
        <video ref={videoRef} playsInline autoPlay muted />
        {conn !== "connected" && (
          <div className="overlay-status">
            <div className="status-pill">{CONN_LABEL[conn]}</div>
            {connDetail && <div className="muted">{connDetail}</div>}
          </div>
        )}
        {conn === "connected" && (
          <button
            className="btn-primary"
            style={{ position: "absolute", right: 12, bottom: 12, zIndex: 2 }}
            onClick={() => {
              const v = videoRef.current;
              if (!v) return;
              v.muted = !v.muted;
              setAudioMuted(v.muted);
            }}
          >
            {audioMuted ? "🔇 開啟聲音" : "🔊 靜音"}
          </button>
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

      {showGamepad && (
        <VirtualGamepad state={gamepadRef.current} enabled={granted} onChange={onChange} />
      )}

      {showLogs && (
        <div
          ref={logBoxRef}
          className="mono"
          style={{
            margin: 12,
            padding: 8,
            maxHeight: 220,
            overflowY: "auto",
            fontSize: 12,
            whiteSpace: "pre-wrap",
            background: "rgba(0,0,0,.35)",
            borderRadius: 6,
          }}
        >
          {logs.map((l, i) => (
            <div key={i}>
              [{l.ts}] {l.tag}: {l.text}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function isNeutralInput(s: InputState): boolean {
  return s.buttons === 0 && s.lx === 0 && s.ly === 0 && s.rx === 0 && s.ry === 0;
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
