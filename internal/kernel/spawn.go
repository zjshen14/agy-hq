package kernel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/routing"
)

func resolveSpawnTarget(vaultDir, trigger string) (workDir, projectID string) {
	fd, hasFD := GetFrontDesk(vaultDir)
	isManual := strings.HasPrefix(trigger, "manual-spawn:")
	if !isManual {
		if hasFD && fd.WorkspaceDir != "" {
			if st, err := os.Stat(fd.WorkspaceDir); err == nil && st.IsDir() {
				workDir = fd.WorkspaceDir
			}
		}
		if workDir == "" && vaultDir != "" {
			cleanV := filepath.Clean(vaultDir)
			if filepath.Base(cleanV) == "local" {
				parent := filepath.Dir(cleanV)
				if st, err := os.Stat(parent); err == nil && st.IsDir() {
					workDir = parent
				}
			}
			if workDir == "" {
				if st, err := os.Stat(cleanV); err == nil && st.IsDir() {
					workDir = cleanV
				}
			}
		}
	} else if wd, err := os.Getwd(); err == nil && wd != "" {
		workDir = wd
	}

	inSidecar := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != ""
	overrideProj := !isManual || inSidecar
	if overrideProj {
		if hasFD {
			projectID = strings.TrimSpace(fd.ProjectID)
		}
		if projectID == "" {
			if sess, ok := loadReachableLSSession(vaultDir); ok && strings.TrimSpace(sess.ProjectID) != "" {
				projectID = strings.TrimSpace(sess.ProjectID)
			}
		}
		if projectID == "" && isCallerInPrimaryVaultWorkspace(vaultDir) {
			projectID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
		}
	} else {
		projectID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
		if projectID == "" && hasFD {
			projectID = strings.TrimSpace(fd.ProjectID)
		}
		if projectID == "" {
			if sess, ok := loadReachableLSSession(vaultDir); ok && strings.TrimSpace(sess.ProjectID) != "" {
				projectID = strings.TrimSpace(sess.ProjectID)
			}
		}
	}
	if workDir != "" {
		verifiedProjID, _, repaired := ResolveVerifiedWorkspaceProjectID(vaultDir, workDir, projectID)
		projectID = verifiedProjID
		if !isManual && hasFD && repaired && strings.TrimSpace(fd.ProjectID) != projectID && filepath.Clean(workDir) == filepath.Clean(primaryVaultWorkspaceDir(vaultDir)) {
			fd.ProjectID = projectID
			fd.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = saveFrontDeskLease(vaultDir, fd)
		}
	}
	return workDir, projectID
}

func configureSpawnCommand(cmd *exec.Cmd, vaultDir, trigger string) {
	workDir, projectID := resolveSpawnTarget(vaultDir, trigger)
	configureSpawnCommandWithTarget(cmd, vaultDir, trigger, workDir, projectID)
}

func configureSpawnCommandWithTarget(cmd *exec.Cmd, vaultDir, trigger, workDir, projectID string) {
	isManual := strings.HasPrefix(trigger, "manual-spawn:")
	if !isManual && workDir != "" {
		cmd.Dir = workDir
	}

	rawEnv := os.Environ()
	cleanEnv := make([]string, 0, len(rawEnv)+1)
	inSidecar := os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != ""
	envProj := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	overrideProj := !isManual || inSidecar || (envProj != projectID)

	for _, kv := range rawEnv {
		if strings.HasPrefix(kv, "ANTIGRAVITY_CONVERSATION_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_TRAJECTORY_ID=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SOURCE_METADATA=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SIDECAR_WEB_PORT=") ||
			strings.HasPrefix(kv, "ANTIGRAVITY_SIDECAR_UI_TOKEN=") {
			continue
		}
		if overrideProj && strings.HasPrefix(kv, "ANTIGRAVITY_PROJECT_ID=") {
			continue
		}
		cleanEnv = append(cleanEnv, kv)
	}
	if overrideProj && projectID != "" {
		cleanEnv = append(cleanEnv, "ANTIGRAVITY_PROJECT_ID="+projectID)
	}
	hasEnvNonEmpty := func(env []string, key string) bool {
		prefix := key + "="
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) && strings.TrimSpace(strings.TrimPrefix(kv, prefix)) != "" {
				return true
			}
		}
		return false
	}
	if !hasEnvNonEmpty(cleanEnv, "ANTIGRAVITY_LS_ADDRESS") || !hasEnvNonEmpty(cleanEnv, "ANTIGRAVITY_CSRF_TOKEN") || !hasEnvNonEmpty(cleanEnv, "ANTIGRAVITY_AGENTAPI_EXE") {
		if creds, ok := discoverLSCredentials(vaultDir); ok {
			rawAddr := strings.TrimSpace(creds.RawAddress)
			if rawAddr == "" {
				rawAddr, _ = normalizeLSAddress(creds.BaseURL)
			}
			setIfMissing := func(key, val string) {
				if val == "" || hasEnvNonEmpty(cleanEnv, key) {
					return
				}
				prefix := key + "="
				filtered := cleanEnv[:0]
				for _, kv := range cleanEnv {
					if !strings.HasPrefix(kv, prefix) {
						filtered = append(filtered, kv)
					}
				}
				cleanEnv = append(filtered, prefix+val)
			}
			setIfMissing("ANTIGRAVITY_LS_ADDRESS", rawAddr)
			setIfMissing("ANTIGRAVITY_CSRF_TOKEN", strings.TrimSpace(creds.CSRFToken))
			setIfMissing("ANTIGRAVITY_AGENTAPI_EXE", strings.TrimSpace(creds.AgentAPIExe))
		}
	}
	cmd.Env = cleanEnv
}

func isQuotaExhaustedError(msg string) bool {
	return strings.Contains(msg, "RESOURCE_EXHAUSTED") || strings.Contains(msg, "429")
}

func resolveRoutineFallbackModel(vaultDir string) string {
	rFallback := routing.Resolve(vaultDir, routing.Request{ExplicitTier: "routine"})
	fallbackModel := strings.TrimSpace(rFallback.FallbackModel)
	if fallbackModel == "" {
		fallbackModel = rFallback.ModelID
	}
	return fallbackModel
}

func spawnTaskViaLS(vaultDir, title, prompt, modelID, workDir, projectID string, onCascadeCreated func(cascadeID string)) (cascadeID string, usedModel string, fallbackToAgentAPI bool, fallbackReason string, err error) {
	_, regAvail := ValidateProjectMatchesWorkspace(vaultDir, projectID, workDir)
	canFallbackSafely := func(requireVerifiedProj bool) bool {
		if _, lpErr := exec.LookPath("agentapi"); lpErr != nil {
			return false
		}
		if workDir == "" {
			return true
		}
		if requireVerifiedProj || regAvail {
			return strings.TrimSpace(projectID) != ""
		}
		return true
	}

	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		if canFallbackSafely(false) {
			return "", "", true, "Language Server credentials unavailable", nil
		}
		return "", "", false, "", fmt.Errorf("spawnTaskViaLS: Language Server credentials unavailable and no verified projectId for workspace %s (refusing unverified agentapi fallback)", workDir)
	}
	modelEnum, resolvedModelID, fellBack, enumErr := resolveModelEnumLive(vaultDir, creds.BaseURL, creds.CSRFToken, modelID)
	if enumErr != nil {
		if canFallbackSafely(true) {
			return "", "", true, fmt.Sprintf("ResolveModelEnum(%q) failed: %v", modelID, enumErr), nil
		}
		return "", "", false, "", fmt.Errorf("spawnTaskViaLS: %w (refusing unverified agentapi fallback without projectId for workspace %s)", enumErr, workDir)
	}

	usedModel = modelID
	if fellBack && resolvedModelID != "" {
		usedModel = resolvedModelID + " (fallback)"
	}
	reqBody := buildCascadeRequestBody(modelEnum, "", "", workDir, projectID)
	cascadeID, err = startCascade(creds.BaseURL, creds.CSRFToken, reqBody)
	if err != nil {
		if isQuotaExhaustedError(err.Error()) && !strings.Contains(usedModel, "flash") && !strings.Contains(usedModel, "hydrogen") {
			fallbackModel := resolveRoutineFallbackModel(vaultDir)
			if fbEnum, _, _, fbEnumErr := resolveModelEnumLive(vaultDir, creds.BaseURL, creds.CSRFToken, fallbackModel); fbEnumErr == nil {
				fbReqBody := buildCascadeRequestBody(fbEnum, "", "", workDir, projectID)
				if fbCascadeID, fbErr := startCascade(creds.BaseURL, creds.CSRFToken, fbReqBody); fbErr == nil {
					cascadeID = fbCascadeID
					usedModel = fallbackModel + " (fallback)"
					err = nil
				}
			}
		}
		if err != nil {
			if isQuotaExhaustedError(err.Error()) {
				return "", usedModel, false, "", err
			}
			if canFallbackSafely(true) {
				return "", "", true, fmt.Sprintf("StartCascade failed: %v", err), nil
			}
			return "", usedModel, false, "", err
		}
	}

	// The conversation exists but has not received its first prompt yet: let the caller link it to pre-registered
	// Vault state (e.g. the task's ConversationID) before the child's first turn can act on that state.
	if onCascadeCreated != nil {
		onCascadeCreated(cascadeID)
	}
	if sendErr := sendUserCascadeMessage(creds.BaseURL, creds.CSRFToken, cascadeID, prompt, 10*time.Second); sendErr != nil {
		msgCmd := exec.Command("agentapi", "send-message", "--title="+title, cascadeID, prompt)
		configureSpawnCommandWithTarget(msgCmd, vaultDir, "", workDir, projectID)
		if msgErr := msgCmd.Run(); msgErr != nil {
			return cascadeID, usedModel, false, "", fmt.Errorf("StartCascade succeeded (%s) but SendUserCascadeMessage failed: %w", cascadeID, sendErr)
		}
	}
	if strings.TrimSpace(title) != "" {
		_ = NewSidebarSyncClient(SidebarSyncConfig{
			BaseURL:   creds.BaseURL,
			CSRFToken: creds.CSRFToken,
		}).UpdateConversationAnnotations(context.Background(), cascadeID, nil, lsConversationPatch{
			Title: strings.TrimSpace(title),
		}, 3*time.Second)
	}
	if step1Err := checkCascadeStep1Error(creds.BaseURL, creds.CSRFToken, cascadeID); step1Err != nil {
		return cascadeID, usedModel, false, "", step1Err
	}
	return cascadeID, usedModel, false, "", nil
}

// SpawnTask creates and starts a top-level Antigravity conversation (via LanguageServerService/StartCascade
// + SendUserCascadeMessage when LS credentials are available, falling back to `agentapi new-conversation` only when workspace-safe),
// enforcing the daily spawn budget, auto-downgrading to routine tier if a higher-tier model hits 429,
// and tripping the circuit breaker only if Hydrogen/Flash is also exhausted.
func SpawnTask(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, force ...bool) (string, error) {
	return SpawnTaskWithOptions(vaultDir, trigger, skill, title, prompt, modelID, dryRun, SpawnTaskOptions{Force: len(force) > 0 && force[0]})
}

// SpawnTaskOptions configures SpawnTaskWithOptions.
type SpawnTaskOptions struct {
	// Force bypasses the daily auto-spawn budget and circuit breaker (SpawnTask's variadic force flag).
	Force bool
	// OnCascadeCreated, when set, receives the new conversation ID on the Language Server path after StartCascade
	// succeeds and before the first prompt is delivered (SendUserCascadeMessage), so the caller can link the ID to
	// pre-registered Vault state before the child's first turn can act on it. It runs synchronously, at most once,
	// and never on dry runs or on the agentapi fallback, which creates and prompts the conversation in one step
	// (callers must still record the ID from the returned output there).
	OnCascadeCreated func(cascadeID string)
}

func recordSpawnErrorQuota(vaultDir, trigger, skill, modelID, rawErr string, isInteractive, trip429 bool, circuitReason string) {
	errText := truncateRunes(rawErr, 240)
	_ = UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
		if !isInteractive && trip429 {
			st.CircuitOpen = true
			st.CircuitReason = circuitReason
		}
		st.Events = append(st.Events, SpawnEvent{
			Timestamp: time.Now().Format(time.RFC3339),
			Trigger:   trigger,
			Skill:     skill,
			Model:     modelID,
			Outcome:   "error: " + errText,
		})
		return true, nil
	})
}

// SpawnTaskWithOptions is SpawnTask with explicit options (see SpawnTaskOptions).
func SpawnTaskWithOptions(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, opts SpawnTaskOptions) (string, error) {
	if modelID == "" {
		modelID, _, _ = ResolveVaultModel(vaultDir, prompt, "", "", "")
	}
	isForced := opts.Force
	isInteractive := isInteractiveSpawnTrigger(trigger)
	if !isInteractive && !isForced {
		state := LoadQuota(vaultDir)
		if state.CircuitOpen {
			return "", fmt.Errorf("quota circuit breaker is OPEN: %s", state.CircuitReason)
		}
		autoUsed := state.EffectiveAutoSpawns()
		if autoUsed >= state.MaxDailySpawns {
			_ = UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
				st.CircuitOpen = true
				st.CircuitReason = fmt.Sprintf("daily spawn budget reached (%d/%d)", autoUsed, st.MaxDailySpawns)
				return true, nil
			})
			return "", fmt.Errorf("daily spawn budget exhausted (%d/%d)", autoUsed, state.MaxDailySpawns)
		}
	}

	var output string
	var fallbackNote string
	if dryRun {
		output = fmt.Sprintf("[dry-run] agentapi new-conversation --model=%s --title=%q (skill=%s)", modelID, title, skill)
	} else {
		workDir, projectID := resolveSpawnTarget(vaultDir, trigger)
		lsOut, lsModel, useAgentAPI, fbReason, lsErr := spawnTaskViaLS(vaultDir, title, prompt, modelID, workDir, projectID, opts.OnCascadeCreated)
		if !useAgentAPI {
			if lsModel != "" {
				modelID = lsModel
			}
			if lsErr != nil {
				recordSpawnErrorQuota(
					vaultDir, trigger, skill, modelID, lsErr.Error(), isInteractive,
					isQuotaExhaustedError(lsErr.Error()), "RESOURCE_EXHAUSTED (429) detected from LanguageServerService",
				)
				return lsOut, lsErr
			}
			output = lsOut
		} else {
			if fbReason != "" {
				fallbackNote = fbReason
				fmt.Fprintf(os.Stderr, "[hq spawn] falling back to agentapi (%s)\n", fbReason)
			}
			cmd := exec.Command("agentapi", "new-conversation", "--model="+modelID, "--title="+title, "--", prompt)
			configureSpawnCommandWithTarget(cmd, vaultDir, trigger, workDir, projectID)
			outBytes, err := cmd.CombinedOutput()
			output = strings.TrimSpace(string(outBytes))
			if err != nil {
				// Quota-aware fallback: if a non-Hydrogen/non-Flash model hit 429, retry once on fallback_on_429 (or routine tier)
				if isQuotaExhaustedError(output) && !strings.Contains(modelID, "flash") && !strings.Contains(modelID, "hydrogen") {
					fallbackModel := resolveRoutineFallbackModel(vaultDir)
					fbCmd := exec.Command("agentapi", "new-conversation", "--model="+fallbackModel, "--title="+title, "--", prompt)
					configureSpawnCommandWithTarget(fbCmd, vaultDir, trigger, workDir, projectID)
					if fbBytes, fbErr := fbCmd.CombinedOutput(); fbErr == nil {
						modelID = fallbackModel + " (fallback)"
						output = strings.TrimSpace(string(fbBytes))
						err = nil
					}
				}
			}
			if err != nil {
				recordSpawnErrorQuota(
					vaultDir, trigger, skill, modelID, err.Error(), isInteractive,
					isQuotaExhaustedError(output), "RESOURCE_EXHAUSTED (429) detected from agentapi",
				)
				return output, fmt.Errorf("agentapi error: %w (%s)", err, output)
			}
		}
	}

	outcome := output
	if fallbackNote != "" {
		outcome = fmt.Sprintf("%s [agentapi fallback: %s]", output, fallbackNote)
	}
	outcome = truncateRunes(outcome, 240)
	_ = UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
		st.normalizeSpawnCounters()
		if isInteractive {
			st.InteractiveSpawnsToday++
		} else {
			st.AutoSpawnsToday++
		}
		st.SpawnsToday++
		st.LastStatus = fmt.Sprintf("SPAWNED (%s -> /%s [%s])", trigger, skill, modelID)
		st.Events = append(st.Events, SpawnEvent{
			Timestamp: time.Now().Format(time.RFC3339),
			Trigger:   trigger,
			Skill:     skill,
			Model:     modelID,
			Outcome:   outcome,
		})
		return true, nil
	})
	return output, nil
}

func recordFrontDeskSubagentQuota(vaultDir, slug, role, modelID, status, outcome string) {
	_ = UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
		st.normalizeSpawnCounters()
		st.InteractiveSpawnsToday++
		st.SpawnsToday++
		st.LastStatus = status
		st.Events = append(st.Events, SpawnEvent{
			Timestamp: time.Now().Format(time.RFC3339),
			Trigger:   "hq-subagent:" + slug,
			Skill:     role,
			Model:     modelID,
			Outcome:   outcome,
		})
		return true, nil
	})
}

// SpawnSubagent launches a nested child subagent conversation under parentConvID on any supported
// Antigravity model (including cross-family models such as claude-opus-5-5-high or claude-sonnet-5-5-high)
// via LanguageServerService/StartCascade (with parentConversationId, subagentDescriptor, and agentScriptItem)
// followed by delivering the initial prompt from parentConvID.
func SpawnSubagent(vaultDir, parentConvID, role, prompt, modelID, workspaceDir string, dryRun bool, force ...bool) (string, error) {
	if modelID == "" {
		modelID, _, _ = ResolveVaultModel(vaultDir, prompt, "", "", "")
	} else if !strings.HasPrefix(strings.TrimSpace(modelID), "MODEL_") {
		modelID, _, _ = ResolveVaultModel(vaultDir, prompt, modelID, "", "")
	}
	parentConvID = strings.TrimSpace(parentConvID)
	if parentConvID == "" {
		parentConvID = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "[standard] general: Subagent Worker"
	}

	_ = len(force) // preserved for API compatibility; interactive subagents do not share daemon auto-spawn cap
	isFrontDesk := IsFrontDeskConversation(vaultDir, parentConvID)

	tier, scope, _, title, slug := ParseSubagentRole(role, modelID)

	if dryRun {
		output := fmt.Sprintf("[dry-run] hq subagent --parent=%s --model=%s --role=%q", parentConvID, modelID, role)
		if isFrontDesk {
			recordFrontDeskSubagentQuota(vaultDir, slug, role, modelID, fmt.Sprintf("SUBAGENT SPAWNED (%s [%s])", role, modelID), output)
		}
		return output, nil
	}

	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		return "", fmt.Errorf("ANTIGRAVITY_LS_ADDRESS and ANTIGRAVITY_CSRF_TOKEN are required to spawn a live subagent")
	}

	modelEnum, resolvedModelID, fellBack, err := resolveModelEnumLive(vaultDir, creds.BaseURL, creds.CSRFToken, modelID)
	if err != nil {
		return "", err
	}
	if fellBack && resolvedModelID != "" {
		modelID = resolvedModelID + " (fallback)"
	}

	candProjID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	if strings.TrimSpace(workspaceDir) != "" {
		candProjID, _, _ = ResolveVerifiedWorkspaceProjectID(vaultDir, workspaceDir, candProjID)
	}
	reqBody := buildCascadeRequestBody(modelEnum, parentConvID, role, workspaceDir, candProjID)
	cascadeID, err := startCascade(creds.BaseURL, creds.CSRFToken, reqBody)
	if err != nil {
		return "", err
	}

	fullPrompt := prompt
	if parentConvID != "" && !strings.Contains(fullPrompt, parentConvID) {
		fullPrompt = fmt.Sprintf("%s\n\nWhen you finish, send your final report back to parent conversation %s using the send_message tool (Recipient=%q).", prompt, parentConvID, parentConvID)
	}

	managedWt := ""
	if IsManagedWorktreeDir(vaultDir, workspaceDir) {
		managedWt = filepath.Clean(workspaceDir)
	}
	if isFrontDesk {
		// Layer-1 subagent: Register in local/memory/tasks.json before sending the initial message so SessionStart sees it immediately.
		_ = RegisterTask(vaultDir, ActiveTask{
			TaskSlug:             slug,
			Title:                title,
			Kind:                 "subagent",
			Scope:                scope,
			Tier:                 tier,
			Model:                modelID,
			ParentConversationID: parentConvID,
			WorktreeDir:          managedWt,
			ConversationID:       cascadeID,
			CreatedAt:            time.Now().Format(time.RFC3339),
			LastUpdatedAt:        time.Now().Format(time.RFC3339),
			LastSummary:          prompt,
			Status:               "active",
		})
	} else {
		// Nested descendant subagent: record in nested conversations so hooks ignore it for task tracking
		_ = RecordNestedConversation(vaultDir, cascadeID, parentConvID)
		if managedWt != "" {
			_, _ = AttachWorktreeToTaskWithOptions(vaultDir, parentConvID, managedWt, "", AttachWorktreeOptions{ReactivateCompleted: false})
		}
	}

	// Deliver initial prompt via SendUserCascadeMessage to kick off Turn 1 immediately
	// (Note: agentapi send-message only queues an inbox message and will not wake a 0-step StartCascade trajectory).
	sent := sendUserCascadeMessage(creds.BaseURL, creds.CSRFToken, cascadeID, fullPrompt, 10*time.Second) == nil
	if !sent && parentConvID != "" {
		msgCmd := exec.Command("agentapi", "send-message", "--title=Message from "+parentConvID, cascadeID, fullPrompt)
		configureSpawnCommandWithTarget(msgCmd, vaultDir, "manual-spawn:subagent", workspaceDir, candProjID)
		_ = msgCmd.Run()
	}
	if step1Err := checkCascadeStep1Error(creds.BaseURL, creds.CSRFToken, cascadeID); step1Err != nil {
		if isFrontDesk {
			_ = CancelTasks(vaultDir, []string{cascadeID}, step1Err.Error())
		}
		_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
			ConversationID:  cascadeID,
			TaskSlug:        slug,
			DesiredArchived: true,
			NextRetryAt:     time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339),
			LastError:       "subagent_step1_error",
		}, time.Now())
		return "", step1Err
	}

	output := fmt.Sprintf("Spawned subagent %s (parent=%s, model=%s, role=%q)", cascadeID, parentConvID, modelID, role)
	if isFrontDesk {
		recordFrontDeskSubagentQuota(vaultDir, slug, role, modelID, fmt.Sprintf("SUBAGENT SPAWNED (%s [%s] -> %s)", role, modelID, cascadeID), output)
	}
	return output, nil
}
