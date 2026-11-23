package yamlmini

import "strings"

// Block represents a top-level YAML key and its raw unindented+indented lines.
type Block struct {
	Key     string
	Content string
}

// Lines normalizes CRLF line endings to LF and splits content into lines.
func Lines(content string) []string {
	return strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
}

// LeadingIndent counts leading indentation (' ' = 1, '\t' = 2).
// Callers should skip empty/comment lines before testing LeadingIndent(line) == 0.
func LeadingIndent(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			n++
		case '\t':
			n += 2
		default:
			return n
		}
	}
	return n
}

func isQuoteBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case ' ', '\t', '[', ',':
		return true
	}
	return false
}

// StripComment removes a trailing `# ...` comment outside single/double quotes
// when preceded by whitespace (or at index 0), and trims outer whitespace.
func StripComment(s string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			if isQuoteBoundary(s, i) {
				inSingle = true
			}
		case '"':
			if isQuoteBoundary(s, i) {
				inDouble = true
			}
		case '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return strings.TrimSpace(s)
}

// UnquoteScalar trims outer whitespace and unquotes matching '...' (unescaping ”)
// or "..." (unescaping \n, \t, \r, \", \\, \/ while preserving regex escapes like \b, \s, \d).
func UnquoteScalar(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\', '/':
					b.WriteByte(inner[i])
				default:
					// Preserve regex escapes such as \b, \s, \d if written inside double quotes.
					b.WriteByte('\\')
					b.WriteByte(inner[i])
				}
				continue
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	return s
}

// SplitKV splits a comment-stripped line on the first unquoted ':' followed by
// space, tab, or end-of-line. Returns trimmed key, trimmed raw val, and ok (key != "").
func SplitKV(line string) (key, val string, ok bool) {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			if isQuoteBoundary(line, i) {
				inSingle = true
			}
		case '"':
			if isQuoteBoundary(line, i) {
				inDouble = true
			}
		case ':':
			if i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t' {
				k := strings.TrimSpace(line[:i])
				if k == "" {
					return "", "", false
				}
				return k, strings.TrimSpace(line[i+1:]), true
			}
		}
	}
	return "", "", false
}

// ParseInlineList parses a bracketed YAML inline list `[a, "b, c"]` quote-aware.
// Returns (items, true) when enclosed in [...], or (nil, false) otherwise.
func ParseInlineList(raw string) ([]string, bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, true
	}
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				if i+1 < len(inner) && inner[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				cur.WriteByte(ch)
				escaped = true
				continue
			}
			cur.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			if strings.TrimSpace(cur.String()) == "" {
				inSingle = true
			}
			cur.WriteByte(ch)
		case '"':
			if strings.TrimSpace(cur.String()) == "" {
				inDouble = true
			}
			cur.WriteByte(ch)
		case ',':
			item := UnquoteScalar(cur.String())
			if item != "" {
				out = append(out, item)
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if last := UnquoteScalar(cur.String()); last != "" {
		out = append(out, last)
	}
	if out == nil {
		return []string{}, true
	}
	return out, true
}

// TrimListItem strips a leading YAML sequence marker ("- ", "-\t", or "-") from a
// comment-stripped, trimmed line and returns the trimmed remainder.
func TrimListItem(clean string) (rest string, ok bool) {
	s := strings.TrimSpace(clean)
	if s == "-" {
		return "", true
	}
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "-\t") {
		return strings.TrimSpace(s[1:]), true
	}
	return "", false
}

// SplitTopLevelBlocks splits YAML content into top-level `key:` blocks, preserving
// original lines (including comments and blank lines inside each block) verbatim.
func SplitTopLevelBlocks(content string) []Block {
	var blocks []Block
	var curKey string
	var curLines []string

	flush := func() {
		if curKey != "" && len(curLines) > 0 {
			blocks = append(blocks, Block{
				Key:     curKey,
				Content: strings.TrimRight(strings.Join(curLines, "\n"), "\n"),
			})
		}
	}

	for _, line := range Lines(content) {
		clean := StripComment(line)
		if clean != "" && LeadingIndent(line) == 0 && !strings.HasPrefix(clean, "-") {
			if k, _, ok := SplitKV(clean); ok {
				flush()
				curKey = k
				curLines = []string{line}
				continue
			}
		}
		if curKey != "" {
			curLines = append(curLines, line)
		}
	}
	flush()
	return blocks
}
