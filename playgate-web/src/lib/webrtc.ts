/**
 * webrtc.ts — viewer-side WebRTC orchestration.
 *
 * Flow (viewer):
 *   1. fetch iceServers (TURN with STUN fallback)
 *   2. create RTCPeerConnection, attach ontrack -> <video>
 *   3. poll signaling for the host's SDP offer
 *   4. setRemoteDescription(offer) -> createAnswer -> push answer
 *   5. exchange ICE candidates (push ours, apply host's)
 *   6. receive "control" DataChannel (host-created) and "input" DataChannel
 *
 * Note: the host creates the data channels (it builds the offer), so the viewer
 * receives them via ondatachannel. The viewer encodes input at 60 Hz and writes
 * to the "input" channel only while granted.
 *
 * Framework-free. No React imports.
 */

import { SignalingClient, classifySignal, type SignalingMessage } from "./signaling";
import { type ControlEvent, parseControlEvent } from "./control-events";
import { dlog } from "./log";

export type ConnectionState =
  | "idle"
  | "fetching-ice"
  | "waiting-offer"
  | "connecting"
  | "connected"
  | "failed"
  | "closed";

export interface WebRTCCallbacks {
  onState?: (state: ConnectionState, detail?: string) => void;
  onTrack?: (stream: MediaStream) => void;
  onControlEvent?: (event: ControlEvent) => void;
  onControlOpen?: () => void;
  onInputOpen?: () => void;
  onControlMessage?: (data: string) => void;
}

export interface ViewerConnectionOptions {
  signaling: SignalingClient;
  callbacks?: WebRTCCallbacks;
  /** Extra payload merged into our first signaling message (e.g. session JWT). */
  authPayload?: Record<string, unknown>;
}

/** Resolves when ICE gathering completes, bounded so a stuck STUN lookup
 * cannot stall the answer forever (the host gives up after 60s). */
function waitForGathering(pc: RTCPeerConnection, timeoutMs = 3000): Promise<void> {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(done, timeoutMs);
    function done() {
      clearTimeout(timer);
      pc.removeEventListener("icegatheringstatechange", check);
      resolve();
    }
    function check() {
      if (pc.iceGatheringState === "complete") done();
    }
    pc.addEventListener("icegatheringstatechange", check);
  });
}

export class ViewerConnection {
  private signaling: SignalingClient;
  private cb: WebRTCCallbacks;
  private authPayload?: Record<string, unknown>;
  private pc: RTCPeerConnection | null = null;
  private inputChannel: RTCDataChannel | null = null;
  private controlChannel: RTCDataChannel | null = null;
  private remoteSet = false;
  private pendingCandidates: RTCIceCandidateInit[] = [];
  private sentAuth = false;
  private iceServers: RTCIceServer[] = [];
  /** Worker-assigned ts of the offer we answered; newer offers mean the host
   * recycled its peer and our connection is dead — we restart on them. */
  private appliedOfferTs: string | null = null;
  /** Set by close(); guards the async start() so a connection closed while
   * start() is still awaiting (React StrictMode double-mount) cannot
   * resurrect itself and keep polling/answering as a zombie. */
  private closed = false;

  constructor(opts: ViewerConnectionOptions) {
    this.signaling = opts.signaling;
    this.cb = opts.callbacks ?? {};
    this.authPayload = opts.authPayload;
  }

  private setState(s: ConnectionState, detail?: string) {
    dlog("webrtc", `state → ${s}`, detail ?? "");
    this.cb.onState?.(s, detail);
  }

  /** True once the host's "input" DataChannel is open. */
  get inputReady(): boolean {
    return this.inputChannel?.readyState === "open";
  }

  /** Send a pre-encoded 13-byte input frame. No-op if channel not open. */
  sendInput(frame: ArrayBuffer): void {
    if (this.inputChannel?.readyState === "open") {
      this.inputChannel.send(frame);
    }
  }

  get peerConnection(): RTCPeerConnection | null {
    return this.pc;
  }

  sendControl(msg: string): void {
    if (this.controlChannel?.readyState === "open") {
      this.controlChannel.send(msg);
    }
  }

  /** Authorize on the live connection (e.g. after redeeming a token code).
   * Sends the auth message immediately when the control channel is open;
   * otherwise it is sent on the next control-channel open. */
  sendAuth(token: string): void {
    this.authPayload = { ...this.authPayload, token };
    if (this.controlChannel?.readyState === "open") {
      try {
        this.controlChannel.send(JSON.stringify({ kind: "auth", token }));
        dlog("webrtc", "auth sent on live control channel");
      } catch (err) {
        dlog("webrtc", "sendAuth failed:", err);
      }
    } else {
      dlog("webrtc", "sendAuth: control channel not open yet; will auth on open");
    }
  }

  async start(): Promise<void> {
    this.setState("fetching-ice");
    const iceServers = await this.signaling.fetchIceServers();
    if (this.closed) {
      dlog("webrtc", "closed during ICE fetch; aborting start");
      return;
    }
    const hasTurn = iceServers.some((s) =>
      ([] as string[]).concat(s.urls as string | string[]).some((u) => u.startsWith("turn")),
    );
    dlog("webrtc", `ice servers: ${iceServers.length} (turn: ${hasTurn})`);
    this.iceServers = iceServers;
    this.createPeer();

    // Begin polling for the host's offer + ICE.
    this.setState("waiting-offer");
    this.signaling.startPolling(
      (msg) => void this.handleSignal(msg),
      (err) => this.setState("connecting", `signaling: ${String(err)}`),
    );

    // Announce ourselves so the host sends us a (per-viewer) offer. The host no
    // longer broadcasts a single offer to the room; it offers per viewerId.
    void this.signaling.hello().catch((err) => dlog("webrtc", "hello failed:", err));
  }

  /** (Re)build the RTCPeerConnection. Called once from start() and again when
   * a newer offer arrives (host recycled its peer; the old pc is dead). */
  private createPeer(): void {
    const pc = new RTCPeerConnection({ iceServers: this.iceServers });
    this.pc = pc;
    this.remoteSet = false;
    this.sentAuth = false;
    this.inputChannel = null;
    this.controlChannel = null;
    this.pendingCandidates = [];

    pc.ontrack = (ev) => {
      dlog("webrtc", `ontrack kind=${ev.track.kind} streams=${ev.streams.length}`);
      // Minimise the receiver's jitter/playout buffer for interactive latency.
      // jitterBufferTarget is the standardised knob; playoutDelayHint is the
      // older Chrome equivalent. Both are best-effort and may be unsupported.
      try {
        const r = ev.receiver as RTCRtpReceiver & {
          jitterBufferTarget?: number | null;
          playoutDelayHint?: number | null;
        };
        r.jitterBufferTarget = 0;
        r.playoutDelayHint = 0;
      } catch {
        /* unsupported: ignore */
      }
      if (ev.streams[0]) this.cb.onTrack?.(ev.streams[0]);
    };

    pc.ondatachannel = (ev) => {
      const ch = ev.channel;
      dlog("webrtc", `ondatachannel label=${ch.label}`);
      if (ch.label === "control") this.attachControlChannel(ch);
      else if (ch.label === "input") this.attachInputChannel(ch);
    };

    pc.oniceconnectionstatechange = () => {
      dlog("webrtc", `iceConnectionState → ${pc.iceConnectionState}`);
    };
    pc.onicegatheringstatechange = () => {
      dlog("webrtc", `iceGatheringState → ${pc.iceGatheringState}`);
    };

    pc.onicecandidate = (ev) => {
      if (ev.candidate) {
        dlog("webrtc", `local candidate: ${ev.candidate.candidate}`);
        // Push our candidate to the signaling server. Include auth on first msg.
        const payload = { ...ev.candidate.toJSON() } as Record<string, unknown>;
        void this.pushWithAuth(payload);
      }
    };

    pc.onconnectionstatechange = () => {
      dlog("webrtc", `connectionState → ${pc.connectionState}`);
      switch (pc.connectionState) {
        case "connected":
          this.setState("connected");
          break;
        case "failed":
          this.setState("failed", "peer connection failed");
          break;
        case "disconnected":
          this.setState("connecting", "disconnected, retrying");
          break;
        case "closed":
          this.setState("closed");
          break;
      }
    };
  }

  private async pushWithAuth(payload: Record<string, unknown>): Promise<void> {
    let body = payload;
    if (!this.sentAuth && this.authPayload) {
      body = { ...payload, ...this.authPayload };
      this.sentAuth = true;
    }
    try {
      await this.signaling.push(body);
    } catch (err) {
      // Non-fatal for candidates (ICE has redundancy), but always surface it:
      // a swallowed answer push means the host never connects.
      dlog("webrtc", "push failed:", err);
    }
  }

  private async handleSignal(msg: SignalingMessage): Promise<void> {
    const kind = classifySignal(msg.payload);
    dlog("webrtc", `signal seq=${msg.seq} ts=${msg.ts} kind=${kind}`);
    try {
      if (kind === "offer") {
        // The host queue is shared by all viewers; ignore offers addressed to a
        // different viewerId (an offer with no `to` is a legacy 1:1 broadcast).
        const to = (msg.payload as { to?: string }).to;
        if (to && to !== this.signaling.viewerId) {
          dlog("webrtc", `ignoring offer for other viewer ${to}`);
          return;
        }
        await this.handleOffer(msg.payload as RTCSessionDescriptionInit, msg.ts);
      } else if (kind === "candidate") {
        await this.addCandidate(msg.payload as RTCIceCandidateInit);
      }
      // answers are ours; ignore.
    } catch (err) {
      dlog("webrtc", `handling ${kind} failed:`, err);
      this.setState("failed", `handling ${kind}: ${String(err)}`);
    }
  }

  private async handleOffer(offer: RTCSessionDescriptionInit, ts?: string): Promise<void> {
    if (!this.pc) return;
    if (this.remoteSet) {
      // Both timestamps come from the Worker's clock, so a lexicographic
      // ISO-8601 comparison is safe (same trick as the host's stale-answer
      // check). An offer NEWER than the one we answered means the host
      // recycled its peer — our current pc is doomed; restart on it. An
      // older/equal one is a stale leftover; ignore.
      if (!ts || !this.appliedOfferTs || ts <= this.appliedOfferTs) {
        dlog("webrtc", `stale offer ignored (ts=${ts ?? "?"} applied=${this.appliedOfferTs ?? "?"})`);
        return;
      }
      dlog("webrtc", "newer offer received — host recycled; restarting peer connection");
      this.pc.close();
      this.createPeer();
    }
    this.setState("connecting");
    await this.pc.setRemoteDescription(offer);
    this.remoteSet = true;
    this.appliedOfferTs = ts ?? null;
    dlog("webrtc", "remote offer applied");

    // Flush any candidates that arrived before the remote description.
    for (const c of this.pendingCandidates) {
      try {
        await this.pc.addIceCandidate(c);
      } catch {
        /* ignore */
      }
    }
    this.pendingCandidates = [];

    const answer = await this.pc.createAnswer();
    await this.pc.setLocalDescription(answer);
    // The host stops polling once it has applied our answer (non-trickle), so
    // wait for ICE gathering to finish and send the complete SDP.
    const t0 = performance.now();
    await waitForGathering(this.pc);
    dlog("webrtc", `ice gathering: ${this.pc.iceGatheringState} after ${Math.round(performance.now() - t0)}ms`);
    const local = this.pc.localDescription ?? answer;
    await this.pushWithAuth({ type: local.type, sdp: local.sdp });
    dlog("webrtc", `answer pushed (${local.sdp?.length ?? 0} bytes)`);
  }

  private async addCandidate(cand: RTCIceCandidateInit): Promise<void> {
    if (!this.pc) return;
    if (!this.remoteSet) {
      this.pendingCandidates.push(cand);
      return;
    }
    try {
      await this.pc.addIceCandidate(cand);
    } catch {
      /* ignore malformed candidate */
    }
  }

  private attachControlChannel(ch: RTCDataChannel): void {
    this.controlChannel = ch;
    ch.onopen = () => {
      dlog("webrtc", "control channel open");
      // The host's session gate authorizes viewers via a control-channel auth
      // message ({"kind":"auth","token":...}), not via signaling payloads.
      const token = this.authPayload?.token;
      if (typeof token === "string" && token !== "") {
        try {
          ch.send(JSON.stringify({ kind: "auth", token }));
          dlog("webrtc", "auth sent on control channel");
        } catch {
          /* channel closed before send; host will never grant control */
          dlog("webrtc", "auth send failed: control channel closed");
        }
      }
      this.cb.onControlOpen?.();
    };
    ch.onmessage = (ev) => {
      const dataStr = String(ev.data);
      dlog("control", dataStr);
      this.cb.onControlMessage?.(dataStr);
      const event = parseControlEvent(dataStr);
      if (event) this.cb.onControlEvent?.(event);
    };
  }

  private attachInputChannel(ch: RTCDataChannel): void {
    this.inputChannel = ch;
    ch.binaryType = "arraybuffer";
    ch.onopen = () => {
      dlog("webrtc", "input channel open");
      this.cb.onInputOpen?.();
    };
  }

  close(): void {
    dlog("webrtc", "close()");
    this.closed = true;
    this.signaling.stop();
    this.inputChannel?.close();
    this.controlChannel?.close();
    this.pc?.close();
    this.pc = null;
    this.setState("closed");
  }
}
