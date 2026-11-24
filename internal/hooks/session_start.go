package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/delegates"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/version"
	"github.com/zjshen14/agy-hq/internal/workspace"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

func classifyWorkspaceForFrontDeskClaim(cleanPath, vaultDir string) (decided, claim bool) {
	if isPathInsideVaultWorktrees(cleanPath, vaultDir) {
		return true, false
	}
	// W3 (RFC-0025 §10): a path inside a provider workspace (ledger entry or trusted root_patterns) is a
	// regular coding session, even though non-git workspace roots carry none of the project markers.
	if isProviderWorkspacePath(vaultDir, cleanPath) {
		return true, false
	}
	if vault.IsAgyHQRepo(cleanPath) {
		return true, true
	}
	cleanVault := filepath.Clean(vaultDir)
	if cleanPath == cleanVault || cleanPath == filepath.Dir(cleanVault) {
		return true, true
	}
	return false, false
}

// shouldAutoClaimVacantFrontDesk decides whether a conversation starting while front_desk.json
// is vacant should automatically claim the [HQ] Front Desk lease.
// External third-party repositories and isolated task worktrees remain normal unblocked coding sessions
// unless explicitly promoted via `hq claim-desk`.
func shouldAutoClaimVacantFrontDesk(payload CommonPayload, vaultDir string) bool {
	if os.Getenv("HQ_AUTO_CLAIM_DESK") == "1" {
		return true
	}
	ws := ""
	if len(payload.WorkspacePaths) > 0 {
		ws = strings.TrimSpace(payload.WorkspacePaths[0])
	}
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil && cwd != "" {
			cleanCWD := filepath.Clean(cwd)
			if decided, claim := classifyWorkspaceForFrontDeskClaim(cleanCWD, vaultDir); decided {
				return claim
			}
			// If cwd is an external project repository (.git, go.mod, or package.json) that is not agy-hq, do not hijack it.
			for _, marker := range []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml"} {
				if _, sErr := os.Stat(filepath.Join(cleanCWD, marker)); sErr == nil {
					return false
				}
			}
		}
		return true
	}
	_, claim := classifyWorkspaceForFrontDeskClaim(filepath.Clean(ws), vaultDir)
	return claim
}

// HandleSessionStart reads local/USER.md, local/MEMORY.md, and active facts,
// returning an injectSteps systemMessage JSON object on stdout.
func HandleSessionStart(r io.Reader, w io.Writer, vaultDir string) error {
	var payload CommonPayload
	_ = json.NewDecoder(r).Decode(&payload)
	_ = hostpaths.PersistFromLiveEnv(vaultDir)
	_ = kernel.RecordLiveLSSession(vaultDir)
	bindSpawnTokenFromTranscript(vaultDir, payload.ConversationID, payload.LastUserInput, payload.TranscriptPath)
	authzPol := authz.Resolve(vaultDir, payload.ConversationID, authz.ResolveHints{})
	isTaintedStart := authzPol.Tainted

	// Zero-touch auto-bootstrap: if USER.md does not exist yet in the target Vault,
	// automatically initialize the private Vault from disk templates/ or embedded templates.FS.
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); os.IsNotExist(err) {
		templateDir := ""
		if rootDir, rErr := vault.ResolveRoot(); rErr == nil && vault.IsAgyHQRepo(rootDir) {
			templateDir = filepath.Join(rootDir, "templates")
		}
		_, _ = vault.Init(vaultDir, templateDir)
	}

	var zone1Sections []string
	var zone2Sections []string

	if plugRoot := strings.TrimSpace(os.Getenv("PLUGIN_ROOT")); plugRoot != "" {
		if m, mErr := version.ReadPluginManifest(plugRoot); mErr == nil && m.Version != "" && m.Version != version.Version {
			zone2Sections = append(zone2Sections, fmt.Sprintf(
				"> ⚠️ **[agy-hq Version Skew]:** Active plugin manifest is `v%s` (`%s`), while `hq` binary is `v%s`. Run `hq upgrade` or `hq plugin install` to synchronize.",
				m.Version, plugRoot, version.Version,
			))
		}
	}

	var matchedTask kernel.ActiveTask
	var hasMatchedTask bool
	var fdState kernel.FrontDeskLease
	var hasFDState bool
	leanAuto := false
	isFrontDesk := false
	isSubagent := false
	isAutoWorker := false

	promptText := strings.TrimSpace(payload.LastUserInput)
	firstUserInputRaw := readFirstUserInputContent(payload.TranscriptPath)
	if promptText == "" && strings.TrimSpace(payload.TranscriptPath) != "" {
		promptText = readFirstUserPromptFromTranscript(payload.TranscriptPath)
	}

	if payload.ConversationID != "" {
		existing, ok := kernel.GetFrontDesk(vaultDir)
		fdState, hasFDState = existing, ok
		isFrontDeskExisting := ok && strings.EqualFold(existing.ConversationID, payload.ConversationID)
		hasQueuedArchive := kernel.HasQueuedSidebarArchive(vaultDir, payload.ConversationID)
		attemptSuperseded := false

		isSubagent = kernel.IsSubagentConversation(vaultDir, payload.ConversationID)
		if !isSubagent {
			if t, alreadyTracked := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); alreadyTracked {
				matchedTask = t
				hasMatchedTask = true
			} else if attemptTask, _, superseded, foundAttempt := kernel.FindTaskAttemptAnywhere(vaultDir, payload.ConversationID); foundAttempt &&
				(superseded || attemptTask.EffectiveKind() == "auto" || attemptTask.EffectiveKind() == "subagent") {
				matchedTask = attemptTask
				hasMatchedTask = true
				attemptSuperseded = superseded
				if attemptTask.EffectiveKind() == "subagent" || strings.HasPrefix(attemptTask.TaskSlug, "subagent-") || strings.HasPrefix(attemptTask.Title, "[Subagent:") {
					isSubagent = true
				}
			} else if !isFrontDeskExisting && !hasQueuedArchive &&
				!hasAutoOrSpawnEvidence(vaultDir, payload.ConversationID, firstUserInputRaw, promptText) {
				nested, _, lin, hasLS := detectNestedDescendant(vaultDir, payload.ConversationID, promptText, existing.ConversationID, 3*time.Minute)
				if nested {
					// Known nested descendant subagent or workflow/agentapi worker: do NOT register into tasks.json!
					isSubagent = true
				} else if sub, found := kernel.PopPendingSubagentForCaller(vaultDir, payload.ConversationID, promptText, 3*time.Minute); found {
					isSubagent = true
					modelName := sub.Model
					if modelName == "" {
						modelName = payload.ModelName
					}
					t := kernel.ActiveTask{
						TaskSlug:             sub.TaskSlug,
						Title:                sub.Title,
						Kind:                 "subagent",
						Scope:                sub.Scope,
						ParentConversationID: sub.ParentConversationID,
						Tier:                 sub.Tier,
						Model:                modelName,
						ConversationID:       payload.ConversationID,
						Status:               "active",
						LastSummary:          "Running in background...",
						CreatedAt:            time.Now().Format(time.RFC3339),
						LastUpdatedAt:        time.Now().Format(time.RFC3339),
					}
					_ = kernel.RegisterTask(vaultDir, t)
					matchedTask = t
					hasMatchedTask = true
				} else if hasLS && lin.IsDirectFrontDeskSubagent {
					isSubagent = true
					t := buildSubagentTaskFromLSLineage(payload.ConversationID, payload.ModelName, "active", "Running in background...", lin)
					_ = kernel.RegisterTask(vaultDir, t)
					matchedTask = t
					hasMatchedTask = true
				} else if linked, okLink := kernel.LinkPendingSpawnTask(vaultDir, payload.ConversationID, payload.ModelName); okLink {
					matchedTask = linked
					hasMatchedTask = true
				}
			}
		} else if t, found := kernel.FindTaskByConversationID(vaultDir, payload.ConversationID); found {
			matchedTask = t
			hasMatchedTask = true
		}

		isAutoWorker = (hasMatchedTask && matchedTask.EffectiveKind() == "auto") ||
			(!hasMatchedTask && !isSubagent && !isFrontDeskExisting &&
				hasAutoOrSpawnEvidence(vaultDir, payload.ConversationID, firstUserInputRaw, promptText))
		leanAuto = isSubagent || isAutoWorker || isTaintedStart || attemptSuperseded
		isFrontDesk = !leanAuto && !hasQueuedArchive && (existing.ConversationID == payload.ConversationID || (!ok && shouldAutoClaimVacantFrontDesk(payload, vaultDir)))
		if isTaintedStart && authzPol.Profile == authz.ProfileQuarantinedReader {
			zone2Sections = append(zone2Sections, "## 🛡️ Active Role: Quarantined Triage Worker (TAINTED)\n- Untrusted external channel content is in scope. Private Vault memory (`MEMORY.md`, wiki topics, facts) is withheld, and outbound commands are restricted to `hq offer stage` and `hq outbox add`.")
		}

		if isFrontDesk {
			resetFrontDeskTurnState(vaultDir, payload.ConversationID)
			writeActiveArtifactDir(vaultDir, payload.ArtifactDirectoryPath)

			_ = kernel.SetFrontDesk(vaultDir, payload.ConversationID, payload.ArtifactDirectoryPath)
			if len(payload.WorkspacePaths) > 0 && strings.TrimSpace(payload.WorkspacePaths[0]) != "" {
				_ = kernel.SetFrontDeskWorkspace(vaultDir, payload.ConversationID, payload.WorkspacePaths[0])
			}
			kernel.ReconcileFrontDeskFromTranscript(vaultDir, payload.TranscriptPath)
			fdState, hasFDState = kernel.GetFrontDesk(vaultDir)
			fd := fdState
			_, unarmedTriggers := kernel.GetUnarmedTriggers(vaultDir)
			disarmTriggers := kernel.GetDisarmTriggers(vaultDir)

			roleBanner := fmt.Sprintf(
				"## 🎯 Active Role: [HQ] Front Desk Coordinator (NON-BLOCKING)\n"+
					"- You are the single Front Desk intake coordinator (`%s`) · Active Vault: `%s`.\n"+
					"- **Hard Mechanical Limit:** You have at most **3 inline lookup tool calls** per turn, and direct code/worktree edits or test runs are mechanically blocked by PreToolUse hooks on this thread.\n"+
					"- For ANY bug investigation, root-cause analysis, or code change, **delegate immediately on Turn 1** via `invoke_subagent` (working in `<vault>/worktrees/<repo>-<task>/`) or `hq spawn` so the user can keep chatting without waiting.\n"+
					"- **Sidebar Pin Recommendation:** %s",
				payload.ConversationID,
				vaultDir,
				kernel.FrontDeskPinTip,
			)
			zone2Sections = append(zone2Sections, roleBanner)

			if len(disarmTriggers) > 0 {
				var pausedLines []string
				var migratedLines []string
				for _, dt := range disarmTriggers {
					killHint := `manage_task(Action="kill")`
					if dt.TaskID != "" {
						killHint = fmt.Sprintf(`manage_task(Action="kill", TaskId=%q)`, dt.TaskID)
					}
					if dt.Reason == "migrated_to_daemon" {
						migratedLines = append(migratedLines, fmt.Sprintf("- `%s` (`%s`): now owned by `hq daemon` (`agy-hq.service`) — cancel duplicate timer in this thread via `%s`.", dt.ID, dt.Cron, killHint))
					} else {
						pausedLines = append(pausedLines, fmt.Sprintf("- `%s` (`%s`): paused via `hq triggers pause` — cancel its active timer in this thread via `%s`.", dt.ID, dt.Cron, killHint))
					}
					if dt.TaskID == "" {
						_ = kernel.ClearArmedTrigger(vaultDir, dt.ID)
					}
				}
				if len(migratedLines) > 0 {
					zone2Sections = append(zone2Sections, "## 🔄 Daemon-Managed Schedules Requiring Duplicate Front Desk Disarm\n"+strings.Join(migratedLines, "\n"))
				}
				if len(pausedLines) > 0 {
					zone2Sections = append(zone2Sections, "## ⏸️ Paused Schedules Requiring Front Desk Disarm\n"+strings.Join(pausedLines, "\n"))
				}
			}

			if !ok || !fd.ConsolePinned || len(unarmedTriggers) > 0 {
				artDir := strings.TrimSpace(payload.ArtifactDirectoryPath)
				if artDir == "" {
					artDir = strings.TrimSpace(fd.ArtifactDir)
				}
				if artDir == "" {
					artDir = vault.ResolveArtifactDir(payload.ConversationID)
				}
				artPath := filepath.Join(artDir, "hq_console.md")
				consolePath := filepath.Join(vaultDir, "CONSOLE.md")

				var initLines []string
				initLines = append(initLines,
					"## 🚀 New [HQ] Front Desk Initialization Required",
					fmt.Sprintf("- Active Front Desk Conversation ID: `%s`.", payload.ConversationID),
					"- **Step 1 (Zero Redundant Reads):** `USER.md`, `MEMORY.md`, active worktrees, tracked Task Conversations, and `local/triggers.yaml` schedules are already loaded in this session context. Do **NOT** run shell commands or `read_file` to re-fetch them.",
				)
				if !fd.ConsolePinned {
					initLines = append(initLines, fmt.Sprintf(
						"- **Step 2 (Pin Live Console Artifact):** Read `%s` and call `write_file` (`Overwrite=true`, `ArtifactMetadata={UserFacing: true, RequestFeedback: false, Summary: \"Live [HQ] Front Desk Console & Task Board\"}`) to `%s` so Antigravity pins the live status artifact in this conversation's UI.",
						consolePath, artPath,
					))
				} else {
					initLines = append(initLines, fmt.Sprintf(
						"- **Step 2 (Pin Live Console Artifact):** Already pinned at `%s`.",
						artPath,
					))
				}
				if len(unarmedTriggers) > 0 {
					initLines = append(initLines, fmt.Sprintf(
						"- **Step 3 (Arm Proactive Schedules from `local/triggers.yaml`):** %d declared schedule(s) are currently unarmed in this Front Desk session. Call the `schedule` tool for each unarmed trigger now:",
						len(unarmedTriggers),
					))
					for _, s := range unarmedTriggers {
						initLines = append(initLines, fmt.Sprintf(
							"  - `%s` (`%s`): `schedule(CronExpression=%q, IsDaemon=true, Prompt=%q)`",
							s.ID, s.Cron, s.FiveFieldCron(), s.DefaultPrompt(),
						))
					}
				}
				initLines = append(initLines, fmt.Sprintf(
					"- **Step 4 (Concise Welcome & Sidebar Pin):** Present a short status summary with clickable links to `[hq_console.md](file://%s)` and all active `[Task:<scope>]` conversations, and recommend the user pin this `[HQ] Front Desk` conversation to their Antigravity sidebar so it stays anchored at the top.",
					artPath,
				))
				zone2Sections = append(zone2Sections, strings.Join(initLines, "\n"))
			}
			if _, _, nudgeBanner := obs.GetFrontDeskContextStatus(vaultDir, payload.TranscriptPath); nudgeBanner != "" {
				zone2Sections = append(zone2Sections, nudgeBanner)
			}
		} else {
			// Subagent / [Auto] worker or secondary/external workspace conversation: never inject Front Desk notice or claim-desk instructions for subagents/[Auto] workers
			if !leanAuto && existing.ConversationID != "" {
				zone2Sections = append(zone2Sections, fmt.Sprintf(
					"## ⚠️ Front Desk Notice\n- Active Vault: `%s`.\n- Another conversation (`%s`) currently holds the Front Desk intake lease (since %s); coding and test execution in this workspace are unrestricted.\n- To avoid split-brain race conditions, either use `%s` as your main intake desk, or if you are replacing/archiving it, run `hq claim-desk --conv=%s` to transfer the Front Desk lease to this thread.\n- **Tip:** %s",
					vaultDir, existing.ConversationID, existing.UpdatedAt, existing.ConversationID, payload.ConversationID,
					kernel.FrontDeskPinTip,
				))
			} else if !leanAuto && !ok {
				zone2Sections = append(zone2Sections, fmt.Sprintf(
					"## 🗂️ Active HQ Vault (`%s`)\n- Mode: Unrestricted Workspace Session (Front Desk coordinator lease is vacant; run `hq claim-desk --conv=%s` in a dedicated chat thread whenever you want to anchor `[HQ] Front Desk`).",
					vaultDir, payload.ConversationID,
				))
			}
		}
	} else if payload.ArtifactDirectoryPath != "" && shouldAutoClaimVacantFrontDesk(payload, vaultDir) &&
		!hasAutoOrSpawnEvidence(vaultDir, "", firstUserInputRaw, promptText) {
		if existing, ok := kernel.GetFrontDesk(vaultDir); !ok {
			writeActiveArtifactDir(vaultDir, payload.ArtifactDirectoryPath)
		} else {
			fdState, hasFDState = existing, ok
		}
	}

	now := time.Now().UTC()
	scopeCtx := ResolveSessionScopes(vaultDir, payload, matchedTask, hasMatchedTask, isFrontDesk, isSubagent, isAutoWorker, leanAuto, promptText)

	// Tainted Session (fail_closed / classifier / quarantined_reader / scoped_delegate): omit all private Vault memory layers.
	if isTaintedStart {
		msg, _ := renderAuthzTaintedSessionBanner(authzPol, zone2Sections)
		return writeSessionStartMessage(w, msg)
	}

	// Zone 1: Byte-Stable Cacheable Prefix (Role-Aware Dual Line + Byte Budgets)
	if userBytes, err := os.ReadFile(filepath.Join(vaultDir, "USER.md")); err == nil {
		if s := strings.TrimSpace(string(userBytes)); s != "" {
			userCap := 2400
			if leanAuto {
				userCap = 1400
			}
			s = trimBytesPreservingLines(s, userCap)
			if !leanAuto && len(userBytes) > vault.MaxUserAdvisoryBytes {
				s += fmt.Sprintf("\n\n> ⚠️ `local/USER.md` is %d bytes (advisory target <= %d bytes). Trim prose to keep startup context lean.", len(userBytes), vault.MaxUserAdvisoryBytes)
			}
			zone1Sections = append(zone1Sections, "## HQ Vault: USER.md\n"+s)
		}
	}

	// MEMORY.md: Active Priorities ONLY (<= 40 lines & <= 2,200 bytes). Omitted in lean_auto to prevent Zone 2 starvation.
	if !leanAuto {
		if memBytes, err := os.ReadFile(filepath.Join(vaultDir, "MEMORY.md")); err == nil {
			if s := strings.TrimSpace(string(memBytes)); s != "" {
				trimmedMem, wasTrunc := vault.TruncateMemoryIndex(s, vault.MaxMemoryIndexLines, vault.MaxMemoryIndexBytes)
				if wasTrunc {
					trimmedMem += fmt.Sprintf("\n\n> ⚠️ **[Roadmap Index Budget Warning]:** `local/MEMORY.md` exceeds %d lines / %d bytes. Run `/sleep-consolidate` or `hq memory status` to keep `MEMORY.md` focused on Active Priorities.", vault.MaxMemoryIndexLines, vault.MaxMemoryIndexBytes)
				}
				zone1Sections = append(zone1Sections, "## HQ Vault: MEMORY.md\n"+trimmedMem)
			}
		}
	}

	topics, _ := vault.LoadTopicIndex(vaultDir)
	if len(topics) > 0 {
		var selectedTopics []vault.TopicPage
		if leanAuto {
			selectedTopics = vault.FilterTopicsForScope(topics, scopeCtx, 5)
		} else {
			topicLimit := 15
			if !isFrontDesk {
				topicLimit = 10
			}
			// Put scope-matched topics first, then remaining topics deterministically up to topicLimit.
			matched := vault.FilterTopicsForScope(topics, scopeCtx, topicLimit)
			seenSlug := make(map[string]bool, len(matched))
			for _, tp := range matched {
				seenSlug[tp.Slug] = true
				selectedTopics = append(selectedTopics, tp)
			}
			for _, tp := range topics {
				if len(selectedTopics) >= topicLimit {
					break
				}
				if !isFrontDesk && vault.TopicConflictsWithScope(tp, scopeCtx) {
					continue
				}
				if !seenSlug[tp.Slug] {
					seenSlug[tp.Slug] = true
					selectedTopics = append(selectedTopics, tp)
				}
			}
		}
		if len(selectedTopics) > 0 {
			var topicLines []string
			topicLines = append(topicLines, "> **Retrieval Directive:** Call `read_file` on relevant topic pages below, or run `hq memory search \"<query>\"` for full-vault BM25 lookup.")
			for _, tp := range selectedTopics {
				summary := trimInlineRunes(tp.Summary, 110)
				if summary != "" {
					topicLines = append(topicLines, fmt.Sprintf("- **[%s](file://%s)** (`%s`): %s", tp.Title, tp.Path, tp.Slug, summary))
				} else {
					topicLines = append(topicLines, fmt.Sprintf("- **[%s](file://%s)** (`%s`)", tp.Title, tp.Path, tp.Slug))
				}
			}
			if len(topics) > len(selectedTopics) {
				topicLines = append(topicLines, fmt.Sprintf("- *(+%d more topic page(s) available via `hq memory search`)*", len(topics)-len(selectedTopics)))
			}
			topicBlock := "## HQ Knowledge Wiki (Topic Index — Read On-Demand)\n" + strings.Join(topicLines, "\n")
			if leanAuto {
				topicBlock = trimBytesPreservingLines(topicBlock, 750)
			} else {
				topicBlock = trimBytesPreservingLines(topicBlock, 1800)
			}
			zone1Sections = append(zone1Sections, topicBlock)
		}
	}

	if !leanAuto {
		if overrides := extension.ResolveActivePackSkillOverrides(vaultDir); len(overrides) > 0 {
			var ovLines []string
			for _, res := range overrides {
				ovLines = append(ovLines, fmt.Sprintf("- `/%s` -> `%s` ([SKILL.md](file://%s))", res.LogicalSkill, res.ActiveTarget, res.SkillPath))
			}
			zone1Sections = append(zone1Sections, "## Active Companion Skill Overrides\n"+strings.Join(ovLines, "\n"))
		}
	}

	rsiLimit := 5
	if leanAuto {
		rsiLimit = 2
	}
	if genRules := extension.LoadGeneralOverlayRules(vaultDir, rsiLimit); len(genRules) > 0 {
		rsiBlock := "## Learned Personal RSI Rules (`general`)\n" + strings.Join(genRules, "\n\n")
		if leanAuto {
			rsiBlock = trimBytesPreservingLines(rsiBlock, 550)
		}
		zone1Sections = append(zone1Sections, rsiBlock)
	}

	// Zone 2: Role-Scoped Dynamic Tail
	if !leanAuto {
		if isFrontDesk {
			if ovs := extension.LoadActiveFrontDeskOverlays(vaultDir, "front_desk"); len(ovs) > 0 {
				zone2Sections = append(zone2Sections, "## Companion Front Desk Policy Overlays (Advisory)\n"+trimBytesPreservingLines(strings.Join(ovs, "\n\n"), extension.MaxOverlayBytes))
			}
		} else if hasMatchedTask {
			taskScope := strings.TrimSpace(matchedTask.Scope)
			if taskScope == "" {
				taskScope = "general"
			}
			if ovs := extension.LoadActiveFrontDeskOverlays(vaultDir, "task:"+taskScope); len(ovs) > 0 {
				zone2Sections = append(zone2Sections, "## Companion Task Policy Overlays (Advisory)\n"+trimBytesPreservingLines(strings.Join(ovs, "\n\n"), extension.MaxOverlayBytes))
			}
		}
	}

	if hasMatchedTask && matchedTask.TaskSlug != "" {
		if _, rawProg, foundProg, pErr := vault.LoadProgress(vaultDir, matchedTask.TaskSlug); pErr == nil && foundProg && strings.TrimSpace(rawProg) != "" {
			progCap := 1200
			if leanAuto {
				progCap = 750
			}
			zone2Sections = append(zone2Sections, "## Active Task Progress Checkpoint\n"+trimBytesPreservingLines(strings.TrimSpace(rawProg), progCap))
		}
		if !leanAuto && !isTaintedStart {
			if nudgeBanner := delegates.FormatZone2NudgeBanner(vaultDir, matchedTask.TaskSlug); nudgeBanner != "" {
				zone2Sections = append(zone2Sections, nudgeBanner)
			}
		}
	}

	// Scoped ## Active Playbook (1:1 Playbook-Owner, 0 on Front Desk)
	if len(topics) > 0 {
		playbooks := vault.SelectScopedPlaybooks(topics, scopeCtx, vault.MaxPlaybookLines, vault.MaxPlaybookBytes)
		for _, pb := range playbooks {
			pbCap := vault.MaxPlaybookBytes
			if leanAuto {
				pbCap = 850
			}
			pbBody := trimBytesPreservingLines(pb.ActivePlaybook, pbCap)
			zone2Sections = append(zone2Sections, fmt.Sprintf("### Scoped Playbook: %s (`%s`)\n%s", pb.Title, pb.PlaybookOwner, pbBody))
		}
		// 1-line Matched Context Hints (Task & Subagent sessions only; 0 on Front Desk)
		if hints := vault.MatchContextHints(topics, scopeCtx, 2); len(hints) > 0 {
			var hLines []string
			for _, h := range hints {
				hLines = append(hLines, fmt.Sprintf("- **%s**: %s (`read_file` [file](file://%s))", h.Title, trimInlineRunes(h.Summary, 90), h.Path))
			}
			zone2Sections = append(zone2Sections, "## Matched Context Hints\n"+strings.Join(hLines, "\n"))
		}
	}

	// Role-Scoped Active Vault Facts (Deterministic Go Physics + Temporal Precedence Directive)
	if activeFacts, _, _, err := vault.LoadFactsStateAt(vaultDir, now); err == nil && len(activeFacts) > 0 {
		factMaxCount, factMaxBytes := 15, 2200
		if isFrontDesk {
			factMaxCount, factMaxBytes = 12, 2000
		} else if leanAuto {
			factMaxCount, factMaxBytes = 6, 850
		}
		selectedFacts := vault.SelectFactsForSession(activeFacts, scopeCtx, now, factMaxCount, factMaxBytes)
		if len(selectedFacts) > 0 {
			var lines []string
			lines = append(lines, "> **Temporal Precedence:** If the user's current request or live tool output contradicts any stored fact below, treat the live statement/output as ground truth and update via `hq memory add-fact`.")
			for _, f := range selectedFacts {
				lines = append(lines, vault.FormatFactBullet(f))
			}
			zone2Sections = append(zone2Sections, "## Active Vault Facts\n"+strings.Join(lines, "\n"))
		}
	}

	reportedLimit := 6
	if leanAuto {
		reportedLimit = 4
	}
	if reportedDigest := outbox.FormatReportedDigest(vaultDir, now, reportedLimit); reportedDigest != "" {
		repCap := 900
		if leanAuto {
			repCap = 600
		}
		zone2Sections = append(zone2Sections, "## Recently Reported to User\n"+trimBytesPreservingLines(reportedDigest, repCap))
	}

	if !leanAuto {
		_, _ = approvals.ReconcileAskRejections(vaultDir, "", "", now)
		_, _ = approvals.ReconcileLocalGitProposals(vaultDir, now)
		if pol, _, pErr := approvals.LoadPolicy(vaultDir); pErr == nil {
			zone2Sections = append(zone2Sections, approvals.FormatActivePolicySummary(pol))
		}
		if pending, err := approvals.List(vaultDir); err == nil && len(pending) > 0 {
			var lines []string
			for _, p := range pending {
				short := p.SHA256
				if len(short) > 12 {
					short = short[:12]
				}
				lines = append(lines, fmt.Sprintf("- `%s` (%s · sha256:%s): %s — `%s`", p.ID, p.ActionType, short, p.Summary, p.Command))
			}
			zone2Sections = append(zone2Sections, "## Pending Approvals in Queue\n"+trimBytesPreservingLines(strings.Join(lines, "\n"), 1200))
		}
		// Unified Active Workspaces section combining managed git worktrees (zero-subprocess fast listing)
		// and provider ledger entries (read-only cache; never calls external providers).
		var layer1Tasks []kernel.ActiveTask
		if allTasks, tErr := kernel.ListTasks(vaultDir); tErr == nil {
			layer1Tasks = kernel.FilterLayer1Tasks(vaultDir, allTasks)
		}
		var gitEntries []workspace.SurfaceEntry
		if wts, err := worktree.List(vaultDir); err == nil && len(wts) > 0 {
			gitEntries = kernel.FastGitWorktreeSurfaceEntries(vaultDir, wts, layer1Tasks, now)
		}
		wsLedger, wsErr := workspace.LoadLedger(vaultDir)
		if wsErr != nil {
			zone2Sections = append(zone2Sections, "## Workspace Ledger Warning\n- ⚠️ `local/memory/workspaces.json` could not be loaded (corrupt, symlinked, or newer schema); workspace edits outside the Vault are blocked fail-closed until repaired.")
		}
		if sec := formatUnifiedActiveWorkspacesSection(wsLedger, gitEntries, layer1Tasks, now); sec != "" {
			zone2Sections = append(zone2Sections, sec)
		}
		if hasMatchedTask && !isFrontDesk {
			if sec := formatActiveTaskWorkspacesSection(wsLedger, matchedTask); sec != "" {
				zone2Sections = append(zone2Sections, sec)
			}
		}
		if len(layer1Tasks) > 0 {
			selected, omitted := kernel.SelectSessionStartTasks(layer1Tasks, now)
			if len(selected) > 0 {
				var lines []string
				for _, t := range selected {
					cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
					lines = append(lines, fmt.Sprintf("- **%s** (`%s`, kind: `%s`, status: `%s`, conv: `%s`)", cleanTitle, t.TaskSlug, t.EffectiveKind(), t.EffectiveStatus(now), t.ConversationID))
				}
				body := trimBytesPreservingLines(strings.Join(lines, "\n"), 1450)
				if omitted > 0 {
					body += fmt.Sprintf("\n- *(+%d older completed/archived task(s) omitted from context; run `hq tasks` to inspect)*", omitted)
				}
				zone2Sections = append(zone2Sections, "## Tracked Task Conversations\n"+body)
			}
		}
		if cfg, err := kernel.LoadEffectiveTriggers(vaultDir); err == nil && len(cfg.Schedules) > 0 {
			if !hasFDState && payload.ConversationID == "" {
				fdState, _ = kernel.GetFrontDesk(vaultDir)
			}
			daemonActive := kernel.IsDaemonActiveFast(vaultDir)
			var lines []string
			for _, s := range cfg.Schedules {
				status := "UNARMED"
				if !s.IsEnabled() {
					status = "PAUSED"
				} else if daemonActive {
					status = "DAEMON_ACTIVE"
				} else if fdState.ArmedTriggers != nil {
					if _, ok := fdState.ArmedTriggers[s.ID]; ok {
						status = "ARMED"
					} else if _, okFQ := fdState.ArmedTriggers[s.FQID()]; okFQ {
						status = "ARMED"
					}
				}
				lines = append(lines, fmt.Sprintf("- `%s` (`%s`, mode: `%s`, status: `%s`, skill: `%s`)", s.ID, s.Cron, s.EffectiveMode(), status, s.Skill))
			}
			zone2Sections = append(zone2Sections, "## Proactive Triggers (`local/triggers.yaml`)\n"+trimBytesPreservingLines(strings.Join(lines, "\n"), 1100))
			if len(fdState.AdHocCrons) > 0 {
				var adHocLines []string
				for _, ac := range fdState.AdHocCrons {
					idPart := ""
					if ac.TaskID != "" {
						idPart = fmt.Sprintf("taskId: `%s`, ", ac.TaskID)
					}
					adHocLines = append(adHocLines, fmt.Sprintf("- **%s** (%s`%s`)", ac.Summary, idPart, ac.Cron))
				}
				zone2Sections = append(zone2Sections, "## Ad-Hoc Conversation Crons (Preserved in Front Desk)\n"+trimBytesPreservingLines(strings.Join(adHocLines, "\n"), 600))
			}
		}
	}

	sections := append(zone1Sections, zone2Sections...)

	if len(sections) == 0 {
		_, err := w.Write([]byte("{}\n"))
		return err
	}

	header := "[agy-hq Session Context Loaded]\n\n"
	if leanAuto {
		header = "[agy-hq Lean Auto/Skill Context Loaded]\n\n"
	}
	budget := vault.MaxSessionFullBytes
	if leanAuto {
		budget = vault.MaxSessionLeanBytes
	}
	msg := assembleSessionMessage(header, sections, budget)
	return writeSessionStartMessage(w, msg)
}

func writeSessionStartMessage(w io.Writer, msg string) error {
	resp := map[string]interface{}{
		"injectSteps": []map[string]interface{}{
			{
				"systemMessage": map[string]interface{}{
					"systemMessage": msg,
				},
			},
		},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}

var reWorktreePathPrompt = regexp.MustCompile(`worktrees/([a-zA-Z0-9._-]+)`)

// readFirstUserPromptFromTranscript performs a bounded <=16KB read of transcript.jsonl to recover Turn-1 user prompt
// when Antigravity SessionStart stdin omits lastUserInput.
func readFirstUserPromptFromTranscript(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	lr := io.LimitReader(f, 16*1024)
	scanner := bufio.NewScanner(lr)
	scanner.Buffer(make([]byte, 0, 4096), 16*1024)
	for scanner.Scan() {
		var step struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if json.Unmarshal(scanner.Bytes(), &step) != nil {
			continue
		}
		if step.Type == "USER_INPUT" && strings.TrimSpace(step.Content) != "" {
			return trimInlineRunes(cleanUserPrompt(step.Content), 512)
		}
	}
	return ""
}

// ResolveSessionScopes derives deterministic canonical scopes for SessionStart selection
// with strict repo precedence: (1) WorktreeDir / worktree path in prompt -> (2) Task Scope/RepoRoot -> (3) WorkspacePaths[0].
func ResolveSessionScopes(vaultDir string, payload CommonPayload, matchedTask kernel.ActiveTask, hasMatchedTask, isFrontDesk, isSubagent, isAutoWorker, leanAuto bool, promptText string) vault.SessionScopeContext {
	role := "task"
	switch {
	case isFrontDesk:
		role = "front_desk"
	case isAutoWorker:
		role = "auto"
	case isSubagent:
		role = "subagent"
	}

	ctx := vault.SessionScopeContext{
		Role:       role,
		PromptText: promptText,
		LeanAuto:   leanAuto,
	}

	scopeSet := make(map[string]bool)
	addScope := func(raw string) {
		s := vault.NormalizeScope(raw)
		if s != "" && !scopeSet[s] {
			scopeSet[s] = true
			ctx.Scopes = append(ctx.Scopes, s)
		}
	}

	extractRepoFromWorktree := func(wt string) string {
		base := strings.ToLower(strings.TrimSpace(filepath.Base(filepath.Clean(wt))))
		if base == "" || base == "." || base == "/" {
			return ""
		}
		prefix := base
		if idx := strings.IndexByte(base, '-'); idx > 0 {
			prefix = base[:idx]
		}
		if prefix == "headquarter" || prefix == "agy" {
			return "agy-hq"
		}
		return prefix
	}

	// Precedence 1: matchedTask.WorktreeDir or explicit <vault>/worktrees/<repo>-<task> path in promptText
	if hasMatchedTask && strings.TrimSpace(matchedTask.WorktreeDir) != "" {
		if r := extractRepoFromWorktree(matchedTask.WorktreeDir); r != "" {
			ctx.RepoSlug = r
		}
	}
	if ctx.RepoSlug == "" && promptText != "" {
		if m := reWorktreePathPrompt.FindStringSubmatch(filepath.ToSlash(promptText)); len(m) >= 2 {
			if r := extractRepoFromWorktree(m[1]); r != "" {
				ctx.RepoSlug = r
			}
		}
	}

	// Precedence 2: matchedTask.RepoRoot / matchedTask.Scope
	if hasMatchedTask {
		if ctx.RepoSlug == "" && strings.TrimSpace(matchedTask.RepoRoot) != "" {
			rr := filepath.Clean(matchedTask.RepoRoot)
			if vault.IsAgyHQRepo(rr) {
				ctx.RepoSlug = "agy-hq"
			} else {
				ctx.RepoSlug = strings.ToLower(filepath.Base(rr))
			}
		}
		if sc := strings.ToLower(strings.TrimSpace(matchedTask.Scope)); sc != "" && sc != "general" {
			ctx.TaskScope = sc
			addScope("domain:" + sc)
			if ctx.RepoSlug == "" && (sc == "headquarter" || sc == "agy-hq") {
				ctx.RepoSlug = "agy-hq"
			}
		}
	}

	// Precedence 3: WorkspacePaths[0]
	if ctx.RepoSlug == "" && len(payload.WorkspacePaths) > 0 {
		ws := strings.TrimSpace(payload.WorkspacePaths[0])
		if ws != "" {
			cleanWS := filepath.Clean(ws)
			if isPathInsideVaultWorktrees(cleanWS, vaultDir) {
				ctx.RepoSlug = extractRepoFromWorktree(cleanWS)
			} else if vault.IsAgyHQRepo(cleanWS) {
				ctx.RepoSlug = "agy-hq"
			} else {
				ctx.RepoSlug = strings.ToLower(filepath.Base(cleanWS))
			}
		}
	}

	if ctx.RepoSlug != "" {
		addScope("repo:" + ctx.RepoSlug)
		if ctx.RepoSlug == "agy-hq" {
			addScope("repo:headquarter")
		}
	}

	// Skill & domain cues from task metadata + Turn-1 prompt
	cueText := strings.ToLower(strings.TrimSpace(matchedTask.TaskSlug + " " + matchedTask.Title + " " + promptText))
	skillDomainMap := map[string]string{
		"thesis-check":      "investing",
		"filing-diff":       "investing",
		"swe-pulse":         "swe",
		"gchat":             "comms",
		"gmail":             "comms",
		"tax-prep":          "finance",
		"ledger-ingest":     "finance",
		"morning-brief":     "ops",
		"sleep-consolidate": "ops",
	}
	for sk, dom := range skillDomainMap {
		if strings.Contains(cueText, sk) {
			if ctx.ActiveSkill == "" {
				ctx.ActiveSkill = sk
			}
			addScope("skill:" + sk)
			addScope("domain:" + dom)
		}
	}

	return ctx
}

func sectionTrimPriority(sec string) (tier int, floor int) {
	// Tier 0 (trim first): operational chrome, indices, and verbose peripheral lists
	// Tier 1 (trim second): roadmap, task checkpoints, approvals, role banners
	// Tier 2 (protected core): USER.md standing rules, Active Vault Facts, Scoped Playbooks
	switch {
	case strings.HasPrefix(sec, "## Companion Front Desk Policy Overlays"),
		strings.HasPrefix(sec, "## Companion Task Policy Overlays"):
		return 2, 2400
	case strings.HasPrefix(sec, "## HQ Vault: USER.md"),
		strings.HasPrefix(sec, "## Active Vault Facts"),
		strings.HasPrefix(sec, "### Scoped Playbook:"):
		return 2, 1600
	case strings.HasPrefix(sec, "## 📬 Queued Peer Priority Nudges"):
		return 1, 400
	case strings.HasPrefix(sec, "## HQ Vault: MEMORY.md"),
		strings.HasPrefix(sec, "## Active Task Progress Checkpoint"),
		strings.HasPrefix(sec, "## Active Task Workspaces"),
		strings.HasPrefix(sec, "## Pending Approvals in Queue"),
		strings.HasPrefix(sec, "## Matched Context Hints"),
		strings.HasPrefix(sec, "## 🎯 Active Role:"),
		strings.HasPrefix(sec, "## 🚀 New [HQ] Front Desk"),
		strings.HasPrefix(sec, "## ⚠️ Front Desk Notice"):
		return 1, 800
	case strings.HasPrefix(sec, "## Tracked Task Conversations"):
		return 0, 420
	default:
		return 0, 260
	}
}

func trimSectionPreservingFooter(sec string, targetBytes int) string {
	// Preserve the omitted-task summary footer on Tracked Task Conversations (and the overflow footer on
	// Active Workspaces) if present.
	if strings.HasPrefix(sec, "## Tracked Task Conversations") || strings.HasPrefix(sec, "## Active Workspaces") {
		lines := strings.Split(sec, "\n")
		if len(lines) > 2 && (strings.Contains(lines[len(lines)-1], "omitted from context") || strings.Contains(lines[len(lines)-1], "more: hq workspace list")) {
			footer := lines[len(lines)-1]
			headBudget := targetBytes - len(footer) - 2
			if headBudget > 120 {
				head := trimBytesPreservingLines(strings.Join(lines[:len(lines)-1], "\n"), headBudget)
				return head + "\n" + footer
			}
		}
	}
	return trimBytesPreservingLines(sec, targetBytes)
}

func assembleSessionMessage(header string, sections []string, maxBytes int) string {
	full := header + strings.Join(sections, "\n\n")
	if maxBytes <= 0 || len(full) <= maxBytes {
		return full
	}
	// Priority-tiered budget trimming: shrink peripheral operational chrome (tier 0) first,
	// then tier 1, keeping protected core memory (USER.md, Active Vault Facts, Scoped Playbooks) intact.
	trimmed := append([]string(nil), sections...)
	for tier := 0; tier <= 2; tier++ {
		for pass := 0; pass < len(trimmed)*2; pass++ {
			cur := header + strings.Join(trimmed, "\n\n")
			if len(cur) <= maxBytes {
				return cur
			}
			excess := len(cur) - maxBytes
			bestIdx := -1
			bestSlack := 0
			bestFloor := 0
			for i, sec := range trimmed {
				st, fl := sectionTrimPriority(sec)
				if st != tier {
					continue
				}
				if maxBytes <= vault.MaxSessionLeanBytes && fl > 650 {
					fl = 650
				}
				slack := len(sec) - fl
				if slack > bestSlack {
					bestSlack = slack
					bestIdx = i
					bestFloor = fl
				}
			}
			if bestIdx < 0 || bestSlack < 32 {
				break
			}
			targetLen := len(trimmed[bestIdx]) - excess - 24
			if targetLen < bestFloor {
				targetLen = bestFloor
			}
			trimmed[bestIdx] = trimSectionPreservingFooter(trimmed[bestIdx], targetLen)
		}
	}
	return capSessionEnvelope(header+strings.Join(trimmed, "\n\n"), maxBytes)
}
