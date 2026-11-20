package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
)

type antigravityProjectConfig struct {
	ID               string `json:"id"`
	IsWorkspaceOnly  bool   `json:"isWorkspaceOnly"`
	ProjectResources struct {
		Resources []struct {
			FolderURI string `json:"folderUri"`
			GitFolder *struct {
				FolderURI string `json:"folderUri"`
			} `json:"gitFolder"`
			WorkspaceFolder *struct {
				FolderURI string `json:"folderUri"`
			} `json:"workspaceFolder"`
		} `json:"resources"`
	} `json:"projectResources"`
}

func resolveProjectsRegistryDir(vaultDir string) string {
	roots := hostpaths.Resolve(vaultDir)
	if strings.TrimSpace(roots.ConfigDir) == "" {
		return ""
	}
	// Hermetic test guard: never read real workstation ~/.gemini/config/projects during temp-vault tests
	// unless ANTIGRAVITY_CONFIG_DIR / HQ_CONFIG_DIR / HQ_HOME_DIR explicitly points into a temp directory.
	if hostpaths.IsTempPath(vaultDir) && !hostpaths.IsTempPath(roots.ConfigDir) {
		return ""
	}
	dir := filepath.Join(roots.ConfigDir, "projects")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

func normalizeWorkspaceFolderPath(raw string) string {
	val := strings.TrimSpace(raw)
	if val == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(val), "file://") {
		if u, err := url.Parse(val); err == nil && u.Path != "" {
			if unesc, uErr := url.PathUnescape(u.Path); uErr == nil && unesc != "" {
				val = unesc
			} else {
				val = u.Path
			}
		} else {
			val = strings.TrimPrefix(val, "file://")
		}
	}
	if abs, err := filepath.Abs(val); err == nil {
		val = abs
	}
	val = filepath.Clean(val)
	if eval, err := filepath.EvalSymlinks(val); err == nil && eval != "" {
		val = filepath.Clean(eval)
	}
	return val
}

type loadedProjectEntry struct {
	id              string
	folders         []string
	isWorkspaceOnly bool
}

type cachedProjectsSnapshot struct {
	dir         string
	fingerprint string
	list        []loadedProjectEntry
	byID        map[string][]string
}

var (
	projectsCacheMu sync.Mutex
	projectsCache   cachedProjectsSnapshot
)

func parseProjectFolders(cfg antigravityProjectConfig) []string {
	var folders []string
	seenFolder := make(map[string]bool)
	addFolder := func(rawURI string) {
		if norm := normalizeWorkspaceFolderPath(rawURI); norm != "" && !seenFolder[norm] {
			seenFolder[norm] = true
			folders = append(folders, norm)
		}
	}
	for _, res := range cfg.ProjectResources.Resources {
		addFolder(res.FolderURI)
		if res.GitFolder != nil {
			addFolder(res.GitFolder.FolderURI)
		}
		if res.WorkspaceFolder != nil {
			addFolder(res.WorkspaceFolder.FolderURI)
		}
	}
	return folders
}

// loadSingleAntigravityProject performs an O(1) lookup of `<dir>/<projectID>.json` without scanning
// sibling project files (which may point to slow network/FUSE mounts).
func loadSingleAntigravityProject(dir, projectID string) (loadedProjectEntry, bool) {
	projectID = strings.TrimSpace(projectID)
	if dir == "" || projectID == "" || projectID == "." || projectID == ".." || strings.ContainsAny(projectID, `/\`) {
		return loadedProjectEntry{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, projectID+".json"))
	if err != nil {
		return loadedProjectEntry{}, false
	}
	var cfg antigravityProjectConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return loadedProjectEntry{}, false
	}
	id := strings.TrimSpace(cfg.ID)
	if id == "" {
		id = projectID
	}
	if id != projectID {
		return loadedProjectEntry{}, false
	}
	return loadedProjectEntry{
		id:              id,
		folders:         parseProjectFolders(cfg),
		isWorkspaceOnly: cfg.IsWorkspaceOnly,
	}, true
}

func loadAntigravityProjects(vaultDir string) ([]loadedProjectEntry, map[string][]string, bool) {
	dir := resolveProjectsRegistryDir(vaultDir)
	if dir == "" {
		return nil, nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, false
	}
	var names []string
	hasher := sha256.New()
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, nil, false
	}
	sort.Strings(names)
	infoByName := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		infoByName[e.Name()] = e
	}
	for _, name := range names {
		if de, ok := infoByName[name]; ok {
			if fi, iErr := de.Info(); iErr == nil {
				_, _ = fmt.Fprintf(hasher, "%s:%d:%d\n", name, fi.Size(), fi.ModTime().UnixNano())
				continue
			}
		}
		_, _ = fmt.Fprintf(hasher, "%s\n", name)
	}
	fp := hex.EncodeToString(hasher.Sum(nil))

	projectsCacheMu.Lock()
	if projectsCache.dir == dir && projectsCache.fingerprint == fp && len(projectsCache.list) > 0 {
		cachedList := projectsCache.list
		cachedByID := projectsCache.byID
		projectsCacheMu.Unlock()
		return cachedList, cachedByID, true
	}
	projectsCacheMu.Unlock()

	byID := make(map[string][]string, len(names))
	var list []loadedProjectEntry
	for _, name := range names {
		raw, rErr := os.ReadFile(filepath.Join(dir, name))
		if rErr != nil {
			continue
		}
		var cfg antigravityProjectConfig
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		id := strings.TrimSpace(cfg.ID)
		if id == "" {
			id = strings.TrimSuffix(name, filepath.Ext(name))
		}
		if id == "" {
			continue
		}
		folders := parseProjectFolders(cfg)
		byID[id] = folders
		list = append(list, loadedProjectEntry{
			id:              id,
			folders:         folders,
			isWorkspaceOnly: cfg.IsWorkspaceOnly,
		})
	}
	if len(list) == 0 {
		return nil, nil, false
	}

	projectsCacheMu.Lock()
	projectsCache = cachedProjectsSnapshot{
		dir:         dir,
		fingerprint: fp,
		list:        list,
		byID:        byID,
	}
	projectsCacheMu.Unlock()

	return list, byID, true
}

func matchProjectIDForFolder(list []loadedProjectEntry, normWorkspace string) string {
	if normWorkspace == "" {
		return ""
	}
	var fallback string
	for _, p := range list {
		for _, f := range p.folders {
			if f == normWorkspace {
				if !p.isWorkspaceOnly {
					return p.id
				}
				if fallback == "" {
					fallback = p.id
				}
			}
		}
	}
	return fallback
}

func workspaceMatchesProjectFolders(vaultDir, workspaceDir, normWS string, folders []string, allowWorktreePrimary bool) bool {
	if slices.Contains(folders, normWS) {
		return true
	}
	if allowWorktreePrimary && IsManagedWorktreeDir(vaultDir, workspaceDir) {
		if primary := normalizeWorkspaceFolderPath(primaryVaultWorkspaceDir(vaultDir)); primary != "" {
			return slices.Contains(folders, primary)
		}
	}
	return false
}

// ValidateProjectMatchesWorkspace checks whether projectID maps to workspaceDir in Antigravity's
// `<ConfigDir>/projects/*.json` registry. When no registry exists on disk (e.g. hermetic tests),
// registryAvailable is false and valid defaults to projectID != "".
func ValidateProjectMatchesWorkspace(vaultDir, projectID, workspaceDir string) (valid bool, registryAvailable bool) {
	projectID = strings.TrimSpace(projectID)
	normWS := normalizeWorkspaceFolderPath(workspaceDir)
	if dir := resolveProjectsRegistryDir(vaultDir); dir != "" && projectID != "" && normWS != "" {
		if entry, ok := loadSingleAntigravityProject(dir, projectID); ok && slices.Contains(entry.folders, normWS) {
			return true, true
		}
	}
	_, byID, avail := loadAntigravityProjects(vaultDir)
	if !avail {
		return projectID != "", false
	}
	if projectID == "" || normWS == "" {
		return false, true
	}
	return slices.Contains(byID[projectID], normWS), true
}

// ResolveVerifiedWorkspaceProjectID validates candidateProjectID against workspaceDir using `<ConfigDir>/projects/*.json`,
// repairing mismatched/poisoned projectIds or resolving an empty projectId from the registry when available.
func ResolveVerifiedWorkspaceProjectID(vaultDir, workspaceDir, candidateProjectID string) (projectID string, registryAvailable bool, repaired bool) {
	cand := strings.TrimSpace(candidateProjectID)
	normWS := normalizeWorkspaceFolderPath(workspaceDir)
	if dir := resolveProjectsRegistryDir(vaultDir); dir != "" && cand != "" && normWS != "" {
		if entry, ok := loadSingleAntigravityProject(dir, cand); ok {
			if workspaceMatchesProjectFolders(vaultDir, workspaceDir, normWS, entry.folders, true) {
				return cand, true, false
			}
		}
	}
	list, byID, avail := loadAntigravityProjects(vaultDir)
	if !avail || normWS == "" {
		return cand, false, false
	}
	if cand != "" && workspaceMatchesProjectFolders(vaultDir, workspaceDir, normWS, byID[cand], true) {
		return cand, true, false
	}
	resolved := matchProjectIDForFolder(list, normWS)
	if resolved == "" && IsManagedWorktreeDir(vaultDir, workspaceDir) {
		if primary := normalizeWorkspaceFolderPath(primaryVaultWorkspaceDir(vaultDir)); primary != "" {
			resolved = matchProjectIDForFolder(list, primary)
		}
	}
	return resolved, true, resolved != cand
}
