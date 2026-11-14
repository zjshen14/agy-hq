package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zjshen14/agy-hq/internal/vault"
)

// Add creates an isolated git worktree under local/worktrees/<repo>-<task>/
// from either a local git repository path or a remote GitHub URL/slug.
func Add(vaultDir, repoSpec, taskName, branch string) (string, error) {
	worktreesRoot := filepath.Join(vaultDir, "worktrees")
	reposRoot := filepath.Join(vaultDir, "repos")
	if err := os.MkdirAll(worktreesRoot, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(reposRoot, 0o755); err != nil {
		return "", err
	}

	var gitRepoDir string
	var repoBase string

	// Check if repoSpec is an existing local directory (or sibling workspace name)
	localCandidate := repoSpec
	if info, err := os.Stat(localCandidate); err != nil || !info.IsDir() {
		if !strings.Contains(repoSpec, "/") && !strings.Contains(repoSpec, ":") {
			if cwd, cErr := os.Getwd(); cErr == nil && cwd != "" {
				sibling := filepath.Join(filepath.Dir(cwd), repoSpec)
				if st, sErr := os.Stat(sibling); sErr == nil && st.IsDir() {
					localCandidate = sibling
				}
			}
		}
	}
	if info, err := os.Stat(localCandidate); err == nil && info.IsDir() {
		absRepo, err := filepath.Abs(localCandidate)
		if err != nil {
			return "", err
		}
		if topOut, tErr := vault.SanitizedGitCommand(absRepo, "rev-parse", "--show-toplevel").Output(); tErr == nil {
			if top := strings.TrimSpace(string(topOut)); top != "" {
				absRepo = filepath.Clean(top)
			}
		}
		gitRepoDir = absRepo
		repoBase = filepath.Base(absRepo)
	} else {
		// Treat as remote URL or GitHub owner/repo slug
		url := repoSpec
		if !strings.Contains(repoSpec, "://") && !strings.HasPrefix(repoSpec, "git@") {
			url = "https://github.com/" + strings.TrimPrefix(repoSpec, "/") + ".git"
		}
		parts := strings.Split(strings.TrimSuffix(url, ".git"), "/")
		repoBase = parts[len(parts)-1]
		bareDir := filepath.Join(reposRoot, repoBase+".git")
		if _, err := os.Stat(bareDir); os.IsNotExist(err) {
			cloneCmd := vault.SanitizedGitCommand("", "clone", "--bare", url, bareDir)
			if out, err := cloneCmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("git clone --bare %s: %w (%s)", url, err, string(out))
			}
		}
		gitRepoDir = bareDir
	}

	safeTask := strings.ReplaceAll(strings.TrimSpace(taskName), "/", "-")
	folderName := fmt.Sprintf("%s-%s", repoBase, safeTask)
	if strings.HasPrefix(safeTask, repoBase+"-") {
		folderName = safeTask
	}
	targetDir := filepath.Join(worktreesRoot, folderName)
	if info, err := os.Stat(targetDir); err == nil && info.IsDir() {
		return targetDir, nil
	}
	if branch == "" {
		branch = "hq/" + safeTask
	}

	cmd := vault.SanitizedGitCommand("", "-C", gitRepoDir, "worktree", "add", "-b", branch, targetDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fallback if branch already exists
		cmdExisting := vault.SanitizedGitCommand("", "-C", gitRepoDir, "worktree", "add", targetDir, branch)
		if out2, err2 := cmdExisting.CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("git worktree add failed: %v (%s / %s)", err, string(out), string(out2))
		}
	}
	return targetDir, nil
}

// Remove deletes an isolated git worktree from local/worktrees/ and prunes its git worktree entry.
func Remove(vaultDir, nameOrPath string) error {
	worktreesRoot := filepath.Join(vaultDir, "worktrees")
	target := nameOrPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktreesRoot, filepath.Base(nameOrPath))
	}
	if _, err := os.Stat(target); err != nil {
		return err
	}
	_ = vault.SanitizedGitCommand("", "-C", target, "worktree", "remove", "--force", target).Run()
	return os.RemoveAll(target)
}

// List returns the directories currently checked out under local/worktrees/.
func List(vaultDir string) ([]string, error) {
	worktreesRoot := filepath.Join(vaultDir, "worktrees")
	entries, err := os.ReadDir(worktreesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(worktreesRoot, e.Name()))
		}
	}
	return out, nil
}
