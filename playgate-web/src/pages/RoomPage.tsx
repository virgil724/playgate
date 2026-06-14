import { useCallback, useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { ApiClient, API_BASE_URL, SIGNALING_BASE_URL, ApiError } from "../lib/api";
import { SignalingClient } from "../lib/signaling";
import { ViewerConnection, type ConnectionState } from "../lib/webrtc";
import { GamepadState } from "../lib/gamepad-state";
import { encodeInput } from "../lib/input-codec";
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

  const onChange = useCallback(() => forceRender((n) => n + 1), []);

  const handleControlEvent = useCallback((ev: ControlEvent) => {
    setStatusMsg(describeEvent(ev));
    const viewerId = sessionRef.current?.viewerId ?? "";
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
      setQueuePos(null);
      gamepadRef.current.reset();
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
          setVideoStats(
            `${codec?.mimeType || "?"} ${fmt || ""} | ${inb.framesPerSecond ?? 0} fps | dec ${
              inb.framesDecoded ?? 0
            } (key ${inb.keyFramesDecoded ?? 0})`
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

  // 60 Hz input loop: sample keyboard/virtual state, merge in the physical
  // gamepad (Gamepad API), encode, send — only while granted.
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
      const state = mergeInput(gamepadRef.current.snapshot(), pollPhysicalGamepad());
      conn.sendInput(encodeInput(state));
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, []);

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
