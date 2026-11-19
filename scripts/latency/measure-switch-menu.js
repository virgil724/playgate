(async () => {
  // Input-to-photon latency on a viewer tab showing the Switch (2) HOME menu
  // through a capture card. Alternates D-pad right (H) and left (F) so the
  // cursor moves between two game tiles, and times keydown -> pixel change in
  // the tile row. Never presses A (it would launch the selected game).
  const TRIALS = 30;
  const canvas = document.querySelector("canvas");
  const video = document.querySelector("video");
  const useCanvas = canvas && getComputedStyle(canvas).display !== "none";
  const src = useCanvas ? canvas : video;
  if (!src) return { error: "no canvas/video" };
  const W = useCanvas ? canvas.width : video.videoWidth;
  const H = useCanvas ? canvas.height : video.videoHeight;
  if (!W || !H) return { error: "no picture yet" };
  // The title line above the selected tile ("game name"), on the 1280x720
  // HOME layout. It only changes when the cursor moves; the tile highlight
  // itself shimmers continuously and would look like input.
  const sx = W / 1280, sy = H / 720;
  const rx = Math.round(90 * sx), ry = Math.round(152 * sy), rw = Math.round(900 * sx), rh = Math.round(40 * sy);
  const DS = 1;
  const cw = Math.ceil(rw / DS), ch = Math.ceil(rh / DS);
  const c = document.createElement("canvas"); c.width = cw; c.height = ch;
  const ctx = c.getContext("2d", { willReadFrequently: true });
  const grab = () => {
    ctx.drawImage(src, rx, ry, rw, rh, 0, 0, cw, ch);
    const d = ctx.getImageData(0, 0, cw, ch).data;
    const y = new Uint8Array(cw * ch);
    for (let i = 0, p = 0; i < d.length; i += 4, p++) y[p] = (d[i] * 77 + d[i + 1] * 150 + d[i + 2] * 29) >> 8;
    return y;
  };
  const diff = (a, b) => { let n = 0; for (let i = 0; i < a.length; i++) if (Math.abs(a[i] - b[i]) > 40) n++; return n; };
  const raf = () => new Promise((r) => requestAnimationFrame(r));
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const key = (type, code) => window.dispatchEvent(new KeyboardEvent(type, { code, key: code.slice(3).toLowerCase(), bubbles: true }));
  const tap = async (code) => { key("keydown", code); await sleep(80); key("keyup", code); };

  let prev = grab(); const noise = [];
  for (let i = 0; i < 90; i++) { await raf(); const m = grab(); noise.push(diff(prev, m)); prev = m; }
  const sorted = [...noise].sort((a, b) => a - b);
  const noiseP90 = sorted[Math.floor(sorted.length * 0.9)];
  const threshold = Math.max(30, noiseP90 * 3);
  // Wait until the picture has been still for a few frames (cursor animation done).
  const settle = async () => { let a = grab(), still = 0, n = 0; while (still < 4 && n++ < 180) { await raf(); const b = grab(); still = diff(a, b) <= threshold / 3 ? still + 1 : 0; a = b; } return a; };
  const waitChange = async (base, t0) => { for (;;) { await raf(); const now = performance.now(); if (now - t0 > 2000) return { ms: null }; const d = diff(base, grab()); if (d > threshold) return { ms: now - t0, d }; } };

  const samples = [], signal = [];
  for (let t = 0; t < TRIALS; t++) {
    const code = t % 2 === 0 ? "KeyH" : "KeyF"; // right, then back left
    const base = await settle();
    await sleep(100 + Math.random() * 150);
    const t0 = performance.now();
    const pressing = tap(code);
    const r = await waitChange(base, t0);
    await pressing;
    samples.push(r.ms == null ? null : Math.round(r.ms));
    if (r.d) signal.push(r.d);
    await sleep(300);
  }
  const ok = samples.filter((x) => x != null).sort((a, b) => a - b);
  const q = (p) => ok[Math.min(ok.length - 1, Math.floor(ok.length * p))];
  signal.sort((a, b) => a - b);
  return { size: [W, H], region: { rx, ry, rw, rh }, noiseP90, threshold, signalMin: signal[0], signalMed: signal[signal.length >> 1],
           n: ok.length, timeouts: samples.length - ok.length, min: ok[0], p50: q(0.5), p90: q(0.9), max: ok[ok.length - 1], samples };
})()
