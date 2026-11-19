package vault

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed testdata/memory_eval_fixtures.json
var embeddedEvalFixtures []byte

// EvalOptions configures EvaluateMemory.
type EvalOptions struct {
	Since    time.Time
	Now      time.Time
	RepoRoot string
}

// MemoryEvalReport captures M1-M4 telemetry and optional fixture/replay results (RFC-0024 §7).
type MemoryEvalReport struct {
	GeneratedAt string `json:"generatedAt"`
	Since       string `json:"since"`

	// M1: Efficiency & Redundancy
	MemoryLines          int     `json:"memoryLines"`
	MemoryBytes          int     `json:"memoryBytes"`
	MemoryOverBudget     bool    `json:"memoryOverBudget"`
	UserBytes            int     `json:"userBytes"`
	ActiveFactCount      int     `json:"activeFactCount"`
	ExpiredFactCount     int     `json:"expiredFactCount"`
	TopicsCount          int     `json:"topicsCount"`
	TopicsTotalBytes     int     `json:"topicsTotalBytes"`
	CrossLayerOverlapPct float64 `json:"crossLayerOverlapPct"`

	// M2: Scope Isolation & Temporal Freshness
	ScopedFactCount         int     `json:"scopedFactCount"`
	GlobalFactCount         int     `json:"globalFactCount"`
	ScopedPlaybookCount     int     `json:"scopedPlaybookCount"`
	OverdueTTLFactCount     int     `json:"overdueTTLFactCount"`
	MissingTTLEventFactsCnt int     `json:"missingTTLEventFactsCount"`
	StaleOrMissingTTLPct    float64 `json:"staleOrMissingTTLPct"`

	// M3: Behavioral Ground-Truth Friction Telemetry
	JournalTurns                 int     `json:"journalTurns"`
	TotalSteps                   int     `json:"totalSteps"`
	TotalToolErrors              int     `json:"totalToolErrors"`
	PreToolDenials               int     `json:"preToolDenials"`
	ToolErrorsPer100Turns        float64 `json:"toolErrorsPer100Turns"`
	PreToolDenialsPer100Turns    float64 `json:"preToolDenialsPer100Turns"`
	InterventionsApproved        int     `json:"interventionsApproved"`
	InterventionsRejected        int     `json:"interventionsRejected"`
	InterventionsExpired         int     `json:"interventionsExpired"`
	InterventionRejectionRatePct float64 `json:"interventionRejectionRatePct"`
	WikiReadCount                int     `json:"wikiReadCount"`

	// M4: Knowledge Wiki Integrity & Signal Density
	InvalidTopicHeaders         int     `json:"invalidTopicHeaders"`
	BrokenTopicLinks            int     `json:"brokenTopicLinks"`
	DuplicatePlaybookOwners     int     `json:"duplicatePlaybookOwners"`
	NearDuplicateTopics         int     `json:"nearDuplicateTopics"`
	StaleReferences             int     `json:"staleReferences"`
	NotesBytes                  int64   `json:"notesBytes"`
	DomainCompressionApplicable bool    `json:"domainCompressionApplicable"`
	DomainCompressionRatio      float64 `json:"domainCompressionRatio"`

	// Optional Layer-1 suites
	Fixtures *FixtureSuiteReport `json:"fixtures,omitempty"`
	Replay   *ReplayEvalReport   `json:"replay,omitempty"`
}

// FixtureResult records pass/fail for one deterministic synthetic fixture.
type FixtureResult struct {
	ID      string `json:"id"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
}

// FixtureSuiteReport summarizes the F1-F4 fixture suite.
type FixtureSuiteReport struct {
	PassedCount int             `json:"passedCount"`
	TotalCount  int             `json:"totalCount"`
	AllPassed   bool            `json:"allPassed"`
	Results     []FixtureResult `json:"results"`
}

// ReplayArmMetrics summarizes SessionStart measurements across sampled historical conversations.
type ReplayArmMetrics struct {
	Label                 string  `json:"label"`
	SampleCount           int     `json:"sampleCount"`
	AvgContextBytes       float64 `json:"avgContextBytes"`
	P50ContextBytes       int     `json:"p50ContextBytes"`
	P95ContextBytes       int     `json:"p95ContextBytes"`
	MaxContextBytes       int     `json:"maxContextBytes"`
	FullOverBudgetCount   int     `json:"fullOverBudgetCount"`
	LeanAutoCount         int     `json:"leanAutoCount"`
	LeanAutoOverBudgetCnt int     `json:"leanAutoOverBudgetCount"`
	LeanAutoStarvedCount  int     `json:"leanAutoStarvedCount"`
	PlaybookInjectedCount int     `json:"playbookInjectedCount"`
	ErrorsCount           int     `json:"errorsCount"`
}

// ReplayEvalReport summarizes Layer-1 historical replay (optionally comparing baseline vs candidate).
type ReplayEvalReport struct {
	SampledConversations int               `json:"sampledConversations"`
	Candidate            ReplayArmMetrics  `json:"candidate"`
	Baseline             *ReplayArmMetrics `json:"baseline,omitempty"`
	AvgByteDeltaPct      float64           `json:"avgByteDeltaPct,omitempty"`
	P95ByteDeltaPct      float64           `json:"p95ByteDeltaPct,omitempty"`
}

// ReplayOptions configures RunHistoricalReplay.
type ReplayOptions struct {
	CandidateBin   string
	CandidateVault string
	BaselineBin    string
	BaselineVault  string
	Since          time.Time
	SampleLimit    int
}

// EvaluateMemory computes deterministic M1-M4 metrics lock-free on vaultDir.
func EvaluateMemory(vaultDir string, opts EvalOptions) (*MemoryEvalReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	since := opts.Since
	if since.IsZero() {
		since = now.AddDate(0, 0, -14)
	}

	audit, err := AuditMemory(vaultDir, nil)
	if err != nil {
		return nil, err
	}

	activeFacts, _, _, _ := LoadFactsStateAt(vaultDir, now)
	scopedFacts := 0
	globalFacts := 0
	var factsText strings.Builder
	for _, f := range activeFacts {
		factsText.WriteString(f.Statement)
		factsText.WriteByte('\n')
		isScoped := false
		for _, s := range f.Scopes {
			ns := NormalizeScope(s)
			if ns != "" && ns != "global" {
				isScoped = true
				break
			}
		}
		if isScoped {
			scopedFacts++
		} else {
			globalFacts++
		}
	}

	scopedPlaybooks := 0
	for _, tp := range audit.Topics {
		if tp.PlaybookOwner != "" && tp.ActivePlaybook != "" {
			scopedPlaybooks++
		}
	}

	// M1: Cross-layer 5-gram shingle overlap across {AGENTS.md, USER.md, MEMORY.md, active facts}
	var layerTexts []string
	if opts.RepoRoot != "" {
		if b, err := os.ReadFile(filepath.Join(opts.RepoRoot, ".agents", "plugins", "agy-hq", "rules", "AGENTS.md")); err == nil {
			layerTexts = append(layerTexts, string(b))
		}
	}
	if b, err := os.ReadFile(filepath.Join(vaultDir, "USER.md")); err == nil {
		layerTexts = append(layerTexts, string(b))
	}
	if b, err := os.ReadFile(filepath.Join(vaultDir, "MEMORY.md")); err == nil {
		layerTexts = append(layerTexts, string(b))
	}
	if factsText.Len() > 0 {
		layerTexts = append(layerTexts, factsText.String())
	}
	overlapPct := computeCrossLayerShingleOverlapPct(layerTexts, 5)

	// M2: Stale or missing TTL rate
	staleOrMissing := audit.OverdueTTLFactCount + len(audit.MissingTTLEventFacts)
	stalePct := 0.0
	if audit.ActiveFactCount > 0 {
		stalePct = round2(100.0 * float64(staleOrMissing) / float64(audit.ActiveFactCount))
	}

	// M3: Journal & intervention ground-truth metrics (avoid double-counting cumulative legacy ToolErrorCount)
	turns := 0
	totalSteps := 0
	totalToolErrors := 0
	preToolDenials := 0
	wikiReads := 0
	legacyMaxErrByConv := make(map[string]int)
	legacyMaxStepsByConv := make(map[string]int)
	_ = WalkJournals(vaultDir, since, true, func(_ string, rec JournalRecord) bool {
		turns++
		hasTurnDelta := rec.TurnStepCount > 0 || rec.StepStartIdx > 0
		if hasTurnDelta {
			totalSteps += rec.TurnStepCount
			totalToolErrors += rec.TurnErrorCount
		} else if cid := strings.TrimSpace(rec.ConversationID); cid != "" {
			if rec.StepCount > legacyMaxStepsByConv[cid] {
				legacyMaxStepsByConv[cid] = rec.StepCount
			}
			if rec.ToolErrorCount > legacyMaxErrByConv[cid] {
				legacyMaxErrByConv[cid] = rec.ToolErrorCount
			}
		}
		preToolDenials += rec.PreToolDenials
		lowerResp := strings.ToLower(rec.ResponseSummary)
		if strings.Contains(lowerResp, "memory/topics/") || strings.Contains(lowerResp, "hq memory search") {
			wikiReads++
		}
		return true
	})
	for _, s := range legacyMaxStepsByConv {
		totalSteps += s
	}
	for _, e := range legacyMaxErrByConv {
		totalToolErrors += e
	}

	errPer100 := 0.0
	denialsPer100 := 0.0
	if turns > 0 {
		errPer100 = round2(100.0 * float64(totalToolErrors) / float64(turns))
		denialsPer100 = round2(100.0 * float64(preToolDenials) / float64(turns))
	}

	approved, rejected, expired := countInterventionsSince(vaultDir, since)
	rejRate := 0.0
	if approved+rejected > 0 {
		rejRate = round2(100.0 * float64(rejected) / float64(approved+rejected))
	}

	// M4: Domain compression ratio (only applicable when Bytes(notes/) >= 20 KB)
	notesBytes := dirTotalBytes(filepath.Join(vaultDir, "notes"))
	compApplicable := notesBytes >= 20*1024 && audit.TopicsTotalBytes > 0
	compRatio := 0.0
	if compApplicable {
		compRatio = round2(float64(notesBytes) / float64(audit.TopicsTotalBytes))
	}

	return &MemoryEvalReport{
		GeneratedAt:                  now.UTC().Format(time.RFC3339),
		Since:                        since.UTC().Format(time.RFC3339),
		MemoryLines:                  audit.MemoryLines,
		MemoryBytes:                  audit.MemoryBytes,
		MemoryOverBudget:             audit.OverBudget,
		UserBytes:                    audit.UserBytes,
		ActiveFactCount:              audit.ActiveFactCount,
		ExpiredFactCount:             audit.ExpiredFactCount,
		TopicsCount:                  len(audit.Topics),
		TopicsTotalBytes:             audit.TopicsTotalBytes,
		CrossLayerOverlapPct:         overlapPct,
		ScopedFactCount:              scopedFacts,
		GlobalFactCount:              globalFacts,
		ScopedPlaybookCount:          scopedPlaybooks,
		OverdueTTLFactCount:          audit.OverdueTTLFactCount,
		MissingTTLEventFactsCnt:      len(audit.MissingTTLEventFacts),
		StaleOrMissingTTLPct:         stalePct,
		JournalTurns:                 turns,
		TotalSteps:                   totalSteps,
		TotalToolErrors:              totalToolErrors,
		PreToolDenials:               preToolDenials,
		ToolErrorsPer100Turns:        errPer100,
		PreToolDenialsPer100Turns:    denialsPer100,
		InterventionsApproved:        approved,
		InterventionsRejected:        rejected,
		InterventionsExpired:         expired,
		InterventionRejectionRatePct: rejRate,
		WikiReadCount:                wikiReads,
		InvalidTopicHeaders:          len(audit.InvalidTopicHeaders),
		BrokenTopicLinks:             len(audit.BrokenTopicLinks),
		DuplicatePlaybookOwners:      len(audit.DuplicatePlaybookOwners),
		NearDuplicateTopics:          len(audit.NearDuplicateTopics),
		StaleReferences:              len(audit.StaleReferences),
		NotesBytes:                   notesBytes,
		DomainCompressionApplicable:  compApplicable,
		DomainCompressionRatio:       compRatio,
	}, nil
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func computeCrossLayerShingleOverlapPct(texts []string, n int) float64 {
	if len(texts) < 2 || n <= 1 {
		return 0
	}
	shingleSets := make([]map[string]bool, 0, len(texts))
	allShingles := make(map[string]int)
	for _, t := range texts {
		toks := tokenizeForBM25(t)
		if len(toks) < n {
			continue
		}
		set := make(map[string]bool)
		for i := 0; i <= len(toks)-n; i++ {
			sh := strings.Join(toks[i:i+n], " ")
			if !set[sh] {
				set[sh] = true
				allShingles[sh]++
			}
		}
		shingleSets = append(shingleSets, set)
	}
	if len(shingleSets) < 2 || len(allShingles) == 0 {
		return 0
	}
	shared := 0
	for _, cnt := range allShingles {
		if cnt >= 2 {
			shared++
		}
	}
	return round2(100.0 * float64(shared) / float64(len(allShingles)))
}

func countInterventionsSince(vaultDir string, since time.Time) (approved, rejected, expired int) {
	p := filepath.Join(vaultDir, "memory", "interventions.jsonl")
	if !isRegularNonSymlinkFile(p) {
		return 0, 0, 0
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec interventionLite
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if ts, ok := parseAnyTimestamp(rec.Timestamp); ok && ts.Before(since) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(rec.Decision)) {
		case "approved":
			approved++
		case "rejected":
			rejected++
		case "expired":
			expired++
		}
	}
	return approved, rejected, expired
}

func dirTotalBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// RunEvalFixtures executes the deterministic F1-F4 synthetic fixture suite inside an isolated temporary directory.
func RunEvalFixtures() (*FixtureSuiteReport, error) {
	var rawSpec struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(embeddedEvalFixtures, &rawSpec); err != nil {
		return nil, fmt.Errorf("invalid embedded eval fixtures: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "hq-memory-eval-fixtures-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	now := time.Now().UTC()
	var results []FixtureResult

	// F1: Scoped Fact Isolation & Front Desk <=3 per-topic diversity cap
	{
		v1 := filepath.Join(tmpDir, "f1")
		_, _, _ = AddFactWithOptions(v1, AddFactOptions{ID: "f-alpha-1", Topic: "architecture", Statement: "Always run go test -race in repo-alpha", Scopes: []string{"repo:repo-alpha"}, Verified: true, Now: now})
		_, _, _ = AddFactWithOptions(v1, AddFactOptions{ID: "f-beta-1", Topic: "architecture", Statement: "Use pnpm build in repo-beta", Scopes: []string{"repo:repo-beta"}, Verified: true, Now: now})
		_, _, _ = AddFactWithOptions(v1, AddFactOptions{ID: "f-global-1", Topic: "comms", Statement: "Reply concisely in active language", Scopes: []string{"global"}, Verified: true, Now: now})
		for i := 1; i <= 4; i++ {
			_, _, _ = AddFactWithOptions(v1, AddFactOptions{
				ID:        fmt.Sprintf("f-fd-%d", i),
				Topic:     "tooling",
				Statement: fmt.Sprintf("Tooling rule %d", i),
				Scopes:    []string{"global"},
				Verified:  true,
				Now:       now,
			})
		}
		active, _, _, _ := LoadFactsStateAt(v1, now)
		alphaSel := SelectFactsForSession(active, SessionScopeContext{Role: "task", RepoSlug: "repo-alpha", Scopes: []string{"repo:repo-alpha"}}, now, 15, 2500)
		hasAlpha, hasBeta := false, false
		for _, f := range alphaSel {
			if f.ID == "f-alpha-1" {
				hasAlpha = true
			}
			if f.ID == "f-beta-1" {
				hasBeta = true
			}
		}
		fdSel := SelectFactsForSession(active, SessionScopeContext{Role: "front_desk"}, now, 10, 1800)
		toolingCnt := 0
		for _, f := range fdSel {
			if f.Topic == "tooling" {
				toolingCnt++
			}
		}
		passed := hasAlpha && !hasBeta && toolingCnt == 3
		results = append(results, FixtureResult{
			ID:      "F1_scoped_isolation",
			Passed:  passed,
			Details: fmt.Sprintf("hasAlpha=%v hasBeta=%v frontDeskToolingCount=%d (want 3)", hasAlpha, hasBeta, toolingCnt),
		})
	}

	// F2: TTL Auto-Expiration & 72h Imminent Boost
	{
		v2 := filepath.Join(tmpDir, "f2")
		_, _, _ = AddFactWithOptions(v2, AddFactOptions{ID: "f-past", Topic: "schedule", Statement: "Old dentist appointment", ExpiresAt: now.Add(-24 * time.Hour).Format(time.RFC3339), Verified: true, Now: now})
		_, _, _ = AddFactWithOptions(v2, AddFactOptions{ID: "f-future", Topic: "schedule", Statement: "Annual renewal in 30 days", ExpiresAt: now.Add(720 * time.Hour).Format(time.RFC3339), Verified: true, Now: now})
		_, _, _ = AddFactWithOptions(v2, AddFactOptions{ID: "f-imminent", Topic: "schedule", Statement: "Quarterly board review in 24h", ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339), Verified: true, Now: now})

		active, expired, _, _ := LoadFactsStateAt(v2, now)
		sel := SelectFactsForSession(active, SessionScopeContext{Role: "front_desk"}, now, 5, 1500)
		pastExpired := len(expired) == 1 && expired[0].ID == "f-past"
		imminentFirst := len(sel) >= 2 && sel[0].ID == "f-imminent"
		passed := pastExpired && imminentFirst
		results = append(results, FixtureResult{
			ID:      "F2_ttl_expiry_and_imminent_boost",
			Passed:  passed,
			Details: fmt.Sprintf("pastExpired=%v imminentFirst=%v activeCount=%d", pastExpired, imminentFirst, len(active)),
		})
	}

	// F3: Playbook Owner 1:1 Enforcement & Front Desk Exclusion
	{
		v3 := filepath.Join(tmpDir, "f3")
		topicsDir := filepath.Join(v3, "memory", "topics")
		_ = os.MkdirAll(topicsDir, 0o700)
		content := "# Alpha Architecture\n> **Summary:** Core build traps.\n> **Scopes:** repo:repo-alpha\n> **Playbook-Owner:** repo:repo-alpha\n\n## Active Playbook\n- Run go test ./... before pushing.\n\n## Details\nDeep notes.\n"
		_ = os.WriteFile(filepath.Join(topicsDir, "alpha-arch.md"), []byte(content), 0o600)
		topics, _ := LoadTopicIndex(v3)
		taskPlaybooks := SelectScopedPlaybooks(topics, SessionScopeContext{Role: "task", RepoSlug: "repo-alpha", Scopes: []string{"repo:repo-alpha"}}, MaxPlaybookLines, MaxPlaybookBytes)
		fdPlaybooks := SelectScopedPlaybooks(topics, SessionScopeContext{Role: "front_desk"}, MaxPlaybookLines, MaxPlaybookBytes)
		passed := len(taskPlaybooks) == 1 && len(fdPlaybooks) == 0
		results = append(results, FixtureResult{
			ID:      "F3_playbook_owner_and_front_desk_exclusion",
			Passed:  passed,
			Details: fmt.Sprintf("taskPlaybooks=%d (want 1) frontDeskPlaybooks=%d (want 0)", len(taskPlaybooks), len(fdPlaybooks)),
		})
	}

	// F4: BM25 Search Precision & Security Boundary (excludes private/** & tainted journals)
	{
		v4 := filepath.Join(tmpDir, "f4")
		_ = os.MkdirAll(filepath.Join(v4, "memory", "topics"), 0o700)
		_ = os.MkdirAll(filepath.Join(v4, "private"), 0o700)
		_ = os.MkdirAll(filepath.Join(v4, "memory", "journal", "2026-11-18"), 0o700)
		_ = os.WriteFile(filepath.Join(v4, "memory", "topics", "investing.md"),
			[]byte("# Investing\n> **Summary:** Earnings playbook.\n\n## Quarterly Earnings Checklist\nVerify gross margin and operating cash flow.\n"), 0o600)
		_ = os.WriteFile(filepath.Join(v4, "private", "taxes.md"),
			[]byte("# Secret Tax\nTOPSECRET_TAX_TOKEN_999 quarterly earnings checklist\n"), 0o600)
		taintedRec, _ := json.Marshal(JournalRecord{
			Timestamp:       now.Format(time.RFC3339),
			ConversationID:  "conv-tainted-1",
			TaintedSession:  true,
			UserPrompt:      "TOPSECRET_TAX_TOKEN_999 tainted prompt",
			ResponseSummary: "TOPSECRET_TAX_TOKEN_999 tainted response",
		})
		_ = os.WriteFile(filepath.Join(v4, "memory", "journal", "2026-11-18", "tainted.json"), taintedRec, 0o600)

		goodHits, _ := SearchMemory(v4, SearchOptions{Query: "quarterly earnings checklist", Limit: 5})
		secretHits, _ := SearchMemory(v4, SearchOptions{Query: "TOPSECRET_TAX_TOKEN_999", Limit: 5})
		foundTopic := len(goodHits) > 0 && goodHits[0].Layer == "topics" && strings.Contains(goodHits[0].Provenance, "#L")
		noSecretLeak := len(secretHits) == 0
		passed := foundTopic && noSecretLeak
		results = append(results, FixtureResult{
			ID:      "F4_bm25_search_and_security_boundary",
			Passed:  passed,
			Details: fmt.Sprintf("foundTopic=%v secretHits=%d (want 0)", foundTopic, len(secretHits)),
		})
	}

	passedCnt := 0
	for _, r := range results {
		if r.Passed {
			passedCnt++
		}
	}
	return &FixtureSuiteReport{
		PassedCount: passedCnt,
		TotalCount:  len(results),
		AllPassed:   passedCnt == len(results),
		Results:     results,
	}, nil
}

// RunHistoricalReplay samples real historical conversations from CandidateVault's journals
// and replays synthetic SessionStart hook invocations against CandidateBin (and optionally BaselineBin + BaselineVault).
func RunHistoricalReplay(opts ReplayOptions) (*ReplayEvalReport, error) {
	if strings.TrimSpace(opts.CandidateBin) == "" {
		return nil, fmt.Errorf("candidate binary path is required for replay")
	}
	if strings.TrimSpace(opts.CandidateVault) == "" {
		return nil, fmt.Errorf("candidate vault path is required for replay")
	}
	limit := opts.SampleLimit
	if limit <= 0 {
		limit = 50
	}

	// Collect distinct untainted conversations (first turn per conversation)
	seen := make(map[string]bool)
	var samples []JournalRecord
	_ = WalkJournals(opts.CandidateVault, opts.Since, true, func(_ string, rec JournalRecord) bool {
		if rec.TaintedSession {
			return true
		}
		cid := strings.TrimSpace(rec.ConversationID)
		if cid == "" || seen[cid] {
			return true
		}
		seen[cid] = true
		samples = append(samples, rec)
		return true
	})

	// Sort deterministically by timestamp descending, then take up to limit
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].Timestamp > samples[j].Timestamp
	})
	if len(samples) > limit {
		samples = samples[:limit]
	}

	candMetrics := runReplayArm("candidate", opts.CandidateBin, opts.CandidateVault, samples)
	rep := &ReplayEvalReport{
		SampledConversations: len(samples),
		Candidate:            candMetrics,
	}

	if strings.TrimSpace(opts.BaselineBin) != "" {
		baseVault := opts.BaselineVault
		if strings.TrimSpace(baseVault) == "" {
			baseVault = opts.CandidateVault
		}
		baseMetrics := runReplayArm("baseline", opts.BaselineBin, baseVault, samples)
		rep.Baseline = &baseMetrics
		if baseMetrics.AvgContextBytes > 0 {
			rep.AvgByteDeltaPct = round2(100.0 * (candMetrics.AvgContextBytes - baseMetrics.AvgContextBytes) / baseMetrics.AvgContextBytes)
		}
		if baseMetrics.P95ContextBytes > 0 {
			rep.P95ByteDeltaPct = round2(100.0 * float64(candMetrics.P95ContextBytes-baseMetrics.P95ContextBytes) / float64(baseMetrics.P95ContextBytes))
		}
	}

	return rep, nil
}

func runReplayArm(label, binPath, vaultDir string, samples []JournalRecord) ReplayArmMetrics {
	m := ReplayArmMetrics{
		Label:       label,
		SampleCount: len(samples),
	}
	if len(samples) == 0 {
		return m
	}

	var sizes []int
	totalBytes := 0

	for _, rec := range samples {
		payload := map[string]interface{}{
			"conversationId": rec.ConversationID,
			"workspacePaths": rec.WorkspacePaths,
			"transcriptPath": rec.TranscriptPath,
			"lastUserInput":  rec.UserPrompt,
		}
		inBytes, _ := json.Marshal(payload)
		cmd := exec.Command(binPath, "hook", "session-start")
		cmd.Stdin = bytes.NewReader(inBytes)
		cmd.Env = append(os.Environ(),
			"HQ_VAULT_DIR="+vaultDir,
			"HQ_NONINTERACTIVE=1",
		)
		out, err := cmd.Output()
		if err != nil {
			m.ErrorsCount++
			continue
		}
		var resp struct {
			AdditionalContext string `json:"additionalContext"`
			InjectSteps       []struct {
				SystemMessage struct {
					SystemMessage string `json:"systemMessage"`
				} `json:"systemMessage"`
			} `json:"injectSteps"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			m.ErrorsCount++
			continue
		}
		ctxText := resp.AdditionalContext
		if ctxText == "" && len(resp.InjectSteps) > 0 {
			ctxText = resp.InjectSteps[0].SystemMessage.SystemMessage
		}
		sz := len(ctxText)
		sizes = append(sizes, sz)
		totalBytes += sz
		if sz > m.MaxContextBytes {
			m.MaxContextBytes = sz
		}

		isTainted := strings.Contains(ctxText, "Quarantined Triage Worker (TAINTED)")
		isLean := !isTainted && (strings.Contains(ctxText, "Lean Auto/Skill Context") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(rec.UserPrompt)), "[auto]"))
		if isTainted {
			if sz > MaxSessionTaintBytes {
				m.LeanAutoOverBudgetCnt++
			}
		} else if isLean {
			m.LeanAutoCount++
			if sz > MaxSessionLeanBytes {
				m.LeanAutoOverBudgetCnt++
			}
			// Starvation check: untainted lean_auto should include Active Vault Facts or Knowledge Wiki
			if !strings.Contains(ctxText, "## Active Vault Facts") && !strings.Contains(ctxText, "## HQ Knowledge Wiki") {
				m.LeanAutoStarvedCount++
			}
		} else if sz > MaxSessionFullBytes {
			m.FullOverBudgetCount++
		}
		if strings.Contains(ctxText, "### Scoped Playbook:") || strings.Contains(ctxText, "## Active Playbook") {
			m.PlaybookInjectedCount++
		}
	}

	if len(sizes) > 0 {
		sort.Ints(sizes)
		m.AvgContextBytes = round2(float64(totalBytes) / float64(len(sizes)))
		m.P50ContextBytes = sizes[len(sizes)/2]
		p95Idx := int(math.Ceil(0.95*float64(len(sizes)))) - 1
		if p95Idx < 0 {
			p95Idx = 0
		}
		if p95Idx >= len(sizes) {
			p95Idx = len(sizes) - 1
		}
		m.P95ContextBytes = sizes[p95Idx]
	}
	return m
}
