# Hyperframes Composition Brief: PlayGate

## Objective
Create a short launch-style brag video for PlayGate — a platform that lets Twitch viewers play a streamer's real Nintendo Switch remotely over WebRTC.

## Output
- Composition directory: `brag-output-2026-06-24-180000/composition/`
- Rendered video: `brag-output-2026-06-24-180000/brag.mp4`
- Format: landscape — 1920x1080
- Duration: 20 seconds

## Source Material
- Project root: `C:/Users/virgil/playgate/`
- Primary files read: `playgate-web/src/styles.css`, `playgate-web/src/pages/RoomPage.tsx`, `playgate-web/src/components/VirtualGamepad.tsx`, `playgate-twitch-bot/src/admin/public/style.css`, `playgate-server/README.md`, `playgate-host/README.md`, `playgate-signaling/README.md`
- Product name: PlayGate
- Tagline / strongest claim: "Your chat doesn't just watch anymore."
- Key UI or visual moment to recreate: The virtual gamepad (dual analog sticks, face buttons X/Y/A/B, d-pad, shoulder buttons) against a dark panel background, with the live video stream above and the connection status topbar showing green "Connected" dot and countdown timer
- Copy that must appear verbatim:
  - "What if your Twitch chat could play your game?"
  - "!play"
  - "Your code is 5b5f6121"
  - "Queue #2"
  - "Connected"
  - "PlayGate"
  - "Your chat doesn't just watch anymore."

## Creative Direction
- Tone preset: polished
- Creative direction: premium infrastructure product film
- Interpretation: Slow, confident reveals. Each scene holds long enough to land. The product's complexity speaks for itself — the video's job is clarity and weight. Transitions are soft crossfades. Typography is clean and generous.
- Angle: This is serious infrastructure — Go services, Cloudflare Durable Objects, NXBT virtual controllers, adaptive bitrate, ed25519 JWTs — all so a Twitch viewer can press A on their phone and watch a real Switch respond. The video treats it like a premium product launch.
- Hook: "What if your Twitch chat could play your game?" — types in on a dark screen
- Outro / punchline: "PlayGate" in large bold type, then "Your chat doesn't just watch anymore."
- Avoid:
  - Generic SaaS language
  - Abstract filler visuals
  - Gaming hype-reel energy (this is polished, not chaotic)
  - Waveform/equalizer visuals

## Visual Identity
- Background: `#0d0f14` (deep blue-black)
- Text: `#e6e9ef` (near-white)
- Accent: `#4f8cff` (bright blue)
- Secondary accent: `#2dd4bf` (teal)
- Muted text: `#8b93a3`
- Panel: `#161a23`
- Panel-2: `#1f2530`
- Border: `#2a3140`
- Twitch purple: `#9147ff` (for Twitch bot scenes)
- Status green: `#22c55e`
- Status amber: `#f59e0b`
- Display font: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif — use Inter or similar clean sans-serif as Hyperframes substitute
- Body font: same family
- Visual references from the project: dark panel cards with border, pill-shaped status indicators, circular gamepad buttons (56px, rounded), analog stick circles, the topbar with dot + status text + countdown

## Storyboard
Use the storyboard in `brag-output-2026-06-24-180000/brag-plan.md` as the creative contract.

Scene summary:
1. The Question — 3s — Hook text types in on dark background: "What if your Twitch chat could play your game?"
2. The Token — 4s — Twitch chat overlay: "!play" typed, whisper slides in with token code highlighted in teal
3. The Redeem — 4s — PlayGate room page: code pasted, Redeem pressed, Queue #2 → green Connected + countdown
4. The Gamepad — 5s — Full virtual gamepad assembles, buttons light up, A pressed, live stream visible above
5. The Name — 4s — "PlayGate" in large type, tagline fades in below

## Audio
- Audio role: cinematic support — clean, steady bed with tasteful SFX at key moments
- Audio arc: quiet typing intrigue → small reveal (token) → rising confidence (redeem) → payoff (gamepad) → resolution (logo)
- Music: `happy-beats-business-moves-vol-12-by-ende-dot-app.mp3`
- Music treatment: fade in gently at 0.0s, hold at 0.35 volume through the video, begin fade at 18s, fully out by 20s
- Music cue guidance: vol-12 preset available (see cues JSON), tempo ~110 BPM. Strong cues at 8.74s (gamepad reveal anchor), 13.11s ("Connected" moment), 17.47s (outro logo slam). Beat grid for sequential typing/reveals.
- Audio-reactive treatment: subtle; dark background glow and accent elements breathe gently with RMS. No waveform/equalizer visuals.
- Audio-coupled moments:
  - Scene 1 typing — keyboard keypress sounds (randomized across 8 files)
  - Scene 2 whisper arrival — card-slide SFX
  - Scene 3 redeem press — click SFX; "Connected" landing — impact bell (beat-locked ~13.11s)
  - Scene 4 gamepad assembly — card-fan SFX; A button press — click
  - Scene 5 logo slam — impactBell_heavy (beat-locked ~17.47s)
- SFX selection guidance: prefer low high-frequency-risk sounds. Soft impacts for reveals, clicks for interactions, one bell for each major payoff. Keep SFX at 0.6-0.75 volume.
- SFX analysis guidance: see `sfx-analysis.md` in skill assets
- Exact SFX choice: Hyperframes should choose filenames, timestamps, density, and volume based on the implemented animation.
- Audio files: already copied into `composition/assets/`

## Hyperframes Instructions
Use the current `hyperframes` skill and CLI workflow. Prefer native Hyperframes conventions over anything in `/brag`.

Requirements:
- Show at least one real UI element from the source project (recreate the virtual gamepad, the topbar status, or the token redeem panel).
- Keep all text readable in the final render.
- Keep the video within 15-25 seconds (target: 20s).
- Include the planned music/SFX layer.
- Treat `/brag` audio notes as guidance, not a fixed cue sheet. Choose SFX after the visual animation exists.
- Treat music cue metadata as optional timing hints. Hyperframes decides exact animation timing and should ignore cues that hurt readability, scene pacing, or the product story.
- Major reveals may move toward nearby strong cues within about 0.15s. Smaller entrances may align to nearby beat points within about 0.10s. Use 1-3 strong cue locks in a 15-25s video unless the edit clearly benefits from more.
- Use SFX to support motion and interaction: card sounds for card-like reveals, short announcement cues for major payoffs, key/click sounds for text or user actions, and restraint when the edit is already busy.
- Honor planned music treatment such as fade-outs, ducking, beat-aligned reveals, or letting a final SFX ring over the music, using the best Hyperframes-supported implementation.
- When music is present and the treatment is not `none`, consider Hyperframes audio-reactive workflow: extract audio data and use RMS/frequency bands for subtle, brand-specific motion. Good targets are glow, depth, background warmth, card presence, title emphasis, or other existing visual elements. Avoid waveform/equalizer visuals, musical-note graphics, generic particle systems, strobing, or heavy pulsing.
- Use local assets for audio and any required runtime/media dependencies when possible.
- Run Hyperframes lint and validate before render.
