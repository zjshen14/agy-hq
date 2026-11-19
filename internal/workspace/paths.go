package workspace

import (
	"path/filepath"
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

// pathWithin reports whether path equals root or sits beneath it, comparing whole path segments so that
// "/a/b" never matches "/a/bc". Both arguments must already be cleaned absolute paths.
func pathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
