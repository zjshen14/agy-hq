package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/zjshen14/agy-hq/internal/store"
)

// ProfileData represents the private on-demand Tier-2 household autofill profile
// stored at <vault>/private/profile.md (0600 permissions, 0 SessionStart tokens,
// excluded from memory search, tainted sessions, and unattended [Auto] workers).
type ProfileData struct {
	UpdatedAt string                       `json:"updatedAt,omitempty"`
	Sections  map[string]map[string]string `json:"sections"`
	Path      string                       `json:"path,omitempty"`
}

// ProfilePath returns the canonical path to <vault>/private/profile.md.
func ProfilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "profile.md")
}

var (
	reProfileKeyClean  = regexp.MustCompile(`[^a-z0-9_]+`)
	reProfileSSN       = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reProfilePEM       = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	reProfileAPIToken  = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{16,}|gh[pou]_[A-Za-z0-9]{20,}|AIza[0-9A-Za-z_\-]{20,}|xox[baprs]-[0-9A-Za-z\-]{16,}|AKIA[0-9A-Z]{16})\b`)
	reProfileDigitSpan = regexp.MustCompile(`(?:\d[\s\-]?){13,19}`)
)

var blockedProfileKeyTokens = []string{
	"password",
	"passwd",
	"passphrase",
	"secret",
	"token",
	"api_key",
	"apikey",
	"private_key",
	"ssn",
	"social_security",
	"credit_card",
	"debit_card",
	"card_number",
	"cvv",
	"cvc",
	"security_code",
	"pin_code",
	"bank_account",
	"routing_number",
	"seed_phrase",
	"mnemonic",
	"passport_number",
}

// Canonical section display ordering for deterministic rendering.
var canonicalProfileSectionOrder = []string{
	"self",
	"addresses",
	"household",
	"travel",
	"emergency",
}

func normalizeProfileSection(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = reProfileKeyClean.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "self"
	}
	return s
}

func normalizeProfileKey(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = reProfileKeyClean.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	return s
}

// ParseProfileSectionAndKey supports either explicit (--section, --key) or shorthand --key="addresses.home".
func ParseProfileSectionAndKey(section, key string) (string, string) {
	k := strings.TrimSpace(key)
	sec := strings.TrimSpace(section)
	if sec == "" && strings.Contains(k, ".") {
		parts := strings.SplitN(k, ".", 2)
		sec = parts[0]
		k = parts[1]
	}
	return normalizeProfileSection(sec), normalizeProfileKey(k)
}

func passesLuhn(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func containsPaymentCardNumber(val string) bool {
	for _, span := range reProfileDigitSpan.FindAllString(val, -1) {
		var digits strings.Builder
		for _, r := range span {
			if unicode.IsDigit(r) {
				digits.WriteRune(r)
			}
		}
		d := digits.String()
		if len(d) >= 13 && len(d) <= 19 && passesLuhn(d) {
			return true
		}
	}
	return false
}

// ValidateProfileEntry rejects Tier-3 secrets (passwords, SSNs, Luhn payment cards, API keys, PEM keys)
// so only Tier-2 autofill PII (phone, home address, household names/schools, KTN/loyalty IDs) is stored.
func ValidateProfileEntry(key, value string) error {
	k := normalizeProfileKey(key)
	if k == "" {
		return fmt.Errorf("profile key is required")
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("profile value is required")
	}
	if len([]rune(v)) > 400 {
		return fmt.Errorf("profile value exceeds 400-rune limit (%d runes)", len([]rune(v)))
	}
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("profile value must be single-line")
	}

	for _, blocked := range blockedProfileKeyTokens {
		if strings.Contains(k, blocked) {
			return fmt.Errorf("refused Tier-3 secret key %q: passwords, SSNs, payment cards, and tokens belong in your OS Keychain or password manager, not private/profile.md", key)
		}
	}
	if reProfileSSN.MatchString(v) {
		return fmt.Errorf("refused Tier-3 secret value matching SSN pattern (XXX-XX-XXXX): never store SSNs in private/profile.md")
	}
	if reProfilePEM.MatchString(v) {
		return fmt.Errorf("refused Tier-3 secret value containing PEM private key")
	}
	if reProfileAPIToken.MatchString(v) {
		return fmt.Errorf("refused Tier-3 secret value matching API/bearer token pattern")
	}
	if containsPaymentCardNumber(v) {
		return fmt.Errorf("refused Tier-3 secret value matching valid payment card number (Luhn check): never store payment cards in private/profile.md")
	}
	return nil
}

// LoadProfile reads <vault>/private/profile.md lock-free (Lstat symlink-safe).
func LoadProfile(vaultDir string) (*ProfileData, error) {
	p := ProfilePath(vaultDir)
	prof := &ProfileData{
		Sections: make(map[string]map[string]string),
		Path:     p,
	}
	if _, err := os.Lstat(p); err != nil {
		if os.IsNotExist(err) {
			return prof, nil
		}
		return nil, err
	}
	if !isRegularNonSymlinkFile(p) {
		return nil, fmt.Errorf("profile path %s is not a regular non-symlink file", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}

	curSection := "self"
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ">") {
			meta := strings.TrimSpace(strings.TrimLeft(line, ">"))
			if strings.HasPrefix(strings.ToLower(meta), "**updated:**") {
				prof.UpdatedAt = strings.TrimSpace(meta[len("**updated:**"):])
			}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			curSection = normalizeProfileSection(strings.TrimPrefix(line, "## "))
			continue
		}
		if strings.HasPrefix(line, "- **") {
			rest := strings.TrimPrefix(line, "- **")
			idx := strings.Index(rest, ":**")
			if idx <= 0 {
				continue
			}
			k := normalizeProfileKey(rest[:idx])
			v := strings.TrimSpace(rest[idx+len(":**"):])
			if k == "" || v == "" {
				continue
			}
			if prof.Sections[curSection] == nil {
				prof.Sections[curSection] = make(map[string]string)
			}
			prof.Sections[curSection][k] = v
		}
	}
	return prof, nil
}

func profileSectionHeading(sec string) string {
	parts := strings.Split(sec, "_")
	for i, p := range parts {
		if len(p) > 0 {
			r := []rune(p)
			r[0] = unicode.ToUpper(r[0])
			parts[i] = string(r)
		}
	}
	return strings.Join(parts, " ")
}

// FormatProfileMarkdown renders ProfileData deterministically as structured Markdown.
func FormatProfileMarkdown(p *ProfileData) string {
	var sb strings.Builder
	updated := strings.TrimSpace(p.UpdatedAt)
	if updated == "" {
		updated = time.Now().Format("2006-01-02")
	}
	sb.WriteString("# Private Household Profile\n")
	sb.WriteString("> **Scope:** Tier-2 Autofill PII Only (0 SessionStart tokens · Excluded from memory search, tainted, and [Auto] sessions)\n")
	sb.WriteString("> **Updated:** " + updated + "\n")

	seen := make(map[string]bool)
	var ordered []string
	for _, sec := range canonicalProfileSectionOrder {
		if len(p.Sections[sec]) > 0 {
			ordered = append(ordered, sec)
			seen[sec] = true
		}
	}
	var extra []string
	for sec, fields := range p.Sections {
		if !seen[sec] && len(fields) > 0 {
			extra = append(extra, sec)
		}
	}
	sort.Strings(extra)
	ordered = append(ordered, extra...)

	for _, sec := range ordered {
		fields := p.Sections[sec]
		if len(fields) == 0 {
			continue
		}
		sb.WriteString("\n## " + profileSectionHeading(sec) + "\n")
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("- **%s:** %s\n", k, fields[k]))
		}
	}
	return sb.String()
}

// SetProfileField validates and atomically persists a single Tier-2 autofill field
// to <vault>/private/profile.md under store.RankMemory (0700 dir, 0600 file).
func SetProfileField(vaultDir, section, key, value string) (*ProfileData, error) {
	sec, k := ParseProfileSectionAndKey(section, key)
	if err := ValidateProfileEntry(k, value); err != nil {
		return nil, err
	}
	val := strings.TrimSpace(value)

	privDir := filepath.Join(vaultDir, "private")
	if err := os.MkdirAll(privDir, 0o700); err != nil {
		return nil, err
	}

	var result *ProfileData
	err := withMemoryLock(vaultDir, func() error {
		prof, err := LoadProfile(vaultDir)
		if err != nil {
			return err
		}
		if prof.Sections == nil {
			prof.Sections = make(map[string]map[string]string)
		}
		if prof.Sections[sec] == nil {
			prof.Sections[sec] = make(map[string]string)
		}
		prof.Sections[sec][k] = val
		prof.UpdatedAt = time.Now().Format("2006-01-02")
		if err := store.WriteAtomic(ProfilePath(vaultDir), []byte(FormatProfileMarkdown(prof)), 0o600); err != nil {
			return err
		}
		result = prof
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UnsetProfileField removes a field from <vault>/private/profile.md under store.RankMemory (0600 atomic write).
// When section is empty and key has no dot prefix, removes matching key across all sections.
func UnsetProfileField(vaultDir, section, key string) (*ProfileData, bool, error) {
	rawSec := strings.TrimSpace(section)
	rawKey := strings.TrimSpace(key)
	var targetSec, k string
	if rawSec != "" || strings.Contains(rawKey, ".") {
		targetSec, k = ParseProfileSectionAndKey(rawSec, rawKey)
	} else {
		k = normalizeProfileKey(rawKey)
	}
	if k == "" {
		return nil, false, fmt.Errorf("profile key is required")
	}

	var result *ProfileData
	removed := false
	err := withMemoryLock(vaultDir, func() error {
		prof, err := LoadProfile(vaultDir)
		if err != nil {
			return err
		}
		if targetSec != "" {
			if fields := prof.Sections[targetSec]; fields != nil {
				if _, ok := fields[k]; ok {
					delete(fields, k)
					removed = true
					if len(fields) == 0 {
						delete(prof.Sections, targetSec)
					}
				}
			}
		} else {
			for sec, fields := range prof.Sections {
				if _, ok := fields[k]; ok {
					delete(fields, k)
					removed = true
					if len(fields) == 0 {
						delete(prof.Sections, sec)
					}
				}
			}
		}
		if removed {
			prof.UpdatedAt = time.Now().Format("2006-01-02")
			if err := store.WriteAtomic(ProfilePath(vaultDir), []byte(FormatProfileMarkdown(prof)), 0o600); err != nil {
				return err
			}
		}
		result = prof
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, removed, nil
}
