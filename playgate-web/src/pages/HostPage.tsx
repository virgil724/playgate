import { useCallback, useEffect, useRef, useState } from "react";
import {
  ApiClient,
  API_BASE_URL,
  SIGNALING_BASE_URL,
  ApiError,
  type RoomStatus,
  type TokenInfo,
} from "../lib/api";
import { buildHostConfig } from "../lib/config-template";

const LS_KEY = "playgate.host.apiKey";

export function HostPage() {
  const [apiKey, setApiKey] = useState<string>(() => localStorage.getItem(LS_KEY) ?? "");
  const [keyInput, setKeyInput] = useState("");
  const [authed, setAuthed] = useState(false);
  const [error, setError] = useState("");
  const apiRef = useRef(new ApiClient(API_BASE_URL));

  const [rooms, setRooms] = useState<RoomStatus[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [tokens, setTokens] = useState<TokenInfo[]>([]);
  const [newRoomName, setNewRoomName] = useState("");
  const [newRoomSecs, setNewRoomSecs] = useState(60);
  const [issueCount, setIssueCount] = useState(5);
  const [issuedCodes, setIssuedCodes] = useState<string[]>([]);
  const [registerName, setRegisterName] = useState("");
  const [hostConfigYaml, setHostConfigYaml] = useState("");
  const [configLoading, setConfigLoading] = useState(false);

  const selectedRoom = rooms.find((r) => r.id === selected) ?? null;

  // A generated config is room-specific — drop it when the selection changes.
  useEffect(() => {
    setHostConfigYaml("");
  }, [selected]);

  const refreshRooms = useCallback(async () => {
    try {
      const { rooms } = await apiRef.current.listRooms();
      setRooms(rooms);
      setAuthed(true);
      setError("");
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setAuthed(false);
        setError("Invalid API key");
      } else {
        setError(err instanceof Error ? err.message : "Failed to load rooms");
      }
    }
  }, []);

  const refreshTokens = useCallback(async (roomId: string) => {
    try {
      const { tokens } = await apiRef.current.listTokens(roomId);
      setTokens(tokens);
    } catch {
      setTokens([]);
    }
  }, []);

  // Apply API key + initial load.
  useEffect(() => {
    apiRef.current.setApiKey(apiKey || undefined);
    if (apiKey) void refreshRooms();
  }, [apiKey, refreshRooms]);

  // Poll selected room status + tokens every 3s.
  useEffect(() => {
    if (!authed || !selected) return;
    void refreshTokens(selected);
    const id = setInterval(() => {
      void refreshRooms();
      void refreshTokens(selected);
    }, 3000);
    return () => clearInterval(id);
  }, [authed, selected, refreshRooms, refreshTokens]);

  const login = () => {
    const k = keyInput.trim();
    if (!k) return;
    localStorage.setItem(LS_KEY, k);
    setApiKey(k);
  };

  const logout = () => {
    localStorage.removeItem(LS_KEY);
    setApiKey("");
    setAuthed(false);
    setRooms([]);
    setSelected(null);
  };

  const register = async () => {
    if (!registerName.trim()) return;
    try {
      const { api_key } = await apiRef.current.registerHost(registerName.trim());
      localStorage.setItem(LS_KEY, api_key);
      setApiKey(api_key);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Register failed");
    }
  };

  const createRoom = async () => {
    if (!newRoomName.trim()) return;
    try {
      const room = await apiRef.current.createRoom(newRoomName.trim(), newRoomSecs);
      setNewRoomName("");
      await refreshRooms();
      setSelected(room.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Create room failed");
    }
  };

  const issue = async () => {
    if (!selected) return;
    try {
      const { codes } = await apiRef.current.issueTokens(selected, issueCount);
      setIssuedCodes(codes);
      await refreshTokens(selected);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Issue failed");
    }
  };

  const revoke = async (code: string) => {
    try {
      await apiRef.current.revokeToken(code);
      if (selected) await refreshTokens(selected);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Revoke failed");
    }
  };

  const kick = async () => {
    if (!selected) return;
    try {
      await apiRef.current.kick(selected);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Kick failed");
    }
  };

  const generateConfig = async () => {
    if (!selected) return;
    setConfigLoading(true);
    try {
      const { public_key } = await apiRef.current.publicKey();
      const yaml = buildHostConfig({
        roomId: selected,
        signalingUrl: SIGNALING_BASE_URL,
        serverUrl: API_BASE_URL,
        apiKey,
        publicKeyBase64: public_key,
      });
      setHostConfigYaml(yaml);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to fetch public key");
    } finally {
      setConfigLoading(false);
    }
  };

  const downloadConfig = (yaml: string) => {
    const blob = new Blob([yaml], { type: "text/yaml" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "config.yaml";
    a.click();
    URL.revokeObjectURL(url);
  };

  const copy = (text: string) => {
    void navigator.clipboard?.writeText(text);
  };

  // ---- Not logged in ----
  if (!authed) {
    return (
      <div className="host">
        <h1>PlayGate — Streamer console</h1>
        <div className="panel">
          <strong>Sign in with your API key</strong>
          <div className="row" style={{ marginTop: 8 }}>
            <input
              placeholder="host API key"
              value={keyInput}
              onChange={(e) => setKeyInput(e.target.value)}
              className="mono"
              style={{ flex: 1, minWidth: 240 }}
            />
            <button className="btn-primary" onClick={login}>
              Sign in
            </button>
          </div>
          {error && <div className="error" style={{ marginTop: 8 }}>{error}</div>}
        </div>
        <div className="panel">
          <strong>New here? Register a host</strong>
          <div className="row" style={{ marginTop: 8 }}>
            <input
              placeholder="channel name"
              value={registerName}
              onChange={(e) => setRegisterName(e.target.value)}
            />
            <button onClick={() => void register()}>Register</button>
          </div>
          <div className="muted" style={{ marginTop: 6, fontSize: 13 }}>
            Your API key will be saved in this browser's localStorage.
          </div>
        </div>
      </div>
    );
  }

  // ---- Logged in ----
  return (
    <div className="host">
      <div className="row">
        <h1 style={{ flex: 1 }}>Streamer console</h1>
        <button onClick={() => void refreshRooms()}>Refresh</button>
        <button onClick={logout}>Sign out</button>
      </div>
      {error && <div className="error">{error}</div>}

      <div className="panel">
        <strong>Rooms</strong>
        <div className="grid-cards" style={{ marginTop: 10 }}>
          {rooms.map((r) => (
            <div
              key={r.id}
              className={`card ${selected === r.id ? "selected" : ""}`}
              onClick={() => setSelected(r.id)}
            >
              <div style={{ fontWeight: 700 }}>{r.name}</div>
              <div className="muted mono" style={{ fontSize: 12 }}>{r.id}</div>
              <div className="row" style={{ marginTop: 6, fontSize: 13 }}>
                <span className={`dot ${r.online ? "ok" : "bad"}`} />
                <span>{r.online ? "online" : "offline"}</span>
                <span className="muted">· queue {r.queue_depth}</span>
              </div>
              <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                controller: {r.current_viewer ? r.current_viewer.slice(0, 8) : "none"}
              </div>
            </div>
          ))}
          {rooms.length === 0 && <div className="muted">No rooms yet.</div>}
        </div>
        <div className="row" style={{ marginTop: 12 }}>
          <input
            placeholder="new room name"
            value={newRoomName}
            onChange={(e) => setNewRoomName(e.target.value)}
          />
          <label className="muted">
            session
            <input
              type="number"
              min={1}
              value={newRoomSecs}
              onChange={(e) => setNewRoomSecs(Number(e.target.value) || 60)}
              style={{ width: 80, marginLeft: 6 }}
            />
            s
          </label>
          <button className="btn-primary" onClick={() => void createRoom()}>
            Create room
          </button>
        </div>
      </div>

      {selectedRoom && (
        <>
          <div className="panel">
            <div className="row">
              <strong style={{ flex: 1 }}>
                {selectedRoom.name}{" "}
                <span className="muted mono" style={{ fontWeight: 400 }}>
                  {selectedRoom.id}
                </span>
              </strong>
              <a href={`/room/${selectedRoom.id}`} target="_blank" rel="noreferrer">
                open viewer ↗
              </a>
            </div>
            <div className="row" style={{ marginTop: 8 }}>
              <span className={`dot ${selectedRoom.online ? "ok" : "bad"}`} />
              <span>{selectedRoom.online ? "Host online" : "Host offline"}</span>
              <span className="muted">
                · current controller:{" "}
                {selectedRoom.current_viewer ? (
                  <span className="mono">{selectedRoom.current_viewer.slice(0, 12)}</span>
                ) : (
                  "none"
                )}
              </span>
              <span className="muted">· queue depth {selectedRoom.queue_depth}</span>
              <button
                className="btn-danger"
                style={{ marginLeft: "auto" }}
                onClick={() => void kick()}
                disabled={!selectedRoom.current_viewer}
              >
                Force kick
              </button>
            </div>
          </div>

          <div className="panel">
            <div className="row">
              <strong style={{ flex: 1 }}>Tokens</strong>
              <label className="muted">
                count
                <input
                  type="number"
                  min={1}
                  max={100}
                  value={issueCount}
                  onChange={(e) => setIssueCount(Number(e.target.value) || 1)}
                  style={{ width: 70, marginLeft: 6 }}
                />
              </label>
              <button className="btn-primary" onClick={() => void issue()}>
                Generate codes
              </button>
            </div>

            {issuedCodes.length > 0 && (
              <div style={{ marginTop: 10 }}>
                <div className="row">
                  <span className="muted">Just generated — copy & distribute:</span>
                  <button onClick={() => copy(issuedCodes.join("\n"))}>Copy all</button>
                </div>
                <textarea readOnly rows={Math.min(issuedCodes.length, 8)} value={issuedCodes.join("\n")} />
              </div>
            )}

            <table className="tokens" style={{ marginTop: 12 }}>
              <thead>
                <tr>
                  <th>Code</th>
                  <th>Status</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {tokens.map((t) => (
                  <tr key={t.code}>
                    <td className="mono">{t.code}</td>
                    <td>
                      <span className={`badge ${t.status}`}>{t.status}</span>
                    </td>
                    <td style={{ textAlign: "right" }}>
                      <button onClick={() => copy(t.code)}>Copy</button>{" "}
                      <button
                        className="btn-danger"
                        onClick={() => void revoke(t.code)}
                        disabled={t.redeemed || t.revoked}
                      >
                        Revoke
                      </button>
                    </td>
                  </tr>
                ))}
                {tokens.length === 0 && (
                  <tr>
                    <td colSpan={3} className="muted">
                      No tokens issued yet.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
          <div className="panel">
            <div className="row">
              <strong style={{ flex: 1 }}>Host config</strong>
              <button
                className="btn-primary"
                onClick={() => void generateConfig()}
                disabled={configLoading}
              >
                {configLoading ? "Fetching…" : "Generate config.yaml"}
              </button>
            </div>

            {hostConfigYaml && (
              <div style={{ marginTop: 10 }}>
                <textarea
                  readOnly
                  rows={14}
                  className="mono"
                  value={hostConfigYaml}
                  style={{ width: "100%" }}
                />
                <div className="row" style={{ marginTop: 6 }}>
                  <button onClick={() => copy(hostConfigYaml)}>Copy</button>
                  <button onClick={() => downloadConfig(hostConfigYaml)}>Download</button>
                </div>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}
