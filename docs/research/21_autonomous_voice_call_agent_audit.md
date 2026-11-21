# Research 21: Outbound Voice Call Agent Feasibility — PSTN/IVR Infrastructure, 90-Minute Hold Gatekeeping, Legal/IRS/Bank Constraints & Sovereign Vault Safety

- **Status:** Complete
- **Date:** 2026-11-20
- **Companion RFC:** [RFC-0026: Autonomous Outbound Voice Call Agent, Zero-Token Long-Hold Gatekeeper & Screened Warm Handoff](../design/0026-autonomous-voice-call-agent.md)
- **Prior Specs Reviewed:** [ADR-0002](../decisions/0002-go-microkernel-binary.md), [ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md), [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0011](../design/0011-ci-pipeline-and-regression-harness.md), [RFC-0012](../design/0012-token-observability.md), [RFC-0014](../design/0014-proactive-engine-and-living-context.md), [RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md), [RFC-0023](../design/0023-front-desk-channel-bridge.md), [RFC-0024](../design/0024-memory-v2-prediction-and-eval.md), [RFC-0025](../design/0025-pluggable-workspace-providers.md)

---

## 1. Executive Summary & Core Findings

Many high-friction personal administration tasks still collapse onto the Public Switched Telephone Network (PSTN): enduring 60–120 minute Internal Revenue Service (IRS) or state agency hold queues, redialing congested customer-service trunks after busy signals or "courtesy disconnects", traversing 5-layer Interactive Voice Response (IVR) keypad trees, disputing ISP/utility billing errors, or rescheduling medical appointments.

This study evaluates the technical, acoustic, legal, identity-verification, and `agy-hq` architectural feasibility of enabling an Antigravity / `agy-hq` personal agent to place outbound phone calls on a user's behalf.

### Five First-Principles Conclusions

1. **Cloud PSTN Call Control Beats Desktop Virtual Audio; Telnyx + Twilio Co-Support Is Optimal:** Binding OS virtual audio loopback devices (PipeWire/PulseAudio null sinks, BlackHole) fails on headless SSH hosts, collides with user calls, leaks system notifications, and lacks carrier-level busy/DTMF/conference primitives. Programmable PSTN APIs (**Telnyx Call Control v2** at `~$0.0105/min` streamed; **Twilio Programmable Voice + Media Streams** at `~$0.0180–$0.0198/min` streamed, up to `24h` call duration) run headlessly over HTTPS + RFC 6455 WebSockets from ~950 LoC of zero-dependency Go stdlib (`Gate H4-A`).
2. **Naive Real-Time Multimodal Streaming Fails Catastrophically on 90-Minute Holds (`$22–$150+` & `15m` Drops):** Upstream Gemini Live API documentation confirms three hard constraints: (a) uncompressed audio-only sessions terminate at **15 minutes** and WebSockets rotate every **~10 minutes** (`GoAway`), (b) persistent sessions **re-bill the accumulated audio context window (`25 tokens/sec = 1,500 tokens/min`) on every turn**, causing ~120 false turns from hold music or comfort messages to compound into **`7.5M–10M` billed audio tokens (`$22.50–$30.00+` on `gemini-3.8-live`, `$150+` on flagship `gpt-realtime`)**, and (c) recorded comfort loops (*"All agents are still busy, please continue to hold"* every 45s) defeat simple energy VADs. Solving long holds requires a **3-Stage Hierarchical Gatekeeper** (`Stage 0: 0-Token Go DSP + Acoustic Loop Fingerprint` $\rightarrow$ `Stage 1: Burst STT via gemini-3.5-transcribe-live + Utterance State Classifier` $\rightarrow$ `Stage 2: On-Demand gemini-3.8-live Wakeup + 4.0s Pre-Roll Ring Buffer`), cutting 90-minute hold AI token spend to **`< $0.06` (`> 99.7%` reduction)**.
3. **IRS (`26 U.S.C. § 6103` / `IRM 21.1.3`) & Bank Fraud Biometrics (`Pindrop` / `Nuance`) Prohibit Full AI Impersonation or Authentication:** An AI bot cannot hold IRS Form 2848 Power of Attorney, cannot pass IRS taxpayer authentication without the taxpayer on the line, and will trigger bank synthetic-voice spoofing flags (`Pindrop` liveness scoring) if it attempts authenticated banking dialogue. Conversely, **navigating unauthenticated IVRs, enduring hold queues, and executing a Warm Conference Handoff the instant a live representative answers is 100% compliant and eliminates 90% of user pain**.
4. **Screened Warm Transfer (`Press 1 to Join`) Prevents the Mobile Voicemail Catastrophe:** When a live IRS representative finally answers at minute 94, blindly bridging the user's mobile phone into the PSTN conference risks the user's cellular voicemail (*"Hi, leave a message..."*) answering after 15 seconds and causing the IRS agent to hang up. `hq` must hold the representative with a polite 1-sentence live-agent stall while dialing the user's mobile on an isolated whisper/screening leg that bridges into the conference **only after the user presses `1`** (backed by [RFC-0023](../design/0023-front-desk-channel-bridge.md) `TierInterrupt` push alerts).
5. **Clear MVP Winner — Build `"Hold-Waiter, IVR Navigator & Screened Warm Bridge"` Before `"Full Autonomous Negotiator"`:** A `hold_and_bridge` MVP solves the highest-value real-world pain (IRS, airlines, banks, insurers, utilities) at `~$1.07` all-in for a 100-minute call (`~$0.04` when catching an automated queue callback), with near-zero hallucination or unauthorized-commitment risk, while laying the exact substrate needed to unlock `autonomous_low_stakes` calls (appointments, store checks, routine billing inquiries) as an opt-in policy mode.

---

## 2. Dimension 1: Telephony, IVR Navigation, Caller ID & Warm Transfer Infrastructure

### 2.1 PSTN Programmable Voice & SIP/WebRTC Transport Comparison

| Capability / Metric | Telnyx Call Control v2 + Media Streaming | Twilio Programmable Voice + Media Streams | Raw SIP + SRTP (`LiveKit SIP` / `Asterisk`) | Managed Voice SaaS (`Vapi` / `Retell` / `Bland`) |
| :--- | :--- | :--- | :--- | :--- |
| **Outbound US Voice + Media Stream Rate** | **`$0.0105 / min`** (`$0.005` trunk + `$0.002` Call Control + `$0.0035` WSS stream; `60s` min) | **`$0.0180 / min`** (`$0.0140` US outbound + `$0.0040` Media Streams; `+$0.0018/leg` in `<Conference>`) | **`$0.005–$0.007 / min`** (carrier SIP trunk only; `$0` media stream surcharge) | **`$0.11 – $0.25+ / min`** (`$0.05–$0.055/m` platform fee + passthrough LLM/TTS/telephony) |
| **Max Single-Call Duration** | Configurable via `time_limit_secs` (`<= 14,400s` / `4h+`) | **`24 hours`** max (`timeLimit` per-call override) | Unlimited (SIP session timers `RFC 4028`) | Typically capped at `1h–2h`; bills platform fee every hold minute |
| **Wire Audio Codecs** | `PCMU` (`8kHz` G.711 $\mu$-law), `PCMA`, **`L16` (`16kHz` PCM16 native)**, `AMR-WB` | `audio/x-mulaw` (`8kHz` G.711 $\mu$-law only on `<Connect><Stream>`) | `G.711`, `G.722` (`16kHz`), `Opus` over SRTP | Abstracted in vendor cloud |
| **Mid-Call Outbound DTMF** | **Native out-of-band REST** (`POST /v2/calls/{id}/actions/send_dtmf`, zero stream drop) | **Asymmetric:** no outbound `dtmf` WS frame on `<Connect><Stream>`; requires `<Conference>` REST leg or in-band Q.23 PCM synthesis | Native `RFC 2833 / 4733` RTP `telephone-event` | Vendor tool call (often flaky on fast multi-digit PINs) |
| **Warm Transfer (`Whisper` / `Barge` / `Handover`)** | Native Conference `supervisor_role: "whisper" \| "barge"` (`$0.002/leg/m`) | Native `<Conference coaching="true">` + participant mute/drop (`$0.0018/leg/m`) | Full local PBX bridge (`chan_pjsip` / LiveKit Room) | Basic `<Dial>` transfer; weak screened whisper-to-handover |
| **Pure-Go `bin/hq` Feasibility (`Gate H4-A`)** | **Yes (`~300 LoC` HTTPS + RFC 6455 WS)** | **Yes (`~300 LoC` HTTPS + TwiML + WS)** | **No in Ring 0** (`>15k LoC` SIP/ICE/SRTP stack; suited for companion pack `hq-voice-sip`) | Yes, but breaks Vault PII sovereignty |

### 2.2 Multi-Layer IVR Navigation & DTMF Nuances

Real-world institutional IVRs combine legacy touch-tone menus, non-bargeable legal notices, and speech-recognition routers (e.g., IRS Natural Language IVR):

1. **Out-of-Band RFC 4733 vs. In-Band ITU-T Q.23 DTMF:**
   - Many enterprise SIP IVRs negotiate `telephone-event/8000` (RFC 4733) and ignore or distort in-band audio tones compressed through G.711 $\mu$-law, while older TDM bridges require clean in-band dual-sine energy.
   - **Telnyx** solves this cleanly via `POST /v2/calls/{call_control_id}/actions/send_dtmf` (`{"digits": "1w2#", "duration_ms": 250}`), which emits carrier-grade RFC 4733 events without interrupting the bidirectional media WebSocket.
   - **Twilio** `<Connect><Stream>` lacks an outbound JSON `dtmf` WebSocket message, and updating the call via `<Play digits="..."/>` tears down and reconnects the `<Stream>` (`300–600ms` blind spot). Placing Twilio calls inside a `<Conference>` from `t=0` (with the AI media stream attached as a conference participant, or synthesizing clean ITU-T Q.23 dual-sine tones—`697/770/852/941 Hz` + `1209/1336/1477 Hz` at `-6 dBm0` per twist, `160ms` mark / `80ms` space—as fallback) avoids stream teardown.
2. **Menu Enumeration Patience & Barge-In Discipline:**
   - Standard conversational VAD (`silenceDurationMs = 500ms`) causes voice agents to press a key after *"For Spanish, press 2..."* before hearing *"For individual tax account inquiries, press 4"* after a `900ms` pause.
   - In `IVR_NAV` state, endpoint silence patience must widen to **`1,600ms`**, and keypad `send_dtmf` must always be preferred over spoken responses whenever numeric options are offered (eliminating PSTN background-noise misrecognition).
3. **IVR Loop & Dead-End Detection:**
   - Normalized Levenshtein similarity (`> 0.88`) across 3 consecutive IVR menu prompts without state progression indicates a menu loop or invalid input, triggering automatic fallback (`0` / `"Representative"`) or clean escalation rather than infinite keypresses.

### 2.3 Busy Signals, IRS "Courtesy Disconnects" & Queue Callback Handling

Long-hold government and airline trunks exhibit three distinct pre-agent failure/shortcut patterns:

| Queue Phenomenon | Carrier / Acoustic Signal | Optimal `agy-hq` Handling |
| :--- | :--- | :--- |
| **Trunk Busy / Congestion** | SIP `486 Busy Here` or `503 Service Unavailable` (`0s` billed duration) | Bounded redial scheduler inside approved `CallPlan` (`max_redials <= 5`, exponential backoff `60s, 120s, 240s` + `±20%` jitter; `$0.00` carrier cost on unanswered legs). |
| **IRS "Courtesy Disconnect"** | Call answers (`200 OK`), plays *"Due to extremely high call volume, we cannot handle your call right now. Please try again later"*, then hangs up (`BYE`) | Stage 1 burst classifier tags `COURTESY_DISCONNECT` (distinguishing it from a completed call); waits `10–15m` cooldown window if retry budget remains, or surfaces structured status to user. |
| **Automated Queue Callback Offer** | IVR announces *"Your estimated wait is 74 minutes. Press 1 to receive a callback at the number you are calling from, or press 2 to enter a different callback number"* | Stage 1 classifier tags `QUEUE_CALLBACK_OFFER`. If `CallPlan.AllowQueueCallback == true`, presses `1` (if outbound Caller ID is user's Verified Mobile) or enters user's `callback_mobile` via `dtmf_only` Vault field, confirms registration, hangs up (`saving ~$0.90+` in hold minutes), and transitions task to `WaitingOn` (`inbound PSTN callback`). |

### 2.4 Caller ID, ANI Matching & STIR/SHAKEN Reputation

Under FCC STIR/SHAKEN mandates, terminating carriers assign three attestation tiers:

- **Attestation Level A (Full):** Carrier verified customer KYC identity **and** confirmed customer owns/leased the outbound E.164 number natively on that carrier. Requires Business/Sole-Proprietor KYC vetting (EIN or DBA registration in Twilio Trust Hub / Telnyx KYC) plus a native `$1.00/mo` DID. Needed when calling local 10-digit businesses (clinics, contractors, restaurants) so outbound calls do not display `"Spam Likely"`.
- **Attestation Level B (Partial) via Verified Personal Caller ID (`$0.00/mo`):** Both Twilio (`OutgoingCallerIds`) and Telnyx allow verifying ownership of the user's existing personal mobile number via OTP. Outbound calls present the user's real mobile number (`+16505550142`), which receives **Attestation Level B** (because the VoIP carrier did not allocate the wireless SIM).
  - **Key Tradeoff:** Toll-free enterprise IVRs (`1-800/888/877/...`, including IRS, airlines, utilities) accept Level B without blocking, **and** presenting the user's real mobile number unlocks automatic ANI account lookup and seamless `"Press 1 to callback this number"` queue callbacks!
- **The Self-Call Anti-Loop Constraint on Warm Transfers:** Wireless carriers (AT&T, Verizon, T-Mobile) drop or route straight to voicemail any inbound call to `+16505550142` whose `From` header is also `+16505550142`. Therefore, **Mobile PSTN Warm Transfer requires one dedicated `$1.00/mo` carrier DID (`bridge_caller_id_e164`)** used exclusively to dial the user's mobile leg into the conference (which the user saves in iOS/Android Contacts as `"HQ Voice Bridge — Emergency Bypass"` so it rings through Focus/Silent mode!).

### 2.5 NAT-Friendly Local Media Ingress

Because `agy-hq` runs local-first behind residential NAT/CGNAT while Twilio/Telnyx initiate an *inbound* `wss://` connection to stream audio, three zero-standing-port ingress options fit `agy-hq`:

1. **Ephemeral Per-Call Tunnel (`cloudflared` / `tailscale funnel` — Default):** Spawned *only* after `hq approve`, bound to `127.0.0.1:0` with a single-use `256-bit` URL capability path (`/ws/<32-byte-hex>`), Ed25519/HMAC carrier webhook signature verification, an `atomic.Bool` single-socket lock, and immediate `SIGKILL` upon call hangup (`0` open ports at rest).
2. **User-Owned Stateless Edge Relay (`<120 LoC` Cloudflare Worker + Durable Object — Optional):** `hq` dials *outbound-only* `wss://` to the user's Durable Object room; carrier connects to the same room. Zero local tunnel binary required; `~12ms` relay hop.
3. **Outbound `sips:` Companion Pack (`hq-voice-sip` via `hq-connector-protocol/v1`):** Outbound `SIP INVITE` over TLS (`5061`) + symmetric SRTP; zero public URLs.

### 2.6 Screened Warm Transfer (`Whisper` $\rightarrow$ `Press-1 Screen` $\rightarrow$ `Barge / Handover`)

When a human representative answers after a 90-minute hold, unscreened call forwarding fails if the user is away from their phone for 15 seconds and mobile voicemail answers the conference. The robust carrier pattern is a **Two-Conference / Screened Coaching Bridge**:

1. **Representative Stall (`t = 0s`):** Call is already inside a carrier conference (or bridged immediately). `gemini-3.8-live` receives the `4.0s` pre-roll audio (capturing *"IRS, this is Agent Miller, ID 1004821"*) and speaks a natural, non-deceptive stall: *"Hello Agent Miller, I'm an automated assistant holding the line for Alice Chen—connecting Alice onto the line right now, please hold just ten seconds."*
2. **Parallel Multi-Channel Alert + Screened Mobile Dial (`t = 0.1s`):** `hq` fires an immediate `TierInterrupt` notification ([RFC-0023](../design/0023-front-desk-channel-bridge.md) mobile channel bridge + `:4242` desktop console alert) **and** places an outbound carrier call from `bridge_caller_id_e164` to `user_mobile_e164` with `muted: true` on entry.
3. **Human Presence Verification (`Press 1` Gate):** On the user's mobile leg (isolated from the representative), carrier TTS whispers: *"Live IRS representative Agent Miller on the line after 88 minutes. Press 1 to take over, or press 2 for AI whisper."*
   - If cellular voicemail answers, voicemail never presses `1`, so voicemail greetings **never leak onto the IRS line**! If unanswered after `22s`, `hq` retries mobile once while keeping the polite stall active up to `45s` (or user joins via `:4242` desktop control).
   - The instant the user presses `1`, `hq` **pauses all AI transcription/audio capture** (protecting subsequent SSN/DOB verification), unmutes the user's mobile leg into the conference, and drops the AI media stream cleanly (`handover`), leaving a pure 1:1 PSTN call between the user and the representative.

---

## 3. Dimension 2: Voice AI Stack & Solving the 90-Minute Hold-Music Problem

### 3.1 Gemini Live API Capabilities & Pricing Audit (Upstream Verified)

From upstream Google Gemini API documentation (`gemini-3.8-live`, `gemini-3.8-live-extended-thinking`, `gemini-3.5-transcribe-live`):

| Model ID | Role in `agy-hq` Voice Stack | Paid Tier Pricing (USD) | Hard Session & Protocol Mechanics |
| :--- | :--- | :--- | :--- |
| **`gemini-3.8-live`** | Primary low-latency S2S dialogue & IVR navigation | Audio In: **`$3.00 / 1M`** (`~$0.0045/min` at `25 tok/s`)<br>Audio Out: **`$12.00 / 1M`** (`~$0.018/min`)<br>Text In/Out: `$0.75 / $4.50` per `1M` | `16kHz` PCM16 in / `24kHz` PCM16 out; async tools (`behavior: "NON_BLOCKING"`); full-lifecycle `send_client_content`; `15m` uncompressed audio cap; `~10m` WS lifetime (`GoAway` + `sessionResumption` `2h` token TTL). |
| **`gemini-3.8-live-extended-thinking`** | Optional complex multi-constraint commercial negotiation | Same token rates (thinking tokens billed at `$12.00 / 1M` output rate) | Streams natural conversational fillers while reasoning/running async tools; turn lifecycle tracked via `interaction_status` (`IN_PROGRESS` vs. `IDLE`). |
| **`gemini-3.5-transcribe-live`** | **Stage 1 Hold-Break Speech Burst Classifier** | Audio In: **`$3.50 / 1M`** (`~$0.005/min`)<br>Text Out: **`$21.00 / 1M`** (`~$0.004/min`)<br>Blended: **`~$0.009 / min`** of active speech | `response_modalities: ["TEXT"]`, `smart` or `verbatim` mode, `custom_vocabulary` speech biasing, Hybrid VAD (`audioStreamEnd`), **zero spoken audio output hazard**. |

### 3.2 Why Naive S2S Streaming Blows Up on a 90-Minute Hold (The Compounding Math)

Upstream Gemini Live billing documentation specifies a **compounding per-turn context model**:
> *"The API charges you per turn for all tokens present in the session context window... Past tokens are re-processed and accounted for in each new turn, up to your configured context window size (`128k` input tokens)."*

Consider what happens if a developer naively pipes a **90-minute IRS hold call** continuously into an S2S WebSocket (`gemini-3.8-live` or `gpt-realtime`):

1. **Raw Token Accumulation:** `90 min × 60 sec × 25 tokens/sec = 135,000 audio tokens` (exceeding the `15m` uncompressed limit and `128k` context window unless `ContextWindowCompressionConfig` `SlidingWindow` is tuned).
2. **120 False Turns from Music & Comfort Loops:** Institutional hold queues interrupt hold music every `30–60s` with a recorded voice announcement (*"Thank you for continuing to hold..."*), plus violin/saxophone crescendos that trip standard neural VADs (`~1.33 turns/min ≈ 120 turns` across `90m`).
3. **Quadratic Context Re-Billing:** Even with a `64k`-token sliding window (`~42 minutes` retained), re-billing an average of `~52,000` context audio tokens across `120` false turns consumes **`~6.2M` input audio tokens (`~$18.60`) + false output/transcription tokens = `$20–$30+` per call** on Gemini Live—and **`$150+`** on uncached flagship OpenAI `gpt-realtime` (`$32/1M` audio in)! Meanwhile, managed wrappers (`Vapi` / `Retell`) charge `$0.12–$0.23/min` continuously across all 90 minutes (**`$10.80–$20.70`**) just to listen to Cisco `Opus No. 1` hold music.

### 3.3 The 3-Stage Hierarchical Hold Gatekeeper (`< $0.06` AI Cost for 90 Minutes)

To eliminate both quadratic token compounding and the `120x` comfort-announcement false-alarm problem, `agy-hq` decouples **hold monitoring** from **conversational S2S** via a 3-stage pipeline:

```
PSTN 8kHz/16kHz PCM
   │
   ▼  (Always buffering last 4.0s in local RAM circular ring buffer)
┌───────────────────────────────────────────────────────────────────────────┐
│ Stage 0: Pure-Go Local DSP & Loop-Hash Filter (0 LLM tokens · $0.000/hr)  │
│  • 20ms frames: RMS energy, Zero-Crossing Rate, spectral flatness         │
│  • 3–8 Hz syllabic envelope modulation index (speech > 0.35 vs music)     │
│  • Quantized 64-bit sub-band energy shimmer hash: memorizes repeating     │
│    hold-music & pre-recorded comfort-loop chunks after 1st occurrence!    │
└──────────────┬────────────────────────────────────────────────────────────┘
               │ Novel speech-like cadence detected (> 350ms, non-cached hash)
               ▼
┌───────────────────────────────────────────────────────────────────────────┐
│ Stage 1: Burst STT + Utterance State Classifier (~$0.009/min speech only) │
│  • Streams 4.0s pre-roll + speech burst (2–8s) to gemini-3.5-transcribe   │
│  • Normalizes transcript & checks local phrase/hash cache                 │
│  • Classifies into 5 deterministic states:                                │
│    1. HOLD_COMFORT_LOOP  ("all agents are busy, continue to hold") -> Stage 0│
│    2. QUEUE_CALLBACK     ("press 1 for a callback") -> Policy DTMF / Stage 0│
│    3. COURTESY_DISCONNECT("high call volume, try later") -> Redial / End  │
│    4. IVR_PROMPT         ("press 2 for billing") -> Wake Stage 2          │
│    5. LIVE_HUMAN_PICKUP  ("this is Agent Lee, ID...") -> Wake Stage 2 +   │
│                           Fire TierInterrupt + Dial Screened Mobile Bridge│
└──────────────┬────────────────────────────────────────────────────────────┘
               │ Only on IVR_PROMPT or LIVE_HUMAN_PICKUP
               ▼
┌───────────────────────────────────────────────────────────────────────────┐
│ Stage 2: On-Demand gemini-3.8-live Session (Clean Context · Zero Hold Bloat)│
│  • Opens fresh WSS (or resumes handle if < 2m gap) seeded ONLY with       │
│    compact text CallContextSummary (< 300 tokens) + 4.0s pre-roll PCM     │
│  • Executes IVR navigation, polite rep stall, or bounded low-stakes task  │
│  • Closes WSS immediately whenever call re-enters HOLD_WAIT or HANDOVER   │
└───────────────────────────────────────────────────────────────────────────┘
```

**Why Stage 0 + Stage 1 Beats Both Pure DSP and Pure LLM:**
- Over a **90-minute IRS hold**, ~82 minutes are silence, ringback, or looping music (`Stage 0` drops 100% locally in `0` network bytes).
- Of the ~8 minutes of recorded comfort announcements (`~100` plays of 2–3 rotating recordings), `Stage 1` transcribes each unique recording **once** (`~18 seconds` of total speech = **`$0.0027`**), caches its normalized text **and** acoustic sub-band hash, and suppresses the remaining 97 repeats at `Stage 0` (`$0.000`).
- Because `Stage 1` uses `response_modalities: ["TEXT"]` (`gemini-3.5-transcribe-live`), the model **physically cannot speak onto the PSTN line** during hold—eliminating the embarrassing failure mode where a voice bot says *"You're welcome! How else can I help?"* to a recorded hold message.
- When a real human representative answers (`*"Internal Revenue Service, Mr. Patel speaking, badge 1009312, how may I help you?"*`), `Stage 1` sees non-templated live greeting structure + interactive pause in `< 450ms`, flushes the `4.0s` pre-roll + transcript into `Stage 2` (`gemini-3.8-live`), captures the agent's name and badge ID verbatim in the redacted call log, plays the polite hold-handoff greeting, and triggers the screened mobile bridge.

---

## 4. Dimension 3: Legal, Regulatory & Counterparty Identity Constraints

### 4.1 California All-Party Recording Consent & AI Transparency Laws

Because `agy-hq` may operate from or place calls touching **California** (one of 13 U.S. all-party wiretapping/recording consent jurisdictions alongside FL, PA, IL, MA, WA, MD, CT, MI, MT, NH, NV, OR):

1. **California Invasion of Privacy Act (`Cal. Penal Code §§ 632, 632.7`; *Smith v. LoanMe, Inc.*, 11 Cal. 5th 183 (2021)):**
   - Prohibits intentionally recording or eavesdropping upon a confidential communication (or cellular/cordless phone communication under `§ 632.7`) without the consent of **all parties**. Statutory civil damages are `$5,000` per violation (`Cal. Penal Code § 637.2`).
   - **Architectural Safeguard (`record: "none"` Default):** By default, `hq` disables carrier recording (`Record=false` at Telnyx/Twilio), writes **zero** `.wav`/`.mp3`/PCM audio files to disk, overwrites its `4.0s` local RAM pre-roll buffer continuously in volatile memory, and persists **only** redacted structured notes/summaries (akin to a human secretary taking contemporaneous written notes, while still giving upfront AI assistant notice).
   - **Opt-In Recording (`record: "disclosed_all_party"`):** When a user explicitly requests audio recording/verbatim transcript capture in `hq call propose`, Ring-0 Go injects a mandatory non-skippable carrier greeting (*"Hello, this is an automated assistant calling on behalf of [User Name]; please note this call is being recorded and transcribed"*) **before** audio persistence starts.
2. **California Bolstering Online Transparency ("BOTS") Act (`Cal. Bus. & Prof. Code § 17940 et seq.`) & State AI Disclosure Laws (Utah SB 149/226, Colorado AI Act):**
   - Prohibits using an automated bot to communicate or interact with a person in California with intent to mislead the other party about its artificial identity in commercial or election contexts.
   - **Hard Invariant (`Zero Human Impersonation & Zero Voice Cloning`):** `agy-hq` **forbids cloning the user's own voice** (`voice_cloning: prohibited` in Ring 0) and requires every live-human interaction to begin with explicit automated-assistant identification (*"Hello, I am an automated assistant calling on behalf of..."* or *"holding the line on behalf of..."*).
3. **Federal TCPA (`47 U.S.C. § 227`) & `FCC 24-17` Declaratory Ruling (Feb 2024):**
   - `FCC 24-17` confirmed that AI-generated voices constitute `"artificial or prerecorded voices"` under the TCPA. The TCPA restricts **unsolicited telemarketing, cold outreach, and bulk robocalls to residential/wireless consumers** without prior express (written) consent.
   - Placing **individual, single-concurrency (`max_concurrent_calls: 1`), user-initiated calls** to published government agency lines, toll-free customer support queues, or businesses with whom the user has an established relationship or specific inquiry—with clear caller identification, local `08:00–21:00` calling-hours enforcement, and zero marketing content—aligns with personal concierge / assistive telecommunications use while mechanically blocking bulk dialing or campaign lists.

### 4.2 IRS, Government Agency & Bank/Brokerage Identity Rules

A critical mistake in generic "AI phone agent" demos is assuming an AI can talk to the IRS or Chase Bank the same way it books a haircut. Federal statutes and financial fraud systems draw a bright line:

| Counterparty Category | Governing Rules & Anti-Bot Systems | Can AI Navigate Unauthenticated IVR & Hold? | Can AI Authenticate Identity (`SSN`/`EIN`/`DOB`) or Discuss Account? | Mandatory `agy-hq` `policy_mode` |
| :--- | :--- | :--- | :--- | :--- |
| **IRS & State Tax Agencies** (`1-800-829-1040`, FTB) | **`26 U.S.C. § 6103`** (FTI confidentiality), **`Treasury Circular 230`**, **`Form 2848` / `8821`**, **`IRM 21.1.3.2.3`** (Oral Disclosure & Taxpayer Auth), post-`enQ` anti-robo-queue policies | **Yes** (single-line call for the taxpayer; navigate topic prompts & hold silently) | **NO.** AI is ineligible for Form 2848 POA; CSR cannot authenticate SSN/DOB/prior-year AGI or disclose tax account data without the taxpayer personally speaking on the line. | **`hold_and_bridge_only`** (mandatory; `SSN`/`EIN`/`DOB`/`AGI` locked to `human_takeover_only`) |
| **Banks & Brokerages** (Chase, Fidelity, Morgan Stanley, Schwab) | **GLBA**, institution fraud rules, **`Pindrop`** (deepfake/TTS artifact + VoIP SIP path scoring), **`Nuance Gatekeeper`** (passive voiceprint match) | **Caution / Partial** (silent listening + keypad DTMF only; avoid synthetic speech on fraud-monitored IVRs) | **NO.** Synthetic TTS speaking for the account holder risks triggering a `Pindrop Red` voice-spoofing alert and **freezing the user's financial account**. | **`hold_and_bridge_only`** (`dtmf_only` IVR allowed; `0` synthetic speech until human rep pickup stall + immediate bridge) |
| **Healthcare & Insurance** (Clinics, UnitedHealthcare, pharmacies) | **HIPAA Privacy Rule (`45 CFR § 164.510(b)`)** | **Yes** | **Appointment scheduling / generic benefits check:** often allowed with subscriber ID/DOB if authorized; **clinical/PHI discussion:** requires patient on line. | **`hold_and_bridge_only`** default (`autonomous_low_stakes` opt-in for basic scheduling) |
| **Routine Commercial & Local Services** (ISPs, airlines, utilities, restaurants, auto shops) | Standard consumer agency law + state AI disclosure | **Yes** | **Yes** (within explicit `CallPlan` guardrails and `dtmf_only` / `spoken_redacted` Vault fields; escalate on unexpected fees/cancellations). | **`hold_and_bridge`** or **`autonomous_low_stakes`** |

> **Lesson from the IRS vs. `enQ` Precedent:** Commercial services like `enQ` provoked IRS countermeasures because they dialed **hundreds of speculative robo-calls in parallel** onto Practitioner Priority lines without a specific taxpayer on deck, auctioning off queue positions to third-party CPAs. `agy-hq` enforces `max_concurrent_calls: 1` for a single human user after explicit per-call `hq approve`, never pre-stuffs queues, accepts native IRS Automated Callbacks whenever offered, and puts the actual taxpayer onto the line within seconds of representative pickup.

---

## 5. Dimension 4: `agy-hq` Safety, Vault PII Quarantine & Kernel Fit

### 5.1 Preserving Every Post-RFC-0025 `agy-hq` Invariant

To integrate cleanly into `main` without violating any CI gate in [`internal/ci/harness.go`](../../internal/ci/harness.go) or [`internal/ci/slop.go`](../../internal/ci/slop.go):

1. **Zero External Go Dependencies (`ADR-0002`, `Gate H4-A`):** [`go.mod`](../../go.mod) remains 100% pure Go stdlib. Carrier HTTPS REST (`net/http`), RFC 6455 WebSockets (`crypto/tls`, `bufio`, `net`), Ed25519/HMAC webhook verification (`crypto/ed25519`, `crypto/hmac`), ITU-T G.711 $\mu$-law + FIR `8kHz <-> 16kHz/24kHz` resampling + Q.23 DTMF synthesis (`math`), and Stage 0 DSP hold-music detection fit cleanly in `internal/voice/` (`~950 LoC`).
2. **Zero Monolith Ceiling Regressions (`Gate H5-F`):** [`internal/ci/slop.go`](../../internal/ci/slop.go) enforces strict LOC ratchets on `internal/kernel/kernel.go` (`2015`), `cmd/hq/main.go` (`615`), `internal/hooks/hooks.go` (`415`), `internal/webui/webui.go` (`1115`), and `internal/kernel/workspace_lifecycle.go` (`1075`). All voice CLI verbs live in `cmd/hq/call_cli.go` (1 dispatch line in `main.go`), hook surfaces/guards live in `internal/hooks/voice_surfaces.go`, and `:4242` SSE Live Call Drawer endpoints live in `internal/webui/calls.go`.
3. **Strict Store Lock Ranking (`Gate H5-C` & `HQ_STRICT_LOCK_RANK=1`):** Active/recent call state in `<vault>/memory/calls.json` acquires a dedicated `store.RankCalls = 68` in [`internal/store/store.go`](../../internal/store/store.go) (ordered cleanly above `RankEvents = 40`, `RankGoals = 50`, `RankOutbox = 60`, `RankChannels = 65` and below `RankTaint = 70`, `RankTasks = 75`, `RankMemory = 78`), and `"calls.json"` is registered in `lockRankedVaultJSONFiles` so raw unlocked `os.WriteFile` calls are statically forbidden.
4. **Ring-0 TCB Protection:** `"internal/voice/"` and `"templates/connectors/voice/"` are added to `Ring0ProtectedPaths` in [`internal/rsi/tcb.go`](../../internal/rsi/tcb.go) so RSI self-improvement can never weaken emergency/toll-fraud firewalls, consent disclosures, or PII redaction.

### 5.2 Three-Tier Vault PII Quarantine (`<vault>/private/calls/`)

Sensitive identity and account credentials live strictly inside `<vault>/private/calls/profiles/<slug>.json` (`0600`, blocked from `read_file`, `grep_search`, and shell `cat` across **all** LLM sessions by [`internal/hooks/pre_tool.go`](../../internal/hooks/pre_tool.go)):

- **Tier 1 — `dtmf_only` (Account #, Member ID, Numeric PIN, `ssn_last4`, Callback Number):** Injected out-of-band onto the PSTN leg by Go (`send_vault_dtmf(field_name)`). **Zero digits ever enter Gemini/OpenAI prompts or Antigravity transcripts.** Logged as `[REDACTED:DTMF:account_number]`.
- **Tier 2 — `spoken_redacted` (Service Address, Billing ZIP, Non-Bank Appointment DOB):** Returned ephemerally *only* inside the isolated voice WebSocket when explicitly listed in the approved `CallPlan`, and scrubbed in real time (exact, digit-by-digit, and spoken-word forms) via an in-memory Aho-Corasick normalizer before writing to `<vault>/calls/` logs or `:4242` SSE streams.
- **Tier 3 — `human_takeover_only` (`ssn`, `ein`, `itin`, `prior_year_agi`, `card_number`, `cvv`, `bank_account`, `otp_code`):** **Hard-coded non-downgradable in Ring 0.** Even if a user or prompt attempts to mark `ssn` as `spoken_redacted`, `internal/voice/plan.go` rejects it at `hq call propose` time. Any counterparty request for a Tier 3 secret immediately triggers `request_human_takeover`, and **all AI listening, transcription, and logging hard-pause the millisecond the user joins the bridge**.

### 5.3 Multi-Channel Interrupt Escalation & Tainted Post-Call Summary

- **Pre-Call Approval (`hq call propose` + `ask_question`):** Built-in rule `phone_call` (`mode: always` in [`internal/approvals/policy.go`](../../internal/approvals/policy.go)) blocks all unapproved dialing (`hq call dial-plan`, `api.telnyx.com/v2/calls`, `api.twilio.com/.../Calls`). The proposal binds target E.164, `policy_mode`, `max_duration`, `max_cost_usd` (enforced both locally and via carrier `time_limit_secs`), `max_redials`, recording consent mode, and allowed Vault field handles under SHA-256.
- **Live Pickup Escalation:** When `Stage 1` detects `LIVE_HUMAN_PICKUP` (or `Stage 2` invokes `request_human_takeover`), `hq` simultaneously: (1) rings the user's mobile via Screened Conference Bridge (`Press 1`), (2) emits a `TierInterrupt` outbox event ([RFC-0014](../design/0014-proactive-engine-and-living-context.md)) fanned out to any configured [RFC-0023](../design/0023-front-desk-channel-bridge.md) mobile channel bridge with `bca_` quick-action buttons, and (3) flashes the `:4242` / AuxPane Live Call Drawer.
- **Quarantined Post-Call Artifacts ([RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md)):** Every call's redacted transcript and summary (`<vault>/calls/YYYY-MM-DD/<call_id>/summary.json`, `0600`) carry `tainted: true` (`source: "pstn:<e164>"`), neutralizing audio prompt injection from untrusted remote speakers while attributing carrier seconds, hold time saved, and token USD to `hq obs` ([RFC-0012](../design/0012-token-observability.md)).

---

## 6. Dimension 5: CUJs, Real-World Dollar Cost Model & MVP Recommendation

### 6.1 End-to-End Dollar Cost Model Across Four Real-World Call Archetypes

Rates used (verified November 2026):
- **Telnyx PSTN + WSS Stream:** `$0.0105/min` (`+$0.009/min` 2nd PSTN + conference leg during human bridge).
- **Twilio PSTN + Media Streams + Conference:** `$0.0198/min` (`+$0.0158/min` 2nd PSTN + conference leg during human bridge).
- **`hq` 3-Stage Gatekeeper + Gemini Live:** `Stage 0` hold DSP = `$0.000/min`; `Stage 1` `gemini-3.5-transcribe-live` novel bursts = `$0.009/min` of novel speech (`< 30s` per `90m` hold $\approx$ `$0.005`); `Stage 2` `gemini-3.8-live` active dialogue = `~$0.022/min` (fresh short-context session); **`$0.000/min` AI cost once handed over 1:1 to human**.
- **Baselines for Comparison:** Naive continuous `gemini-3.8-live` (compounded `64k` sliding window across comfort-loop turns); Managed Voice SaaS (`Vapi` / `Retell` at `$0.15/min` blended).

| Call Archetype | Telnyx + `hq` Gatekeeper + Gemini 3.8 Live *(Recommended)* | Twilio + `hq` Gatekeeper + Gemini 3.8 Live | Naive S2S *(No Hold Gatekeeper, Compounded)* | Managed SaaS (`Vapi` / `Retell` @ `$0.15/m`) |
| :--- | :--- | :--- | :--- | :--- |
| **1. Painful IRS Hold + Warm Bridge (`100m` total)**<br>*`2m` IVR + `88m` hold + `10m` 1:1 user/CSR (`AI dropped`)* | **`$1.07`**<br>*(`$0.95` PSTN hold/IVR + `$0.06` AI + `$0.06` 1:1 PSTN legs)* | **`$1.91`**<br>*(`$1.78` PSTN + `$0.06` AI + `$0.07` 1:1)* | **`$23.80+`**<br>*(`$0.95` PSTN + `~$22.85` compounded audio tokens!)* | **`$15.00`**<br>*(plus PII/FTI exposure if not dropped)* |
| **1b. IRS Queue with Automated Callback (`3m` active)**<br>*`2m` IVR + catches `"Press 1 for callback"` at `m=3`* | **`$0.05`**<br>*(`3m` PSTN `$0.032` + `$0.02` AI; IRS calls user mobile directly!)* | **`$0.08`** | `$0.12` (often misses callback prompt) | `$0.45` |
| **2. Airline / ISP Hold + Autonomous/Bridge (`50m` total)**<br>*`2m` IVR + `43m` hold + `5m` live agent resolution* | **`$0.68`**<br>*(`$0.53` PSTN + `$0.15` AI)* | **`$1.14`**<br>*(`$0.99` PSTN + `$0.15` AI)* | **`$8.90+`** | **`$7.50`** |
| **3. Busy-Signal Redials + Hold (`5x` busy + `60m` queue)**<br>*`5` SIP `486` redials (`$0`) + `55m` hold + `5m` human bridge* | **`$0.71`**<br>*(`$0.00` busy attempts + `$0.63` PSTN + `$0.08` AI/bridge)* | **`$1.25`** | **`$13.40+`** | **`$9.00`** |
| **4. Short Routine Errand (`3m` total)**<br>*Appointment reschedule / store inventory check* | **`$0.10`**<br>*(`$0.032` PSTN + `$0.068` AI)* | **`$0.13`** | **`$0.11`** | **`$0.45`** |

*(Fixed monthly carrier overhead: **`$1.00 / month`** for 1 native DID `bridge_caller_id_e164` used to ring the user's mobile cleanly during Warm Transfer, paired with `$0.00/mo` Verified Personal Caller ID—or `$2.00/mo` for 2 native DIDs under Sole-Proprietor Level A attestation.)*

### 6.2 Product & Architecture Contrast: `"Hold-Waiter + Warm Transfer"` MVP vs. `"Full Autonomous Agent"`

| Evaluation Axis | Option A: `"Hold-Waiter, IVR Navigator & Screened Warm Transfer"` *(Recommended Phase 2A/2B MVP)* | Option B: `"Full Autonomous Customer-Service Negotiator"` *(Deferred to Opt-In Phase 2C)* |
| :--- | :--- | :--- |
| **Core Value Proposition** | Navigates IVR trees, redials busy trunks, endures 60–120m holds at `$0` AI token burn, catches queue callbacks, stalls live rep for 10s, and bridges user's mobile (`Press 1`). | Conducts open-ended multi-turn dialogue, answers rep verification questions, negotiates credits/rates, and commits to outcomes without user joining. |
| **Works on IRS, FTB, DMV & Banks?** | **YES (`100%` target fit).** Stops right at human pickup before § 6103 / voiceprint authentication begins. | **NO (`0%` legal/fraud viability on IRS & banks).** Blocked by Form 2848 / IRM 21.1.3 and `Pindrop` anti-spoofing locks. |
| **PII & Secret Blast Radius** | **Minimal (`dtmf_only` only).** Zero spoken PII needed; transcription auto-pauses the instant user presses `1`. | **High.** Requires `spoken_redacted` address/ZIP/DOB handling, strict negotiation guardrails, and live hallucination defense. |
| **Acoustic & Latency Failure Risk** | **Very Low.** Keypad DTMF (`IVR_NAV`) + `Stage 0/1` hold classifier + 1-sentence polite stall are deterministic and forgiving of `600ms` latency. | **Moderate-to-High.** Fast human interruptions, heavy call-center background chatter, and multi-offer negotiation edge cases require careful tuning. |
| **User Time Saved on a 95m Call** | **`88 of 95 minutes` (`93%` of misery eliminated)** while user keeps 100% control over the 7-minute human conversation. | `95 of 95 minutes` *when* successful, but only on low-stakes commercial calls (~30% of painful call volume). |

**Recommendation:** Ship **Option A (`hold_and_bridge`) as the rock-solid default MVP (`Phase 2A`–`2B`)**, solving the exact 60–120 minute IRS/bank/airline hold problem immediately and safely, then layer **Option B (`autonomous_low_stakes`) in `Phase 2C`** as an explicit `CallPlan` policy mode restricted to non-government, non-bank vendors.

### 6.3 Blockers, Dependencies & Operational Preconditions

1. **Carrier Account + `$1/mo` Bridge DID Setup (`~10 min` one-time user action):** User needs a Telnyx or Twilio pay-as-you-go account, verifies their personal mobile number (`OutgoingCallerIds` / Verified Number), and leases one `$1.00/mo` US DID (`bridge_caller_id_e164`) saved in phone contacts with **Emergency Bypass** enabled so 90-minute hold pickups always ring audibly.
2. **Optional Sole-Proprietor KYC for Local-Business Attestation A (`1–3 business days`):** Not required for dialing `1-800` IRS/airline/ISP/bank toll-free numbers (which work immediately on Day 1 with Verified Caller ID), but recommended if placing calls to local 10-digit clinics/restaurants so CNAM/STIR-SHAKEN Level A prevents `"Spam Likely"` filtering.
3. **Ephemeral WSS Ingress Binary (`cloudflared` or `tailscale`):** `hq doctor` checks for `cloudflared` (zero-config Quick Tunnel works without even a Cloudflare account for testing; Named Tunnel or Tailscale Funnel recommended for rock-solid >2h sessions so temporary Quick Tunnel edge restarts never blip a 120-minute call, paired with automatic `<Conference>` stream re-attachment in `supervisor.go`).
