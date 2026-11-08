package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// Check if repoSpec is an existing local directory
	if info, err := os.Stat(repoSpec); err == nil && info.IsDir() {
		absRepo, err := filepath.Abs(repoSpec)
		if err != nil {
			return "", err
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
			cloneCmd := exec.Command("git", "clone", "--bare", url, bareDir)
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
	if branch == "" {
		branch = "hq/" + safeTask
	}

	cmd := exec.Command("git", "-C", gitRepoDir, "worktree", "add", "-b", branch, targetDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fallback if branch already exists
		cmdExisting := exec.Command("git", "-C", gitRepoDir, "worktree", "add", targetDir, branch)
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
	_ = exec.Command("git", "-C", target, "worktree", "remove", "--force", target).Run()
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
