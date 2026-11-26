package rsi

import (
	"fmt"
	"os"
	"strings"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/vault"
)

const (
	minRuleBodyChars       = 24
	maxRuleBlockLines      = 5
	nearDuplicateThreshold = 0.70
)

// CheckRuleDrawbacks implements Critic 1 (RFC-0029): deterministic quality & non-redundancy
// detectors that reject degenerate, chat-paste, multi-line script dump, or near-duplicate rules
// against existing skill rules, Ring-3 active facts/playbooks, and rejection tombstones.
func CheckRuleDrawbacks(rootDir, vaultDir string, h *RSIHypothesis) (bool, string) {
	if h == nil || h.DeltaOp == nil {
		return true, ""
	}
	if h.DeltaOp.Op == "deprecate_skill_rule" {
		return true, ""
	}

	raw := strings.ReplaceAll(strings.TrimSpace(h.DeltaOp.Content), "\r\n", "\n")
	if raw == "" {
		return false, "Critic-1 drawback: empty rule content"
	}

	body := extractRuleProseBody(raw)
	if len(body) < minRuleBodyChars {
		return false, fmt.Sprintf("Critic-1 drawback: actionable rule body is too short (%d chars < %d minimum)", len(body), minRuleBodyChars)
	}

	// Reject heredocs, raw timestamp prefixes, and multi-line script dumps.
	if strings.Contains(body, "<<") {
		return false, "Critic-1 drawback: heredoc/script dump (`<<`) is not allowed inside a Markdown skill rule"
	}
	if strings.Contains(body, "Created At:") || strings.Contains(body, "Completed At:") {
		return false, "Critic-1 drawback: raw command timestamp wrapper (`Created At:` / `Completed At:`) leaked into rule body"
	}
	bodyLines := strings.Split(body, "\n")
	if len(bodyLines) > maxRuleBlockLines {
		return false, fmt.Sprintf("Critic-1 drawback: rule body spans %d lines (max %d lines; compile multi-step scripts instead of pasting code blocks)", len(bodyLines), maxRuleBlockLines)
	}
	if strings.Count(body, "```") >= 2 {
		return false, "Critic-1 drawback: fenced code block inside a single RSI rule is disallowed (keep rules concise single-bullet directives)"
	}

	// Reject chat status / narration leakage from recovery transcripts.
	cleanLower := strings.ToLower(strings.TrimSpace(strings.TrimLeft(body, "-*# ")))
	for _, prefix := range []string{
		"dispatched ", "worker ", "launched ", "subagent ", "i will ", "let me ", "sure, ", "okay, ",
	} {
		if strings.HasPrefix(cleanLower, prefix) {
			return false, fmt.Sprintf("Critic-1 drawback: conversational status narration (%q) is not an actionable rule", strings.TrimSpace(prefix))
		}
	}

	candStems := stemSet(body)
	if len(candStems) < 3 {
		return false, "Critic-1 drawback: rule lacks sufficient semantic tokens (< 3 meaningful terms)"
	}

	// 1. Near-duplicate check against existing active rules in the same target file(s)
	for _, tf := range h.TargetFiles {
		absPath, err := ResolveHypothesisTargetPath(rootDir, vaultDir, h, tf)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		for _, r := range ParseRSIRules(tf, string(b)) {
			if r.ID == h.DeltaOp.RuleID {
				continue
			}
			if blk, ok := extension.ExtractRSIRuleBlock(string(b), r.ID); ok {
				existingBody := extractRuleProseBody(blk)
				if sim := jaccardSimilarity(candStems, stemSet(existingBody)); sim >= nearDuplicateThreshold {
					return false, fmt.Sprintf("Critic-1 drawback: near-duplicate of existing rule %s (Jaccard=%.2f >= %.2f)", r.ID, sim, nearDuplicateThreshold)
				}
			}
		}
	}

	// 2. Near-duplicate check against Ring-3 active facts and Active Playbooks (Single-Owner enforcement)
	if vaultDir != "" {
		if activeFacts, _, _, err := vault.LoadFactsState(vaultDir); err == nil {
			for _, f := range activeFacts {
				if sim := jaccardSimilarity(candStems, stemSet(f.Statement)); sim >= nearDuplicateThreshold {
					return false, fmt.Sprintf("Critic-1 drawback: duplicates active Ring-3 fact %s (Jaccard=%.2f >= %.2f)", f.ID, sim, nearDuplicateThreshold)
				}
			}
		}
		if topics, err := vault.LoadTopicIndex(vaultDir); err == nil {
			for _, tp := range topics {
				if strings.TrimSpace(tp.ActivePlaybook) == "" {
					continue
				}
				for _, line := range strings.Split(tp.ActivePlaybook, "\n") {
					line = strings.TrimSpace(line)
					if len(line) < minRuleBodyChars {
						continue
					}
					if sim := jaccardSimilarity(candStems, stemSet(line)); sim >= nearDuplicateThreshold {
						return false, fmt.Sprintf("Critic-1 drawback: duplicates Ring-3 Active Playbook bullet in topic %q (Jaccard=%.2f >= %.2f)", tp.Slug, sim, nearDuplicateThreshold)
					}
				}
			}
		}

		// 3. Near-duplicate check against rejected/reverted tombstones in archive.jsonl
		if arch, err := LoadArchive(vaultDir); err == nil {
			for _, rec := range arch {
				if rec.Status != "rejected" && rec.Status != "reverted" {
					continue
				}
				for _, hfp := range h.SourceFingerprints {
					if hfp != "" && containsString(rec.SourceFingerprints, hfp) {
						return false, fmt.Sprintf("Critic-1 drawback: source fingerprint %s matches rejected/reverted tombstone %s", hfp, rec.ID)
					}
				}
				if sim := jaccardSimilarity(candStems, stemSet(rec.Title)); sim >= nearDuplicateThreshold {
					return false, fmt.Sprintf("Critic-1 drawback: repeats rejected/reverted tombstone %s (Jaccard=%.2f >= %.2f)", rec.ID, sim, nearDuplicateThreshold)
				}
			}
		}
	}

	return true, ""
}

func extractRuleProseBody(block string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "<!-- rsi:rule") || strings.HasPrefix(trimmed, "<!-- /rsi:rule") || strings.HasPrefix(trimmed, "## ") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func stemSet(text string) map[string]bool {
	out := make(map[string]bool)
	for _, st := range vault.TokenizeStems(text) {
		out[st] = true
	}
	return out
}

func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
