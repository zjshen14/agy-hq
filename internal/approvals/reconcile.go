package approvals

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
)

var (
	reGitTagCmd       = regexp.MustCompile(`\bgit\s+tag\s+(?:-[a-zA-Z]+\s+(?:"[^"]*"|'[^']*'\s+)?)*([vV]?[0-9]+\.[0-9]+[^\s;&|"']*)`)
	reGHReleaseTagCmd = regexp.MustCompile(`\bgh\s+release\s+create\s+["']?([vV]?[0-9]+\.[0-9]+[^\s;&|"']*)["']?`)
	reGitPushRefCmd   = regexp.MustCompile(`\bgit\s+push\s+(?:-[a-zA-Z-]+\s+)*([^\s;&|"'-][^\s;&|"']*)\s+([^\s;&|"'-][^\s;&|"']*)`)
)

// SupersedeByConvAndType atomically supersedes older pending proposals from the same (ConversationID, ActionType)
// unless KeepPrior was requested, preventing duplicate pending proposals when an agent re-drafts an action (RFC-0021 §4.4, F5).
func SupersedeByConvAndType(vaultDir, convID, actionType, newPropID string, now time.Time) ([]Proposal, error) {
	convID = strings.TrimSpace(convID)
	actionType = strings.TrimSpace(actionType)
	if convID == "" || actionType == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var superseded []Proposal
	err := forEachPendingProposal(vaultDir, func(fullPath string, p Proposal) bool {
		if p.ID != newPropID && p.ConversationID == convID && p.ActionType == actionType {
			if settled, ok := settlePendingProposal(vaultDir, fullPath, p, "superseded", "superseded",
				fmt.Sprintf("superseded by newer proposal %s in same conversation (%s)", newPropID, actionType), newPropID, now); ok {
				superseded = append(superseded, settled)
			}
		}
		return true
	})
	return superseded, err
}

// ReconcileAskRejections scans active AskRecords and transitions proposals to `rejected` (with SuppressResume=true, 0 LLM tokens)
// whenever the conversation transcript contains a completed user selection of `Reject (<propID>)` (RFC-0021 §4.4, F4).
func ReconcileAskRejections(vaultDir, convID, transcriptPath string, now time.Time) ([]Proposal, error) {
	if strings.TrimSpace(vaultDir) == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	asksDir := approvalAsksDir(vaultDir)
	entries, err := os.ReadDir(asksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	roots := hostpaths.Resolve(vaultDir)
	var reconciled []Proposal

	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rawAsk, rErr := os.ReadFile(filepath.Join(asksDir, e.Name()))
		if rErr != nil {
			continue
		}
		var ask AskRecord
		if json.Unmarshal(rawAsk, &ask) != nil || ValidateProposalID(ask.ProposalID) != nil {
			continue
		}
		if convID != "" && ask.ConvID != strings.TrimSpace(convID) {
			continue
		}

		// Ensure proposal is still pending before reading transcript
		pendingPath := filepath.Join(vaultDir, "memory", "pending", ask.ProposalID+".json")
		rawProp, pErr := os.ReadFile(pendingPath)
		if pErr != nil {
			_ = os.Remove(filepath.Join(asksDir, e.Name()))
			continue
		}
		var prop Proposal
		if json.Unmarshal(rawProp, &prop) != nil || prop.ID != ask.ProposalID {
			continue
		}

		tPath := strings.TrimSpace(transcriptPath)
		if tPath == "" && roots.BrainDir != "" && strings.TrimSpace(ask.ConvID) != "" {
			tPath = filepath.Join(roots.BrainDir, strings.TrimSpace(ask.ConvID), ".system_generated", "logs", "transcript.jsonl")
		}
		if tPath == "" {
			continue
		}

		wantRejectParen := "Reject (" + ask.ProposalID + ")"
		wantRejectSpace := "Reject " + ask.ProposalID
		codeTok := "code:" + ask.NonceCode
		rejectedInTranscript := false
		if sErr := scanCompletedAskTranscriptSteps(
			tPath,
			ask.AskStepMin,
			func(content string) bool { return !strings.Contains(content, ask.ProposalID) },
			func(_ int, content string) bool {
				if strings.Contains(content, codeTok) {
					// User approved rather than rejected
					return true
				}
				if strings.Contains(content, wantRejectParen) || strings.Contains(content, wantRejectSpace) {
					rejectedInTranscript = true
					return true
				}
				return false
			},
		); sErr != nil {
			continue
		}

		if rejectedInTranscript {
			if err := RejectProposalWithOptions(vaultDir, ask.ProposalID, RejectOptions{
				Reason:         "reconciled from ask_question reject selection",
				SuppressResume: true,
			}); err == nil {
				prop.Status = "rejected"
				reconciled = append(reconciled, prop)
			}
		}
	}
	return reconciled, nil
}

func localGitRefSHA(dir, ref string) string {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(ref) == "" {
		return ""
	}
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || st == nil {
		return ""
	}
	return localBareRepoRefSHA(dir, ref)
}

func localGitRemotePath(dir, remote string) string {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(remote) == "" {
		return ""
	}
	u, err := runApprovalGitCmd(dir, 20*time.Millisecond, "remote", "get-url", remote)
	if err != nil || u == "" || strings.Contains(u, "://") || strings.Contains(u, "@") {
		return ""
	}
	if !filepath.IsAbs(u) {
		u = filepath.Join(dir, u)
	}
	if st, sErr := os.Stat(u); sErr == nil && st.IsDir() {
		return u
	}
	return ""
}

func localBareRepoRefSHA(bareOrWorkDir, ref string) string {
	if strings.TrimSpace(bareOrWorkDir) == "" || strings.TrimSpace(ref) == "" {
		return ""
	}
	out, err := runApprovalGitCmd(bareOrWorkDir, 20*time.Millisecond, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}

func parseGitTargetsFromCommand(cmdDisplay string) (remote, branch, tag string) {
	remote = "origin"
	if m := reGitTagCmd.FindStringSubmatch(cmdDisplay); len(m) >= 2 {
		tag = strings.TrimSpace(m[1])
	} else if m := reGHReleaseTagCmd.FindStringSubmatch(cmdDisplay); len(m) >= 2 {
		tag = strings.TrimSpace(m[1])
	}
	if m := reGitPushRefCmd.FindStringSubmatch(cmdDisplay); len(m) >= 3 {
		remote = strings.TrimSpace(m[1])
		ref := strings.TrimSpace(m[2])
		if idx := strings.LastIndexByte(ref, ':'); idx >= 0 && idx+1 < len(ref) {
			ref = ref[idx+1:]
		}
		switch {
		case strings.HasPrefix(ref, "refs/tags/"):
			tag = strings.TrimPrefix(ref, "refs/tags/")
		case strings.HasPrefix(ref, "refs/heads/"):
			branch = strings.TrimPrefix(ref, "refs/heads/")
		case tag == "" && (strings.HasPrefix(ref, "v") || strings.HasPrefix(ref, "V")) && len(ref) > 1 && ref[1] >= '0' && ref[1] <= '9':
			tag = ref
		default:
			branch = ref
		}
	}
	return remote, branch, tag
}

func enrichGitBaselineMetadata(workDir, cmdDisplay string, orig map[string]string) map[string]string {
	lower := strings.ToLower(cmdDisplay)
	if !strings.Contains(lower, "git ") && !strings.Contains(lower, "gh release ") {
		return orig
	}
	effDir := extractLeadingCDDir(cmdDisplay, workDir)
	if effDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			effDir = cwd
		}
	}
	if effDir == "" {
		return orig
	}
	remote, branch, tag := parseGitTargetsFromCommand(cmdDisplay)
	if branch == "" && tag == "" {
		return orig
	}

	out := make(map[string]string, len(orig)+6)
	for k, v := range orig {
		out[k] = v
	}
	out["gitEffDir"] = effDir
	out["gitRemote"] = remote
	if tag != "" {
		out["gitTargetTag"] = tag
		out["gitPreTagSHA"] = localGitRefSHA(effDir, "refs/tags/"+tag)
		if localRem := localGitRemotePath(effDir, remote); localRem != "" {
			out["gitPreRemoteTagSHA"] = localBareRepoRefSHA(localRem, "refs/tags/"+tag)
		}
	}
	if branch != "" {
		out["gitTargetBranch"] = branch
		headSHA := localGitRefSHA(effDir, "refs/heads/"+branch)
		if headSHA == "" {
			headSHA = localGitRefSHA(effDir, "HEAD")
		}
		out["gitTargetBranchSHA"] = headSHA
		out["gitPreRemoteBranchSHA"] = localGitRefSHA(effDir, "refs/remotes/"+remote+"/"+branch)
	}
	return out
}

// ReconcileLocalGitProposals inspects pending git tag / push proposals strictly offline (<20ms local ref checks, 0 network calls)
// and transitions proposals whose target ref/tag has already landed externally to `reconciled_external` (RFC-0021 §4.4, F5).
func ReconcileLocalGitProposals(vaultDir string, now time.Time) ([]Proposal, error) {
	if strings.TrimSpace(vaultDir) == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var reconciled []Proposal
	err := forEachPendingProposal(vaultDir, func(fullPath string, p Proposal) bool {
		effDir := ""
		if p.Metadata != nil {
			effDir = strings.TrimSpace(p.Metadata["gitEffDir"])
		}
		if effDir == "" {
			effDir = extractLeadingCDDir(p.Command, p.WorkDir)
		}
		if effDir == "" {
			return true
		}

		remote, branch, tag := parseGitTargetsFromCommand(p.Command)
		if p.Metadata != nil {
			if r := strings.TrimSpace(p.Metadata["gitRemote"]); r != "" {
				remote = r
			}
			if b := strings.TrimSpace(p.Metadata["gitTargetBranch"]); b != "" {
				branch = b
			}
			if t := strings.TrimSpace(p.Metadata["gitTargetTag"]); t != "" {
				tag = t
			}
		}
		if branch == "" && tag == "" {
			return true
		}

		landedReason := ""
		if tag != "" {
			curTagSHA := localGitRefSHA(effDir, "refs/tags/"+tag)
			hasPreKey := false
			preTagSHA := ""
			preRemTagSHA := ""
			if p.Metadata != nil {
				_, hasPreKey = p.Metadata["gitPreTagSHA"]
				preTagSHA = p.Metadata["gitPreTagSHA"]
				preRemTagSHA = p.Metadata["gitPreRemoteTagSHA"]
			}
			if hasPreKey && preTagSHA == "" && curTagSHA != "" {
				landedReason = fmt.Sprintf("local git tag %s (%s) already created externally", tag, curTagSHA[:min(12, len(curTagSHA))])
			} else if localRem := localGitRemotePath(effDir, remote); localRem != "" {
				curRemTagSHA := localBareRepoRefSHA(localRem, "refs/tags/"+tag)
				if curRemTagSHA != "" && (preRemTagSHA == "" || curRemTagSHA == curTagSHA) {
					landedReason = fmt.Sprintf("remote git tag %s/%s (%s) already pushed externally", remote, tag, curRemTagSHA[:min(12, len(curRemTagSHA))])
				}
			} else if remTagTrack := localGitRefSHA(effDir, "refs/remotes/"+remote+"/tags/"+tag); remTagTrack != "" {
				landedReason = fmt.Sprintf("remote tracking tag %s/%s already present locally", remote, tag)
			}
		}

		if landedReason == "" && branch != "" {
			targetSHA := ""
			preRemoteSHA := ""
			if p.Metadata != nil {
				targetSHA = strings.TrimSpace(p.Metadata["gitTargetBranchSHA"])
				preRemoteSHA = strings.TrimSpace(p.Metadata["gitPreRemoteBranchSHA"])
			}
			if targetSHA == "" {
				targetSHA = localGitRefSHA(effDir, "refs/heads/"+branch)
			}
			curRemoteSHA := localGitRefSHA(effDir, "refs/remotes/"+remote+"/"+branch)
			if curRemoteSHA == "" {
				if localRem := localGitRemotePath(effDir, remote); localRem != "" {
					curRemoteSHA = localBareRepoRefSHA(localRem, "refs/heads/"+branch)
				}
			}
			if targetSHA != "" && curRemoteSHA == targetSHA && (preRemoteSHA == "" || preRemoteSHA != targetSHA) {
				landedReason = fmt.Sprintf("git branch %s/%s already at target commit %s", remote, branch, curRemoteSHA[:min(12, len(curRemoteSHA))])
			}
		}

		if landedReason != "" {
			if settled, ok := settlePendingProposal(vaultDir, fullPath, p, "reconciled_external", "reconciled_external", landedReason, "", now); ok {
				reconciled = append(reconciled, settled)
			}
		}
		return true
	})
	return reconciled, err
}
