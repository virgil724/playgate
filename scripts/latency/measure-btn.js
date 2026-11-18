(async () => {
  // Input-to-photon latency on a viewer tab (testsrc + overlay host). Watches the part of the BTN line that changes
  // (after the fixed "BTN  " prefix), located from the pixels, so a one-glyph
  // change ('-' <-> 'A') is far above background noise.
  const TRIALS = 25;
  const canvas = document.querySelector("canvas");
  const video = document.querySelector("video");
  const useCanvas = canvas && getComputedStyle(canvas).display !== "none";
  const src = useCanvas ? canvas : video;
  if (!src) return { error: "no canvas/video" };
  const H = useCanvas ? canvas.height : video.videoHeight;
  if (!H) return { error: "no picture yet" };
  const fs = Math.max(14, Math.floor(H / 24));
  const ry = 16 + 2 * fs - 4, rh = Math.ceil(fs * 1.3);
  const isY = (d, i) => d[i] > 170 && d[i + 1] > 170 && d[i + 2] < 110;
  const mk = (w) => { const c = document.createElement("canvas"); c.width = w; c.height = rh; return c.getContext("2d", { willReadFrequently: true }); };
  // Locate glyph clusters on the full line (neutral state: "BTN  -").
  const LW = 20 * fs, lctx = mk(LW);
  lctx.drawImage(src, 0, ry, LW, rh, 0, 0, LW, rh);
  const ld = lctx.getImageData(0, 0, LW, rh).data;
  const col = new Array(LW).fill(0);
  for (let y = 0; y < rh; y++) for (let x = 0; x < LW; x++) if (isY(ld, (y * LW + x) * 4)) col[x]++;
  const clusters = []; let start = -1, gap = 0;
  for (let x = 0; x < LW; x++) {
    if (col[x] > 0) { if (start < 0) start = x; gap = 0; }
    else if (start >= 0 && ++gap > fs * 0.4) { clusters.push([start, x - gap]); start = -1; gap = 0; }
  }
  if (start >= 0) clusters.push([start, LW - 1]);
  if (clusters.length < 2) return { error: "could not find glyph clusters", clusters };
  // Words: "BTN" then the button list; take everything from the 2nd word on.
  const rx = Math.max(0, clusters[1][0] - Math.ceil(fs * 0.3)), rw = Math.ceil(fs * 5);
  const sctx = mk(rw);
  const grab = () => {
    sctx.drawImage(src, rx, ry, rw, rh, 0, 0, rw, rh);
    const d = sctx.getImageData(0, 0, rw, rh).data;
    const m = new Uint8Array(rw * rh);
    for (let i = 0, p = 0; i < d.length; i += 4, p++) m[p] = isY(d, i) ? 1 : 0;
    return m;
  };
  const diff = (a, b) => { let n = 0; for (let i = 0; i < a.length; i++) n += a[i] ^ b[i]; return n; };
  const raf = () => new Promise((r) => requestAnimationFrame(r));
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const key = (type) => window.dispatchEvent(new KeyboardEvent(type, { code: "KeyJ", key: "j", bubbles: true }));

  let prev = grab(); const noise = [];
  for (let i = 0; i < 60; i++) { await raf(); const m = grab(); noise.push(diff(prev, m)); prev = m; }
  const noiseMax = Math.max(...noise);
  const threshold = Math.max(20, noiseMax * 3);
  const settle = async () => { let a = grab(), still = 0, n = 0; while (still < 3 && n++ < 120) { await raf(); const b = grab(); still = diff(a, b) <= threshold / 2 ? still + 1 : 0; a = b; } return a; };
  const waitChange = async (base, t0) => { for (;;) { await raf(); const now = performance.now(); if (now - t0 > 3000) return { ms: null }; const d = diff(base, grab()); if (d > threshold) return { ms: now - t0, d }; } };

  const samples = [], signal = [];
  for (let t = 0; t < TRIALS; t++) {
    for (const type of ["keydown", "keyup"]) {
      const base = await settle();
      const t0 = performance.now();
      key(type);
      const r = await waitChange(base, t0);
      samples.push(r.ms == null ? null : Math.round(r.ms));
      if (r.d) signal.push(r.d);
      await sleep(type === "keydown" ? 150 : 250 + Math.random() * 200);
    }
  }
  key("keyup");
  const ok = samples.filter((x) => x != null).sort((a, b) => a - b);
  const q = (p) => ok[Math.min(ok.length - 1, Math.floor(ok.length * p))];
  return { region: { rx, rw, ry, rh, fs }, noiseMax, threshold, signalMin: Math.min(...signal), signalMed: signal.sort((a,b)=>a-b)[signal.length>>1],
           n: ok.length, timeouts: samples.length - ok.length, min: ok[0], p50: q(0.5), p90: q(0.9), max: ok[ok.length - 1], samples };
})()
