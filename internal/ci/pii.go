package ci

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// PIIViolation describes a deterministic PII, Vault boundary, or credential leak detected by CheckPII.
type PIIViolation struct {
	Invariant string `json:"invariant"` // P1..P5
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
)

// CheckPII executes the 5 mathematical PII & Two-Layer Vault boundary invariants (RFC-0011 §4).
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
	checkRuntimeHome := runtimeHome != "" && len(runtimeHome) > 5 && !isAllowedHomePrefix(runtimeHome+"/") && !strings.HasPrefix(rootDir, os.TempDir())

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
			violations = append(violations, scanLineForPII(curFile, 0, added, userEmail, runtimeHome, checkRuntimeHome)...)
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
			violations = append(violations, scanLineForPII(rel, idx+1, line, userEmail, runtimeHome, checkRuntimeHome)...)
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

func scanLineForPII(relFile string, lineNum int, line, gitUserEmail, runtimeHome string, checkRuntimeHome bool) []PIIViolation {
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

	return out
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
