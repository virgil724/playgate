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
}

export interface ViewerConnectionOptions {
  signaling: SignalingClient;
  callbacks?: WebRTCCallbacks;
  /** Extra payload merged into our first signaling message (e.g. session JWT). */
  authPayload?: Record<string, unknown>;
}

export class ViewerConnection {
  private signaling: SignalingClient;
  private cb: WebRTCCallbacks;
  private authPayload?: Record<string, unknown>;
  private pc: RTCPeerConnection | null = null;
  private inputChannel: RTCDataChannel | null = null;
  private remoteSet = false;
  private pendingCandidates: RTCIceCandidateInit[] = [];
  private sentAuth = false;

  constructor(opts: ViewerConnectionOptions) {
    this.signaling = opts.signaling;
    this.cb = opts.callbacks ?? {};
    this.authPayload = opts.authPayload;
  }

  private setState(s: ConnectionState, detail?: string) {
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

  async start(): Promise<void> {
    this.setState("fetching-ice");
    const iceServers = await this.signaling.fetchIceServers();

    const pc = new RTCPeerConnection({ iceServers });
    this.pc = pc;

    pc.ontrack = (ev) => {
      if (ev.streams[0]) this.cb.onTrack?.(ev.streams[0]);
    };

    pc.ondatachannel = (ev) => {
      const ch = ev.channel;
      if (ch.label === "control") this.attachControlChannel(ch);
      else if (ch.label === "input") this.attachInputChannel(ch);
    };

    pc.onicecandidate = (ev) => {
      if (ev.candidate) {
        // Push our candidate to the signaling server. Include auth on first msg.
        const payload = { ...ev.candidate.toJSON() } as Record<string, unknown>;
        void this.pushWithAuth(payload);
      }
    };

    pc.onconnectionstatechange = () => {
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

    // Begin polling for the host's offer + ICE.
    this.setState("waiting-offer");
    this.signaling.startPolling(
      (msg) => void this.handleSignal(msg),
      (err) => this.setState("connecting", `signaling: ${String(err)}`),
    );
  }

  private async pushWithAuth(payload: Record<string, unknown>): Promise<void> {
    let body = payload;
    if (!this.sentAuth && this.authPayload) {
      body = { ...payload, ...this.authPayload };
      this.sentAuth = true;
    }
    try {
      await this.signaling.push(body);
    } catch {
      // Non-fatal; ICE has redundancy.
    }
  }

  private async handleSignal(msg: SignalingMessage): Promise<void> {
    const kind = classifySignal(msg.payload);
    if (kind === "offer") {
      await this.handleOffer(msg.payload as RTCSessionDescriptionInit);
    } else if (kind === "candidate") {
      await this.addCandidate(msg.payload as RTCIceCandidateInit);
    }
    // answers are ours; ignore.
  }

  private async handleOffer(offer: RTCSessionDescriptionInit): Promise<void> {
    if (!this.pc || this.remoteSet) return;
    this.setState("connecting");
    await this.pc.setRemoteDescription(offer);
    this.remoteSet = true;

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
    // Include the session JWT (authPayload) with the answer so the host (T6)
    // can read it from the signaling message.
    await this.pushWithAuth({ type: answer.type, sdp: answer.sdp });
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
    ch.onopen = () => this.cb.onControlOpen?.();
    ch.onmessage = (ev) => {
      const event = parseControlEvent(ev.data);
      if (event) this.cb.onControlEvent?.(event);
    };
  }

  private attachInputChannel(ch: RTCDataChannel): void {
    this.inputChannel = ch;
    ch.binaryType = "arraybuffer";
    ch.onopen = () => this.cb.onInputOpen?.();
  }

  close(): void {
    this.signaling.stop();
    this.inputChannel?.close();
    this.pc?.close();
    this.pc = null;
    this.setState("closed");
  }
}
