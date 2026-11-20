package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/routing"
	"github.com/zjshen14/agy-hq/internal/store"
)

var staticModelEnumMap = map[string]string{
	"gemini-4-neon-xhigh":      "MODEL_PLACEHOLDER_M535",
	"gemini-4-neon-high":       "MODEL_PLACEHOLDER_M532",
	"gemini-4-neon-medium":     "MODEL_PLACEHOLDER_M533",
	"gemini-4-neon-low":        "MODEL_PLACEHOLDER_M534",
	"gemini-4-hydrogen-xhigh":  "MODEL_PLACEHOLDER_M528",
	"gemini-4-hydrogen-high":   "MODEL_PLACEHOLDER_M525",
	"gemini-4-hydrogen-medium": "MODEL_PLACEHOLDER_M526",
	"gemini-4-hydrogen-low":    "MODEL_PLACEHOLDER_M527",
	"gemini-4-carbon-high":     "MODEL_PLACEHOLDER_M420",
	"gemini-4-carbon-medium":   "MODEL_PLACEHOLDER_M421",
	"gemini-4-carbon-low":      "MODEL_PLACEHOLDER_M422",
	"gemini-4-argon-high":      "MODEL_PLACEHOLDER_M417",
	"gemini-4-argon-medium":    "MODEL_PLACEHOLDER_M418",
	"gemini-4-argon-low":       "MODEL_PLACEHOLDER_M419",
	"claude-opus-5-5-low":      "MODEL_PLACEHOLDER_M400",
	"claude-opus-5-5-medium":   "MODEL_PLACEHOLDER_M401",
	"claude-opus-5-5-high":     "MODEL_PLACEHOLDER_M402",
	"claude-sonnet-5-5-low":    "MODEL_PLACEHOLDER_M403",
	"claude-sonnet-5-5-medium": "MODEL_PLACEHOLDER_M404",
	"claude-sonnet-5-5-high":   "MODEL_PLACEHOLDER_M405",
	"gpt-oss-120b-medium":      "MODEL_OPENAI_GPT_OSS_120B_MEDIUM",
}

type lsCredentials struct {
	RawAddress      string `json:"address,omitempty"`
	BaseURL         string `json:"baseUrl,omitempty"`
	CSRFToken       string `json:"csrfToken,omitempty"`
	AgentAPIExe     string `json:"agentApiExe,omitempty"`
	PluginShimDir   string `json:"pluginShimDir,omitempty"`
	CredentialScope string `json:"credentialScope,omitempty"`
	AppDataDir      string `json:"appDataDir,omitempty"`
	GeminiDir       string `json:"geminiDir,omitempty"`
	ConfigRelDir    string `json:"configRelDir,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
}

func lsSessionFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "runtime", "ls_session.json")
}

func normalizeLSAddress(addr string) (rawHostPort, baseURL string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", ""
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		baseURL = strings.TrimRight(addr, "/")
		if u, err := url.Parse(baseURL); err == nil {
			rawHostPort = u.Host
		}
		return rawHostPort, baseURL
	}
	rawHostPort = strings.TrimRight(addr, "/")
	baseURL = "http://" + rawHostPort
	return rawHostPort, baseURL
}

func normalizeLSBaseURL(addr string) string {
	_, baseURL := normalizeLSAddress(addr)
	return baseURL
}

func isTCPReachable(baseURL string) bool {
	return classifyTCPDialError(baseURL) == ""
}

func classifyTCPDialError(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "empty"
	}
	u, err := url.Parse(trimmed)
	hostPort := ""
	scheme := ""
	if err == nil && u.Host != "" {
		hostPort = u.Host
		scheme = u.Scheme
	} else {
		hostPort, _ = normalizeLSAddress(trimmed)
	}
	if hostPort == "" {
		return "empty_host"
	}
	if _, _, splitErr := net.SplitHostPort(hostPort); splitErr != nil {
		if strings.EqualFold(scheme, "https") {
			hostPort = net.JoinHostPort(hostPort, "443")
		} else {
			hostPort = net.JoinHostPort(hostPort, "80")
		}
	}
	conn, dialErr := net.DialTimeout("tcp", hostPort, 250*time.Millisecond)
	if dialErr != nil {
		msg := strings.ToLower(dialErr.Error())
		switch {
		case strings.Contains(msg, "refused"):
			return "refused"
		case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
			return "timeout"
		case strings.Contains(msg, "no such host"):
			return "dns"
		default:
			return "other"
		}
	}
	_ = conn.Close()
	return ""
}

// RecordLiveLSSession persists live Language Server session coordinates from the current
// process environment into `<vault>/private/runtime/ls_session.json` (`0600`) without ever
// clobbering existing credentials or desktop plugin-CLI coordinates from headless/quarantined runs.
func RecordLiveLSSession(vaultDir string) error {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	p := lsSessionFilePath(vaultDir)
	var existing lsCredentials
	hasExisting := false
	if raw, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(raw, &existing) == nil {
			hasExisting = true
		}
	}

	rawAddrEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	csrfEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN"))
	agentExeEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE"))
	shimDirEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PLUGIN_SHIM_DIR"))
	scopeEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE"))
	appDataEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_APP_DATA_DIR"))
	geminiDirEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_GEMINI_DIR"))
	configRelEnv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONFIG_REL_DIR"))
	primaryWS := primaryVaultWorkspaceDir(vaultDir)
	projEnv := ""
	if isCallerInPrimaryVaultWorkspace(vaultDir) {
		projEnv = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	}
	projRepaired := false
	if primaryWS != "" {
		verifiedExisting, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, primaryWS, existing.ProjectID)
		if existing.ProjectID != verifiedExisting {
			existing.ProjectID = verifiedExisting
			projRepaired = true
		}
		if projEnv != "" {
			if projEnv == existing.ProjectID {
				projEnv = verifiedExisting
			} else {
				verifiedEnv, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, primaryWS, projEnv)
				projEnv = verifiedEnv
			}
		}
	}

	// Never persist quarantined sentinel paths or non-existent shim directories.
	// Note: Antigravity's /tmp/agy-plugin-cli-<id> directory is created per run_command and cleaned up
	// upon command exit, whereas hooks receive ANTIGRAVITY_CREDENTIAL_SCOPE without ANTIGRAVITY_PLUGIN_SHIM_DIR;
	// keep scopeEnv intact unless explicitly quarantined by /nonexistent-hq-sentinel.
	if shimDirEnv == "/nonexistent-hq-sentinel" {
		shimDirEnv = ""
		scopeEnv = ""
	} else if shimDirEnv != "" {
		if st, err := os.Stat(shimDirEnv); err != nil || !st.IsDir() {
			shimDirEnv = ""
		}
	}
	if shimDirEnv == "" && existing.PluginShimDir != "" && existing.PluginShimDir != "/nonexistent-hq-sentinel" {
		if st, err := os.Stat(existing.PluginShimDir); err != nil || !st.IsDir() {
			existing.PluginShimDir = ""
		}
	}

	pick := func(cur, prev string) string {
		if cur != "" {
			return cur
		}
		return strings.TrimSpace(prev)
	}

	if rawAddrEnv == "" || csrfEnv == "" {
		if hasExisting && existing.BaseURL != "" && existing.CSRFToken != "" && isTCPReachable(existing.BaseURL) {
			updated := existing
			changed := projRepaired
			setIfNew := func(dst *string, val string) {
				if val != "" && *dst != val {
					*dst = val
					changed = true
				}
			}
			setIfNew(&updated.AgentAPIExe, agentExeEnv)
			setIfNew(&updated.PluginShimDir, shimDirEnv)
			setIfNew(&updated.CredentialScope, scopeEnv)
			setIfNew(&updated.AppDataDir, appDataEnv)
			setIfNew(&updated.GeminiDir, geminiDirEnv)
			setIfNew(&updated.ConfigRelDir, configRelEnv)
			setIfNew(&updated.ProjectID, projEnv)
			if changed {
				updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				data, err := json.MarshalIndent(updated, "", "  ")
				if err != nil {
					return err
				}
				return store.WriteAtomic(p, append(data, '\n'), 0o600)
			}
		}
		return nil
	}

	normRaw, normBase := normalizeLSAddress(rawAddrEnv)
	if normBase == "" {
		return nil
	}
	// Never persist an unreachable LS address or clobber a healthy persisted session with a dead/refused hook env port.
	if !isTCPReachable(normBase) {
		return nil
	}
	creds := lsCredentials{
		RawAddress:      normRaw,
		BaseURL:         normBase,
		CSRFToken:       csrfEnv,
		AgentAPIExe:     pick(agentExeEnv, existing.AgentAPIExe),
		PluginShimDir:   pick(shimDirEnv, existing.PluginShimDir),
		CredentialScope: pick(scopeEnv, existing.CredentialScope),
		AppDataDir:      pick(appDataEnv, existing.AppDataDir),
		GeminiDir:       pick(geminiDirEnv, existing.GeminiDir),
		ConfigRelDir:    pick(configRelEnv, existing.ConfigRelDir),
		ProjectID:       pick(projEnv, existing.ProjectID),
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	if creds.PluginShimDir == "/nonexistent-hq-sentinel" {
		creds.PluginShimDir = ""
		creds.CredentialScope = ""
	}
	sameSession := func(a, b lsCredentials) bool {
		a.UpdatedAt, b.UpdatedAt = "", ""
		return a == b
	}
	if hasExisting && sameSession(existing, creds) {
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(existing.UpdatedAt)); err == nil && time.Since(ts) < 60*time.Second {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(p, append(data, '\n'), 0o600)
}

func loadReachableLSSession(vaultDir string) (lsCredentials, bool) {
	if strings.TrimSpace(vaultDir) == "" {
		return lsCredentials{}, false
	}
	raw, err := os.ReadFile(lsSessionFilePath(vaultDir))
	if err != nil {
		return lsCredentials{}, false
	}
	var creds lsCredentials
	if json.Unmarshal(raw, &creds) != nil {
		return lsCredentials{}, false
	}
	if creds.BaseURL == "" && creds.RawAddress != "" {
		creds.RawAddress, creds.BaseURL = normalizeLSAddress(creds.RawAddress)
	} else if creds.RawAddress == "" && creds.BaseURL != "" {
		creds.RawAddress, creds.BaseURL = normalizeLSAddress(creds.BaseURL)
	}
	creds.CSRFToken = strings.TrimSpace(creds.CSRFToken)
	creds.AgentAPIExe = strings.TrimSpace(creds.AgentAPIExe)
	if creds.BaseURL == "" || creds.CSRFToken == "" || !isTCPReachable(creds.BaseURL) {
		return lsCredentials{}, false
	}
	return creds, true
}

// HasReachableLSSession reports whether live or persisted Antigravity LS credentials are
// discoverable and currently listening on TCP.
func HasReachableLSSession(vaultDir string) bool {
	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		return false
	}
	return isTCPReachable(creds.BaseURL)
}

// LSSessionDiagnostics reports non-secret Language Server credential discovery status for `hq doctor`.
type LSSessionDiagnostics struct {
	EnvConfigured       bool   `json:"envConfigured"`
	EnvReachable        bool   `json:"envReachable"`
	PersistedExists     bool   `json:"persistedExists"`
	PersistedReachable  bool   `json:"persistedReachable"`
	PersistedMatchesEnv bool   `json:"persistedMatchesEnv"`
	PersistedScheme     string `json:"persistedScheme,omitempty"`
	PersistedUpdatedAt  string `json:"persistedUpdatedAt,omitempty"`
	PersistedReason     string `json:"persistedReason,omitempty"`
	EffectiveSource     string `json:"effectiveSource"` // env | persisted | daemon_hub | none
}

// DiagnoseLSSession inspects LS credential availability across env, persisted runtime state,
// and daemon hub files without exposing addresses or CSRF tokens.
func DiagnoseLSSession(vaultDir string) LSSessionDiagnostics {
	var diag LSSessionDiagnostics
	envAddr := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	envCSRF := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN"))
	diag.EnvConfigured = envAddr != "" && envCSRF != ""
	_, envBase := normalizeLSAddress(envAddr)
	if envBase != "" {
		diag.EnvReachable = isTCPReachable(envBase)
	}

	if strings.TrimSpace(vaultDir) != "" {
		p := lsSessionFilePath(vaultDir)
		raw, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				diag.PersistedReason = "missing"
			} else {
				diag.PersistedReason = fmt.Sprintf("read_error: %v", err)
			}
		} else {
			diag.PersistedExists = true
			var creds lsCredentials
			if uErr := json.Unmarshal(raw, &creds); uErr != nil {
				diag.PersistedReason = fmt.Sprintf("json_error: %v", uErr)
			} else {
				diag.PersistedUpdatedAt = strings.TrimSpace(creds.UpdatedAt)
				if creds.BaseURL == "" && creds.RawAddress != "" {
					creds.RawAddress, creds.BaseURL = normalizeLSAddress(creds.RawAddress)
				}
				if u, uErr := url.Parse(strings.TrimSpace(creds.BaseURL)); uErr == nil {
					diag.PersistedScheme = u.Scheme
				}
				diag.PersistedMatchesEnv = envBase != "" && strings.EqualFold(strings.TrimSpace(creds.BaseURL), strings.TrimSpace(envBase))
				switch {
				case strings.TrimSpace(creds.BaseURL) == "":
					diag.PersistedReason = "empty_address"
				case strings.TrimSpace(creds.CSRFToken) == "":
					diag.PersistedReason = "empty_csrf"
				case !isTCPReachable(creds.BaseURL):
					// Capture non-secret dial error class (e.g. connection refused vs timeout vs dns)
					diag.PersistedReason = "tcp_unreachable:" + classifyTCPDialError(creds.BaseURL)
				default:
					diag.PersistedReachable = true
					diag.PersistedReason = "ok"
				}
			}
		}
	}

	switch {
	case diag.EnvConfigured && diag.EnvReachable:
		diag.EffectiveSource = "env"
	case diag.PersistedReachable:
		diag.EffectiveSource = "persisted"
	default:
		if _, ok := discoverLSCredentials(vaultDir); ok {
			diag.EffectiveSource = "daemon_hub"
		} else {
			diag.EffectiveSource = "none"
		}
	}
	return diag
}

func discoverLSCredentials(vaultDir string) (lsCredentials, bool) {
	if addr, csrf := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS")), strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN")); addr != "" && csrf != "" {
		rawAddr, baseURL := normalizeLSAddress(addr)
		if baseURL != "" && isTCPReachable(baseURL) {
			creds := lsCredentials{
				RawAddress:  rawAddr,
				BaseURL:     baseURL,
				CSRFToken:   csrf,
				AgentAPIExe: strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")),
			}
			if sess, ok := loadReachableLSSession(vaultDir); ok {
				if creds.AgentAPIExe == "" {
					creds.AgentAPIExe = sess.AgentAPIExe
				}
				creds.PluginShimDir = sess.PluginShimDir
				creds.CredentialScope = sess.CredentialScope
				creds.AppDataDir = sess.AppDataDir
				creds.GeminiDir = sess.GeminiDir
				creds.ConfigRelDir = sess.ConfigRelDir
				creds.ProjectID = sess.ProjectID
			}
			return creds, true
		}
	}
	if creds, ok := loadReachableLSSession(vaultDir); ok {
		return creds, true
	}

	roots := hostpaths.Resolve(vaultDir)
	if roots.AppDataDir == "" {
		return lsCredentials{}, false
	}
	// In hermetic temp-vault tests without an explicit temp AppDataDir, never probe the live host's ~/.gemini/antigravity/daemon.
	if hostpaths.IsTempPath(vaultDir) && !hostpaths.IsTempPath(roots.AppDataDir) && os.Getenv("HQ_ALLOW_LIVE_LS_DISCOVERY") != "1" {
		return lsCredentials{}, false
	}

	matches, err := filepath.Glob(filepath.Join(roots.AppDataDir, "daemon", "hub_*.json"))
	if err != nil || len(matches) == 0 {
		return lsCredentials{}, false
	}
	type hubCandidate struct {
		path    string
		modTime time.Time
	}
	candidates := make([]hubCandidate, 0, len(matches))
	for _, m := range matches {
		if st, sErr := os.Stat(m); sErr == nil && !st.IsDir() {
			candidates = append(candidates, hubCandidate{path: m, modTime: st.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	for _, c := range candidates {
		data, rErr := os.ReadFile(c.path)
		if rErr != nil {
			continue
		}
		var hub struct {
			PID       int    `json:"pid"`
			HTTPPort  int    `json:"httpPort"`
			CSRFToken string `json:"csrfToken"`
		}
		if json.Unmarshal(data, &hub) != nil || hub.HTTPPort <= 0 || strings.TrimSpace(hub.CSRFToken) == "" {
			continue
		}
		if hub.PID > 0 && !isProcessAlive(hub.PID) {
			continue
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", hub.HTTPPort)
		if !isTCPReachable(baseURL) {
			continue
		}
		return lsCredentials{
			RawAddress: fmt.Sprintf("127.0.0.1:%d", hub.HTTPPort),
			BaseURL:    baseURL,
			CSRFToken:  strings.TrimSpace(hub.CSRFToken),
		}, true
	}
	return lsCredentials{}, false
}

func buildCascadeRequestBody(modelEnum, parentConvID, role, workspaceDir, projectID string) map[string]interface{} {
	reqBody := map[string]interface{}{
		"baseTrajectoryIdentifier": map[string]interface{}{
			"lastActiveDoc": true,
		},
		"source":         "CORTEX_TRAJECTORY_SOURCE_AGENT_API",
		"trajectoryType": "CORTEX_TRAJECTORY_TYPE_CASCADE",
		"requestedModel": modelEnum,
		"customAgentSpec": map[string]interface{}{
			"codingAgent":                 map[string]interface{}{},
			"commandExecutionPolicy":      "eager",
			"enforcedWorkspaceValidation": false,
			"cascadeConfig": map[string]interface{}{
				"plannerConfig": map[string]interface{}{
					"planModel": modelEnum,
					"requestedModel": map[string]interface{}{
						"model": modelEnum,
					},
				},
			},
		},
	}
	if parentConvID != "" {
		reqBody["parentConversationId"] = parentConvID
		reqBody["subagentDescriptor"] = map[string]interface{}{
			"typeName": "self",
			"role":     role,
		}
		reqBody["agentScriptItem"] = map[string]interface{}{
			"name": "self",
		}
	}
	projectID = strings.TrimSpace(projectID)
	workspaceDir = strings.TrimSpace(workspaceDir)
	// StartCascade rejects requests specifying both projectEnvConfig and workspaceUris (HTTP 400 invalid_argument).
	if projectID != "" {
		reqBody["projectEnvConfig"] = map[string]interface{}{
			"projectId":                 projectID,
			"defaultProjectEnvironment": map[string]interface{}{},
		}
	} else if workspaceDir != "" {
		if absDir, absErr := filepath.Abs(workspaceDir); absErr == nil {
			workspaceDir = absDir
		}
		reqBody["workspaceUris"] = []string{"file://" + filepath.ToSlash(workspaceDir)}
	}
	return reqBody
}

func postLSJSON(lsBaseURL, csrfToken, method string, payload interface{}, connectProto bool, timeout time.Duration, maxBytes int64) (int, []byte, error) {
	var rawBody []byte
	switch v := payload.(type) {
	case json.RawMessage:
		rawBody = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return 0, nil, err
		}
		rawBody = b
	}
	endpoint := fmt.Sprintf("%s/exa.language_server_pb.LanguageServerService/%s", normalizeLSBaseURL(lsBaseURL), method)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if connectProto {
		req.Header.Set("Connect-Protocol-Version", "1")
	}
	req.Header.Set("x-codeium-csrf-token", csrfToken)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	reader := io.Reader(resp.Body)
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes)
	}
	respBytes, _ := io.ReadAll(reader)
	return resp.StatusCode, respBytes, nil
}

func startCascade(lsBaseURL, csrfToken string, reqBody map[string]interface{}) (string, error) {
	status, respBytes, err := postLSJSON(lsBaseURL, csrfToken, "StartCascade", reqBody, false, 10*time.Second, 0)
	if err != nil {
		return "", fmt.Errorf("StartCascade RPC failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("StartCascade HTTP %d: %s", status, strings.TrimSpace(string(respBytes)))
	}

	var startResp struct {
		CascadeID string `json:"cascadeId"`
	}
	if err := json.Unmarshal(respBytes, &startResp); err != nil || startResp.CascadeID == "" {
		return "", fmt.Errorf("invalid StartCascade response: %s", strings.TrimSpace(string(respBytes)))
	}
	return startResp.CascadeID, nil
}

func sendUserCascadeMessage(lsBaseURL, csrfToken, cascadeID, text string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	status, respBody, err := postLSJSON(lsBaseURL, csrfToken, "SendUserCascadeMessage", map[string]interface{}{
		"cascadeId": cascadeID,
		"items": []map[string]interface{}{
			{"text": text},
		},
	}, false, timeout, 0)
	if err != nil {
		return fmt.Errorf("SendUserCascadeMessage failed: %w", err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("SendUserCascadeMessage HTTP %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

type lsModelConfig struct {
	ModelID      string `json:"modelId"`
	Label        string `json:"label"`
	ModelOrAlias struct {
		Model string `json:"model"`
	} `json:"modelOrAlias"`
}

func normalizeModelLabelSlug(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func matchLiveModelConfig(configs []lsModelConfig, candidate string) (lsModelConfig, bool) {
	candNorm := strings.ToLower(strings.TrimSpace(candidate))
	candDot := strings.ReplaceAll(candNorm, ".", "-")
	for _, cfg := range configs {
		if strings.EqualFold(strings.TrimSpace(cfg.ModelID), candNorm) {
			return cfg, true
		}
	}
	for _, cfg := range configs {
		lblSlug := normalizeModelLabelSlug(cfg.Label)
		if lblSlug != "" && (lblSlug == candNorm || strings.ReplaceAll(lblSlug, ".", "-") == candDot) {
			return cfg, true
		}
	}
	return lsModelConfig{}, false
}

func selectLiveFallbackModelConfig(requestedNorm string, customCandidates []string, configs []lsModelConfig, defaultOverrideModel string) (lsModelConfig, bool) {
	if len(configs) == 0 {
		return lsModelConfig{}, false
	}
	for _, cand := range customCandidates {
		if cfg, ok := matchLiveModelConfig(configs, cand); ok {
			return cfg, true
		}
	}
	var candidates []string
	var familyKeywords []string
	switch {
	case strings.Contains(requestedNorm, "carbon") || strings.Contains(requestedNorm, "argon") ||
		strings.Contains(requestedNorm, "opus") || strings.Contains(requestedNorm, "sonnet") ||
		strings.Contains(requestedNorm, "pro"):
		candidates = []string{
			"gemini-4-carbon-high",
			"gemini-4-carbon-medium",
			"gemini-4-neon-high",
			"gemini-4-argon-high",
			"claude-opus-5-5-high",
			"claude-sonnet-5-5-high",
		}
		familyKeywords = []string{"carbon", "argon", "opus", "sonnet", "pro", "neon"}
	case strings.Contains(requestedNorm, "neon"):
		if strings.HasSuffix(requestedNorm, "-low") {
			candidates = []string{
				"gemini-4-neon-medium",
				"gemini-4-hydrogen-low",
				"gemini-4-hydrogen-medium",
			}
		} else if strings.HasSuffix(requestedNorm, "-high") || strings.HasSuffix(requestedNorm, "-xhigh") {
			candidates = []string{
				"gemini-4-neon-high",
				"gemini-4-neon-medium",
				"gemini-4-hydrogen-high",
				"gemini-4-carbon-medium",
			}
		} else {
			candidates = []string{
				"gemini-4-neon-medium",
				"gemini-4-neon-high",
				"gemini-4-hydrogen-high",
				"gemini-4-hydrogen-medium",
			}
		}
		familyKeywords = []string{"neon", "hydrogen", "flash", "carbon"}
	case strings.Contains(requestedNorm, "lite") || strings.HasSuffix(requestedNorm, "-low") || strings.Contains(requestedNorm, "flash"):
		candidates = []string{
			"gemini-4-hydrogen-low",
			"gemini-4-hydrogen-medium",
		}
		familyKeywords = []string{"lite", "flash", "hydrogen"}
	default:
		candidates = []string{
			"gemini-4-neon-medium",
			"gemini-4-neon-high",
			"gemini-4-hydrogen-high",
			"gemini-4-hydrogen-medium",
			"gemini-4-carbon-medium",
		}
		familyKeywords = []string{"neon", "hydrogen", "flash", "carbon"}
	}
	for _, cand := range candidates {
		if cfg, ok := matchLiveModelConfig(configs, cand); ok {
			return cfg, true
		}
	}
	for _, kw := range familyKeywords {
		for _, cfg := range configs {
			idLower := strings.ToLower(strings.TrimSpace(cfg.ModelID))
			lblLower := strings.ToLower(strings.TrimSpace(cfg.Label))
			if strings.Contains(idLower, kw) || strings.Contains(lblLower, kw) {
				return cfg, true
			}
		}
	}
	if def := strings.TrimSpace(defaultOverrideModel); def != "" {
		for _, cfg := range configs {
			if strings.EqualFold(strings.TrimSpace(cfg.ModelOrAlias.Model), def) {
				return cfg, true
			}
		}
		var out lsModelConfig
		out.ModelID = def
		out.ModelOrAlias.Model = def
		return out, true
	}
	return configs[0], true
}

func resolveModelEnumLive(vaultDir, lsAddr, csrfToken, modelID string) (modelEnum string, resolvedModelID string, fellBack bool, err error) {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return "", "", false, fmt.Errorf("unknown Antigravity model ID %q", modelID)
	}
	if strings.HasPrefix(trimmed, "MODEL_") {
		return trimmed, trimmed, false, nil
	}
	normalized := strings.ToLower(trimmed)

	if lsAddr != "" && csrfToken != "" {
		if status, respBytes, doErr := postLSJSON(lsAddr, csrfToken, "GetCascadeModelConfigData", json.RawMessage("{}"), false, 2*time.Second, 0); doErr == nil && status >= 200 && status < 300 {
			var payload struct {
				ClientModelConfigs         []lsModelConfig `json:"clientModelConfigs"`
				DefaultOverrideModelConfig struct {
					ModelOrAlias struct {
						Model string `json:"model"`
					} `json:"modelOrAlias"`
				} `json:"defaultOverrideModelConfig"`
			}
			if json.Unmarshal(respBytes, &payload) == nil {
				var activeConfigs []lsModelConfig
				for _, cfg := range payload.ClientModelConfigs {
					if strings.TrimSpace(cfg.ModelOrAlias.Model) != "" {
						activeConfigs = append(activeConfigs, cfg)
					}
				}
				if len(activeConfigs) > 0 {
					if matched, ok := matchLiveModelConfig(activeConfigs, normalized); ok {
						resID := strings.TrimSpace(matched.ModelID)
						if resID == "" {
							resID = trimmed
						}
						return matched.ModelOrAlias.Model, resID, false, nil
					}
					// Never fall back to staticModelEnumMap when live clientModelConfigs is non-empty:
					// retired MODEL_PLACEHOLDER_* enums cause silent HTTP 200 spawns that fail at Step 1.
					customCands := routing.CustomFallbackCandidates(vaultDir, normalized)
					if fb, ok := selectLiveFallbackModelConfig(normalized, customCands, activeConfigs, payload.DefaultOverrideModelConfig.ModelOrAlias.Model); ok {
						fbID := strings.TrimSpace(fb.ModelID)
						if fbID == "" {
							fbID = fb.ModelOrAlias.Model
						}
						fmt.Fprintf(os.Stderr, "[hq model] warning: model %q is absent from live Language Server clientModelConfigs; falling back to active model %q (%s)\n", modelID, fbID, fb.ModelOrAlias.Model)
						return fb.ModelOrAlias.Model, fbID, true, nil
					}
					return "", "", false, fmt.Errorf("model %q is not available in live Language Server clientModelConfigs", modelID)
				}
			}
		}
	}

	if enumVal, ok := staticModelEnumMap[normalized]; ok {
		return enumVal, trimmed, false, nil
	}
	return "", "", false, fmt.Errorf("unknown Antigravity model ID %q", modelID)
}

// ResolveModelEnum maps a human-readable Antigravity model ID (e.g. "claude-opus-5-5-high")
// to its LanguageServerService Model enum string (e.g. "MODEL_PLACEHOLDER_M402") by querying
// GetCascadeModelConfigData on the local language server, falling back to an active live config
// when the LS returns a non-empty list and using staticModelEnumMap only when LS is unreachable.
func ResolveModelEnum(lsAddr, csrfToken, modelID string) (string, error) {
	enumVal, _, _, err := resolveModelEnumLive("", lsAddr, csrfToken, modelID)
	return enumVal, err
}

type lsCascadeStepError struct {
	Message          string `json:"message,omitempty"`
	UserErrorMessage string `json:"userErrorMessage,omitempty"`
	ShortError       string `json:"shortError,omitempty"`
	FullError        string `json:"fullError,omitempty"`
}

func (e *lsCascadeStepError) bestText() string {
	if e == nil {
		return ""
	}
	for _, s := range []string{e.ShortError, e.UserErrorMessage, e.Message, e.FullError} {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

type lsCascadeStep struct {
	Type         string `json:"type"`
	Status       string `json:"status"`
	ErrorMessage *struct {
		Message          string              `json:"message,omitempty"`
		UserErrorMessage string              `json:"userErrorMessage,omitempty"`
		ShortError       string              `json:"shortError,omitempty"`
		FullError        string              `json:"fullError,omitempty"`
		Error            *lsCascadeStepError `json:"error,omitempty"`
	} `json:"errorMessage,omitempty"`
	Error *lsCascadeStepError `json:"error,omitempty"`
}

func (s lsCascadeStep) extractErrorMessage() string {
	if s.ErrorMessage != nil {
		if msg := s.ErrorMessage.Error.bestText(); msg != "" {
			return msg
		}
		for _, cand := range []string{s.ErrorMessage.ShortError, s.ErrorMessage.UserErrorMessage, s.ErrorMessage.Message, s.ErrorMessage.FullError} {
			if t := strings.TrimSpace(cand); t != "" {
				return t
			}
		}
	}
	if msg := s.Error.bestText(); msg != "" {
		return msg
	}
	return "CORTEX_STEP_TYPE_ERROR_MESSAGE at step 1"
}

type lsGetCascadeTrajectoryResponse struct {
	Status        string `json:"status"`
	NumTotalSteps int    `json:"numTotalSteps"`
	Trajectory    struct {
		Metadata *LSTrajectoryMetadata `json:"metadata,omitempty"`
		Steps    []lsCascadeStep       `json:"steps"`
	} `json:"trajectory"`
}

// LSConversationLineage captures Layer-1 vs nested classification derived from Language Server trajectory metadata.
type LSConversationLineage struct {
	Found                     bool
	ParentConvID              string
	RootConvID                string
	NestingDepth              int
	IsWorkflowChild           bool
	HasSubagentSpec           bool
	SubagentRole              string
	SubagentModel             string
	SourceTitle               string
	DirectParentID            string
	IsNestedDescendant        bool
	IsDirectFrontDeskSubagent bool
}

// EvaluateLSTrajectoryLineage classifies a conversation using LS trajectory metadata and Front Desk / Vault ancestry.
func EvaluateLSTrajectoryLineage(vaultDir, convID string, meta LSTrajectoryMetadata) LSConversationLineage {
	fdID, knownFDs, nestedMap, parentByConv := buildAncestryMaps(vaultDir)
	return evaluateLSTrajectoryLineageWithMaps(fdID, knownFDs, nestedMap, parentByConv, convID, meta)
}

func evaluateLSTrajectoryLineageWithMaps(fdID string, knownFDs map[string]bool, nestedMap map[string]NestedConversation, parentByConv map[string]string, convID string, meta LSTrajectoryMetadata) LSConversationLineage {
	cid := normalizeUUID(convID)

	parentID := normalizeUUID(meta.ParentConversationID)
	rootID := normalizeUUID(meta.RootConversationID)
	var toolCallerID string
	var toolName string
	var sourceTitle string
	var hideFromList bool
	if meta.SourceMetadata != nil {
		hideFromList = meta.SourceMetadata.HideFromConversationList
		if meta.SourceMetadata.Tool != nil {
			toolCallerID = normalizeUUID(meta.SourceMetadata.Tool.ConversationID)
			sourceTitle = strings.TrimSpace(meta.SourceMetadata.Tool.Title)
			if meta.SourceMetadata.Tool.ToolCall != nil {
				toolName = strings.TrimSpace(meta.SourceMetadata.Tool.ToolCall.Name)
			}
		}
	}
	isExplicitTitledSource := strings.HasPrefix(sourceTitle, "[Task:") || strings.HasPrefix(sourceTitle, "[Auto]")
	isWorkflowChild := hideFromList ||
		strings.EqualFold(toolName, "run_workflow") ||
		(strings.EqualFold(toolName, "run_command") && !isExplicitTitledSource)

	hasSubagentSpec := meta.SubagentSpec != nil
	var subRole, subModel string
	if meta.SubagentSpec != nil {
		subRole = strings.TrimSpace(meta.SubagentSpec.Role)
		subModel = strings.TrimSpace(meta.SubagentSpec.Model)
	}

	directParent := parentID
	if directParent == "" {
		directParent = toolCallerID
	}
	if directParent == "" && rootID != "" && rootID != cid {
		directParent = rootID
	}
	if directParent == cid {
		directParent = ""
	}

	lin := LSConversationLineage{
		Found:           true,
		ParentConvID:    parentID,
		RootConvID:      rootID,
		NestingDepth:    meta.NestingDepth,
		IsWorkflowChild: isWorkflowChild,
		HasSubagentSpec: hasSubagentSpec,
		SubagentRole:    subRole,
		SubagentModel:   subModel,
		SourceTitle:     sourceTitle,
		DirectParentID:  directParent,
	}

	if cid != "" && knownFDs[cid] {
		return lin
	}

	hasAncestry := false
	if directParent != "" {
		if !knownFDs[directParent] {
			hasAncestry = true
		} else if hasAnc, _ := hasNonFrontDeskAncestryWithMaps(knownFDs, nestedMap, parentByConv, cid, directParent); hasAnc {
			hasAncestry = true
		}
	}

	if isWorkflowChild ||
		meta.NestingDepth > 1 ||
		(rootID != "" && cid != "" && rootID != cid && !knownFDs[rootID]) ||
		hasAncestry {
		lin.IsNestedDescendant = true
		return lin
	}

	if fdID != "" && knownFDs[directParent] && (hasSubagentSpec || meta.NestingDepth == 1) {
		lin.IsDirectFrontDeskSubagent = true
	}
	return lin
}

// InspectLSConversationLineage queries the Language Server for convID's trajectory metadata and classifies its lineage.
// Evaluates omitted proto3 metadata directly on HTTP 200 responses without falling back to a full trajectory dump;
// fails open (returns Found=false) when LS credentials are absent or unreachable.
func InspectLSConversationLineage(vaultDir, convID string) (LSConversationLineage, bool) {
	cid := normalizeUUID(convID)
	if cid == "" {
		return LSConversationLineage{}, false
	}
	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		return LSConversationLineage{}, false
	}
	if resp, supported, err := inspectCascadeTrajectory(creds.BaseURL, creds.CSRFToken, convID, DefaultSidebarUnaryTimeout); err == nil && supported && resp != nil {
		var meta LSTrajectoryMetadata
		if resp.Trajectory.Metadata != nil {
			meta = *resp.Trajectory.Metadata
		}
		lin := EvaluateLSTrajectoryLineage(vaultDir, convID, meta)
		return lin, true
	}
	// Fallback to JetboxSubscribeToSummaries / GetAllCascadeTrajectoriesIncludeSubtrajectories only when GetCascadeTrajectory failed or was unsupported (404/405/501).
	all, ok := FetchAllLSConversationLineage(vaultDir)
	if !ok {
		return LSConversationLineage{}, false
	}
	lin, exists := all[cid]
	return lin, exists
}

// FetchAllLSConversationLineage fetches all trajectories (including subtrajectories) from LS and evaluates lineage by UUID.
func FetchAllLSConversationLineage(vaultDir string) (map[string]LSConversationLineage, bool) {
	return fetchAllLSConversationLineageWithTimeout(vaultDir, DefaultSidebarBatchTimeout)
}

func fetchAllLSConversationLineageWithTimeout(vaultDir string, timeout time.Duration) (map[string]LSConversationLineage, bool) {
	creds, ok := discoverLSCredentials(vaultDir)
	if !ok {
		return nil, false
	}
	if timeout <= 0 {
		timeout = DefaultSidebarBatchTimeout
	}
	client := NewSidebarSyncClient(SidebarSyncConfig{
		BaseURL:   creds.BaseURL,
		CSRFToken: creds.CSRFToken,
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Prefer JetboxSubscribeToSummaries (streaming, sub-second even with thousands of subtrajectories)
	// and fall back to GetAllCascadeTrajectoriesIncludeSubtrajectories only if Jetbox is unsupported or fails.
	summaries, err := client.GetJetboxTrajectorySummariesIncludeSubtrajectories(ctx)
	if err != nil || len(summaries) == 0 {
		fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), DefaultSidebarBatchTimeout)
		defer fallbackCancel()
		summaries, err = client.GetAllCascadeTrajectoriesIncludeSubtrajectories(fallbackCtx)
		if err != nil || len(summaries) == 0 {
			return nil, false
		}
	}
	fdID, knownFDs, nestedMap, parentByConv := buildAncestryMaps(vaultDir)
	out := make(map[string]LSConversationLineage, len(summaries))
	for rawID, sum := range summaries {
		cid := normalizeUUID(rawID)
		if cid == "" {
			continue
		}
		out[cid] = evaluateLSTrajectoryLineageWithMaps(fdID, knownFDs, nestedMap, parentByConv, cid, sum.TrajectoryMetadata)
	}
	return out, true
}

func inspectCascadeTrajectory(lsBaseURL, csrfToken, cascadeID string, timeout time.Duration) (respData *lsGetCascadeTrajectoryResponse, supported bool, err error) {
	if strings.TrimSpace(lsBaseURL) == "" || strings.TrimSpace(csrfToken) == "" || strings.TrimSpace(cascadeID) == "" {
		return nil, false, nil
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	status, rawBytes, err := postLSJSON(lsBaseURL, csrfToken, "GetCascadeTrajectory", map[string]string{"cascadeId": cascadeID}, true, timeout, 4*1024*1024)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		return nil, false, nil
	}
	if status < 200 || status >= 300 {
		return nil, true, fmt.Errorf("GetCascadeTrajectory HTTP %d: %s", status, strings.TrimSpace(string(rawBytes)))
	}
	var parsed lsGetCascadeTrajectoryResponse
	if decErr := json.Unmarshal(rawBytes, &parsed); decErr != nil {
		return nil, true, decErr
	}
	return &parsed, true, nil
}

func isImmediateStep1ExecutorError(resp *lsGetCascadeTrajectoryResponse) (string, bool) {
	if resp == nil {
		return "", false
	}
	st := strings.TrimSpace(resp.Status)
	if st != "" && st != "CASCADE_RUN_STATUS_IDLE" && st != "CASCADE_RUN_STATUS_UNSPECIFIED" {
		return "", false
	}
	steps := resp.Trajectory.Steps
	if len(steps) != 2 {
		return "", false
	}
	if steps[0].Type != "" && steps[0].Type != "CORTEX_STEP_TYPE_USER_INPUT" {
		return "", false
	}
	if steps[1].Type != "CORTEX_STEP_TYPE_ERROR_MESSAGE" {
		return "", false
	}
	return steps[1].extractErrorMessage(), true
}

func checkCascadeStep1Error(lsBaseURL, csrfToken, cascadeID string) error {
	const maxChecks = 4
	const pollDelay = 120 * time.Millisecond
	for attempt := 0; attempt < maxChecks; attempt++ {
		if attempt > 0 {
			time.Sleep(pollDelay)
		}
		traj, supported, err := inspectCascadeTrajectory(lsBaseURL, csrfToken, cascadeID, 2*time.Second)
		if !supported || err != nil || traj == nil {
			return nil
		}
		if errMsg, failed := isImmediateStep1ExecutorError(traj); failed {
			return fmt.Errorf("cascade %s failed at step 1: %s", cascadeID, errMsg)
		}
		st := strings.TrimSpace(traj.Status)
		if st == "CASCADE_RUN_STATUS_RUNNING" || len(traj.Trajectory.Steps) >= 2 {
			return nil
		}
	}
	return nil
}
