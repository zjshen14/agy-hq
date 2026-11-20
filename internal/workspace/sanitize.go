package workspace

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/zjshen14/agy-hq/internal/outbox"
)

const (
	// MaxChanges caps how many changes a single Info may carry after sanitization.
	MaxChanges = 50
	// MaxTitleRunes clamps sanitized change titles.
	MaxTitleRunes = 80
	// MaxURLLength drops longer change URLs.
	MaxURLLength = 300
	// MaxBaseRunes clamps the opaque display-only sync point.
	MaxBaseRunes = 64
	// MaxPathLength rejects absurdly long roots and work directories.
	MaxPathLength = 4096
)

var (
	reWorkspaceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	reChangeID    = regexp.MustCompile(`^[A-Za-z0-9._/:#-]{1,64}$`)
	reShortLabel  = regexp.MustCompile(`^[a-z0-9_-]{0,32}$`)
	reCreateName  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// ValidWorkspaceID reports whether id is a well-formed provider workspace ID ([A-Za-z0-9._-], <= 128 chars).
func ValidWorkspaceID(id string) bool {
	return reWorkspaceID.MatchString(id)
}

// ValidateCreateName checks that a create name is derived from a task slug (^[a-z0-9][a-z0-9_-]{0,62}$),
// never free text (RFC-0025 §6.3). It returns a *ProviderError with CodeInvalid otherwise.
func ValidateCreateName(name string) error {
	if !reCreateName.MatchString(name) {
		return &ProviderError{Code: CodeInvalid, Message: fmt.Sprintf("create name %q must match ^[a-z0-9][a-z0-9_-]{0,62}$", clipForError(name))}
	}
	return nil
}

// SanitizeInfo validates and clamps one provider-reported workspace before core stores or renders it
// (RFC-0025 §11.3). Unknown enum values, malformed identifiers, and unsafe paths are errors; display-only text
// is scrubbed and clamped; unsafe URLs and timestamps are dropped. Provider is always set to providerFQID.
//
// The external adapter applies it to every provider response. Built-in providers are trusted Go code and are
// not required to pass it (core/git-worktree, for example, uses absolute paths as workspace IDs).
func SanitizeInfo(providerFQID string, in Info) (Info, error) {
	fqid := strings.TrimSpace(providerFQID)
	if fqid == "" {
		return Info{}, fmt.Errorf("sanitize workspace info: provider FQID is required")
	}
	out := Info{Provider: fqid}
	if !reWorkspaceID.MatchString(in.WorkspaceID) {
		return Info{}, fmt.Errorf("invalid workspace_id %q", clipForError(in.WorkspaceID))
	}
	out.WorkspaceID = in.WorkspaceID

	name := in.Name
	if name == "" {
		name = in.WorkspaceID
	}
	if !reWorkspaceID.MatchString(name) {
		return Info{}, fmt.Errorf("workspace %s: invalid name %q", in.WorkspaceID, clipForError(name))
	}
	out.Name = name

	root, err := sanitizeAbsPath("root", in.Root)
	if err != nil {
		return Info{}, fmt.Errorf("workspace %s: %w", in.WorkspaceID, err)
	}
	if root == string(filepath.Separator) {
		return Info{}, fmt.Errorf("workspace %s: root must not be the filesystem root", in.WorkspaceID)
	}
	workDir := in.WorkDir
	if workDir == "" {
		workDir = root
	}
	workDir, err = sanitizeAbsPath("work_dir", workDir)
	if err != nil {
		return Info{}, fmt.Errorf("workspace %s: %w", in.WorkspaceID, err)
	}
	if !PathWithin(workDir, root) {
		return Info{}, fmt.Errorf("workspace %s: work_dir %q is outside root %q", in.WorkspaceID, workDir, root)
	}
	out.Root = root
	out.WorkDir = workDir

	flavor := strings.ToLower(strings.TrimSpace(in.Flavor))
	if !reShortLabel.MatchString(flavor) {
		return Info{}, fmt.Errorf("workspace %s: invalid flavor %q", in.WorkspaceID, clipForError(in.Flavor))
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if !reShortLabel.MatchString(kind) {
		return Info{}, fmt.Errorf("workspace %s: invalid kind %q", in.WorkspaceID, clipForError(in.Kind))
	}
	out.Flavor = flavor
	out.Kind = kind

	if !in.State.Valid() {
		return Info{}, fmt.Errorf("workspace %s: invalid state %q", in.WorkspaceID, clipForError(string(in.State)))
	}
	out.State = in.State
	out.Base = SanitizeDisplayText(in.Base, MaxBaseRunes)

	// Validate all changes first and retain open changes ahead of closed ones when clamping to MaxChanges,
	// so truncating a long history can never hide unlanded work behind older landed/abandoned changes.
	var openChanges, closedChanges []ChangeRef
	for _, c := range in.Changes {
		if !reChangeID.MatchString(c.ID) {
			return Info{}, fmt.Errorf("workspace %s: invalid change id %q", in.WorkspaceID, clipForError(c.ID))
		}
		if !c.State.Valid() {
			return Info{}, fmt.Errorf("workspace %s: change %s has invalid state %q", in.WorkspaceID, c.ID, clipForError(string(c.State)))
		}
		clean := ChangeRef{
			ID:         c.ID,
			State:      c.State,
			NeedsOwner: c.NeedsOwner,
			Title:      SanitizeDisplayText(c.Title, MaxTitleRunes),
			URL:        sanitizeChangeURL(c.URL),
			UpdatedAt:  sanitizeTimestamp(c.UpdatedAt),
		}
		if clean.State.Open() {
			openChanges = append(openChanges, clean)
		} else {
			closedChanges = append(closedChanges, clean)
		}
	}
	if len(in.Changes) <= MaxChanges {
		// Preserve provider order when no truncation occurred.
		out.Changes = make([]ChangeRef, 0, len(in.Changes))
		oi, ci := 0, 0
		for _, c := range in.Changes {
			if c.State.Open() {
				out.Changes = append(out.Changes, openChanges[oi])
				oi++
			} else {
				out.Changes = append(out.Changes, closedChanges[ci])
				ci++
			}
		}
	} else {
		out.Changes = make([]ChangeRef, 0, MaxChanges)
		if len(openChanges) >= MaxChanges {
			out.Changes = append(out.Changes, openChanges[:MaxChanges]...)
		} else {
			out.Changes = append(out.Changes, openChanges...)
			rem := MaxChanges - len(out.Changes)
			if rem > len(closedChanges) {
				rem = len(closedChanges)
			}
			out.Changes = append(out.Changes, closedChanges[:rem]...)
		}
	}
	return out, nil
}

// sanitizeAbsPath requires an absolute, Clean-stable path without '..' segments or control/format characters.
func sanitizeAbsPath(field, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if len(p) > MaxPathLength {
		return "", fmt.Errorf("%s exceeds %d bytes", field, MaxPathLength)
	}
	if HasControlOrFormatRune(p) {
		return "", fmt.Errorf("%s contains control or invisible formatting characters", field)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s %q must be absolute", field, clipForError(p))
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%s %q must not contain '..'", field, clipForError(p))
		}
	}
	if filepath.Clean(p) != p {
		return "", fmt.Errorf("%s %q must be a clean path", field, clipForError(p))
	}
	return p, nil
}

// HasControlOrFormatRune reports whether s contains the Unicode replacement character, control characters, or invisible formatting runes.
func HasControlOrFormatRune(s string) bool {
	for _, r := range s {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// SanitizeDisplayText strips control and invisible formatting characters (newlines and tabs become spaces),
// collapses whitespace, neutralizes imperative/injection cues, and clamps to maxRunes.
func SanitizeDisplayText(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	collapsed := strings.Join(strings.Fields(b.String()), " ")
	return outbox.SanitizeAntiCue(collapsed, maxRunes)
}

// sanitizeChangeURL keeps only short, well-formed https URLs without credentials; anything else is dropped.
func sanitizeChangeURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" || len(u) > MaxURLLength || !strings.HasPrefix(u, "https://") {
		return ""
	}
	for _, r := range u {
		if r <= ' ' || r == 0x7f || r == '"' || r == '\'' || r == '<' || r == '>' || r == '`' || r == '\\' || unicode.Is(unicode.Cf, r) || r > unicode.MaxASCII {
			return ""
		}
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return u
}

// sanitizeTimestamp normalizes an RFC 3339 timestamp to UTC seconds, dropping anything unparsable.
func sanitizeTimestamp(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// clipForError keeps provider-controlled values short and single-line inside error messages.
func clipForError(s string) string {
	return SanitizeDisplayText(s, 80)
}
