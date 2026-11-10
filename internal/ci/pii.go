package ci

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zjshen14/agy-hq/internal/vault"
)

// PIIViolation describes a deterministic PII, Vault boundary, or credential leak detected by CheckPII.
type PIIViolation struct {
	Invariant string `json:"invariant"` // P1..P6
	File      string `json:"file"`
	Line      int    `json:"line,omitempty"`
	Detail    string `json:"detail"`
}

func (v PIIViolation) String() string {
	if v.Line > 0 {
		return fmt.Sprintf("[%s] %s:%d — %s", v.Invariant, v.File, v.Line, v.Detail)
	}
	return fmt.Sprintf("[%s] %s — %s", v.Invariant, v.File, v.Detail)
}

var (
	reForbiddenPath = regexp.MustCompile(`(?i)^(?:local|vault|bin)/|^hq$|(?:^|/)\.env(?:\..+)?$|\.(?:pem|key|p12|pfx|secret|credentials)$|(?:^|/)client_secret.*\.json$|(?:^|/)token.*\.json$`)
	reHomeDirPath   = regexp.MustCompile(`(/home/[a-zA-Z0-9._\-]+/|/Users/[a-zA-Z0-9._\-]+/|[cC]:\\[uU]sers\\[a-zA-Z0-9._\-]+\\)`)
	reEmailLiteral  = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reSSNLiteral    = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reGChatSpaceID  = regexp.MustCompile(`(?:^|[^A-Za-z0-9_\-])(spaces/[A-Za-z0-9_\-]{8,})`)

	secretPatterns = []struct {
		Name    string
		Pattern *regexp.Regexp
	}{
		{"Google OAuth Access Token", regexp.MustCompile(`\bya29\.[0-9A-Za-z_\-]{20,}\b`)},
		{"Google OAuth Refresh Token", regexp.MustCompile(`\b1//[0-9A-Za-z_\-]{40,}\b`)},
		{"Google API Key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
		{"GitHub Token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{36,}|github_pat_[A-Za-z0-9_]{50,})\b`)},
		{"OpenAI/Anthropic API Key", regexp.MustCompile(`\bsk-(?:proj-|ant-)?[A-Za-z0-9_\-]{24,}\b`)},
		{"Slack Token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z\-]{10,}\b`)},
		{"Private Key PEM Block", regexp.MustCompile(`-----BEGIN (?:RSA |OPENSSH |EC |DSA )?PRIVATE KEY-----`)},
	}

	allowedHomePrefixes = []string{
		"/home/runner/",
		"/home/user/",
		"/home/ubuntu/",
		"/users/username/",
		"/users/you/",
		"/users/user/",
		"/users/runner/",
		`c:\users\username\`,
		`c:\users\user\`,
	}

	// AllowedEmailDomains lists synthetic/placeholder domains permitted in tracked code, docs, and tests.
	AllowedEmailDomains = []string{
		"@example.com",
		"@example.org",
		"@example.net",
		"@partner.io",
		"@localhost",
		"@agy-hq.local",
		"@company.com",
		"@domain.com",
		"@yourdomain.com",
		"@acme.com",
		"@evil.com",
		"@fidelity.com",
		"@unknown-domain.example",
	}

	// AllowedEmailAddresses lists specific synthetic/system addresses permitted in tracked files.
	AllowedEmailAddresses = map[string]bool{
		"user@gmail.com":          true,
		"you@gmail.com":           true,
		"me@gmail.com":            true,
		"alice@gmail.com":         true,
		"bob@gmail.com":           true,
		"primary@gmail.com":       true,
		"secondary@gmail.com":     true,
		"sender@gmail.com":        true,
		"recipient@gmail.com":     true,
		"chat-noreply@google.com": true,
		"noreply@github.com":      true,
	}

	allowedGChatSpacePrefixes = []string{
		"spaces/AAAA",
		"spaces/AAQA_SYNTH",
		"spaces/SPACE",
		"spaces/TEST",
		"spaces/DEMO",
		"spaces/SAMPLE",
		"spaces/EVIL",
		"spaces/cdp-",
		"spaces/gmail-chat-",
	}

	allowedGChatSpaceExact = map[string]bool{
		"spaces/pinned-item":     true,
		"spaces/direct-messages": true,
		"spaces/shortcut":        true,
		"spaces/browse-spaces":   true,
	}
)

// CheckPII executes the 6 mathematical PII & Two-Layer Vault boundary invariants (RFC-0011 §4 & RFC-0013 §2).
// If stagedOnly is true, it inspects staged index changes (`git diff --cached`); otherwise it audits
// the entire tracked repository tree (`git ls-files`).
func CheckPII(rootDir string, stagedOnly bool) ([]PIIViolation, error) {
	var violations []PIIViolation

	// Invariant P2: Verify root .gitignore protects /local/ and /vault/, and no gitlink submodule exists on local/
	if !stagedOnly {
		gitignoreBytes, err := os.ReadFile(filepath.Join(rootDir, ".gitignore"))
		if err != nil {
			violations = append(violations, PIIViolation{
				Invariant: "P2-VaultIgnore",
				File:      ".gitignore",
				Detail:    "root .gitignore file is missing",
			})
		} else {
			gi := string(gitignoreBytes)
			if !strings.Contains(gi, "/local/") || !strings.Contains(gi, "/vault/") {
				violations = append(violations, PIIViolation{
					Invariant: "P2-VaultIgnore",
					File:      ".gitignore",
					Detail:    "root .gitignore must explicitly ignore both /local/ and /vault/",
				})
			}
		}

		stageOut, err := gitOutput(rootDir, "ls-files", "--stage")
		if err == nil {
			for _, line := range strings.Split(stageOut, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "160000 ") {
					fields := strings.Fields(line)
					path := fields[len(fields)-1]
					if path == "local" || strings.HasPrefix(path, "local/") || path == "vault" || strings.HasPrefix(path, "vault/") {
						violations = append(violations, PIIViolation{
							Invariant: "P2-SubmoduleLeak",
							File:      path,
							Detail:    "private Vault directory is registered as a gitlink/submodule (mode 160000)",
						})
					}
				}
			}
		}
	}

	userEmail := strings.ToLower(strings.TrimSpace(gitConfigEmail(rootDir)))
	runtimeHome := strings.TrimSpace(os.Getenv("HOME"))
	checkRuntimeHome := runtimeHome != "" && len(runtimeHome) > 5 && !isAllowedHomePrefix(runtimeHome+"/") && !vault.IsTempPath(rootDir)
	denylist := loadLocalPIIDenylist(rootDir)

	if stagedOnly {
		namesOut, err := gitOutput(rootDir, "diff", "--cached", "--name-only", "--diff-filter=ACMR")
		if err != nil {
			return violations, fmt.Errorf("git diff --cached --name-only: %w", err)
		}
		for _, rel := range strings.Split(namesOut, "\n") {
			rel = strings.TrimSpace(filepath.ToSlash(rel))
			if rel == "" {
				continue
			}
			if v, bad := checkForbiddenPath(rel); bad {
				violations = append(violations, v)
			}
		}

		diffOut, err := gitOutput(rootDir, "diff", "--cached", "-U0")
		if err != nil {
			return violations, fmt.Errorf("git diff --cached: %w", err)
		}
		curFile := ""
		for _, line := range strings.Split(diffOut, "\n") {
			if strings.HasPrefix(line, "+++ b/") {
				curFile = strings.TrimSpace(line[6:])
				continue
			}
			if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
				continue
			}
			added := line[1:]
			violations = append(violations, scanLineForPII(curFile, 0, added, userEmail, runtimeHome, checkRuntimeHome, denylist)...)
		}
		return violations, nil
	}

	// Full tracked tree scan via git ls-files
	filesOut, err := gitOutput(rootDir, "ls-files")
	if err != nil {
		return violations, fmt.Errorf("git ls-files: %w", err)
	}
	for _, rel := range strings.Split(filesOut, "\n") {
		rel = strings.TrimSpace(filepath.ToSlash(rel))
		if rel == "" {
			continue
		}
		if v, bad := checkForbiddenPath(rel); bad {
			violations = append(violations, v)
			continue
		}
		absPath := filepath.Join(rootDir, filepath.FromSlash(rel))
		data, err := os.ReadFile(absPath)
		if err != nil || isBinaryContent(data) {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for idx, line := range lines {
			violations = append(violations, scanLineForPII(rel, idx+1, line, userEmail, runtimeHome, checkRuntimeHome, denylist)...)
		}
	}

	return violations, nil
}

func checkForbiddenPath(relSlash string) (PIIViolation, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(relSlash), "./")
	if strings.HasSuffix(clean, ".env.example") {
		return PIIViolation{}, false
	}
	if reForbiddenPath.MatchString(clean) {
		return PIIViolation{
			Invariant: "P1-ForbiddenPath",
			File:      clean,
			Detail:    "tracked path violates Two-Layer Vault / binary / credential boundary",
		}, true
	}
	return PIIViolation{}, false
}

func scanLineForPII(relFile string, lineNum int, line, gitUserEmail, runtimeHome string, checkRuntimeHome bool, denylist []string) []PIIViolation {
	var out []PIIViolation

	// Invariant P3: Workstation Home Path Freedom
	if checkRuntimeHome && strings.Contains(line, runtimeHome) {
		out = append(out, PIIViolation{
			Invariant: "P3-WorkstationPath",
			File:      relFile,
			Line:      lineNum,
			Detail:    fmt.Sprintf("contains local HOME directory path (%s)", runtimeHome),
		})
	}
	if strings.Contains(line, "/home/") || strings.Contains(line, "/Users/") || strings.Contains(line, `\Users\`) || strings.Contains(line, `\users\`) {
		for _, m := range reHomeDirPath.FindAllString(line, -1) {
			if !isAllowedHomePrefix(m) {
				out = append(out, PIIViolation{
					Invariant: "P3-WorkstationPath",
					File:      relFile,
					Line:      lineNum,
					Detail:    fmt.Sprintf("contains non-synthetic workstation home path %q", m),
				})
			}
		}
	}

	// Invariant P4: High-Precision Secret & Credential Freedom
	if hasSecretCandidate(line) {
		for _, sp := range secretPatterns {
			if sp.Pattern.MatchString(line) {
				// Skip regex definition lines in pii.go/ci_test.go
				if strings.HasSuffix(relFile, "internal/ci/pii.go") && strings.Contains(line, "regexp.MustCompile") {
					continue
				}
				out = append(out, PIIViolation{
					Invariant: "P4-SecretPattern",
					File:      relFile,
					Line:      lineNum,
					Detail:    fmt.Sprintf("matched %s pattern", sp.Name),
				})
			}
		}
	}
	if strings.Contains(line, "-") {
		for _, ssn := range reSSNLiteral.FindAllString(line, -1) {
			if ssn == "123-45-6789" {
				continue
			}
			out = append(out, PIIViolation{
				Invariant: "P4-SSNPattern",
				File:      relFile,
				Line:      lineNum,
				Detail:    "matched SSN pattern (\\d{3}-\\d{2}-\\d{4})",
			})
		}
	}

	// Invariant P5: Strict Email Domain & Address Allowlist
	if strings.Contains(line, "@") {
		for _, email := range reEmailLiteral.FindAllString(line, -1) {
			lower := strings.ToLower(email)
			if gitUserEmail != "" && lower == gitUserEmail && !IsAllowedSyntheticEmail(lower) {
				out = append(out, PIIViolation{
					Invariant: "P5-PersonalEmail",
					File:      relFile,
					Line:      lineNum,
					Detail:    fmt.Sprintf("contains git user.email (%s)", email),
				})
				continue
			}
			if !IsAllowedSyntheticEmail(lower) {
				out = append(out, PIIViolation{
					Invariant: "P5-EmailAllowlist",
					File:      relFile,
					Line:      lineNum,
					Detail:    fmt.Sprintf("email %q is not in the synthetic test/doc allowlist", email),
				})
			}
		}
	}

	// Invariant P6: Contextual & Relational PII Freedom (Google Chat Space IDs & Local Vault Denylist)
	if strings.Contains(line, "spaces/") {
		if !(strings.HasSuffix(relFile, "internal/ci/pii.go") && strings.Contains(line, "regexp.MustCompile")) {
			for _, sub := range reGChatSpaceID.FindAllStringSubmatch(line, -1) {
				if len(sub) > 1 && !IsAllowedSyntheticGChatSpaceID(sub[1]) {
					out = append(out, PIIViolation{
						Invariant: "P6-ContextualPII",
						File:      relFile,
						Line:      lineNum,
						Detail:    fmt.Sprintf("non-synthetic Google Chat space ID %q (use synthetic prefix spaces/AAAA_... or spaces/TEST_...)", sub[1]),
					})
				}
			}
		}
	}
	if len(denylist) > 0 {
		lowerLine := strings.ToLower(line)
		for _, term := range denylist {
			if strings.Contains(lowerLine, term) {
				prefix := term
				if len(prefix) > 2 {
					prefix = prefix[:2]
				}
				out = append(out, PIIViolation{
					Invariant: "P6-ContextualPII",
					File:      relFile,
					Line:      lineNum,
					Detail:    fmt.Sprintf("matched user-configured private term from local/private/pii-denylist.txt (%s***)", prefix),
				})
			}
		}
	}

	return out
}

// IsAllowedSyntheticGChatSpaceID returns true if spaceLit uses an approved synthetic prefix or DOM route constant.
func IsAllowedSyntheticGChatSpaceID(spaceLit string) bool {
	s := strings.TrimSpace(spaceLit)
	if allowedGChatSpaceExact[s] {
		return true
	}
	for _, pref := range allowedGChatSpacePrefixes {
		if strings.HasPrefix(s, pref) {
			return true
		}
	}
	return false
}

func loadLocalPIIDenylist(rootDir string) []string {
	candidates := []string{
		filepath.Join(rootDir, "local", "private", "pii-denylist.txt"),
	}
	if envVault := strings.TrimSpace(os.Getenv("HQ_VAULT_DIR")); envVault != "" {
		candidates = append(candidates, filepath.Join(envVault, "private", "pii-denylist.txt"))
	}
	seen := make(map[string]bool)
	var terms []string
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") || len(line) < 3 {
				continue
			}
			lower := strings.ToLower(line)
			if !seen[lower] {
				seen[lower] = true
				terms = append(terms, lower)
			}
		}
	}
	return terms
}

// CheckPIIHistory audits the entire git commit history (`git log --all -p`), commit author metadata,
// and local branch refs for leaked PII, forbidden Vault paths, credentials, or stray test branches.
func CheckPIIHistory(rootDir string) ([]PIIViolation, error) {
	var violations []PIIViolation

	// 1. Check local refs/heads/ for stray test branches leaked from subprocess git calls
	if refsOut, err := gitOutput(rootDir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"); err == nil {
		for _, br := range strings.Split(refsOut, "\n") {
			br = strings.TrimSpace(br)
			if strings.HasPrefix(br, "hq/sample-repo-") {
				violations = append(violations, PIIViolation{
					Invariant: "P6-HistoryBranch",
					File:      "refs/heads/" + br,
					Detail:    "stray synthetic test branch found in repository refs",
				})
			}
		}
	}

	// 2. Check commit metadata for private Vault commit bleed (hq-vault@localhost)
	if metaOut, err := gitOutput(rootDir, "log", "--all", "--format=%H|%ae|%ce"); err == nil {
		for _, line := range strings.Split(metaOut, "\n") {
			parts := strings.Split(strings.TrimSpace(line), "|")
			if len(parts) < 3 {
				continue
			}
			sha := parts[0]
			shortSHA := sha
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}
			ae := strings.ToLower(strings.TrimSpace(parts[1]))
			ce := strings.ToLower(strings.TrimSpace(parts[2]))
			if ae == "hq-vault@localhost" || ce == "hq-vault@localhost" {
				violations = append(violations, PIIViolation{
					Invariant: "P1-VaultCommitBleed",
					File:      "commit:" + shortSHA,
					Detail:    "private Vault commit (hq-vault@localhost) leaked into root repository history",
				})
			}
		}
	}

	// 3. Scan full historical commit diffs (`git log --all -p --no-color -U0`)
	logOut, err := gitOutput(rootDir, "log", "--all", "-p", "--no-color", "-U0")
	if err != nil {
		return violations, fmt.Errorf("git log --all -p: %w", err)
	}

	userEmail := strings.ToLower(strings.TrimSpace(gitConfigEmail(rootDir)))
	runtimeHome := strings.TrimSpace(os.Getenv("HOME"))
	checkRuntimeHome := runtimeHome != "" && len(runtimeHome) > 5 && !isAllowedHomePrefix(runtimeHome+"/") && !vault.IsTempPath(rootDir)
	denylist := loadLocalPIIDenylist(rootDir)

	curSHA := ""
	curFile := ""
	checkedCommitFile := make(map[string]bool)

	for _, line := range strings.Split(logOut, "\n") {
		if strings.HasPrefix(line, "commit ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				curSHA = fields[1]
				if len(curSHA) > 7 {
					curSHA = curSHA[:7]
				}
			}
			curFile = ""
			continue
		}
		if strings.HasPrefix(line, "+++ b/") {
			curFile = strings.TrimSpace(line[6:])
			key := curSHA + ":" + curFile
			if !checkedCommitFile[key] {
				checkedCommitFile[key] = true
				if v, bad := checkForbiddenPath(curFile); bad {
					v.File = curSHA + ":" + curFile
					violations = append(violations, v)
				}
			}
			continue
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		fileRef := curFile
		if curSHA != "" && curFile != "" {
			fileRef = curSHA + ":" + curFile
		} else if curSHA != "" {
			fileRef = "commit:" + curSHA
		}
		added := line[1:]
		violations = append(violations, scanLineForPII(fileRef, 0, added, userEmail, runtimeHome, checkRuntimeHome, denylist)...)
	}

	return violations, nil
}

// ReplacementRule defines a single deterministic history-scrubbing text replacement (`old==>new` or `regex:pat==>new`).
type ReplacementRule struct {
	RawPattern  string
	Replacement string
	Regex       *regexp.Regexp
}

// HistoryScrubReport summarizes dry-run or applied history scrubbing results.
type HistoryScrubReport struct {
	DryRun              bool     `json:"dryRun"`
	RuleCount           int      `json:"ruleCount"`
	MatchedCommits      int      `json:"matchedCommits"`
	MatchedReplacements int      `json:"matchedReplacements"`
	SampleCommits       []string `json:"sampleCommits,omitempty"`
}

// ParseReplacementRules parses a `git-filter-repo` compatible replacement map file (`literal==>replacement` or `regex:pat==>replacement`).
func ParseReplacementRules(replacementsPath string) ([]ReplacementRule, error) {
	data, err := os.ReadFile(replacementsPath)
	if err != nil {
		return nil, fmt.Errorf("read replacements file %s: %w", replacementsPath, err)
	}
	var rules []ReplacementRule
	for lineIdx, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(raw, "==>")
		if idx < 0 {
			return nil, fmt.Errorf("%s:%d: expected 'pattern==>replacement' syntax", replacementsPath, lineIdx+1)
		}
		lhs := strings.TrimLeft(raw[:idx], " \t")
		rhs := strings.TrimRight(raw[idx+3:], "\r\n")
		if lhs == "" {
			return nil, fmt.Errorf("%s:%d: empty left-hand pattern", replacementsPath, lineIdx+1)
		}
		if strings.HasPrefix(lhs, "regex:") {
			pat := strings.TrimPrefix(lhs, "regex:")
			re, rErr := regexp.Compile(pat)
			if rErr != nil {
				return nil, fmt.Errorf("%s:%d: invalid regex %q: %w", replacementsPath, lineIdx+1, pat, rErr)
			}
			rules = append(rules, ReplacementRule{RawPattern: lhs, Replacement: rhs, Regex: re})
		} else {
			rules = append(rules, ReplacementRule{RawPattern: lhs, Replacement: rhs})
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("no replacement rules found in %s", replacementsPath)
	}
	return rules, nil
}

// ApplyReplacementRulesToText applies replacement rules in order and returns the transformed text and match count.
func ApplyReplacementRulesToText(text string, rules []ReplacementRule) (string, int) {
	cur := text
	totalHits := 0
	for _, r := range rules {
		if r.Regex != nil {
			matches := r.Regex.FindAllStringIndex(cur, -1)
			if len(matches) > 0 {
				totalHits += len(matches)
				cur = r.Regex.ReplaceAllString(cur, r.Replacement)
			}
		} else {
			cnt := strings.Count(cur, r.RawPattern)
			if cnt > 0 {
				totalHits += cnt
				cur = strings.ReplaceAll(cur, r.RawPattern, r.Replacement)
			}
		}
	}
	return cur, totalHits
}

// ApplyReplacementRulesToTree walks treeDir (skipping .git and binary files) and applies replacement rules in place.
func ApplyReplacementRulesToTree(treeDir, replacementsPath string) (int, error) {
	rules, err := ParseReplacementRules(replacementsPath)
	if err != nil {
		return 0, err
	}
	modifiedFiles := 0
	walkErr := filepath.WalkDir(treeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil || isBinaryContent(data) {
			return nil
		}
		updated, hits := ApplyReplacementRulesToText(string(data), rules)
		if hits > 0 && updated != string(data) {
			info, sErr := d.Info()
			perm := os.FileMode(0o644)
			if sErr == nil {
				perm = info.Mode().Perm()
			}
			if wErr := os.WriteFile(path, []byte(updated), perm); wErr != nil {
				return wErr
			}
			modifiedFiles++
		}
		return nil
	})
	return modifiedFiles, walkErr
}

// ScrubGitHistory previews (`apply=false`) or rewrites (`apply=true`) repository commit history using
// deterministic replacement rules (`literal==>replacement` or `regex:pat==>replacement`).
func ScrubGitHistory(rootDir, replacementsPath string, apply bool) (*HistoryScrubReport, error) {
	absRep, err := filepath.Abs(replacementsPath)
	if err != nil {
		return nil, err
	}
	rules, err := ParseReplacementRules(absRep)
	if err != nil {
		return nil, err
	}

	logOut, err := gitOutput(rootDir, "log", "--all", "-p", "--no-color", "-U0")
	if err != nil {
		return nil, fmt.Errorf("git log --all -p: %w", err)
	}

	rep := &HistoryScrubReport{
		DryRun:    !apply,
		RuleCount: len(rules),
	}
	curSHA := ""
	commitMatched := make(map[string]bool)
	for _, line := range strings.Split(logOut, "\n") {
		if strings.HasPrefix(line, "commit ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				curSHA = fields[1]
				if len(curSHA) > 7 {
					curSHA = curSHA[:7]
				}
			}
			continue
		}
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "@@ ") {
			continue
		}
		_, hits := ApplyReplacementRulesToText(line, rules)
		if hits > 0 {
			rep.MatchedReplacements += hits
			if curSHA != "" && !commitMatched[curSHA] {
				commitMatched[curSHA] = true
				rep.MatchedCommits++
				if len(rep.SampleCommits) < 12 {
					rep.SampleCommits = append(rep.SampleCommits, curSHA)
				}
			}
		}
	}

	if !apply {
		return rep, nil
	}

	gitStat, sErr := os.Stat(filepath.Join(rootDir, ".git"))
	if sErr != nil || !gitStat.IsDir() {
		return rep, fmt.Errorf("refusing --apply inside a linked git worktree (.git is not a standalone directory); run from the primary repository checkout")
	}
	statusOut, stErr := gitOutput(rootDir, "status", "--porcelain")
	if stErr != nil {
		return rep, fmt.Errorf("git status check failed: %w", stErr)
	}
	if strings.TrimSpace(statusOut) != "" {
		return rep, fmt.Errorf("refusing --apply with dirty working tree; commit or stash changes first")
	}

	exePath := strings.TrimSpace(os.Getenv("HQ_BIN_PATH"))
	if exePath == "" {
		var eErr error
		exePath, eErr = os.Executable()
		if eErr != nil {
			return rep, fmt.Errorf("resolve executable path: %w", eErr)
		}
	}
	treeFilter := fmt.Sprintf("%q ci apply-replacements --dir=. --file=%q", exePath, absRep)
	msgFilter := fmt.Sprintf("%q ci apply-replacements-stdin --file=%q", exePath, absRep)

	fbCmd := vault.SanitizedGitCommand(rootDir, "filter-branch", "-f", "--tree-filter", treeFilter, "--msg-filter", msgFilter, "--", "--all")
	fbCmd.Env = append(fbCmd.Env, "FILTER_BRANCH_SQUELCH_WARNING=1")
	if out, fErr := fbCmd.CombinedOutput(); fErr != nil {
		return rep, fmt.Errorf("git filter-branch failed: %w (%s)", fErr, strings.TrimSpace(string(out)))
	}

	// Purge refs/original/* backup refs, expire reflogs, and prune unreachable objects
	if origRefs, rErr := gitOutput(rootDir, "for-each-ref", "--format=%(refname)", "refs/original/"); rErr == nil {
		for _, ref := range strings.Split(origRefs, "\n") {
			ref = strings.TrimSpace(ref)
			if ref != "" {
				_, _ = gitOutput(rootDir, "update-ref", "-d", ref)
			}
		}
	}
	_, _ = gitOutput(rootDir, "reflog", "expire", "--expire=now", "--all")
	_, _ = gitOutput(rootDir, "gc", "--prune=now", "--quiet")

	return rep, nil
}

// IsAllowedSyntheticEmail returns true if lowerEmail belongs to AllowedEmailAddresses or AllowedEmailDomains.
func IsAllowedSyntheticEmail(lowerEmail string) bool {
	lower := strings.ToLower(strings.TrimSpace(lowerEmail))
	if AllowedEmailAddresses[lower] {
		return true
	}
	for _, dom := range AllowedEmailDomains {
		if strings.HasSuffix(lower, dom) {
			return true
		}
	}
	return false
}

func hasSecretCandidate(line string) bool {
	return strings.Contains(line, "ya29.") ||
		strings.Contains(line, "1//") ||
		strings.Contains(line, "AIza") ||
		strings.Contains(line, "ghp_") ||
		strings.Contains(line, "gho_") ||
		strings.Contains(line, "ghu_") ||
		strings.Contains(line, "ghs_") ||
		strings.Contains(line, "ghr_") ||
		strings.Contains(line, "github_pat_") ||
		strings.Contains(line, "sk-") ||
		strings.Contains(line, "xox") ||
		strings.Contains(line, "PRIVATE KEY")
}

func isAllowedHomePrefix(p string) bool {
	lower := strings.ToLower(p)
	for _, allow := range allowedHomePrefixes {
		if strings.HasPrefix(lower, allow) {
			return true
		}
	}
	return false
}

func isBinaryContent(data []byte) bool {
	sample := data
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	return bytes.IndexByte(sample, 0) >= 0
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = vault.SanitizedGitEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gitConfigEmail(dir string) string {
	out, err := gitOutput(dir, "config", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
