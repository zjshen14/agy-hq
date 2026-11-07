# 02 — Academic & Research Frontier for a Proactive Personal Assistant Agent

*Compiled 2026-11-07. Scope: research literature and influential technical reports relevant to building a generic proactive personal agent for (1) side-project software engineering, (2) investment research, (3) life admin (email, reservations, taxes, bookkeeping), possibly built lightly on Google Antigravity.*

> **How to read the numbers.** Benchmark figures move fast. Numbers from the original papers are reliable. "Current SOTA" figures for late 2026 come from leaderboards and aggregators found by web search, and you should check them before relying on them. They are marked **(≈, verify)**. Some 2025–2026 papers are listed by title and venue only, where I could not confirm the arXiv ID.

---

## TL;DR

1. **Memory:** Simple, inspectable memory competes with elaborate memory. On LoCoMo, a filesystem-backed agent (Letta) scored about as well as specialized memory products. Graph and temporal memory (Zep/Graphiti) helps most with *changing facts* and *time-aware* questions. That matters for life admin and portfolios. The big remaining weaknesses are conflict resolution, implicit preference updates, and abstention.
2. **Proactivity:** This is still immature. The best agents reach only about 40% end-to-end on proactive benchmarks such as PROBE. HCI work agrees on the rules: interrupt at task boundaries, use ambient (non-modal) surfaces, use decision-theoretic gating as in Horvitz, and give the user an adjustable proactivity threshold.
3. **Self-improvement:** Reusable workflows and skills work (Voyager, AWM, ACE). But **skills the agent writes for itself without verification add roughly nothing** (SkillsBench), while curated skills add about 16 percentage points. Use *verified, incremental, curated* procedural memory, not blind accumulation.
4. **Computer/web use:** Desktop benchmarks such as OSWorld-Verified look near or above human level **(≈, verify)**. On the *live* web, especially **write-heavy** tasks (bookings, forms), agents are much less reliable: about 60% for frontier agents on Online-Mind2Web in 2025, and under 50% on write tasks in industry benches. Use APIs first, the browser as a fallback, and confirm state changes in the backend.
5. **Safety:** Prompt injection is unsolved at the model level. Adaptive attacks get past 12 published defenses more than 90% of the time. The defenses that hold up are *architectural*: CaMeL, dual-LLM, plan-then-execute, capability/information-flow control, breaking the **lethal trifecta**, and human approval for irreversible actions.
6. **Long horizon:** METR time horizons keep doubling, roughly every 4–7 months. But context rot means more context ≠ better results. Compaction, structured external notes, and *read-only* sub-agents feeding a *single writer* are the patterns that work in practice.
7. **Finance and tax:** Raw LLMs are unreliable at exact calculation. TaxCalcBench shows a minority of returns fully correct for bare models. Finance agents reach about 50–65% on analyst tasks, and trading-agent backtests are badly affected by look-ahead bias. Use deterministic engines for the math and the LLM for orchestration and explanation.
8. **Evaluation:** Use **pass^k** (consistency), not pass@k. Build a private eval set from your own tasks, scored against real state rather than the transcript.

---

## 1. Long-term memory for agents

### Key papers

| Paper | Year | Link | Takeaway | Design implication |
|---|---|---|---|---|
| **MemGPT: Towards LLMs as Operating Systems** (Packer et al.) | 2023 | [arXiv:2310.08560](https://arxiv.org/abs/2310.08560) | Treats the context window like RAM and external storage like disk. The LLM pages information in and out with tool calls ("self-editing memory"). Became **Letta**. | Give the agent explicit memory tools (core memory blocks it can edit, plus archival search) rather than hiding memory in a retrieval pipeline. |
| **Generative Agents: Interactive Simulacra of Human Behavior** (Park et al.) | 2023 | [arXiv:2304.03442](https://arxiv.org/abs/2304.03442) | A memory stream with retrieval scored on **recency × importance × relevance**, plus periodic **reflection** that builds higher-level insights from raw observations. Ablations show reflection matters for believable long-term behavior. | Run periodic reflection jobs that turn raw logs into durable insights ("user prefers aisle seats", "repo X uses pnpm"). Score memories on importance, not just similarity. |
| **A-MEM: Agentic Memory for LLM Agents** (Xu et al.) | 2025 | [arXiv:2502.12110](https://arxiv.org/abs/2502.12110) | Zettelkasten-style atomic notes with LLM-generated keywords, tags, and links. New memories can **update older notes** ("memory evolution"). Beats static memory baselines on LoCoMo. | Store memories as small linked notes. When a new fact arrives, let the agent revise related notes rather than only appending. |
| **Mem0: Building Production-Ready AI Agents with Scalable Long-Term Memory** (Chhikara et al.) | 2025 | [arXiv:2504.19413](https://arxiv.org/abs/2504.19413) | An extract → consolidate (ADD/UPDATE/DELETE/NOOP) → retrieve pipeline. About 26% relative gain over OpenAI memory on LoCoMo (LLM-judge), with **91% lower p95 latency and over 90% fewer tokens** than full context. The graph variant adds about 2%. | Explicit *consolidation* (deduplicating and overwriting stale facts) is the key operation. It beats raw RAG over transcripts on cost and latency. |
| **Zep: A Temporal Knowledge Graph Architecture for Agent Memory** (Rasmussen et al.) | 2025 | [arXiv:2501.13956](https://arxiv.org/abs/2501.13956) | **Graphiti** is a *bi-temporal* knowledge graph: it records when a fact was true and when it was ingested. It has episodic, semantic-entity, and community layers, with hybrid retrieval (BM25 + vector + graph traversal). DMR 94.8% vs MemGPT 93.4%. Up to **+18.5% on LongMemEval** with about 90% lower latency. | For changing facts (address, account balances, holdings, subscriptions, tax status), keep *validity intervals* and invalidate rather than delete. |
| **LoCoMo: Evaluating Very Long-Term Conversational Memory of LLM Agents** (Maharana et al.) | 2024 | [arXiv:2402.17753](https://arxiv.org/abs/2402.17753) | Very long multi-session dialogues (about 300 turns, up to 35 sessions). QA covers single-hop, multi-hop, temporal, and open-domain questions. | A standard benchmark, but it is getting saturated and easy to game (see the Letta filesystem result). Don't treat LoCoMo scores as decisive. |
| **LongMemEval: Benchmarking Chat Assistants on Long-Term Interactive Memory** (Wu et al., ICLR 2025) | 2024 | [arXiv:2410.10813](https://arxiv.org/abs/2410.10813) | 500 questions testing five abilities: extraction, multi-session reasoning, **temporal reasoning, knowledge updates, abstention**. Commercial assistants lose about 30% accuracy over long histories. The paper splits memory into **indexing → retrieval → reading** and proposes session decomposition, fact-augmented keys, and time-aware query expansion. | Index *facts* extracted from sessions, not just raw turns. Add timestamps to everything. Test abstention ("I don't know that about you") explicitly. |
| **Sleep-time Compute: Beyond Inference Scaling at Test-time** (Lin et al., Letta/UC Berkeley) | 2025 | [arXiv:2504.13171](https://arxiv.org/abs/2504.13171) | Pre-processing context while idle cuts test-time compute about 5× for the same accuracy and raises accuracy by up to 13–18%. Amortized across related queries, cost per query drops 2.5×. The gain depends on **how predictable future queries are**. | Run background "sleep-time" agents, e.g. overnight, to consolidate memory, pre-digest new emails, filings, and repo changes, and prepare briefings. Personal assistants have very predictable recurring queries, which is the ideal case. |
| **MemoryAgentBench: Evaluating Memory in LLM Agents via Incremental Multi-Turn Interactions** (Hu et al., ICLR 2026) | 2025 | [arXiv:2507.05257](https://arxiv.org/abs/2507.05257) | Feeds information incrementally and tests four skills: accurate retrieval, test-time learning, long-range understanding, **conflict resolution**. No system masters all four. Long-context models and RAG fail on different ones, and conflict resolution is the weakest. | Pick memory designs knowing the trade-offs. Add explicit overwrite/supersede logic because models won't do it reliably on their own. |
| **PrefEval** ("Do LLMs Recognize Your Preferences?", ICLR 2025) / **PersonaMem** | 2025 | [arXiv:2502.09597](https://arxiv.org/abs/2502.09597), [arXiv:2504.14225](https://arxiv.org/abs/2504.14225) | Proactive preference following collapses quickly. In zero-shot settings it falls below 10% after about 10 turns for many models. **Implicit** preference changes, inferred from life events, are handled far worse than explicit ones. | Write preferences down explicitly as structured profile entries and inject them at decision points. Don't rely on the model "remembering" them from chat history. When an inference matters, confirm it with the user. |

### File-based vs vector vs graph memory: what the evidence says

- **File-based (Markdown/JSON in a workspace, `AGENTS.md`-style):** Letta showed a GPT-4o-mini agent with plain file tools (grep/search/open) reaching **about 74% on LoCoMo**, competitive with specialized memory systems ([Letta blog: "Benchmarking AI Agent Memory: Is a Filesystem All You Need?"](https://www.letta.com/blog/benchmarking-ai-agent-memory)). Files are human-readable, editable, diffable, and can be version-controlled with git. They are also native to coding agents like Antigravity. The weaknesses: no fuzzy semantic recall unless you add search, and no built-in way to track time.
- **Vector:** Good for fuzzy recall over large unstructured archives such as email, notes, and filings. Weak at multi-hop questions, knowledge updates, and staleness, because appended facts contradict each other.
- **Graph / temporal KG:** Best for entities and relations that change over time and need multi-hop reasoning (people ↔ accounts ↔ obligations ↔ dates). It costs LLM calls at ingestion and adds operational complexity.
- **2025–2026 surveys** (e.g., *Memory in the Age of AI Agents*; *Graph-based Agent Memory: Taxonomy, Techniques, and Applications*; *From Storage to Experience*) agree on **hybrid designs** and on an **active memory lifecycle**: extract → consolidate → reflect → prune. Simply storing more doesn't help.

**Practical recommendation:** Start with **files as the source of truth**: a profile, per-domain fact files, decision logs, and skills. Put a **search index** on top (BM25 + embeddings). Add a **temporal fact store** only for domains where facts change: finances, subscriptions, deadlines, contacts. Run consolidation and reflection as sleep-time jobs.

---

## 2. Proactive agents

### Key papers

| Paper | Year | Link | Takeaway | Design implication |
|---|---|---|---|---|
| **Principles of Mixed-Initiative User Interfaces** (Horvitz, CHI '99) | 1999 | [PDF](https://erichorvitz.com/chi99horvitz.pdf) | The foundational decision-theoretic framing. Act only when the expected utility of acting, given uncertainty about the user's goal, exceeds that of not acting, *including interruption cost*. If uncertain, start a cheap clarification dialog. "Doing less, correctly" beats doing more, incorrectly. | Implement a three-zone gate: **stay silent / ask or suggest / act**, based on confidence × benefit − (error cost + interruption cost). |
| **If Not Now, When? Effects of Interruption at Different Moments** (Adamczyk & Bailey, CHI '04) and related breakpoint research (Iqbal & Bailey) | 2004–08 | [ACM DL](https://dl.acm.org/doi/10.1145/985692.985715) | Interruptions at **task breakpoints** cost much less (annoyance, frustration, resumption lag) than mid-task. | Batch notifications and deliver them at natural boundaries: after a commit or test run, between meetings, or in a morning/evening digest. |
| **Proactive Agent: Shifting LLM Agents from Reactive Responses to Active Assistance** (Lu et al.) | 2024 | [arXiv:2410.12361](https://arxiv.org/abs/2410.12361) | A data pipeline: collect activity traces → generate proactive task predictions → humans accept/reject → train a **reward model** (about 91% agreement with human labels). **ProactiveBench** has about 6.8k events. Fine-tuning a 7B model greatly improves proactive F1. | Log every proactive suggestion with accept/reject/ignore. That log *is* your training and eval data for the gate. |
| **Need Help? Designing Proactive AI Assistants for Programming** (Chen et al., CHI 2025) | 2024 | [arXiv:2410.04596](https://arxiv.org/abs/2410.04596) | Proactive coding suggestions raised productivity and were preferred, but **poorly timed or too frequent** suggestions were disruptive. Good timing and "presence" design mattered more than raw suggestion quality. | Prefer ambient UI (badges, side panels, digests) over modal interruptions. Let users tune frequency. |
| **Assistance or Disruption? Exploring and Evaluating the Design and Trade-offs of Proactive AI Programming Support** (CHI 2025) | 2025 | (CHI '25 proceedings) | Compared prompt-only vs proactive agents ("Codellaborator"). Proactivity cut friction for some tasks but caused workflow disruption and loss of control. Visible AI presence and user control reduced the downsides. | Make the agent's state visible ("I'm watching CI; 2 suggestions queued"). Always offer snooze, dismiss, and "don't suggest this again". |
| **Beyond Reactivity: Measuring Proactive Problem Solving in LLM Agents (PROBE)** | 2025 | arXiv (Fastino et al.) | Search a personal datastore (email, calendar, docs) → identify a hidden bottleneck → resolve it. 1,000 samples. **The best frontier models reach about 40% end-to-end.** | Proactive "find the problem I didn't ask about" is hard. Start with narrow, high-precision triggers (deadlines, renewals, failed CI, price thresholds) rather than open-ended monitoring. |
| Newer proactive benchmarks: **π-Bench** (proactive personal assistants in long-horizon workflows), **ProEvent** (calendar from IM), **PARE/Pare-Bench**, **ProAgentBench**, **ATRBench** ("Ask Now, Use Later") | 2025–26 | arXiv | They converge on four dimensions: *intervention timing*, *hidden-intent inference*, *act vs clarify*, *cross-session persistence*. ATRBench in particular shows that agents fail to **ask early for reusable preferences** that later tasks need. | Build a "preference elicitation" behavior: when a task depends on an unknown, stable preference, ask once and store it. |

### Ambient agents and user modeling

- "Ambient agents" (a term popularized by LangChain in 2025) are agents triggered by **event streams** (new email, calendar change, price move, CI failure) rather than chat. They interact through a few human-in-the-loop patterns: **notify**, **question**, **review/approve**. This maps directly onto Horvitz's zones.
- User modeling research (PrefEval, PersonaMem, ATRBench) shows models are poor at *inferring* preferences implicitly. Combine explicit profile entries with confirmation of inferred preferences.

**Design implications:** An event bus with triggers; a utility-based gate; digest batching; an accept/reject log feeding back into the gate; a user-adjustable proactivity threshold per domain (e.g., aggressive for CI failures, conservative for investment ideas).

---

## 3. Self-improving agents and skill libraries

| Paper | Year | Link | Takeaway | Design implication |
|---|---|---|---|---|
| **Voyager: An Open-Ended Embodied Agent with LLMs** (Wang et al.) | 2023 | [arXiv:2305.16291](https://arxiv.org/abs/2305.16291) | An automatic curriculum, an **executable code skill library** indexed by description embeddings, and iterative prompting with environment feedback and **self-verification**. Skills compose and transfer. 3.3× more unique items than baselines. | Store skills as *code plus a description*. Admit a skill to the library only after it has been verified to succeed. |
| **Reflexion: Language Agents with Verbal Reinforcement Learning** (Shinn et al.) | 2023 | [arXiv:2303.11366](https://arxiv.org/abs/2303.11366) | After a failure, the agent writes a verbal self-reflection into episodic memory and retries. Large gains on coding (HumanEval) and decision tasks *when there is a reliable feedback signal*. | Reflection works when grounded in a real signal (tests, API errors, user correction). It is weak without one. |
| **ExpeL: LLM Agents Are Experiential Learners** (Zhao et al.) | 2023 | [arXiv:2308.10144](https://arxiv.org/abs/2308.10144) | Extracts cross-task insights from successful and failed trajectories, with no weight updates. | Keep an "insights" file per domain, distilled from trajectories. |
| **Agent Workflow Memory (AWM)** (Wang et al.) | 2024 | [arXiv:2409.07429](https://arxiv.org/abs/2409.07429) | Induces reusable *workflows* from successful trajectories, online or offline. **+51% relative success on WebArena** and +24.6% on Mind2Web, with fewer steps. Generalizes across websites and domains. | After each successful life-admin task (e.g., "renew car registration"), distill a reusable workflow. |
| **SkillWeaver: Web Agents can Self-Improve by Discovering and Honing Skills** | 2025 | [arXiv:2504.07079](https://arxiv.org/abs/2504.07079) | The agent explores websites and distills **Python API skills**, then practices and debugs them. Clear gains on WebArena and real sites. Skills transfer to weaker agents. | Turn repeated browser flows into scripted, tested skills (Playwright functions) instead of re-planning each time. |
| **Dynamic Cheatsheet** | 2025 | [arXiv:2504.07952](https://arxiv.org/abs/2504.07952) | A persistent, self-curated memory of strategies and code snippets at test time. Big gains on math and puzzles. | A lightweight version of the same idea: an evolving "tips" file. |
| **Agentic Context Engineering (ACE)** (Zhang et al.) | 2025 | [arXiv:2510.04618](https://arxiv.org/abs/2510.04618) | Generator → Reflector → Curator loop that maintains an evolving **playbook** through **incremental delta updates** (bullets with IDs and helpful/harmful counters). This avoids "brevity bias" and **context collapse**, where repeated full rewrites erode detail. +10.6% on agent tasks (AppWorld) and +8.6% on finance. | **Never let the LLM rewrite your memory or instruction files wholesale.** Use append/patch operations with provenance and usefulness counters. |
| **Anthropic Agent Skills** (SKILL.md, progressive disclosure; now an open standard at agentskills.io) | 2025 | [anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills) | A skill is a folder: `SKILL.md` (name and description frontmatter plus instructions) and optional scripts and references. **Progressive disclosure:** only metadata (about 100 tokens) loads at startup, the body loads on activation, resources load on demand. | Antigravity already supports this format. Use it as the procedural-memory substrate. |
| **SkillsBench** (Benchmarking how well Agent Skills work) | 2026 | [skillsbench.ai](https://skillsbench.ai) | **Curated skills: about +16 pp** average pass rate (about 34% → about 50%), with the largest gains in niche domains (healthcare, manufacturing). Software engineering gains only about 4.5 pp. **Self-generated skills: about −1.3 pp (no benefit).** Focused skills (2–3 modules) beat large documentation dumps. | Skills are most valuable for *your* niche procedures: tax workflows, brokerage quirks, your repo conventions. Have agents *propose* skills, then verify them (tests, replay) or have a human review them before admitting them. |
| 2026 verification-focused follow-ups (e.g., **Skill-V** versioned and falsifiable skill contracts; **CoEvoSkills** generator + verifier) | 2026 | arXiv | Libraries that only grow go stale and lose retrieval precision (a "scaling tax" as the skill pool grows). Answer: versioning, regression replay, applicability filters, pruning. | Version skills in git, keep replay tests, and retire unused or failing skills. |

---

## 4. Computer-use and web agents

### Benchmarks and status

| Benchmark | Year | Link | What it measures | Status |
|---|---|---|---|---|
| **Mind2Web** | 2023 | [arXiv:2306.06070](https://arxiv.org/abs/2306.06070) | 2k+ tasks on 137 real websites (offline action prediction). | A foundational dataset. Step-level accuracy doesn't capture end-to-end reliability. |
| **WebArena** / **VisualWebArena** | 2023/24 | [arXiv:2307.13854](https://arxiv.org/abs/2307.13854), [arXiv:2401.13649](https://arxiv.org/abs/2401.13649) | Self-hosted realistic sites (shopping, GitLab, Reddit, CMS, maps) with functional state checks. | Started around 14% (GPT-4, 2023). Top agents reached about 60%+ by 2025, higher by 2026 **(≈, verify)**. |
| **WebVoyager** | 2024 | [arXiv:2401.13919](https://arxiv.org/abs/2401.13919) | Live sites, multimodal. | Scores of about 90% were reported, but they are **inflated**: easy tasks and lenient judging (see Online-Mind2Web). |
| **OSWorld** / **OSWorld-Verified** | 2024/25 | [arXiv:2404.07972](https://arxiv.org/abs/2404.07972), [os-world.github.io](https://os-world.github.io) | 369 real desktop tasks (Ubuntu/Windows apps), with execution-based checks. Human about 72%. | Started at 12% (2024). By late 2026, leaderboard aggregators report top agents **above the ~72% human baseline, about 75–86% (≈, verify)**, i.e., close to saturated. Longer-horizon successors are emerging. |
| **An Illusion of Progress? Assessing the Current State of Web Agents (Online-Mind2Web)** | 2025 | [arXiv:2504.01382](https://arxiv.org/abs/2504.01382) | 300 tasks on 136 live sites, human-evaluated. | Operator **61%**, Claude CU 3.7 **56%**, Browser Use **30%**, SeeAct 31%. Many agents were *no better than a 2024 baseline*. WebJudge (an LLM judge) agrees with humans about 85% of the time. |
| **REAL** (deterministic replicas of real sites) | 2025 | [arXiv:2504.11543](https://arxiv.org/abs/2504.11543) | Clones of popular sites (travel, e-commerce, email) with state checks. | Frontier models scored about 40–50% at release. Write tasks are the hard part. |
| **BrowseComp** | 2025 | [arXiv:2504.12516](https://arxiv.org/abs/2504.12516) | Hard, persistent information-seeking tasks (read-only). | Useful for deep research. It says nothing about transactions. |
| Industry: **Web Bench** (Halluminate/Skyvern) | 2025 | [halluminate.ai](https://halluminate.ai) | Separates READ from WRITE tasks on live sites. | Read tasks over 75%. **Write tasks (forms, logins, bookings) peaked under 50%.** Failures come from auth, CAPTCHAs, pop-ups, date pickers, and anti-bot measures. |

### How reliable for real bookings and form filling?

- **Read/research tasks:** fairly reliable.
- **Transactional, write-heavy tasks on live sites:** still *coin-flip to moderate* reliability for long flows. Errors compound per step. Calendars and date pickers, MFA, CAPTCHAs, and bot protection are common failure points. Agents also **claim success without it happening**. Benchmarks check backend state for exactly this reason.
- **Security interacts with competence:** WASP ([arXiv:2504.18575](https://arxiv.org/abs/2504.18575)) found agents *start* following injected instructions in up to about 86% of cases but complete the attacker's goal only 0–17% of the time. That is "security by incompetence", and it will **disappear as agents improve**.

**Design implications:**
- Order of preference: **API/MCP connector > structured automation script (a skill) > generic computer-use**.
- Always verify the end state independently: a confirmation email, a booking reference lookup, a bank transaction.
- Stop before submit or pay and show a human-readable diff ("About to book X on Y for $Z, refundable until …").
- Cache successful flows as workflows or skills (AWM/SkillWeaver).

---

## 5. Safety for personal agents

### Key work

| Work | Year | Link | Takeaway | Design implication |
|---|---|---|---|---|
| **Not what you've signed up for: Compromising Real-World LLM-Integrated Applications with Indirect Prompt Injection** (Greshake et al.) | 2023 | [arXiv:2302.12173](https://arxiv.org/abs/2302.12173) | Defined **indirect prompt injection**: instructions hidden in retrieved data (web pages, emails, docs) hijack the agent. Demonstrated data theft, worming, and manipulation. | Every email, web page, PDF, and calendar invite is a possible attacker channel. Life-admin agents are the *most* exposed. |
| **The Dual LLM pattern** (Willison) | 2023 | [simonwillison.net/2023/Apr/25/dual-llm-pattern](https://simonwillison.net/2023/Apr/25/dual-llm-pattern/) | A **privileged LLM** with tools never sees untrusted content. A **quarantined LLM** processes untrusted content and has no tools. They communicate through symbolic variable references. | Basis for CaMeL. Practical for email triage: the quarantined model summarizes, the privileged model acts on references only. |
| **AgentDojo** (Debenedetti et al., NeurIPS 2024 D&B) | 2024 | [arXiv:2406.13352](https://arxiv.org/abs/2406.13352) | A dynamic environment with workspace, Slack, banking, and travel suites: 97 tasks and 629 injection cases. Measures utility and security together. | Use AgentDojo-style tests in your own eval suite (inject into your own email fixtures). |
| **CaMeL: Defeating Prompt Injections by Design** (Debenedetti et al., Google/DeepMind/ETH) | 2025 | [arXiv:2503.18813](https://arxiv.org/abs/2503.18813) | The P-LLM writes a *program* from the trusted query only. The Q-LLM parses untrusted data with no tools. A custom interpreter tracks **data provenance (capabilities)** and enforces **policies** before tool calls. AgentDojo: **about 67% (later about 77%) tasks solved with provable security** vs about 84% undefended utility with no security. | The most credible design for agents that read email *and* send messages or move money. Costs about 10 points of utility, and some tasks (where data must drive control flow) become impossible. |
| **Design Patterns for Securing LLM Agents against Prompt Injections** (Beurer-Kellner et al.; IBM/Invariant/ETH/Google/Microsoft) | 2025 | [arXiv:2506.08837](https://arxiv.org/abs/2506.08837) | Six patterns: **Action-Selector, Plan-Then-Execute, LLM Map-Reduce, Dual LLM, Code-Then-Execute, Context-Minimization**. Principle: once an agent has read untrusted input, it must not be able to take consequential actions driven by it. | Choose the pattern per workflow. Email triage → Map-Reduce/Dual LLM. Bookings → Plan-Then-Execute plus human confirmation. Coding → sandbox plus egress allowlist. |
| **The lethal trifecta** (Willison) | 2025 | [simonwillison.net/2025/Jun/16/the-lethal-trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/) | Private data + untrusted content + an external communication channel = exfiltration is possible. Break **at least one leg** per execution context. | Make this a *static check* on every agent and session configuration: refuse or force approval when all three are present. |
| **The Attacker Moves Second: Stronger Adaptive Attacks Bypass Defenses against LLM Jailbreaks and Prompt Injections** (Nasr, Carlini et al.; OpenAI/Anthropic/GDM/ETH) | 2025 | [arXiv:2510.09023](https://arxiv.org/abs/2510.09023) | Adaptive attacks (RL, search, gradient, human red-teaming) got past **12 recent defenses with attack success rates above 90%**, even though most had reported near-zero. | Don't rely on classifiers, "spotlighting", or prompt-level defenses for anything high-stakes. Use them only as an extra layer on top of architectural controls. |
| **WASP** (Meta et al.) | 2025 | [arXiv:2504.18575](https://arxiv.org/abs/2504.18575) | Web agents get partially hijacked in up to about 86% of cases. Low end-to-end attack success is due to incompetence, not robustness. | Expect risk to *rise* as agents become more capable. |
| **PrivacyLens** / **AgentDAM** / CI-Bench (contextual integrity) | 2024–25 | [arXiv:2409.00138](https://arxiv.org/abs/2409.00138), [arXiv:2503.09780](https://arxiv.org/abs/2503.09780) | Agents **over-share** personal data in tool calls and forms even without an attack. They break data-minimization norms, and persistent memory makes cross-context leaks worse. | Scope memory by domain (tax data shouldn't appear in a restaurant booking form). Add a data-minimization check on outbound fields. |

### Permissioning and human-in-the-loop

- **Classify each action by reversibility and blast radius.** Read-only → auto. Reversible writes (drafts, labels, calendar holds, local git branches) → auto with logging and undo. **Irreversible or external actions (send email, pay, trade, file taxes, delete, push to main, sign) → explicit confirmation**, showing the exact payload.
- **Capability scoping per task.** Grant tokens just in time, least privilege, time-boxed. Use spend limits or virtual cards for payments. Use egress allowlists in sandboxes.
- **Provenance tagging.** Any argument derived from untrusted content (e.g., a recipient address or amount taken from an email) needs approval, CaMeL-style.
- **Audit log** of every tool call, with inputs and outputs, so actions can be replayed and recovered.

---

## 6. Long-horizon execution and context engineering

| Work | Year | Link | Takeaway | Design implication |
|---|---|---|---|---|
| **Lost in the Middle** (Liu et al.) | 2023 | [arXiv:2307.03172](https://arxiv.org/abs/2307.03172) | U-shaped recall: information in the middle of long contexts is under-used. | Put key instructions and state at the start or end. Keep contexts lean. |
| **Context Rot** (Chroma technical report: Hong, Troynikov, Huber) | 2025 | [research.trychroma.com/context-rot](https://research.trychroma.com/context-rot) | Across 18 frontier models, performance drops **continuously as input length grows**, well before the window limit. It is worse with semantic (not lexical) matching and with distractors, and happens even on trivial tasks. | Fitting in the window doesn't mean the model will use it. Curate the smallest high-signal context. |
| **LLMs Get Lost in Multi-Turn Conversation** (Laban et al.) | 2025 | [arXiv:2505.06120](https://arxiv.org/abs/2505.06120) | Average **−39%** from single-turn to underspecified multi-turn, driven by **+112% unreliability**. Models commit early to assumptions and anchor on wrong turns. | Periodically **restate** the consolidated task spec in a fresh context. Ask before assuming. Restart from a clean spec when a session drifts. |
| **Effective context engineering for AI agents** (Anthropic) | 2025 | [anthropic.com/engineering/effective-context-engineering-for-ai-agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents) | Three levers: **compaction** (summarize and restart), **structured note-taking** (external files such as NOTES.md or to-do lists), **sub-agents** (isolated contexts returning 1–2k-token summaries). Also just-in-time retrieval instead of preloading. | Antigravity-style agents already have files and sub-agents. Make NOTES and TODO files mandatory for multi-session tasks. |
| **How we built our multi-agent research system** (Anthropic) | 2025 | [anthropic.com/engineering/multi-agent-research-system](https://www.anthropic.com/engineering/multi-agent-research-system) | An orchestrator with parallel sub-agents beat a single agent by **90.2%** on internal research evals. **Token usage explains about 80% of the variance.** It costs about 15× the tokens of chat. Best for breadth-first, parallelizable work. | Use multi-agent fan-out for investment research (many tickers or filings), not for tightly coupled coding. |
| **Don't Build Multi-Agents** (Cognition, Walden Yan) and the 2026 follow-up | 2025–26 | [cognition.ai/blog/dont-build-multi-agents](https://cognition.ai/blog/dont-build-multi-agents) | Parallel *writers* make conflicting implicit decisions. Share full context and traces. The 2026 update: **read-only sub-agents feeding a single-threaded writer** works well. | Allow only one writer per artifact (repo, ledger, inbox). Sub-agents investigate. The main agent decides and writes. |
| **Measuring AI Ability to Complete Long Tasks** (METR, Kwa et al.) | 2025 | [arXiv:2503.14499](https://arxiv.org/abs/2503.14499) | The 50% time horizon on software tasks has **doubled about every 7 months since 2019**, faster (about 4 months) since 2024. Reliability at 80% is much shorter than at 50%. By late 2026, frontier models are reported at **about 8 to over 16 hours at 50% (≈, verify; high uncertainty above about 16h)**. | Plan for agents that can run multi-hour tasks unattended, but design for the **80% horizon**, which is several times shorter. Add checkpoints, verification gates, and resumable state. |

**Planning:** Plan-then-execute with explicit to-do artifacts gives better reliability *and* better security (see the design-patterns paper). Re-plan at checkpoints. Use the verifier available in each domain (tests for code, reconciliation for books, a tax engine for taxes).

---

## 7. Domain-specific: investment research, personal finance, tax

| Paper | Year | Link | Takeaway | Design implication |
|---|---|---|---|---|
| **FinanceBench** (Islam et al., Patronus) | 2023 | [arXiv:2311.11944](https://arxiv.org/abs/2311.11944) | 10k QA over SEC filings. GPT-4-Turbo with retrieval **failed or refused about 81%** at launch. Long-context helped but was impractical. | Retrieval over filings is non-trivial. Use table-aware parsing and cite page numbers. |
| **FinMem** (Yu et al.) | 2023 | [arXiv:2311.13743](https://arxiv.org/abs/2311.13743) | A trading agent with layered memory (short/mid/long-term decay) and a character profile. | Memory layering by time decay fits market data, but see the look-ahead caveats. |
| **FinAgent: A Multimodal Foundation Agent for Financial Trading** (Zhang et al., KDD 2024) | 2024 | [arXiv:2402.18485](https://arxiv.org/abs/2402.18485) | Multimodal (charts, news, prices), tool-augmented, with reflection. Reported strong backtest gains. | Reflection on past trades is useful. Treat backtest claims skeptically. |
| **FinRobot: An Open-Source AI Agent Platform for Financial Applications** (AI4Finance) | 2024 | [arXiv:2405.14767](https://arxiv.org/abs/2405.14767) | Layered platform. Equity-research agents use Data-CoT → Concept-CoT → Thesis-CoT to produce analyst-style reports. | A useful template for a structured research pipeline (data → analysis → thesis → report). |
| **TradingAgents: Multi-Agents LLM Financial Trading Framework** | 2024 | [arXiv:2412.20138](https://arxiv.org/abs/2412.20138) | Firm-like roles: analysts, a bull/bear debate, a trader, risk management. | Debate and role separation can surface counter-arguments. Use it for *research*, not autonomous trading. |
| **InvestorBench** | 2024 | [arXiv:2412.18174](https://arxiv.org/abs/2412.18174) | Benchmarks LLM agents on stock, crypto, and ETF decision tasks. | Agent decision quality varies widely across market regimes. |
| **Finance Agent Benchmark** (Vals AI + Stanford) | 2025 | [arXiv:2508.00828](https://arxiv.org/abs/2508.00828) | 537 expert questions using EDGAR and search tools, with "dealbreaker" gating. o3 scored about **46.8% at about \$3.79/query**. Returns flatten above about \$1/query. 2026 leaderboard tops are around **60–65%+ (≈, verify)**. | Even the best agents get load-bearing facts wrong about a third of the time. **Every number needs a citation and automated cross-checks.** |
| **Look-ahead bias / memorization critiques** of LLM trading backtests (several 2024–26 papers, e.g. "lookahead propensity" measurement, entity anonymization) | 2024–26 | (various, arXiv/SSRN) | LLMs memorize historical outcomes, so in-sample backtests are contaminated. Larger models can look *more* skilled while leaking more. | Evaluate only on post-training-cutoff data. Anonymize entities in evals. Never trust a backtest of an LLM strategy without these controls. |
| **TaxCalcBench** (Column Tax) | 2025 | [github.com/column-tax/tax-calc-bench](https://github.com/column-tax/tax-calc-bench) | Compute a full federal 1040 from complete inputs. Bare frontier models got only **about a third of returns fully correct** at launch, failing on tax tables, rounding, and eligibility rules. Later reasoning models are better (about 50–64%, ≈ verify). Agent + deterministic engine setups approach 100%. | **The LLM should never be the tax calculator.** Use a deterministic engine (or tax software) for computation. The LLM gathers documents, classifies, explains, and flags anomalies. |
| **Agentic Context Engineering** (finance results) | 2025 | [arXiv:2510.04618](https://arxiv.org/abs/2510.04618) | +8.6% on finance benchmarks (FiNER, formula tasks) from evolving playbooks. | Maintain a domain playbook of XBRL quirks, accounting rules, and broker CSV formats. |

**Bookkeeping:** There is little academic work specifically on LLM bookkeeping. The lessons carry over from tax and FinanceBench: classify transactions with the LLM, enforce **double-entry invariants and reconciliation** in code, and review exceptions by hand.

---

## 8. Evaluating personal assistants

| Benchmark | Year | Link | What it measures | Key insight |
|---|---|---|---|---|
| **τ-bench** (Sierra) | 2024 | [arXiv:2406.12045](https://arxiv.org/abs/2406.12045) | Tool-agent-user interaction in retail and airline domains, with domain policies and a simulated user. Checks final database state. | Introduced **pass^k** (all k trials succeed). GPT-4o was under 50% pass^1 on retail and **about 25% pass^8**, i.e., agents are inconsistent. |
| **τ²-bench** | 2025 | [arXiv:2506.07982](https://arxiv.org/abs/2506.07982) | **Dual-control** (telecom): both user and agent act on a shared state, formalized as a Dec-POMDP. | Performance drops sharply when the agent must guide the user to act. Relevant when the assistant needs *you* to do something (2FA, signature). |
| **AppWorld** (Trivedi et al., ACL 2024) | 2024 | [arXiv:2407.18901](https://arxiv.org/abs/2407.18901) | 9 everyday apps (Venmo, Gmail-like, Spotify, etc.), 457 APIs, about 100 simulated people. Tasks require rich code that calls APIs. State-based tests plus collateral-damage checks. | **The closest benchmark to personal life admin.** GPT-4o was about 49% TGC / 30% SGC at launch. Scenario Goal Completion (all variants) is much lower than per-task scores. ACE-style context evolution has since raised the state of the art substantially. |
| **TheAgentCompany** (Xu et al., CMU) | 2024 | [arXiv:2412.14161](https://arxiv.org/abs/2412.14161) | A simulated software company (GitLab, Plane, RocketChat, OwnCloud). 175 long-horizon professional tasks with partial-credit checkpoints. | About 24% fully completed at launch (Claude 3.5 Sonnet). Best agents in 2026 are about **30–43% (≈, verify)**. Agents still fail at social and coordination steps and with complex UIs. |
| **GAIA** | 2023 | [arXiv:2311.12983](https://arxiv.org/abs/2311.12983) | General-assistant questions needing browsing, tools, and multimodality. | Largely saturated at the top levels. Still useful as a smoke test. |
| **SWE-bench (Verified)** | 2023/24 | [arXiv:2310.06770](https://arxiv.org/abs/2310.06770) | Resolving real GitHub issues. | Relevant to the side-project use case. Close to saturated on Verified. Harder variants (SWE-bench Pro, multi-language) are now the frontier. |

**Evaluation methodology lessons:**
- **pass^k beats pass@k** for an assistant that acts for you, because you experience every run.
- **Check state, not the transcript.** Did the booking exist, did the email go to the right person, does the ledger balance.
- Measure **collateral damage** (AppWorld): unintended side effects count as failures.
- Use **adaptive red-teaming** for security (AgentDojo-style plus "attacker moves second").
- For proactivity, track **precision of interventions** (accept rate) and **interruption cost** (dismissals, snoozes), not just recall.
- **Build a private eval set** from your own recurring tasks. Replay it whenever you change models, prompts, skills, or memory.

---

## Design implications: prioritized architectural principles

Ordered by how strongly the research supports them and how much they affect the user's three use cases.

1. **Security by architecture, not by prompt (P0).**
   Treat all inbound content (email, web, PDFs, filings) as untrusted. For each workflow, break at least one leg of the **lethal trifecta**. Use **Dual-LLM / CaMeL-style** separation for flows that read untrusted content and then act externally: a quarantined parser with no tools, and a privileged planner that sees only references. Track **provenance**: arguments derived from untrusted data need approval. Model-level defenses are an extra layer only, because adaptive attacks beat them more than 90% of the time.

2. **Tier actions by reversibility, and keep a human in the loop for irreversible ones (P0).**
   Read → auto. Reversible write → auto plus log plus undo. Send, pay, trade, file, delete, push → **explicit confirmation showing the exact payload**. Use spend caps, scoped short-lived credentials, sandboxes with egress allowlists, and a full audit log.

3. **Deterministic tools for anything that must be exactly right (P0).**
   Tax computation, bookkeeping invariants, portfolio math, and date arithmetic belong in code or engines. The LLM orchestrates, classifies, explains, and flags. Finance and tax benchmarks show bare-LLM error rates that are unacceptable here.

4. **Files as the memory source of truth, plus search, plus temporal facts where needed (P1).**
   Human-readable Markdown/JSON memory (profile, per-domain facts, decisions, playbooks) under git. Add a hybrid search index. Add **bi-temporal fact tracking** (valid-from/valid-to) for facts that change: finances, subscriptions, addresses, deadlines. This is inspectable, cheap, and competitive on benchmarks.

5. **An active memory lifecycle run as sleep-time compute (P1).**
   Background jobs extract facts, **consolidate** them (update/supersede, not just append), reflect into insights, prune, and pre-compute briefings. **Incremental delta edits with provenance and usefulness counters** (ACE), never wholesale LLM rewrites, to avoid context collapse. Test conflict resolution and abstention explicitly.

6. **Proactivity through a decision-theoretic gate, delivered at boundaries (P1).**
   Event-driven triggers (new email, deadline approaching, CI failure, price or filing alert) feed a Horvitz-style gate: **silent / suggest or ask / act**, based on confidence × benefit − (error cost + interruption cost). Batch into digests at natural breakpoints. Use ambient surfaces, not modal pop-ups. Thresholds are per domain and user-tunable. **Log accept/reject/ignore** and use it to calibrate. Start with narrow, high-precision triggers; open-ended "find my problems" agents reach only about 40%.

7. **Explicit preference elicitation and a structured user profile (P1).**
   Models handle implicit preferences poorly. Ask once for stable, reusable preferences (ATRBench), store them explicitly, confirm inferred ones, and inject them at decision points. Scope personal data by domain to prevent cross-context leaks (contextual integrity).

8. **Verified procedural memory: skills and workflows (P1).**
   Use the SKILL.md / progressive-disclosure format, which Antigravity supports natively. Distill successful trajectories into workflows and scripts (AWM, SkillWeaver). **Admit skills only after verification**: tests, replay, or human review. Self-generated, unverified skills add ~0 (SkillsBench). Version them, run regression replays, and prune the stale ones.

9. **Context engineering as a first-class concern (P1).**
   Keep contexts minimal and high-signal (context rot). Require NOTES/TODO artifacts for multi-session work. Compact and restate the task spec periodically (lost-in-conversation). Use **read-only sub-agents feeding a single writer** per artifact (repo, ledger, inbox). Fan out in parallel only for breadth-first research, e.g. screening many tickers, where the roughly 15× token cost pays off.

10. **API-first action, browser as fallback, with independent end-state checks (P2).**
    Live-web write tasks are still only moderately reliable. Prefer MCP/API connectors. Next best is a scripted browser skill. Generic computer use comes last. Always verify the outcome out of band (confirmation email, booking lookup, bank feed), because agents claim success that didn't happen.

11. **Design for the 80% horizon, with checkpoints and resumable state (P2).**
    METR horizons are long and growing, but reliability falls steeply with task length. Break long jobs into verifiable milestones with persisted state, so a failed step can be retried or handed to the human without losing work.

12. **Investment research: rigor over autonomy (P2).**
    Every numeric claim must cite a source (filing, page, line) and pass automated cross-checks. Use bull/bear or debate roles to surface counter-arguments. Never trust LLM backtests without post-cutoff, anonymized evaluation. No autonomous trading; at most, proposals plus approval.

13. **A continuous private evaluation harness (P2).**
    Replay your own task suite, scored on **state** and **pass^k**, plus injected-attack cases (AgentDojo-style) and proactive-precision metrics, on every change of model, prompt, skill, or memory. That is how you find out whether "self-improvement" actually improves anything.

---

### Quick mapping to the three use cases

| Use case | Most relevant principles | Main research-backed risk |
|---|---|---|
| Side-project SWE | 4, 8, 9, 11, 13 (sandbox from 1–2) | Parallel-writer drift; context rot over long sessions |
| Investment research | 3, 6, 9 (parallel fan-out), 12 | Hallucinated or uncited numbers; look-ahead-biased conclusions |
| Life admin (email, bookings, taxes, books) | 1, 2, 3, 5, 6, 7, 10 | Indirect prompt injection via email or web; irreversible actions; tax math errors; over-sharing PII |
