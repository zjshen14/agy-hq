// Package workspace implements the RFC-0025 Pluggable Workspace Provider seam: the vendor-neutral Provider
// interface, the registry of built-in and pack-declared providers, the hq-workspace-provider/v1 stdio adapter,
// response sanitization, and the ownership ledger at <vault>/memory/workspaces.json.
//
// The package must not import internal/kernel; the kernel imports it (and registers core/git-worktree).
package workspace

import (
	"errors"

	"github.com/zjshen14/agy-hq/internal/extension"
)

const (
	// ProtocolV1 is the frozen JSON-over-stdio protocol spoken by pack workspace providers (RFC-0025 §6, §16.7).
	ProtocolV1 = extension.WorkspaceProviderProtocolV1
	// CoreGitProviderID is the FQID of the built-in git worktree provider implemented by internal/kernel.
	CoreGitProviderID = "core/git-worktree"
)

// State is the normalized workspace cleanliness reported by a provider.
type State string

const (
	StateClean   State = "clean"
	StateDirty   State = "dirty"
	StateMissing State = "missing"
	StateUnknown State = "unknown"
)

// Valid reports whether s is one of the four protocol states.
func (s State) Valid() bool {
	switch s {
	case StateClean, StateDirty, StateMissing, StateUnknown:
		return true
	}
	return false
}

// ChangeState is the normalized review lifecycle of a change held by a workspace.
type ChangeState string

const (
	ChangeLocal     ChangeState = "local" // not yet uploaded for review
	ChangeDraft     ChangeState = "draft"
	ChangeInReview  ChangeState = "in_review"
	ChangeApproved  ChangeState = "approved"
	ChangeLanded    ChangeState = "landed" // replaces git "merged into the default branch"
	ChangeAbandoned ChangeState = "abandoned"
)

// Valid reports whether s is one of the six protocol change states.
func (s ChangeState) Valid() bool {
	switch s {
	case ChangeLocal, ChangeDraft, ChangeInReview, ChangeApproved, ChangeLanded, ChangeAbandoned:
		return true
	}
	return false
}

// Open reports whether a change in state s still holds unlanded work (anything but landed or abandoned).
func (s ChangeState) Open() bool {
	return s != ChangeLanded && s != ChangeAbandoned
}

// ChangeRef is an opaque, display-safe reference to a change held by a workspace.
type ChangeRef struct {
	ID         string      `json:"id"` // opaque, <= 64 chars of [A-Za-z0-9._/:#-]
	State      ChangeState `json:"state"`
	NeedsOwner bool        `json:"needs_owner"`          // waiting on the workspace owner
	Title      string      `json:"title,omitempty"`      // sanitized, <= 80 runes
	URL        string      `json:"url,omitempty"`        // optional, https only, never fetched by hq
	UpdatedAt  string      `json:"updated_at,omitempty"` // RFC 3339
}

// Info describes one workspace. Provider is always filled in by core, never by the provider.
type Info struct {
	Provider    string      `json:"provider"`     // FQID, e.g. "core/git-worktree" or "<pack>/<id>"
	WorkspaceID string      `json:"workspace_id"` // stable within the provider
	Name        string      `json:"name"`
	Root        string      `json:"root"`             // absolute, clean
	WorkDir     string      `json:"work_dir"`         // where the agent should run (Root or a directory inside it)
	Flavor      string      `json:"flavor,omitempty"` // provider-defined short label, display only
	Kind        string      `json:"kind,omitempty"`   // "primary" | "auxiliary" (auxiliary hidden by default)
	State       State       `json:"state"`
	Changes     []ChangeRef `json:"changes,omitempty"`
	Base        string      `json:"base,omitempty"` // opaque sync point, display only
}

// CountOpenChanges returns how many changes in changes still hold unlanded work.
func CountOpenChanges(changes []ChangeRef) int {
	n := 0
	for _, c := range changes {
		if c.State.Open() {
			n++
		}
	}
	return n
}

// Capabilities declares which optional verbs a provider supports.
type Capabilities struct {
	Create        bool `json:"create"`
	Remove        bool `json:"remove"`
	Changes       bool `json:"changes"`
	SharedHistory bool `json:"shared_history"`
}

// ListRequest pages through a provider's inventory.
type ListRequest struct {
	Cursor           string `json:"cursor,omitempty"`
	Limit            int    `json:"limit,omitempty"` // <= MaxListLimit; <= 0 means MaxListLimit
	IncludeAuxiliary bool   `json:"include_auxiliary,omitempty"`
}

// CreateRequest asks a provider for an isolated workspace for a task.
type CreateRequest struct {
	Name           string `json:"name"` // derived by core from the task slug (see ValidateCreateName)
	Base           string `json:"base,omitempty"`
	ShareFrom      string `json:"share_from,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// CreateResult reports the created (or pre-existing) workspace. Existed=true means the workspace already
// existed before the call, so core must ledger it as attached rather than owned.
type CreateResult struct {
	Info    Info `json:"workspace"`
	Existed bool `json:"existed"`
}

// RemoveRequest asks a provider to delete a workspace. The provider must re-check RequireClean and
// RequireNoOpenChanges atomically and refuse with CodeDirty / CodeHasOpenChanges instead of deleting.
type RemoveRequest struct {
	WorkspaceID          string `json:"workspace_id"`
	ExpectedRoot         string `json:"expected_root"`
	RequireClean         bool   `json:"require_clean"`
	RequireNoOpenChanges bool   `json:"require_no_open_changes"`
}

// Error codes shared by provider responses and core-side precondition failures.
const (
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeDirty          = "dirty"
	CodeHasOpenChanges = "has_open_changes"
	CodeUnsupported    = "unsupported"
	CodeUnavailable    = "unavailable"
	CodeInvalid        = "invalid"
)

// ValidErrorCode reports whether code is one of the protocol error codes.
func ValidErrorCode(code string) bool {
	switch code {
	case CodeNotFound, CodeConflict, CodeDirty, CodeHasOpenChanges, CodeUnsupported, CodeUnavailable, CodeInvalid:
		return true
	}
	return false
}

// ProviderError is a structured, protocol-level failure: either an `ok:false` provider response or a core-side
// precondition refusal expressed in the same vocabulary. Transport failures (trust verification, timeouts,
// malformed or oversized responses) are returned as ordinary errors instead.
type ProviderError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return "workspace provider error: " + e.Code
	}
	return "workspace provider error: " + e.Code + ": " + e.Message
}

// ErrorCode returns the ProviderError code carried by err (or wrapped inside it), or "" when there is none.
func ErrorCode(err error) string {
	var pe *ProviderError
	if errors.As(err, &pe) && pe != nil {
		return pe.Code
	}
	return ""
}

// Binding roles for ActiveTask.Workspaces.
const (
	RolePrimary   = "primary"
	RoleSecondary = "secondary"
	RoleReadonly  = "readonly"
)

// Binding attaches a workspace to a task. JSON uses camelCase to match the rest of tasks.json.
type Binding struct {
	Key      string `json:"key"` // LedgerKey(Provider, workspace ID)
	Provider string `json:"provider"`
	Root     string `json:"root"`
	WorkDir  string `json:"workDir,omitempty"`
	Role     string `json:"role"` // RolePrimary | RoleSecondary | RoleReadonly
}

// MergeBindings unions two binding lists by Key: existing order is preserved, an incoming binding replaces the
// existing one with the same Key, and new keys are appended in incoming order. Bindings without a Key are
// dropped. It returns nil when the union is empty.
func MergeBindings(existing, incoming []Binding) []Binding {
	incomingByKey := make(map[string]Binding, len(incoming))
	for _, b := range incoming {
		if b.Key != "" {
			incomingByKey[b.Key] = b
		}
	}
	var out []Binding
	seen := make(map[string]bool, len(existing)+len(incoming))
	for _, b := range existing {
		if b.Key == "" || seen[b.Key] {
			continue
		}
		seen[b.Key] = true
		if nb, ok := incomingByKey[b.Key]; ok {
			b = nb
		}
		out = append(out, b)
	}
	for _, b := range incoming {
		if b.Key == "" || seen[b.Key] {
			continue
		}
		seen[b.Key] = true
		out = append(out, incomingByKey[b.Key])
	}
	return out
}
