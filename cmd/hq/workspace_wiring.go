package main

import (
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/worktree"
)

// The built-in core/git-worktree workspace provider (RFC-0025 §4.2) creates worktrees with worktree.Add. The
// kernel cannot import internal/worktree directly (worktree -> vault -> kernel), so the binary wires it here.
func init() {
	kernel.RegisterGitWorktreeAdder(worktree.Add)
}
