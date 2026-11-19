package workspace

import "context"

// Provider is implemented by the built-in core/git-worktree provider (internal/kernel) and by the external
// stdio adapter for pack-declared providers (ExternalProvider).
//
// Implementations never decide policy: callers (the lifecycle layer) enforce ledger ownership, approvals, and
// task-binding rules, and must not call a Provider while holding the tasks or ledger lock.
type Provider interface {
	// ID returns the provider FQID ("core/git-worktree" or "<pack>/<id>").
	ID() string
	// Capabilities reports which optional verbs the provider supports.
	Capabilities() Capabilities
	// Probe reports whether the provider owns absolute path, returning the owning workspace when it does.
	Probe(ctx context.Context, path string) (Info, bool, error)
	// List pages through the provider's inventory and returns the next cursor ("" when done).
	List(ctx context.Context, req ListRequest) ([]Info, string, error)
	// Create makes (or idempotently returns) an isolated workspace.
	Create(ctx context.Context, req CreateRequest) (CreateResult, error)
	// Status returns fresh state and changes for one workspace.
	Status(ctx context.Context, workspaceID string) (Info, error)
	// Remove deletes one workspace after the provider re-checks the request's preconditions.
	Remove(ctx context.Context, req RemoveRequest) error
}

// VaultScoped is implemented by built-in providers whose operations depend on the Vault (for example
// core/git-worktree, which lives under <vault>/worktrees/). Resolve calls ForVault so the returned Provider is
// bound to the caller's Vault.
type VaultScoped interface {
	ForVault(vaultDir string) Provider
}

// Describer is an optional interface for built-in providers that supply a one-line description.
type Describer interface {
	Description() string
}
