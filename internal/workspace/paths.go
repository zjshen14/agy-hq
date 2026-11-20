package workspace

import (
	"path/filepath"
	"runtime"
	"strings"
)

// CanonicalPath returns the absolute, cleaned, symlink-resolved form of p. When p (or a trailing part of it)
// does not exist yet, the deepest existing ancestor is resolved and the remaining segments are re-appended, so
// paths that are about to be created still canonicalize consistently. It never spawns a subprocess and returns
// "" for an empty path.
func CanonicalPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	abs = filepath.Clean(abs)
	if resolved, rErr := filepath.EvalSymlinks(abs); rErr == nil && resolved != "" {
		return filepath.Clean(resolved)
	}
	var tail []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = append(tail, filepath.Base(cur))
		if resolved, rErr := filepath.EvalSymlinks(parent); rErr == nil && resolved != "" {
			out := filepath.Clean(resolved)
			for i := len(tail) - 1; i >= 0; i-- {
				out = filepath.Join(out, tail[i])
			}
			return out
		}
		cur = parent
	}
}

// cleanAbsPath returns the absolute, cleaned form of p without touching the filesystem.
func cleanAbsPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// PathWithin reports whether path equals root or sits beneath it, comparing whole path segments so that
// "/a/b" never matches "/a/bc". Both arguments must already be cleaned absolute paths. On case-insensitive
// host OSes (macOS/Windows) comparison is case-folding so case-variant paths cannot bypass workspace leases.
func PathWithin(path, root string) bool {
	return pathWithinCase(path, root, runtime.GOOS == "darwin" || runtime.GOOS == "windows")
}

// pathCandidates returns deduplicated clean and symlink-resolved forms of cleanAbs.
// When canonicalFirst is true, the canonical path precedes the lexical clean path.
func pathCandidates(cleanAbs string, canonicalFirst bool) []string {
	if cleanAbs == "" {
		return nil
	}
	canon := CanonicalPath(cleanAbs)
	if canon == "" || canon == cleanAbs {
		return []string{cleanAbs}
	}
	if canonicalFirst {
		return []string{canon, cleanAbs}
	}
	return []string{cleanAbs, canon}
}

func vaultRootsFor(vaultDir string) []string {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	var roots []string
	for _, v := range []string{cleanAbsPath(vaultDir), CanonicalPath(vaultDir)} {
		if v == "" || v == string(filepath.Separator) {
			continue
		}
		dup := false
		for _, r := range roots {
			if r == v {
				dup = true
				break
			}
		}
		if !dup {
			roots = append(roots, v)
		}
	}
	return roots
}

func withinAny(p string, roots []string) bool {
	for _, r := range roots {
		if PathWithin(p, r) {
			return true
		}
	}
	return false
}

func rootContainsAny(root string, dirs []string) bool {
	for _, d := range dirs {
		if PathWithin(d, root) {
			return true
		}
	}
	return false
}

func pathWithinCase(path, root string, caseInsensitive bool) bool {
	if path == "" || root == "" {
		return false
	}
	if !caseInsensitive {
		if path == root {
			return true
		}
		if root == string(filepath.Separator) {
			return strings.HasPrefix(path, root)
		}
		return strings.HasPrefix(path, root+string(filepath.Separator))
	}
	if strings.EqualFold(path, root) {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	prefix := root + string(filepath.Separator)
	return len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix)
}
