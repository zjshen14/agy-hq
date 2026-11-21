package redact

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// SecretPattern pairs a human-readable rule name with a compiled detection regex.
type SecretPattern struct {
	Name    string
	Pattern *regexp.Regexp
}

// Violation describes a single deterministic redaction / privacy finding.
type Violation struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

var (
	reHomeDirPath = regexp.MustCompile(`(/home/[a-zA-Z0-9._\-]+/|/Users/[a-zA-Z0-9._\-]+/|[cC]:\\[uU]sers\\[a-zA-Z0-9._\-]+\\)`)
	reSSNLiteral  = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reVaultPath   = regexp.MustCompile(`(?i)(?:^|[\s"'=(:/])(?:local/private/|private/(?:gmail|gchat|credentials|connectors|channels|delegates|runtime|taint|exec-cache|trust\.json|approval_|pii-denylist\.txt|daemon_token)|\.system_generated/)`)
	reTokenBlob   = regexp.MustCompile(`[A-Za-z0-9+/_=-]{40,}`)

	defaultSecretPatterns = []SecretPattern{
		{"OAuth Access Token", regexp.MustCompile(`\bya29\.[0-9A-Za-z_\-]{20,}\b`)},
		{"OAuth Refresh Token", regexp.MustCompile(`\b1//[0-9A-Za-z_\-]{40,}\b`)},
		{"Cloud API Key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
		{"GitHub Token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{36,}|github_pat_[A-Za-z0-9_]{50,})\b`)},
		{"Model API Key", regexp.MustCompile(`\bsk-(?:proj-|ant-)?[A-Za-z0-9_\-]{24,}\b`)},
		{"Slack Token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z\-]{10,}\b`)},
		{"Private Key PEM Block", regexp.MustCompile(`-----BEGIN (?:RSA |OPENSSH |EC |DSA )?PRIVATE KEY-----`)},
	}

	harnessTokenPatterns = []SecretPattern{
		{"Spawn Token", regexp.MustCompile(`\[hq-spawn:[0-9a-fA-F]{16,64}\]`)},
		{"Proposal ID", regexp.MustCompile(`\bprop-[0-9]{8,}-[0-9a-fA-F]{8,}\b`)},
		{"Bridge Callback Token", regexp.MustCompile(`\bbc[ar]_[A-Za-z0-9_\-]{16,}\b`)},
		{"Host IPC Env", regexp.MustCompile(`\bANTIGRAVITY_(?:CSRF_TOKEN|LS_ADDRESS|AGENTAPI_EXE|CONVERSATION_ID|PLUGIN_SHIM_DIR)\b`)},
	}
)

// DefaultSecretPatterns returns compiled credential/secret regexes.
func DefaultSecretPatterns() []SecretPattern {
	out := make([]SecretPattern, len(defaultSecretPatterns))
	copy(out, defaultSecretPatterns)
	return out
}

// HarnessTokenPatterns returns compiled internal harness control-token regexes.
func HarnessTokenPatterns() []SecretPattern {
	out := make([]SecretPattern, len(harnessTokenPatterns))
	copy(out, harnessTokenPatterns)
	return out
}

// StripInvisibleUnicode strips Unicode Cf (format / bidi-override / zero-width) runes and
// non-whitespace ASCII control characters to prevent homoglyph/zero-width filter evasion.
func StripInvisibleUnicode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			continue
		}
		if r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// LoadVaultPIIDenylist reads non-comment terms from <vault>/private/pii-denylist.txt.
func LoadVaultPIIDenylist(vaultDir string) []string {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	p := filepath.Join(vaultDir, "private", "pii-denylist.txt")
	st, err := os.Lstat(p)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// FindViolations checks text (after invisible-Unicode stripping) for secrets, host/Vault paths,
// harness tokens, SSNs, Vault pii-denylist terms, and caller-supplied sensitive terms.
func FindViolations(vaultDir, text string, extraSensitiveTerms []string) []Violation {
	clean := StripInvisibleUnicode(text)
	lower := strings.ToLower(clean)
	var out []Violation

	for _, sp := range defaultSecretPatterns {
		if sp.Pattern.MatchString(clean) {
			out = append(out, Violation{Kind: "secret", Detail: sp.Name})
		}
	}
	for _, hp := range harnessTokenPatterns {
		if hp.Pattern.MatchString(clean) {
			out = append(out, Violation{Kind: "harness_token", Detail: hp.Name})
		}
	}
	if reHomeDirPath.MatchString(clean) {
		out = append(out, Violation{Kind: "home_path", Detail: "host home directory path detected"})
	}
	if reVaultPath.MatchString(clean) {
		out = append(out, Violation{Kind: "vault_path", Detail: "protected Vault or internal log path detected"})
	}
	if strings.TrimSpace(vaultDir) != "" {
		cleanVault := filepath.ToSlash(filepath.Clean(vaultDir))
		if len(cleanVault) >= 4 && strings.Contains(filepath.ToSlash(clean), cleanVault) {
			out = append(out, Violation{Kind: "vault_path", Detail: "absolute Vault root path detected"})
		}
	}
	if reSSNLiteral.MatchString(clean) {
		out = append(out, Violation{Kind: "pii_ssn", Detail: "SSN-like pattern detected"})
	}
	for _, term := range LoadVaultPIIDenylist(vaultDir) {
		if len(term) >= 3 && strings.Contains(lower, strings.ToLower(term)) {
			out = append(out, Violation{Kind: "pii_denylist", Detail: "matched Vault pii-denylist term"})
			break
		}
	}
	for _, term := range extraSensitiveTerms {
		t := strings.TrimSpace(term)
		if len(t) >= 3 && strings.Contains(lower, strings.ToLower(t)) {
			out = append(out, Violation{Kind: "sensitive_term", Detail: "matched configured sensitive term"})
			break
		}
	}
	return out
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	counts := make(map[rune]int, 64)
	total := 0
	for _, r := range s {
		counts[r]++
		total++
	}
	var ent float64
	ft := float64(total)
	for _, c := range counts {
		p := float64(c) / ft
		ent -= p * math.Log2(p)
	}
	return ent
}

// HasHighEntropyBlob flags unbroken >=40-char base64/hex-like high-entropy blobs that do not appear
// verbatim in allowVerbatim (defense-in-depth against covert encoded exfiltration).
func HasHighEntropyBlob(text string, allowVerbatim []string) (string, bool) {
	clean := StripInvisibleUnicode(text)
	for _, m := range reTokenBlob.FindAllString(clean, -1) {
		if strings.HasPrefix(m, "http") || strings.Contains(m, "://") {
			continue
		}
		if shannonEntropy(m) < 4.3 {
			continue
		}
		allowed := false
		for _, src := range allowVerbatim {
			if src != "" && strings.Contains(src, m) {
				allowed = true
				break
			}
		}
		if !allowed {
			preview := m
			if len(preview) > 16 {
				preview = preview[:16] + "..."
			}
			return preview, true
		}
	}
	return "", false
}

// ScrubForUntrustedOutput strips invisible Unicode and replaces secrets, harness tokens,
// host home paths, and Vault paths with "[REDACTED]".
func ScrubForUntrustedOutput(vaultDir, text string) string {
	s := StripInvisibleUnicode(text)
	for _, sp := range defaultSecretPatterns {
		s = sp.Pattern.ReplaceAllString(s, "[REDACTED]")
	}
	for _, hp := range harnessTokenPatterns {
		s = hp.Pattern.ReplaceAllString(s, "[REDACTED]")
	}
	s = reHomeDirPath.ReplaceAllString(s, "[REDACTED_PATH]/")
	s = reVaultPath.ReplaceAllString(s, " [REDACTED_VAULT]")
	if strings.TrimSpace(vaultDir) != "" {
		cleanVault := filepath.Clean(vaultDir)
		s = strings.ReplaceAll(s, cleanVault, "[REDACTED_VAULT]")
		slashVault := filepath.ToSlash(cleanVault)
		if slashVault != cleanVault {
			s = strings.ReplaceAll(s, slashVault, "[REDACTED_VAULT]")
		}
	}
	for _, term := range LoadVaultPIIDenylist(vaultDir) {
		if len(term) >= 3 {
			reTerm := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(term))
			s = reTerm.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}
