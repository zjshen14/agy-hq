package kernel

import (
	"fmt"
	"sync"
)

const (
	// LSDeliveryStrategyWhenIdle queues messages without preempting active turns or running subagents.
	LSDeliveryStrategyWhenIdle = "MESSAGE_DELIVERY_STRATEGY_WHEN_IDLE"

	// DefaultAutoCompactContextTokens mirrors obs.DefaultBudgetConfig().WarnContextTokens
	// as a safe fallback when a vault budget.json does not specify an override.
	DefaultAutoCompactContextTokens int64 = 120_000

	// DefaultCompactMinPlannerSteps requires at least two uncompacted PLANNER_RESPONSE steps
	// after the latest completed CHECKPOINT before pre-run compaction triggers.
	DefaultCompactMinPlannerSteps = 2
)

// FrontDeskContextSnapshot captures live prompt context tokens and uncompacted planner activity
// for the active Front Desk conversation.
type FrontDeskContextSnapshot struct {
	ConversationID                  string `json:"conversationId"`
	TranscriptPath                  string `json:"transcriptPath,omitempty"`
	CurrentContextTokens            int64  `json:"currentContextTokens"`
	AutoCompactContextTokens        int64  `json:"autoCompactContextTokens"`
	PlannerResponsesSinceCheckpoint int    `json:"plannerResponsesSinceCheckpoint"`
	LastCheckpointStepIdx           int    `json:"lastCheckpointStepIdx"`
	CompactionCount                 int    `json:"compactionCount"`
}

// FrontDeskCompactionDecision explains whether an opted-in front_desk_cron delivery should prepend /compact.
type FrontDeskCompactionDecision struct {
	ShouldCompact                   bool
	Reason                          string
	CurrentContextTokens            int64
	ThresholdTokens                 int64
	PlannerResponsesSinceCheckpoint int
	MinPlannerSteps                 int
}

var (
	frontDeskContextInspectorMu sync.RWMutex
	frontDeskContextInspector   func(vaultDir string) (FrontDeskContextSnapshot, bool)
)

// RegisterFrontDeskContextInspector wires internal/obs context inspection into kernel without
// introducing a kernel -> obs import cycle.
func RegisterFrontDeskContextInspector(fn func(vaultDir string) (FrontDeskContextSnapshot, bool)) {
	frontDeskContextInspectorMu.Lock()
	defer frontDeskContextInspectorMu.Unlock()
	frontDeskContextInspector = fn
}

func inspectFrontDeskContext(vaultDir string) (FrontDeskContextSnapshot, bool) {
	frontDeskContextInspectorMu.RLock()
	fn := frontDeskContextInspector
	frontDeskContextInspectorMu.RUnlock()
	if fn == nil {
		return FrontDeskContextSnapshot{}, false
	}
	return fn(vaultDir)
}

// EvaluateFrontDeskCompactionGate decides whether an opted-in Front Desk delivery for sessionID
// should prepend a system /compact slashCommand before delivering the continuation prompt.
func EvaluateFrontDeskCompactionGate(vaultDir, sessionID string, thresholdOverride int64, minPlannerOverride int) FrontDeskCompactionDecision {
	minSteps := minPlannerOverride
	if minSteps <= 0 {
		minSteps = DefaultCompactMinPlannerSteps
	}
	threshold := thresholdOverride
	if threshold <= 0 {
		threshold = DefaultAutoCompactContextTokens
	}
	dec := FrontDeskCompactionDecision{
		ThresholdTokens: threshold,
		MinPlannerSteps: minSteps,
	}

	fd, ok := GetFrontDesk(vaultDir)
	fdID := normalizeUUID(fd.ConversationID)
	targetID := normalizeUUID(sessionID)
	if !ok || fdID == "" || targetID == "" || fdID != targetID {
		dec.Reason = "not_active_front_desk"
		return dec
	}

	snap, snapOK := inspectFrontDeskContext(vaultDir)
	if !snapOK {
		dec.Reason = "context_unavailable"
		return dec
	}
	if snapID := normalizeUUID(snap.ConversationID); snapID != "" && snapID != targetID {
		dec.Reason = "conversation_mismatch"
		return dec
	}

	if thresholdOverride <= 0 && snap.AutoCompactContextTokens > 0 {
		dec.ThresholdTokens = snap.AutoCompactContextTokens
	}
	dec.CurrentContextTokens = snap.CurrentContextTokens
	dec.PlannerResponsesSinceCheckpoint = snap.PlannerResponsesSinceCheckpoint

	if dec.CurrentContextTokens < dec.ThresholdTokens {
		dec.Reason = fmt.Sprintf("below_token_watermark(%d<%d)", dec.CurrentContextTokens, dec.ThresholdTokens)
		return dec
	}
	if dec.PlannerResponsesSinceCheckpoint < dec.MinPlannerSteps {
		dec.Reason = fmt.Sprintf("insufficient_planner_activity(%d<%d)", dec.PlannerResponsesSinceCheckpoint, dec.MinPlannerSteps)
		return dec
	}

	dec.ShouldCompact = true
	dec.Reason = fmt.Sprintf("gated_compact(ctx=%d>=%d,planner=%d>=%d)",
		dec.CurrentContextTokens, dec.ThresholdTokens,
		dec.PlannerResponsesSinceCheckpoint, dec.MinPlannerSteps)
	return dec
}
