package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zjshen14/agy-hq/internal/rsi"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func runRSICLI(rootDir, vaultDir string, args []string) {
	sub, args := splitSubcommand(args, "status")
	denyIfCallerTainted(vaultDir, "rsi "+sub)

	switch sub {
	case "mine":
		fs := flag.NewFlagSet("rsi mine", flag.ExitOnError)
		sinceStr := fs.String("since", "watermark", "Lookback duration (watermark, 24h, 168h, or 0s/all)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)

		var events []rsi.FrictionEvent
		var err error
		mode := strings.ToLower(strings.TrimSpace(*sinceStr))
		switch mode {
		case "", "watermark":
			events, err = rsi.MineSinceWatermark(rootDir, vaultDir)
		case "0s", "0", "all":
			events, err = rsi.Mine(rootDir, vaultDir, 0)
		default:
			d, pErr := parseFlexibleDuration(*sinceStr)
			if pErr != nil {
				fmt.Fprintf(os.Stderr, "hq rsi mine invalid --since %q: %v\n", *sinceStr, pErr)
				os.Exit(1)
			}
			events, err = rsi.Mine(rootDir, vaultDir, d)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi mine failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(events)
			return
		}
		fmt.Printf("RSI Stage 1 (Mine): %d deduplicated friction event(s) in local/memory/rsi/observations.jsonl\n", len(events))
		for _, ev := range events {
			fmt.Printf("  - [%s] %s (%s · substrate:%s · impact:%d · count:%d · convs:%d · status:%s)\n",
				ev.ID, ev.SkillOrPkg, ev.Category, ev.TargetSubstrate, ev.ImpactScore, ev.OccurrenceCount, len(ev.ConversationIDs), ev.Status)
			fmt.Printf("      Excerpt: %s\n", ev.ErrorExcerpt)
		}

	case "context":
		posID, flagArgs := splitFirstPositionalAndFlags(args)
		fs := flag.NewFlagSet("rsi context", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if posID == "" && len(fs.Args()) > 0 {
			posID = fs.Args()[0]
		}
		if posID == "" {
			fmt.Fprintln(os.Stderr, "usage: hq rsi context <obs-id> [--json]")
			os.Exit(1)
		}
		pkt, err := rsi.BuildObservationContext(rootDir, vaultDir, posID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi context failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(pkt)
			return
		}
		fmt.Printf("Observation Context: %s (%s · skill:%s · target:%s · activeRules:%d/%d)\n",
			pkt.Observation.ID, pkt.Observation.Category, pkt.TargetSkill, pkt.TargetFile, pkt.ActiveRuleCount, pkt.MaxRulesForTarget)
		fmt.Printf("  Excerpt: %s\n", pkt.Observation.ErrorExcerpt)
		for _, ex := range pkt.TranscriptExcerpts {
			fmt.Printf("\n%s\n", ex)
		}
		if len(pkt.PriorTombstones) > 0 {
			fmt.Println("\nPrior Rejected/Reverted Tombstones:")
			for _, tb := range pkt.PriorTombstones {
				fmt.Printf("  ! %s\n", tb)
			}
		}

	case "record-correction":
		fs := flag.NewFlagSet("rsi record-correction", flag.ExitOnError)
		skill := fs.String("skill", "", "Target named skill slug (e.g. gchat, morning-brief)")
		summary := fs.String("summary", "", "Concise summary of the user workflow correction")
		diagnosis := fs.String("diagnosis", "", "Actionable rule guidance to prevent recurrence")
		convID := fs.String("conv", "", "Optional conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		if *convID == "" {
			*convID = vault.ResolveConversationID()
		}
		ev, err := rsi.RecordCorrection(vaultDir, *skill, *convID, *summary, *diagnosis)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi record-correction failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(ev)
			return
		}
		fmt.Printf("Recorded user correction %s for skill %q (status: %s)\n", ev.ID, ev.SkillOrPkg, ev.Status)

	case "hypothesize":
		targetObs, flagArgs := splitFirstPositionalAndFlags(args)
		fs := flag.NewFlagSet("rsi hypothesize", flag.ExitOnError)
		obsFlag := fs.String("obs", "", "Optional observation ID or fingerprint")
		fromJSON := fs.String("from-json", "", "Path to JSON HypothesisInput file (or '-' for stdin)")
		force := fs.Bool("force", false, "Bypass Meta-ROI and 75% Quota Circuit Breaker thresholds")
		upstream := fs.Bool("upstream", false, "Tier 2 Maintainer Mode: target tracked upstream skills inside an isolated git worktree")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)

		if strings.TrimSpace(*fromJSON) != "" {
			var raw []byte
			var rErr error
			if *fromJSON == "-" {
				raw, rErr = io.ReadAll(os.Stdin)
			} else {
				raw, rErr = os.ReadFile(*fromJSON)
			}
			if rErr != nil {
				fmt.Fprintf(os.Stderr, "hq rsi hypothesize --from-json read failed: %v\n", rErr)
				os.Exit(1)
			}
			var spec rsi.HypothesisInput
			if err := json.Unmarshal(raw, &spec); err != nil {
				fmt.Fprintf(os.Stderr, "hq rsi hypothesize --from-json parse failed: %v\n", err)
				os.Exit(1)
			}
			h, err := rsi.HypothesizeFromJSON(rootDir, vaultDir, spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq rsi hypothesize --from-json failed: %v\n", err)
				os.Exit(1)
			}
			if *asJSON {
				printJSON(h)
				return
			}
			fmt.Printf("Synthesized semantic RSI hypothesis %s [%s]: %s\n", h.ID, strings.Join(h.TargetFiles, ", "), h.Title)
			return
		}

		if *obsFlag != "" {
			targetObs = *obsFlag
		}
		hyps, err := rsi.HypothesizeWithOptions(rootDir, vaultDir, targetObs, rsi.HypothesizeOptions{
			Force:    *force,
			Upstream: *upstream,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi hypothesize failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(hyps)
			return
		}
		fmt.Printf("RSI Stage 2 (Hypothesize): synthesized %d hypothesis specification(s)\n", len(hyps))
		for _, h := range hyps {
			tierTag := "tier:personal"
			if h.Tier == "upstream" {
				tierTag = fmt.Sprintf("tier:upstream branch:%s", h.Branch)
			}
			fmt.Printf("  - %s [%s · %s · fault:%s · savedSteps:%d]: %s\n",
				h.ID, h.Ring.Label(), tierTag, h.FaultLayer, h.ExpectedSavedSteps, h.Title)
		}

	case "verify":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq rsi verify <rsi-id> [--json]")
			os.Exit(1)
		}
		rsiID := args[0]
		fs := flag.NewFlagSet("rsi verify", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args[1:])
		h, err := rsi.Verify(rootDir, vaultDir, rsiID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi verify failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(h)
		} else {
			v := h.Verification
			fmt.Printf("RSI Stage 3 (4-Gate Sandboxed Verification) — %s [%s]\n", h.ID, strings.ToUpper(h.Status))
			fmt.Printf("  Gate 1 (TCB & Anti-Goodhart AST): %v — %s\n", v.Gate1TCBPassed, v.Gate1Detail)
			fmt.Printf("  Gate 2 (Counterfactual/Critic-1): %v — %s\n", v.Gate2ReproPassed, v.Gate2Detail)
			fmt.Printf("  Gate 3 (Tests & PII Guard):       %v — %s\n", v.Gate3TestSuitePassed, v.Gate3Detail)
			fmt.Printf("  Gate 4 (Budget & Contradiction):  %v — %s\n", v.Gate4BudgetPassed, v.Gate4Detail)
			if v.RequiresLiveSafetyProbe {
				fmt.Printf("  Live Safety Probe Required:       YES — %s\n", v.SafetyProbeReason)
			}
		}
		if h.Verification == nil || !h.Verification.AllPassed {
			os.Exit(1)
		}

	case "probes":
		posID, flagArgs := splitFirstPositionalAndFlags(args)
		fs := flag.NewFlagSet("rsi probes", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if posID == "" && len(fs.Args()) > 0 {
			posID = fs.Args()[0]
		}
		if posID == "" {
			fmt.Fprintln(os.Stderr, "usage: hq rsi probes <rsi-id> [--json]")
			os.Exit(1)
		}
		pack, err := rsi.BuildSafetyProbes(vaultDir, posID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi probes failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(pack)
			return
		}
		fmt.Printf("Safe-Personal-RSI Behavioral Probe Pack for %s (diff: %s)\n", pack.RSIID, shortPrefix(pack.DiffSHA256, 12))
		for _, c := range pack.Cases {
			fmt.Printf("  - [%s] (%s): %s\n", c.ID, c.Category, c.ExpectedBehavior)
		}

	case "record-probe":
		posID, flagArgs := splitFirstPositionalAndFlags(args)
		fs := flag.NewFlagSet("rsi record-probe", flag.ExitOnError)
		passed := fs.Bool("passed", false, "Whether all 6 behavioral safety probes passed")
		summary := fs.String("summary", "", "Brief probe evaluation summary")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if posID == "" && len(fs.Args()) > 0 {
			posID = fs.Args()[0]
		}
		if posID == "" {
			fmt.Fprintln(os.Stderr, "usage: hq rsi record-probe <rsi-id> --passed=true|false [--summary=\"...\"] [--json]")
			os.Exit(1)
		}
		h, err := rsi.RecordSafetyProbeResult(vaultDir, posID, *passed, *summary)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi record-probe failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(h)
			return
		}
		fmt.Printf("Recorded behavioral safety probe for %s (passed=%v, diffSha=%s)\n", h.ID, h.SafetyProbePassed, shortPrefix(h.SafetyProbeDiffSHA, 12))

	case "propose":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq rsi propose <rsi-id> [--conv=<conv-id>]")
			os.Exit(1)
		}
		rsiID := args[0]
		fs := flag.NewFlagSet("rsi propose", flag.ExitOnError)
		convID := fs.String("conv", "", "Optional conversation ID")
		_ = fs.Parse(args[1:])
		hqBin := resolveProposalHQBin(rootDir)
		prop, h, err := rsi.Propose(rootDir, vaultDir, rsiID, *convID, hqBin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi propose failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Staged RSI proposal %s for %s (type: %s, sha256: %s)\n", prop.ID, h.ID, prop.ActionType, shortPrefix(prop.SHA256, 12))
		fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, shortPrefix(prop.SHA256, 12))

	case "apply":
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq rsi apply <rsi-id> --expect-diff-sha256=<prefix>")
			os.Exit(1)
		}
		rsiID := args[0]
		fs := flag.NewFlagSet("rsi apply", flag.ExitOnError)
		expectSHA := fs.String("expect-diff-sha256", "", "Expected diff SHA-256 prefix from verified proposal")
		_ = fs.Parse(args[1:])
		out, err := rsi.Apply(rootDir, vaultDir, rsiID, *expectSHA)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi apply failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Print(out)

	case "stage-nightly":
		fs := flag.NewFlagSet("rsi stage-nightly", flag.ExitOnError)
		maxProp := fs.Int("max", 2, "Maximum Ring-2 RSI proposals to stage per nightly run")
		convID := fs.String("conv", "", "Optional conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		if *convID == "" {
			*convID = vault.ResolveConversationID()
		}
		rep, err := rsi.StageNightly(rootDir, vaultDir, *convID, resolveProposalHQBin(rootDir), *maxProp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi stage-nightly failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("RSI Nightly Staging: staged=%d proposals, advisories=%d, needsProbe=%d, failedVerify=%d\n",
			len(rep.StagedProposalIDs), len(rep.AdvisoryCardIDs), len(rep.NeedsProbeIDs), len(rep.FailedVerifyIDs))

	case "eval":
		fs := flag.NewFlagSet("rsi eval", flag.ExitOnError)
		sinceStr := fs.String("since", "14d", "Evaluation lookback window (e.g. 7d, 14d, 336h)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		sinceDur, err := parseFlexibleDuration(*sinceStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi eval invalid --since %q: %v\n", *sinceStr, err)
			os.Exit(1)
		}
		rep, err := rsi.Evaluate(rootDir, vaultDir, sinceDur)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi eval failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("RSI Held-Out Ratchet Evaluation (since %s)\n", rep.Since)
		fmt.Printf("  Selection Split: %d convs · %d turns · %.1f err/100t · %.0f tok/turn\n",
			rep.Selection.Conversations, rep.Selection.Turns, rep.Selection.ErrorRatePer100Turns, rep.Selection.MeanTokensPerTurn)
		fmt.Printf("  Held-Out Split:  %d convs · %d turns · %.1f err/100t · %.0f tok/turn\n",
			rep.HeldOut.Conversations, rep.HeldOut.Turns, rep.HeldOut.ErrorRatePer100Turns, rep.HeldOut.MeanTokensPerTurn)
		fmt.Printf("  Funnel:          obs=%d (%d open) · draft=%d · verified=%d · proposed=%d · canary=%d · promoted=%d · rejected=%d · reverted=%d\n",
			rep.Funnel.ObservationsTotal, rep.Funnel.ObservationsOpen, rep.Funnel.HypothesesDraft, rep.Funnel.HypothesesVerified,
			rep.Funnel.HypothesesProposed, rep.Funnel.CanariesActive, rep.Funnel.PromotedCount, rep.Funnel.RejectedCount, rep.Funnel.RevertedCount)
		if len(rep.Regressions) > 0 {
			fmt.Println("  GUARD Regressions:")
			for _, reg := range rep.Regressions {
				fmt.Printf("    ! %s\n", reg)
			}
		}

	case "ablate":
		fs := flag.NewFlagSet("rsi ablate", flag.ExitOnError)
		model := fs.String("model", "", "Target upgraded model ID to ablate older model-scoped rules against")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		staged, err := rsi.Ablate(rootDir, vaultDir, *model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi ablate failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(staged)
			return
		}
		fmt.Printf("RSI Ablation Sweep: staged %d deprecation hypothesis(es)\n", len(staged))
		for _, h := range staged {
			fmt.Printf("  - %s: %s (%s)\n", h.ID, h.Title, h.RootCause)
		}

	case "compact":
		fs := flag.NewFlagSet("rsi compact", flag.ExitOnError)
		maxAgeStr := fs.String("max-age", "7d", "Archive draft hypotheses older than this duration (e.g. 7d, 168h)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		maxAge, err := parseFlexibleDuration(*maxAgeStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi compact invalid --max-age %q: %v\n", *maxAgeStr, err)
			os.Exit(1)
		}
		rep, err := rsi.Compact(rootDir, vaultDir, maxAge)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi compact failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("RSI Compact: archivedDrafts=%d, archivedCanaries=%d, removedGeneral=%v, observations=%d->%d\n",
			rep.ArchivedDrafts, rep.ArchivedCanaries, rep.RemovedGeneralFile, rep.ObservationsBefore, rep.ObservationsAfter)

	case "status":
		fs := flag.NewFlagSet("rsi status", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		st := rsi.GetStatus(rootDir, vaultDir)
		if *asJSON {
			printJSON(st)
			return
		}
		fmt.Printf("HQ Recursive Self-Improvement (RSI) Engine Status\n")
		fmt.Printf("  Observations:        %d total (%d open)\n", st.TotalObservations, st.OpenObservations)
		fmt.Printf("  Hypotheses:          %d total (%d verified ready)\n", st.TotalHypotheses, st.VerifiedReady)
		fmt.Printf("  Canary Archive:      %d active canary, %d promoted, %d rejected/reverted tombstones\n", st.ActiveCanaries, st.PromotedPatches, st.RejectedTombstone)
		fmt.Printf("  Actionable Advisories: %d (Ring-1 CLI / script compile / graduation)\n", st.AdvisoryCount)
		fmt.Printf("  Skill Rule Budget:   %d/%d global active rsi:rule block(s) across %d skill(s) (overlay cap: %d/skill)\n",
			st.ActiveSkillRules, rsi.MaxGlobalActiveRules, st.TotalSkills, rsi.MaxRulesPerOverlay)
		if !st.QuotaAllowed {
			fmt.Printf("  Quota Circuit Breaker: PAUSED (%s)\n", st.QuotaMessage)
		} else {
			fmt.Printf("  Quota Circuit Breaker: OK (<75%% rolling utilization)\n")
		}

	case "export":
		fs := flag.NewFlagSet("rsi export", flag.ExitOnError)
		upstream := fs.Bool("upstream", false, "Export clean Vault rule/hypothesis into Tier 2 upstream PR branch")
		convID := fs.String("conv", "", "Optional conversation ID")
		asJSON := fs.Bool("json", false, "Output JSON")
		posID, flagArgs := splitFirstPositionalAndFlags(args)
		_ = fs.Parse(flagArgs)
		if posID == "" && len(fs.Args()) > 0 {
			posID = fs.Args()[0]
		}
		if posID == "" {
			fmt.Fprintln(os.Stderr, "usage: hq rsi export --upstream <rule-id-or-rsi-id> [--conv=<conv-id>] [--json]")
			os.Exit(1)
		}
		if !*upstream {
			fmt.Fprintln(os.Stderr, "hq rsi export currently requires --upstream to graduate a Vault rule to an upstream PR")
			os.Exit(1)
		}
		if *convID == "" {
			*convID = vault.ResolveConversationID()
		}
		prop, hyp, err := rsi.ExportUpstream(rootDir, vaultDir, posID, *convID, resolveProposalHQBin(rootDir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi export failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		if *asJSON {
			printJSON(map[string]interface{}{
				"proposal":   prop,
				"hypothesis": hyp,
			})
			return
		}
		fmt.Printf("Staged Tier 2 upstream RSI PR proposal %s for %s (branch: %s, worktree: %s, sha256: %s)\n",
			prop.ID, posID, hyp.Branch, hyp.WorktreeDir, shortPrefix(prop.SHA256, 12))
		fmt.Printf("  Approve with: %s approve %s --sha256=%s\n", displayHQCmd(rootDir), prop.ID, shortPrefix(prop.SHA256, 12))

	case "migrate-overlays":
		fs := flag.NewFlagSet("rsi migrate-overlays", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		rep, err := rsi.MigrateOverlays(rootDir, vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq rsi migrate-overlays failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		fmt.Printf("Migrated %d tracked RSI rule(s) into personal Vault overlays/skills (cleanedFiles=%d, updatedTargets=%d)\n",
			rep.ExtractedRules, len(rep.CleanedFiles), len(rep.UpdatedTargets))

	default:
		fmt.Fprintf(os.Stderr, "unknown rsi subcommand: %s (expected mine, context, record-correction, hypothesize, verify, probes, record-probe, propose, apply, stage-nightly, eval, ablate, compact, export, migrate-overlays, status)\n", sub)
		os.Exit(1)
	}
}
