package store

import (
	"regexp"
	"strings"
)

var reHTTPOrHTTPSScheme = regexp.MustCompile(`(?i)https?://`)

func isASCIIAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// DefangURLs neutralizes http:// and https:// schemes in tainted text so external clients never auto-fetch links.
func DefangURLs(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	locs := reHTTPOrHTTPSScheme.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if start > 0 && isASCIIAlphaNum(s[start-1]) {
			continue
		}
		if b.Len() == 0 {
			b.Grow(len(s) + len(locs))
		}
		b.WriteString(s[last:start])
		if end-start == 8 { // len("https://")
			b.WriteString("hxxps://")
		} else {
			b.WriteString("hxxp://")
		}
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// TruncateRunes truncates s to at most maxRunes runes, appending "..." when truncated.
func TruncateRunes(s string, maxRunes int) string {
	return TruncateRunesWithSuffix(s, maxRunes, "...")
}

// TruncateRunesWithSuffix truncates s to at most maxRunes runes, appending suffix when truncated.
func TruncateRunesWithSuffix(s string, maxRunes int, suffix string) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	sufRunes := []rune(suffix)
	if len(sufRunes) == 0 || maxRunes <= len(sufRunes) {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-len(sufRunes)]) + suffix
}

// ShortConvID returns an 8-character prefix of a trimmed conversation ID (or "unknown" if empty).
func ShortConvID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "unknown"
	}
	return id
}
