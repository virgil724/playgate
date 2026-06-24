# Brag Plan: PlayGate

## What is this app?
PlayGate lets Twitch viewers play a streamer's real Nintendo Switch remotely over WebRTC — viewers redeem a token, get a live video stream with a virtual gamepad, and their inputs travel back to the Switch in under 150ms.

## The angle
This is real infrastructure, not a demo. Five Go services, Cloudflare Durable Objects for signaling, NXBT virtual controllers, adaptive bitrate, ed25519 JWT sessions, and a Twitch bot that whispers codes to viewers — all wired together so chat can literally take over the stream. The video should feel like a premium product launch for something that sounds impossible but isn't.

## Hook (first 2-3 seconds)
A bold question, stated clean and serious:
> "What if your Twitch chat could play your game?"

## Key moments (the middle)
- The Twitch chat fires `!play` — a token code slides in via private whisper
- The viewer pastes the code, hits Redeem — queue position appears, then the live stream connects with a green dot and countdown timer
- A full virtual gamepad (dual analog sticks, face buttons, shoulder triggers) appears — the viewer presses A, and on-screen the Switch responds in real time

## Outro / punchline
> "PlayGate"
> "Your chat doesn't just watch anymore."

## User flow worth showing
Twitch chat `!play` → bot whispers token code → viewer pastes code in browser → "Queue #2" → green "Connected" + ⏱90s countdown → virtual gamepad lights up → viewer presses buttons → Switch game responds

## Tone
- Preset: polished
- Creative direction: premium infrastructure product film
- Interpretation: Slow, confident reveals. Each scene holds long enough to land. The product's complexity speaks for itself — the video's job is clarity and weight.

## Format: landscape — 1920x1080
## Duration: 20s

## Visual identity (from the project)
- Background: `#0d0f14` (deep blue-black)
- Accent: `#4f8cff` (bright blue)
- Secondary accent: `#2dd4bf` (teal)
- Text: `#e6e9ef` (near-white)
- Muted: `#8b93a3`
- Display font: system-ui, -apple-system (clean, modern sans-serif)
- Body font: system-ui (same)
- Twitch purple accent: `#9147ff` (for Twitch bot scenes)
- Strongest visual element: the virtual gamepad with its dual stick layout and face button grid against the dark panel background

## Share copy (draft)
Your Twitch chat doesn't just watch your game anymore. They play it.

## Audio direction
- Role: cinematic support
- Music: vol-12 (steady, clean — perfect for polished/cinematic)
- Music treatment: fade in gently at 0.0s, hold at 0.35 volume through the video, begin fade at 18s, fully out by 20s
- Music cue guidance: vol-12 preset available, tempo ~110 BPM. Strong cues at 8.74s (use for gamepad reveal), 13.11s (use for "Connected" moment), 17.47s (use for outro logo slam). Beat grid useful for sequential reveals.
- Audio-reactive treatment: subtle; the dark background glow and accent elements breathe gently with the music's RMS. No waveform/equalizer visuals.
- SFX posture: sparse, tasteful. 3-4 cues at major moments only.
- Audio-coupled moments: token code appearing (card slide), redeem button press (click), connection going live (bell impact), gamepad buttons lighting up (soft clicks)
- Restraint rule: this is not a hype video. Sound supports, never dominates.

## Storyboard

### Scene 1 — The Question — 3s
Dark background. Large, clean text types in center screen:
> "What if your Twitch chat"
> "could play your game?"
Text holds for a beat after typing completes. Subtle blue glow pulses behind.
Sequential/interaction: yes — text types in character by character, line by line
Audio intent: quiet intrigue, the music bed fades in under the typing
Audio-coupled idea: keyboard typing sounds on each character
Music: vol-12 fading in from 0
Transition mood: clean crossfade → Scene 2

### Scene 2 — The Token — 4s
A Twitch chat overlay appears (dark theme, `#0e0e10` background). A message from a viewer types out:
> "user123: !play"
Then a whisper slides in from the right:
> "🤖 Bot whispered: Your code is 5b5f6121"
The code highlights in teal (`#2dd4bf`).
Sequential/interaction: yes — chat message types in, then whisper notification slides in from right with a card-slide sound
Audio intent: a small reveal — something is being handed to the viewer
Audio-coupled idea: typing for chat message, card-slide SFX for whisper arrival
Transition mood: clean slide → Scene 3

### Scene 3 — The Redeem — 4s
The PlayGate room page appears. A token code is pasted into the input field. The "Redeem" button is pressed. The UI transitions:
- "Queue #2" appears in amber (`#f59e0b`)
- Then it changes: green dot lights up, "Connected" appears, countdown "⏱ 90s" ticks in
The topbar status bar animates: dot goes from warn (amber) to ok (green).
Sequential/interaction: yes — paste code → press button → queue position → connected state transition. Simulated button press with pointer cursor.
Audio intent: rising confidence — the system is accepting the viewer
Audio-coupled idea: click on redeem press, then a soft impact bell when "Connected" lands (beat-locked to strong cue ~13.11s)
Music: bed building slightly
Transition mood: clean → Scene 4

### Scene 4 — The Gamepad — 5s
The full virtual gamepad fills the lower half of the frame. Above it, a live game stream plays (abstracted as a dark rectangle with a glowing "LIVE" pill). The gamepad lights up:
- Left analog stick nudges (nub moves)
- Face button "A" lights up in accent blue (`#4f8cff`)
- "⏱ 87s" countdown ticks in the topbar
The viewer is now in control. The gamepad buttons glow subtly with the music's bass.
Sequential/interaction: yes — gamepad elements light up one by one: left stick, d-pad, face buttons, shoulders. Then "A" is pressed and held (turns blue).
Audio intent: this is the payoff — the viewer has the controller
Audio-coupled idea: card-fan sound as gamepad components assemble, soft click on A press (beat-locked to strong cue ~8.74s adjusted to scene start)
Audio-reactive: gamepad button glow and background warmth breathe with RMS
Transition mood: dramatic crossfade → Scene 5

### Scene 5 — The Name — 4s
Cut to clean dark background. "PlayGate" in large, bold display type, centered. Under it, smaller:
> "Your chat doesn't just watch anymore."
Hold for the full scene. Subtle glow behind the name.
Sequential/interaction: none — a confident static hold
Audio intent: resolution, weight. The final bell impact lands here (beat-locked to strong cue ~17.47s)
Audio-coupled idea: impactBell_heavy for the logo slam
Music: holding steady, then begins to fade
Transition mood: hold to black

**Music mood for this video:** clean, steady, confident
**Audio summary:** A quiet build from typing intrigue through token exchange to the gamepad payoff, with a bell impact on the final logo and the music fading under the last line.
