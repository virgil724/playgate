(async () => {
  // Input-to-photon AND input-to-ear latency on a viewer tab showing the
  // Switch (2) HOME menu through a capture card. Alternates D-pad right (H)
  // and left (F) so the cursor moves between two game tiles (playing the
  // menu click SFX) and times keydown -> title-row pixel change AND keydown
  // -> audio peak onset simultaneously. Never presses A.
  const TRIALS = 30;
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  let canvas, video, audio, useCanvas, src, W = 0, H = 0, audioStream = null;
  const readyDeadline = performance.now() + 20000;
  while (performance.now() < readyDeadline) {
    canvas = document.querySelector("canvas");
    video = document.querySelector("video");
    audio = document.querySelector("audio");
    useCanvas = Boolean(canvas && getComputedStyle(canvas).display !== "none");
    src = useCanvas ? canvas : video;
    W = src ? (useCanvas ? canvas.width : video.videoWidth) : 0;
    H = src ? (useCanvas ? canvas.height : video.videoHeight) : 0;
    audioStream = audio && audio.srcObject instanceof MediaStream ? audio.srcObject : null;
    if (W > 300 && H > 150 && audioStream && audioStream.getAudioTracks().length > 0) break;
    await sleep(250);
  }
  if (!src) return { error: "no canvas/video" };
  if (!W || !H) return { error: "no picture yet" };
  if (!audioStream || audioStream.getAudioTracks().length === 0) {
    return { error: "no audio track on <audio>" };
  }
  await sleep(1000);

  // Ensure the remote audio element is actively playing so Chrome decodes it.
  try {
    audio.muted = false;
    await audio.play();
  } catch {
    // Fall back to muted play if autoplay policy blocks unmuted play.
    try {
      audio.muted = true;
      await audio.play();
    } catch {
      /* ignore */
    }
  }

  const audioCtx = new AudioContext({ latencyHint: "interactive" });
  try {
    await audioCtx.resume();
    const source = audioCtx.createMediaStreamSource(audioStream);
    const analyser = audioCtx.createAnalyser();
    analyser.fftSize = 256;
    analyser.smoothingTimeConstant = 0;
    // Keep the graph pulled by the audio clock without duplicating sound output.
    const sink = audioCtx.createGain();
    sink.gain.value = 0;
    source.connect(analyser);
    analyser.connect(sink);
    sink.connect(audioCtx.destination);

    const timeBuf = new Float32Array(analyser.fftSize);
    const audioPeak = () => {
      analyser.getFloatTimeDomainData(timeBuf);
      let peak = 0;
      for (let i = 0; i < timeBuf.length; i++) {
        const a = Math.abs(timeBuf[i]);
        if (a > peak) peak = a;
      }
      return peak;
    };

    const sx = W / 1280, sy = H / 720;
    const rx = Math.round(90 * sx), ry = Math.round(152 * sy), rw = Math.round(900 * sx), rh = Math.round(40 * sy);
    const c = document.createElement("canvas"); c.width = rw; c.height = rh;
    const ctx = c.getContext("2d", { willReadFrequently: true });
    const grab = () => {
      ctx.drawImage(src, rx, ry, rw, rh, 0, 0, rw, rh);
      const d = ctx.getImageData(0, 0, rw, rh).data;
      const y = new Uint8Array(rw * rh);
      for (let i = 0, p = 0; i < d.length; i += 4, p++) y[p] = (d[i] * 77 + d[i + 1] * 150 + d[i + 2] * 29) >> 8;
      return y;
    };
    const diff = (a, b) => { let n = 0; for (let i = 0; i < a.length; i++) if (Math.abs(a[i] - b[i]) > 40) n++; return n; };
    const raf = () => new Promise((r) => requestAnimationFrame(r));
    const key = (type, code) => window.dispatchEvent(new KeyboardEvent(type, { code, key: code.slice(3).toLowerCase(), bubbles: true }));
    const tap = async (code) => { key("keydown", code); await sleep(80); key("keyup", code); };

    // Calibrate both video and audio noise floors over ~1.5s while idle.
    let prev = grab();
    const vNoise = [], aNoise = [];
    const calTimer = setInterval(() => aNoise.push(audioPeak()), 4);
    for (let i = 0; i < 90; i++) { await raf(); const m = grab(); vNoise.push(diff(prev, m)); prev = m; }
    clearInterval(calTimer);

    const pct = (arr, p) => {
      if (!arr.length) return null;
      const s = [...arr].sort((a, b) => a - b);
      return s[Math.min(s.length - 1, Math.floor(s.length * p))];
    };
    const vNoiseP90 = pct(vNoise, 0.9) ?? 0;
    const vThreshold = Math.max(30, vNoiseP90 * 3);
    const aNoiseP90 = pct(aNoise, 0.9) ?? 0;
    const aThreshold = Math.max(0.015, aNoiseP90 * 5);

    const settle = async () => {
      let a = grab(), still = 0, n = 0;
      while (still < 4 && n++ < 180) {
        await raf();
        const b = grab();
        const vQuiet = diff(a, b) <= vThreshold / 3;
        const aQuiet = audioPeak() <= aThreshold * 0.6;
        still = vQuiet && aQuiet ? still + 1 : 0;
        a = b;
      }
      return a;
    };

    const waitBoth = (base, t0) => new Promise((resolve) => {
      let videoMs = null, videoDiff = null;
      let audioMs = null, audioSig = null;
      let done = false;
      const finish = () => {
        if (done) return;
        done = true;
        clearInterval(aTimer);
        resolve({ videoMs, videoDiff, audioMs, audioSig });
      };
      const aTimer = setInterval(() => {
        if (done) return;
        const now = performance.now();
        if (audioMs === null) {
          const p = audioPeak();
          if (p > aThreshold) {
            audioMs = now - t0;
            audioSig = p;
          }
        }
        if ((videoMs !== null && audioMs !== null) || now - t0 > 2000) finish();
      }, 2);
      const vLoop = () => {
        if (done) return;
        const now = performance.now();
        if (videoMs === null) {
          const d = diff(base, grab());
          if (d > vThreshold) {
            videoMs = now - t0;
            videoDiff = d;
          }
        }
        if ((videoMs !== null && audioMs !== null) || now - t0 > 2000) {
          finish();
          return;
        }
        requestAnimationFrame(vLoop);
      };
      requestAnimationFrame(vLoop);
    });

    const vSamples = [], aSamples = [], avDelta = [];
    const vSignal = [], aSignal = [];
    for (let t = 0; t < TRIALS; t++) {
      const code = t % 2 === 0 ? "KeyH" : "KeyF";
      const base = await settle();
      await sleep(100 + Math.random() * 150);
      const t0 = performance.now();
      const pressing = tap(code);
      const r = await waitBoth(base, t0);
      await pressing;
      const vm = r.videoMs == null ? null : Math.round(r.videoMs);
      const am = r.audioMs == null ? null : Math.round(r.audioMs);
      vSamples.push(vm);
      aSamples.push(am);
      if (vm != null && am != null) avDelta.push(am - vm);
      if (r.videoDiff != null) vSignal.push(r.videoDiff);
      if (r.audioSig != null) aSignal.push(Number(r.audioSig.toFixed(4)));
      await sleep(300);
    }

    const summarize = (arr) => {
      const ok = arr.filter((x) => x != null).sort((a, b) => a - b);
      const q = (p) => (ok.length ? ok[Math.min(ok.length - 1, Math.floor(ok.length * p))] : null);
      return {
        n: ok.length,
        timeouts: arr.length - ok.length,
        min: ok[0] ?? null,
        p50: q(0.5),
        p90: q(0.9),
        max: ok[ok.length - 1] ?? null,
      };
    };

    return {
      size: [W, H],
      audioCtxState: audioCtx.state,
      audioBaseLatencyMs: Number(((audioCtx.baseLatency ?? 0) * 1000).toFixed(2)),
      audioOutputLatencyMs: Number(((audioCtx.outputLatency ?? 0) * 1000).toFixed(2)),
      videoCal: { noiseP90: vNoiseP90, threshold: vThreshold, signalMin: pct(vSignal, 0), signalMed: pct(vSignal, 0.5) },
      audioCal: { noiseP90: Number(aNoiseP90.toFixed(5)), threshold: Number(aThreshold.toFixed(5)), signalMin: pct(aSignal, 0), signalMed: pct(aSignal, 0.5) },
      video: summarize(vSamples),
      audio: summarize(aSamples),
      avDeltaAudioMinusVideo: summarize(avDelta),
      vSamples,
      aSamples,
      avDelta,
    };
  } finally {
    await audioCtx.close().catch(() => {});
  }
})()
