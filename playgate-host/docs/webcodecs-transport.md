# WebCodecs video transport over an unreliable DataChannel

Status: design (T10). Implemented by T11 (host) and T12 (web); FEC (section 8) by T13.
All integers are **big-endian** unless stated otherwise.

## 0. Terminology warning: "delivery mode"

The existing "delivery mode" (commits `69860f3`, `f54c14e`; `playgate-twitch-bot/src/config.ts`,
admin page) selects how the **Twitch bot hands a token code to a viewer**
(`whisper | chat | dual`). It has nothing to do with video. There is **no existing
video-delivery mode** in `playgate-host` or `playgate-web`. This document therefore
introduces a new, independent setting (section 6), and T12 should not put the video
option in the twitch-bot admin selector. Today video is always one RTP H.264 track
(`TrackLocalStaticSample`, `rtc/peer.go` `setupVideoTrack`) rendered into a `<video>`
element (`RoomPage.tsx`: `videoRef.current.srcObject = stream`).

## 1. Goals / non-goals

Goals
- Recover from packet loss in about one RTT plus one IDR, instead of waiting for the next GOP.
  PLI is currently not wired (`host/connect.go:272`) and the encoder is the ffmpeg CLI.
- Give the viewer full control of decode, drop and render policy (no browser jitter buffer).
- Allow application-level FEC later (T13) with a layout that is fixed now.
- Reuse the existing signaling, ICE and DTLS/SCTP stack. No new server, no WebSocket media.

Non-goals
- Audio (stays on the Opus RTP track).
- Replacing the RTP track path. It remains the default and the fallback.
- HEVC/AV1 (the `codec` field is reserved; only H.264 Annex-B is defined).
- Congestion control. T8/T9 (GCC) work on the RTP path; for DataChannel mode, bitrate
  adaptation uses the feedback in section 4 (open question 1).
- Reliability. A frame with a missing fragment is dropped, never retransmitted.

## 2. Research: how moonlight-web does it

Repository: `https://github.com/MrCreativ3001/moonlight-web-stream` (Rust server in `src/`,
TypeScript client in `web/`). Found by search; the similarly named `linckosz/moonlight-web`
is a different (C++/Qt) project and was not read. Source was read via the GitHub contents
API on the default branch at the time of this task (2026-11-18); no commit SHA was pinned.

Key findings (paths relative to the repo root):

1. **Two transports.** `web/stream/transport/webrtc.ts` and `web/stream/transport/web_socket.ts`.
   README: WebSocket is the fallback when WebRTC/ICE fails ("Data Transport" option).
2. **Over WebRTC, video is a normal RTP track, not a DataChannel.** `webrtc.ts` does
   `addTransceiver("video", {direction:"recvonly"})` and picks the `ontrack` stream
   (lines ~50, 236-260); the server RTP-packetizes H.264/H.265/AV1 itself with
   `H264Payloader` at `RTP_OUTBOUND_MTU` (`src/api/stream/webrtc/video.rs` ~142-250).
   DataChannels are only used for **input**, e.g. `moonlight.control.controller` with
   `ordered:false, maxRetransmits:0` (`webrtc.ts` ~457-483). Keyframe request on this path is
   RTCP PLI: `video.rs` ~343-351 turns a PLI into `VideoChannelEvent::SignalIdr`.
   **Consequence: moonlight-web is not a precedent for video over an unreliable
   DataChannel.** What we take from it is the WebCodecs half and the IDR/backpressure logic.
3. **WebCodecs path only runs over WebSocket.** `web_socket.ts` ~120-125: a binary WS message is
   `[channel:u8][payload]`; channel VIDEO goes to `videoPipeline.submitData(view.subarray(1))`.
4. **Frame framing (WebSocket path).** Server `src/api/stream/web_socket/mod.rs` ~398-407 builds
   `[0x(VIDEO)][frame_type:u8 (1=IDR,0=delta)][timestamp_us:u32 BE][Annex-B access unit]`.
   Client `web/stream/video/depackitize_pipe.ts` reads `frameType`, `u32 timestamp`, rest is data,
   `duration = max(0, ts - lastTs)` (clamped because the u32 wraps at ~71 min). **No
   fragmentation exists**: WebSocket/TCP delivers whole frames. So there is nothing to copy for
   fragmentation, reassembly or loss handling; section 3 is original design.
5. **Decoder config.** `web/stream/video/video_decoder_pipe.ts`: first tries an in-band codec
   string (`avc3.42E01E`, no `description`) and feeds Annex-B straight to `EncodedVideoChunk`
   (the non-translator branch of `submitDecodeUnit`); config tries
   `hardwareAcceleration:"prefer-hardware"` + `optimizeForLatency:true`, then without
   hardware, then plain. If in-band is unsupported it uses `H264StreamVideoTranslator`
   (`annex_b_translator.ts`): parse SPS (NAL 7) for the `avc1.PPCCLL` string, collect PPS (NAL 8),
   build an avcC `description` (`h264MakeAvcC`), rewrite Annex-B start codes to 4-byte length
   prefixes, drop SEI(6)/filler(12), and `decoder.reset()+configure()` when a new SPS/PPS arrives.
   Delta frames before the first keyframe are discarded (`needsKeyFrame`).
6. **Client-driven IDR.** `video_decoder_pipe.ts` `pollRequestIdr()`: if
   `decodeQueueSize*1000/fps > 200 ms` and queue > 2, reset the decoder and ask for an IDR;
   `web_socket.ts` ~122-125 polls it after every video message and sends `ControlPacket.RequestIdr`.
7. **Server-side latency control (WebSocket only).** `web_socket/delay_controller.rs`: pings
   with u64 ids, rolling 10 s RTT window, `queue_delay = latest_rtt - min_rtt`; Congested
   >= 100 ms, Severe >= 500 ms. `web_socket/mod.rs` ~44-56, 297-400: when behind, the server
   **drops video until an IDR is sent** after the queue drained (resume threshold 50 ms, IDR
   request min interval 250 ms, rebase after 500 ms, 300 ms grace after resume).
   Take-away: drop-until-IDR plus a rate limit on IDR requests is the proven pattern.
8. **Render / pipeline choice.** `web/stream/video/pipeline.ts` `PIPELINES`: the array is an ordered
   list of candidate pipelines tested for support. Data pipelines: `DepacketizeVideoPipe ->
   VideoDecoderPipe -> VideoMediaStreamTrackGeneratorPipe -> <video>` (Chromium), `-> CanvasFrameDrawPipe
   -> MainCanvasRenderer` (default/Firefox), a Safari worker variant, then an
   OpenH264 (wasm) software decoder, then `MediaSource`. `canvasVsync=false` draws inside the decoder
   output callback with a `desynchronized` canvas context (lowest latency); `true` draws in rAF.
   README: `VideoDecoder` needs a secure context (HTTPS); otherwise it falls back to an older
   API with more latency.

Not verified (do not rely on these):
- No jitter buffer logic was found in the client WebCodecs path: frames are decoded on arrival.
  I did not read `canvas.ts`/`canvas_frame.ts` line by line.
- Exact SCTP/DataChannel behaviour of moonlight-web's RTC stack (it uses a Rust WebRTC
  library); only the input channels were inspected.
- Firefox/Safari acceptance of Annex-B without `description` is inferred from moonlight-web
  keeping a translator fallback; it was not tested here.
- The actual SCTP max message size negotiated by Pion v4.2.15 / pion/sctp v1.10.0 was not
  checked in source. The design below avoids depending on it (section 3.3).

## 3. Frame fragment wire format (channel label `video`)

### 3.1 Channel

Host (offerer) creates `video` with `ordered=false, maxRetransmits=0`, `binaryType`
`arraybuffer` on the viewer, exactly like `input` in `rtc/peer.go` `setupDataChannels`.
Label constant: `VideoChannelLabel = "video"`. One message = one fragment. Messages
flow host to viewer only.

### 3.2 Header (28 bytes, then payload)

| Off | Size | Field           | Notes |
|-----|------|-----------------|-------|
| 0   | 1    | `version`       | `1`. Receiver drops messages with an unknown version. |
| 1   | 1    | `flags`         | bit0 `KEY` (IDR access unit); bit1 `PARAM` (access unit starts with SPS+PPS; always true on IDR, required); bit2 `PARITY` (this fragment is FEC parity); bits3-7 reserved, send 0, ignore on receive. |
| 2   | 2    | `epoch`         | Increments whenever the stream parameters change (encoder restart, resolution, bitrate-driven reopen, codec). A new epoch invalidates all partial frames of older epochs and requires a keyframe. Starts at 0, wraps. |
| 4   | 4    | `frame_id`      | Per-viewer monotonically increasing frame counter, wraps at 2^32 (compare with serial-number arithmetic). Does not reset on epoch change. |
| 8   | 4    | `pts_us`        | Presentation time in microseconds from `EncodedPacket.PTS`, truncated to u32 (wraps ~71.6 min). Receiver computes duration as `(pts - lastPts) mod 2^32`, clamped to 0 if > 1 s. |
| 12  | 4    | `frame_size`    | Total Annex-B access-unit length in bytes (sum of data-fragment payloads, no padding). |
| 16  | 2    | `frag_idx`      | 0-based index. Data fragments are `0..data_count-1`; parity fragments are `data_count..data_count+parity_count-1`. |
| 18  | 2    | `data_count`    | `k`, number of data fragments, >= 1. |
| 20  | 2    | `parity_count`  | `m`, number of parity fragments (0 = no FEC; see section 8). Total fragments `n = k+m`. |
| 22  | 1    | `codec`         | `0` = H.264 Annex-B. `1`=HEVC, `2`=AV1 reserved. |
| 23  | 1    | `fec_scheme`    | `0` none, `1` XOR (single parity), `2` Reed-Solomon over GF(256), other values reserved. Receivers that do not implement the scheme treat parity fragments as absent. |
| 24  | 4    | `send_ms`       | Host monotonic milliseconds at send, low 32 bits. Used for jitter/e2e-delay estimation (delta between frames, never absolute). |
| 28  | var  | `payload`       | Slice of the access unit (data) or parity bytes. |

Rules
- **Fragment size.** `MAX_FRAGMENT_PAYLOAD = 1172` bytes, so a message is <= 1200 bytes.
  Data fragments `0..k-2` carry exactly `1172` bytes; fragment `k-1` carries the remainder
  (`frame_size - 1172*(k-1)`, 1..1172). `k = ceil(frame_size / 1172)`. A frame with
  `k > 65535 - m` must not be sent (an IDR of 77 MB; cap enforced by the encoder bitrate anyway).
  Receivers derive each data fragment's expected length from `frame_size`, so a short
  or long fragment is invalid and discarded.
- **Parity fragments** always have payload length `stride` (= the data fragment payload
  length, `1172` by default; see section 8). Shorter data (only the last fragment) is
  zero-padded for the computation only; padding is not transmitted for data fragments.
- Frames are sent as `k` fragments back to back, fragment 0 first. Keyframes are not
  interleaved with the next delta frame.
- `frame_id` and `frag_idx` uniquely identify a message; duplicates are ignored.

### 3.3 Why 1200 bytes and not the SCTP max message size

SCTP can carry large user messages, but they are chunked into multiple DATA chunks and a
single chunk lost with `maxRetransmits=0` abandons the whole message. Keeping each message
within one SCTP packet (typical path MTU 1200-1280 after DTLS+SCTP+UDP/IP overhead, the same
budget RTP uses) means one lost packet = exactly one lost fragment. The negotiated
`a=max-message-size` is irrelevant at this size (the SDP default and browser limits are >= 16 KiB).
The constant is a host config knob (`webrtc.video_fragment_bytes`, default 1200 total message
bytes, range 600-1400) and the receiver must accept any fragment size as long as lengths
are consistent with `frame_size`; it infers the stride from fragment 0's payload length when
`k > 1` (fragment 0 length = stride; all data fragments except the last must equal it).
So the 1172 above is only the default stride.

### 3.4 Host send behaviour

- Check `dc.BufferedAmount()` before each frame. If it exceeds `video_buffer_max_bytes`
  (default 256 KiB, roughly 2 frames at the nominal bitrate), **drop the whole frame**
  (do not send a partial frame) and mark the viewer as needing a keyframe; the next
  keyframe request is rate limited (below).
- Mark the viewer `needsKey` after any dropped frame, after the channel opens, and after a
  `KEYFRAME_REQUEST`. Send nothing but an IDR frame until the next IDR has been sent
  (same idea as `seenKeyframe` in `rtc/peer.go` `WriteSample`).
- **IDR must carry SPS+PPS in-band** (`PARAM` flag, Annex-B NAL types 7 and 8 before the
  slice). The ffmpeg CLI path and libx264 need `repeat-headers` / `-bsf:v h264_mp4toannexb`
  semantics to guarantee this; T11 must assert it (check the first NALs of every keyframe,
  log and fall back to prepending cached SPS/PPS if absent).
- Pace: write fragments of one frame with no deliberate delay for frames <= 8 fragments;
  for larger frames (IDR) spread over up to `idr_pace_ms` (default 8 ms) in equal bursts to
  avoid self-induced burst loss. Pace is a tuning knob, not wire-visible.
- **Epoch bump timing (as implemented in T11).** The host increments `epoch` on the first IDR sent
  after a change (transport switch, encoder restart detected as a PTS regression, or a changed
  SPS/PPS), never on a delta frame, so every frame carrying a new epoch is a `KEY` frame. A frame
  dropped by the BufferedAmount rule does not consume a `frame_id`.
- `ForceKeyframe` rate limit: at most one per `keyframe_min_interval_ms` (default 250,
  matching moonlight-web's `IDR_REQUEST_INTERVAL`; see T6 for the encoder-side debounce).
  Requests inside the window are coalesced, not dropped: the pending request fires at window end.

## 4. Viewer to host feedback (channel label `videofb`)

A second channel, created by the host, **reliable and ordered** (default DataChannel options),
because these messages are small, rare, and a lost keyframe request defeats the point.
Binary, fixed-size per type. Common 4-byte prefix:

| Off | Size | Field      | Notes |
|-----|------|------------|-------|
| 0   | 1    | `version`  | `1`. |
| 1   | 1    | `type`     | see below. |
| 2   | 2    | `flags`    | reserved, send 0. |

### 4.1 `type = 0x01` KEYFRAME_REQUEST (viewer to host), 16 bytes total

| Off | Size | Field         | Notes |
|-----|------|---------------|-------|
| 4   | 2    | `epoch`       | Epoch of the stream the viewer is stuck on. Host ignores a request whose `epoch` is older than the epoch of the last IDR it sent (a newer IDR already fixed it). |
| 6   | 1    | `reason`      | 0 unknown, 1 frame lost (incomplete at timeout), 2 decoder error / reconfigure, 3 decode queue backlog, 4 initial join, 5 waiting for IDR after epoch change. |
| 7   | 1    | reserved      | 0 |
| 8   | 4    | `last_good_id`| `frame_id` of the last frame successfully submitted to the decoder (`0xFFFFFFFF` if none). Reserved for reference-frame invalidation (section 4.4). |
| 12  | 4    | `lost_id`     | `frame_id` of the first frame detected lost/incomplete. |

Host action: call `ForceKeyframe` (rate limited per 3.4), set viewer `needsKey`.

### 4.2 `type = 0x02` FRAME_REPORT (viewer to host), 4 + 20 = 24 bytes

Sent every `report_interval_ms` (viewer default 250, host may override via `hello`), not per frame:

| Off | Size | Field            | Notes |
|-----|------|------------------|-------|
| 4   | 4    | `highest_id`     | Highest `frame_id` seen (any fragment). |
| 8   | 2    | `frames_complete`| Frames fully reassembled (incl. via FEC) since the previous report, saturating. |
| 10  | 2    | `frames_lost`    | Frames abandoned (timeout / superseded) since the previous report. |
| 12  | 2    | `frags_recv`     | Fragments received since the previous report. |
| 14  | 2    | `frags_expected` | Fragments expected for those frames (`sum(n)` of observed frames; lets host compute raw loss rate for T13/FEC ratio and ABR). |
| 16  | 2    | `recovered`      | Frames that were completed by FEC recovery (at least one data fragment rebuilt from parity), not fragments. 0 without FEC. |
| 18  | 2    | `decode_queue`   | `VideoDecoder.decodeQueueSize` at report time. |
| 20  | 2    | `jitter_ms_x10`  | Smoothed inter-frame arrival jitter in 0.1 ms. |
| 22  | 2    | `dec_ms_x10`     | Smoothed decode time (submit to output) in 0.1 ms. |

Host action: feed `frags_expected/frags_recv` and `frames_lost` to ABR/FEC ratio selection. Not
required for correctness; a host may ignore it.

### 4.3 `type = 0x03` HELLO (viewer to host, first message after open), 4 + 8 = 12 bytes

| Off | Size | Field        | Notes |
|-----|------|--------------|-------|
| 4   | 1    | `caps`       | bit0 WebCodecs H.264 supported (`VideoDecoder.isConfigSupported` true); bit1 hardware accelerated config accepted; bit2 Annex-B without `description` accepted; bit3 can do XOR FEC; bit4 can do RS FEC. |
| 5   | 1    | `max_fec`    | Max `parity_count` viewer will process. The host never sends more parity fragments than this; `0` therefore means "no parity" even if a cap bit is set. Viewers that support FEC send `255`. |
| 6   | 2    | `report_ms`  | Requested report interval. 0 = default. |
| 8   | 4    | reserved     | 0 |

Host replies by sending a keyframe (reason "initial join"); it must not send video before HELLO.

### 4.4 Reserved for reference-frame invalidation / FEC

- `type 0x04` NACK_RANGE: reserved. Intent: viewer lists lost `frame_id`s; an encoder with
  RPS/long-term-ref support (libav backend, NVENC `invalidateRefFrames`) re-encodes the next
  P-frame without them instead of a full IDR. Fields `last_good_id` and `lost_id` in 4.1 already
  carry the information; the host today ignores them.
- `type 0x05` FEC_CONFIG (host to viewer, on `videofb`): still reserved and **not used by T13**.
  FEC parameters are in each fragment header (`parity_count`, `fec_scheme`), and the scheme is
  negotiated by the HELLO cap bits alone, so no out-of-band message is needed for correctness.
- Unknown `type` or `version`: receiver ignores the message. Never close the channel.

## 5. Browser pipeline (T12)

```
videochannel.onmessage -> parseHeader -> Reassembler -> FrameAssembler
   -> DecodeGate (needsKey, epoch) -> VideoDecoder -> render (canvas)
                 ^------------------ KEYFRAME_REQUEST via videofb -------+
```

### 5.1 Reassembler

- Key: `(epoch, frame_id)`. Keep at most `MAX_INFLIGHT = 8` partial frames (ring). Slot holds a
  preallocated `Uint8Array(frame_size)`, a received-bitmap, and first-arrival time.
- Place data fragments at `idx * stride` (stride = fragment 0's payload length, or from the first fragment seen: use
  the host's known `MAX_FRAGMENT_PAYLOAD`; if fragment 0 never arrives but others do, use the length of
  any non-last fragment). Validate `idx < k`, lengths consistent with `frame_size`; drop on mismatch.
- A frame is **complete** when all `k` data fragments arrived, or, with FEC, when enough of the `n`
  fragments arrived and the missing data fragments were rebuilt from parity (section 8). Recovery is
  attempted on every arrival once `received_data + received_parity >= k`, before any loss declaration.
- **Completion order.** Frames are submitted to the decoder strictly in increasing `frame_id`.
  If frame `N+1` completes before `N`:
  - hold `N+1` for at most `reorder_wait_ms` (default 5 ms, bounded by one frame interval);
  - after that, declare `N` lost.
- **Loss declaration** (any one of): a later frame completed and `N` is still incomplete after
  `reorder_wait_ms`; `N` is older than `frame_timeout_ms` (default `max(20, 2 * frame_interval)`) since its
  first fragment; the slot ring evicts it.
- **On loss of frame N**: drop `N`; set `needsKey = true`; discard every partial and complete
  delta frame until a `KEY` frame completes (a P-frame after a gap would decode to corruption);
  send KEYFRAME_REQUEST (reason 1) unless one was sent less than `keyframe_min_interval_ms` ago
  (re-send if no IDR arrives within `max(250 ms, 3 * RTT)`, using the
  RTT from section 5.5 or 250 ms).
- A lost **IDR** is handled the same way (it is just a lost frame).
- Do **not** show or decode partial frames. There is no concealment in v1.

### 5.2 Decoder configuration

1. On the first complete `KEY` frame of an epoch, locate SPS (NAL type 7) in the Annex-B data
   (start codes `00 00 01` or `00 00 00 01`; NAL type is `byte & 0x1f`).
2. Build the codec string `avc1.` + hex of SPS bytes 1..3 (`profile_idc`, `constraint_flags`,
   `level_idc`), e.g. host's Constrained Baseline L4.2 is `avc1.42e02a`
   (`rtc/peer.go` registers `profile-level-id=42e02a`; the SPS is authoritative).
3. `VideoDecoder.configure({codec, hardwareAcceleration:"prefer-hardware", optimizeForLatency:true})`
   with **no `description`**: per the WebCodecs AVC registration an absent `description`
   means the chunks are **Annex-B**, so IDR frames with in-band SPS/PPS are fed unchanged
   (the same strategy as moonlight-web's primary path, finding 5). Do not set `codedWidth/Height`
   unless parsed from the SPS; they are optional.
4. Probe with `isConfigSupported` in this order: hardware+latency, latency only, plain.
5. If the browser rejects Annex-B (decoder `error` callback on a known-good stream, or `isConfigSupported`
   false), switch to **avcC mode** (moonlight-web's translator): build `description`
   (avcC: version 1, profile, compat, level, `lengthSizeMinusOne=3`, 1 SPS, 1 PPS), rewrite
   each Annex-B NAL to a 4-byte big-endian length prefix, strip SPS/PPS/AUD/SEI from chunk
   data, `reset()` + `configure()` on every new SPS/PPS that differs from the previous.
   Implement Annex-B first; avcC mode is a documented fallback, mandatory only if Safari/Firefox
   testing needs it.
6. `new EncodedVideoChunk({type: KEY ? "key" : "delta", timestamp: pts_us_unwrapped, duration, data})`.
   `timestamp` must be monotonic: unwrap the u32 `pts_us` by adding `2^32` on a backwards jump of
   more than `2^31`. On epoch change: `decoder.reset()`, `configure` again, `needsKey = true`.
7. `decoder.error` callback: close, reconfigure, send KEYFRAME_REQUEST reason 2. After 3 errors in 10 s
   trigger the fallback of section 7.
8. Backpressure (copied from moonlight-web finding 6): if `decodeQueueSize > 2` and
   `decodeQueueSize * frame_interval_ms > 200`, reset the decoder, set `needsKey`, request a keyframe
   (reason 3). Also never call `decode()` while `decodeQueueSize > 4`; count the frame as dropped.

### 5.3 Render

- `output(frame)` callback: if a previous undrawn `VideoFrame` exists, `close()` it (latest wins);
  otherwise keep exactly one. Draw in `requestAnimationFrame` (`ctx.drawImage(frame,0,0)`), then
  `frame.close()`. Option `desynchronized:true` canvas context with draw-on-output for lowest latency
  (moonlight-web's `canvasVsync=false`); default to rAF to avoid tearing. Always `close()` frames.
- Canvas replaces `<video>` in `RoomPage.tsx`; the existing `object-fit`/overlay layout applies to
  the canvas. Input handling is unchanged (it uses the `input` channel).
- Stats currently computed from `getStats` `inbound-rtp` (`RoomPage.tsx` ~261-288) do not exist in
  this mode; show frame rate, dropped/lost counts, decode ms, jitter from section 4.2 data.

### 5.4 Jitter / latency

No jitter buffer; frames render as decoded. Reorder tolerance is the 5 ms hold in 5.1.
Delay estimation uses `send_ms` deltas vs local `performance.now()` deltas (never absolute).
Smoothed inter-frame arrival jitter goes in FRAME_REPORT. A latency cap is enforced by
dropping, not buffering: the 5.2.8 queue rule.

### 5.5 RTT

Use `RTCPeerConnection.getStats()` `candidate-pair.currentRoundTripTime` (poll 1 s). If unavailable,
assume 100 ms.

## 6. Integration: transport selection (new, not the twitch-bot delivery mode)

Host config (`config.yaml`, `webrtc:` block, default `track`):

```yaml
webrtc:
  video_transport: track      # track | webcodecs | auto
  video_fragment_bytes: 1200
  video_buffer_max_bytes: 262144
  video_idr_pace_ms: 8                  # section 3.4 pacing (0 = default 8, <0 = off)
  video_keyframe_min_interval_ms: 250   # section 3.4 ForceKeyframe coalescing
```

- `track` (default): current behaviour, no extra channels.
- `webcodecs`: the host creates `video` and `videofb` channels (in the same offer as `control`/`input`,
  so no signaling change). The RTP video track is **still negotiated** (so the SDP is identical across modes and a
  viewer can fall back without renegotiation) but **receives no samples** until the viewer asks for it.
- `auto`: same as `webcodecs` for capable viewers; the viewer chooses (see below).

Per-viewer selection, over the existing reliable `control` channel (JSON text, same as
`protocols.md` section 4). Viewer to host after the channels open:
`{"kind":"video_transport","use":"webcodecs"}` or `{"kind":"video_transport","use":"track"}`.
Host default before receiving this message: `track` if `video_transport` is `track` or `auto`, `webcodecs` if
`webcodecs`; either way it switches on receipt (if the configured mode allows it). Host acknowledges with
`{"kind":"video_transport","active":"track"|"webcodecs"}` and from then on writes each viewer's packets to exactly one
sink (RTP `WriteSample` or the DataChannel fragmenter). Per-viewer peers already exist in this repo
(`ViewerConnection` per `viewerId`), so the mode is per viewer. Each switch forces a keyframe and bumps `epoch`.

Viewer side: `ViewerConnection.attachChannel` handles labels `video` and `videofb` (today only
`control` and `input`, `webrtc.ts` `ondatachannel`). URL override `?transport=track|webcodecs`
for testing. The video-transport choice does not belong in the twitch-bot admin page (section 0);
if an operator toggle is wanted it is the host config above.

## 7. Fallback when WebCodecs is unsupported or fails

Viewer picks `track` if any of: `typeof VideoDecoder === "undefined"`, `!isSecureContext`,
`isConfigSupported` false for all three configs, `?transport=track`, three decoder errors in 10 s,
the `video` channel never opens within 5 s, or no complete keyframe within 3 s after HELLO.
It sends `{"kind":"video_transport","use":"track"}`, tears down the canvas pipeline and attaches the existing `<video>`
(`ontrack` is always wired). The host stops writing to the DataChannel and starts RTP at the next IDR.
Returning to `webcodecs` is not automatic within a session. The reverse-compat case (host without
`video` channel): viewer sees no `video` label within 3 s of control open and uses `track`.

## 8. FEC (T13)

Off by default (`webrtc.video_fec: off`). Everything below is wire-compatible with the 28-byte
header; no new header fields.

### 8.1 Shards

- `stride` is the data-fragment payload length (`fragment_bytes - 28`, 1172 by default). A parity
  fragment's payload length equals `stride`, so a receiver that has seen any parity fragment also
  knows `stride`.
- Shard `i` (`0 <= i < k`) is bytes `[i*stride, (i+1)*stride)` of the access unit. The **last** shard
  is zero-padded up to `stride` for the computation only. After recovery the receiver trims the
  rebuilt last shard to `frame_size - (k-1)*stride` bytes (the length it already derives from
  `frame_size`). Parity shard `j` is sent as fragment `frag_idx = k + j`, with `PARITY` set,
  `parity_count = m`, `fec_scheme` set and every other header field copied from the data fragments
  (including `KEY`/`PARAM`). Parity fragments are sent after all data fragments of the frame.
- FEC applies only when `k >= 2` and `k + m <= 255`. Larger frames (about > 290 KB at the default
  stride) and single-fragment frames are sent without parity (`parity_count = 0`, `fec_scheme = 0`).
  Striping is **not** defined; the per-frame decision is carried by the header, so viewers need no
  other state.

### 8.2 Scheme 1: XOR (interleaved groups)

`m` parity shards (1 <= m <= k). Data shard `i` belongs to group `i mod m`; parity shard `j` is the
XOR of all data shards of group `j`. Each group recovers at most one lost data fragment, so up to
`m` consecutive lost data fragments (a burst) are always recoverable when their parity arrived.
`m = 1` is the plain "XOR of all k fragments" scheme. Cheap (no multiplications).

### 8.3 Scheme 2: Reed-Solomon over GF(256), systematic

- Field: GF(2^8), primitive polynomial `0x11d`, generator `2`; `exp[i] = 2^i`, `log` its inverse
  (`exp[8] = 0x1d`, `exp[254] = 0x8e`). Addition is XOR.
- Parity shard `j` (`0 <= j < m`) = sum over `i` of `C[j][i] * data[i]` (per byte), with the Cauchy
  matrix `C[j][i] = 1 / ((k + j) XOR i)`. The x-set `{k..k+m-1}` and y-set `{0..k-1}` are disjoint,
  so every entry is defined and every square submatrix is invertible: **any `k` of the `n = k+m`
  fragments recover the frame** (MDS). The data fragments stay verbatim (systematic).
- Recovery with `e` missing data fragments: pick any `e` received parity shards, subtract the
  contribution of the known data shards, and solve the `e x e` Cauchy system by Gauss-Jordan.
- Golden vector (checked by both implementations): `k = 4, m = 2, stride = 8`,
  `data[i][b] = (i*37 + b*11 + 5) & 0xff`:
  XOR parity `4a4a7e564abad6ce`, `5e4acadef6cadab6`;
  RS parity `b84d6d2bb562d3ff`, `8a99c8d08dfeb396`.
  Table checks: `gfMul(0x53,0xca) = 143`, `gfInv(0x53) = 140`, `C[0][0] = 71` and `C[1][3] = 122` for `k = 4`.
- Implementations: `playgate-host/internal/rtc/videofec.go`, `playgate-web/src/lib/webcodecs/fec.ts`.
  No third-party library: a table-driven GF(256) with a small solver is ~150 lines, and the
  parameters above must be identical on both ends, which a dependency would not guarantee.

### 8.4 Negotiation and policy

- HELLO `caps` bit3 = viewer can decode XOR, bit4 = RS; `max_fec` = max `parity_count`. The viewer
  in `playgate-web` advertises both bits and `max_fec = 255`.
- The host sends parity only when its configured scheme's bit is set in that viewer's HELLO; there
  is no automatic switch between schemes. Receivers that do not know a `fec_scheme` treat parity
  fragments as absent, so enabling FEC never breaks an old viewer.
- Host config: `webrtc.video_fec: off | xor | rs` and `webrtc.video_fec_ratio` (0-1, default 0.1).
  `m = ceil(k * ratio)`, at least 1 for `k >= 2`, capped by the viewer's `max_fec`, by `255 - k`
  and, for XOR, by `k`. Never protect `k == 1`. Adapting the ratio from FRAME_REPORT loss is future work.
- Parity fragments are part of the same send burst: they count toward pacing (the
  more-than-8-fragments spread) and obey the same per-frame `BufferedAmount` drop (the check
  happens before the frame, parity included, is queued).

## 9. Test and acceptance notes

- Golden vectors: T11 and T12 each add a unit test for header encode/decode using the vector
  `ver=1 flags=0x03 epoch=0x0001 frame_id=0x00000010 pts_us=0x00010000 frame_size=2400
  frag_idx=1 data_count=3 parity_count=0 codec=0 fec=0 send_ms=0x00000064` which encodes to
  `01 03 00 01 00 00 00 10 00 01 00 00 00 00 09 60 00 01 00 03 00 00 00 00 00 00 00 64`
  (2400 = 0x0960; stride 1172, so k = 3).
- Loss simulation: drop 1 fragment of an IDR and verify exactly one KEYFRAME_REQUEST and recovery in
  RTT + one IDR; drop 1 fragment of a P-frame: same.
- Compare freeze duration vs RTP track under `tc netem loss 2% delay 20ms` (plan section "驗證").

## 10. Open questions

1. **Bitrate adaptation in DataChannel mode.** GCC (T8) is RTP/TWCC only. Options: derive
   target bitrate from `FRAME_REPORT` loss and `BufferedAmount` (AIMD in `internal/abr`), or use the
   SCTP RTT. Needs a decision before T9 covers this mode.
2. **SCTP congestion control.** SCTP is congestion controlled even with `maxRetransmits=0` and
   can stall a burst (large IDR) behind cwnd; Chrome's cwnd behaviour vs Pion's was not measured.
   If IDR bursts exceed ~100 ms, split the IDR across `idr_pace_ms` or reduce IDR size (T1 VBV).
3. **Does Pion hold unreliable-message order in its send queue and drop expired ones?** With
   `maxRetransmits=0` messages should be abandoned, not queued; verify with a netem test before relying on it.
4. **Annex-B without `description` on Safari/Firefox.** Needs a real-browser matrix (Chrome desktop, Chrome
   Android, Safari 17+, Firefox). avcC mode is the designed fallback.
5. **Secure context.** `VideoDecoder` requires HTTPS or localhost; confirm deployments of `playgate-web`
   are HTTPS. If not, `track` is the only mode.
6. **Per-viewer encode.** Host has one encoder shared across viewers; a per-viewer `ForceKeyframe` is an IDR
   for everyone (extra bitrate for others). Acceptable for now; reference-frame invalidation would fix it.
7. **Multi-GOP policy.** With PLI-driven recovery the GOP can grow (T6 plans 3 s), which also affects late-join time.
8. **Audio sync.** Audio stays on RTP; video in this mode carries `pts_us` only, so lip sync is best-effort.
   There is no shared clock mapping between RTP audio and `pts_us` in v1.

## 11. T12 viewer implementation notes (clarifications, no wire changes)

Implemented in `playgate-web/src/lib/webcodecs/`. Points where the spec was ambiguous:

1. **Frame-id gaps.** The viewer treats a gap in `frame_id` before a delta frame as a lost frame
   (needs a KEY). So the host must only skip `frame_id`s (e.g. a frame dropped for `BufferedAmount`)
   when it also sets `needsKey` and the next frame sent is a KEY, exactly as 3.4 says.
2. **5.2.8 `decodeQueueSize > 4`.** Dropping a delta frame corrupts every following frame, so the viewer
   also sets `needsKey` and sends KEYFRAME_REQUEST (reason 3), not only counts the drop.
3. **Parked deltas.** If a delta completes while an earlier KEY frame is still reassembling, it is kept
   (not dropped) and delivered right after the KEY completes; otherwise reordering around an IDR would
   trigger a second loss.
4. **Viewer sequence.** On control open the viewer sends `video_transport use=webcodecs` (or `track`);
   `HELLO` goes out only once both `video` and `videofb` are open and the decoder probe finished.
   A host ack `active:"track"` after the viewer asked for `webcodecs` is treated as "host refuses" and
   triggers the fallback. The host must therefore not send `active:"track"` for a viewer that asked for
   `webcodecs` unless it really will not stream over the DataChannel.
5. **FRAME_REPORT counters.** `frags_recv` counts every valid fragment (including parity);
   `frags_expected` adds `k+m` once per distinct `frame_id`, including frames that arrive late.
6. **Stride.** Taken from the first non-last data fragment seen; the last fragment is placed at
   `frame_size - len`, so fragment 0 is not required to arrive first.

## 12. T13 FEC notes (clarifications, no wire changes)

1. Parity payload length is `stride`, not a hard-coded 1172, because `video_fragment_bytes` is configurable.
2. The XOR scheme is interleaved groups (`i mod m`), a superset of the single-parity text that
   section 8 used to reserve (`m == 1` is identical).
3. `FRAME_REPORT.recovered` counts **frames** rescued by FEC. `frags_recv` / `frags_expected` still
   count parity fragments, so the host can compute raw fragment loss with or without FEC.
4. A recovered frame is indistinguishable from a normally completed one for the rest of the pipeline
   (ordering, loss declaration, keyframe policy).
5. `FEC_CONFIG (0x05)` stays reserved and unused.
