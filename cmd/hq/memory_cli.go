package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/vault"
	"github.com/zjshen14/agy-hq/internal/webui"
)

func runMemoryCLI(rootDir, vaultDir string, args []string) {
	sub := "status"
	rest := args
	if len(args) >= 1 {
		sub = args[0]
		rest = args[1:]
	}
	// Tainted quarantine boundary: only `memory lookup` (which strips private fields) is allowed in tainted sessions.
	if sub != "lookup" {
		denyIfCallerTainted(vaultDir, "memory "+sub)
	}
	switch sub {
	case "status", "audit":
		var completedSlugs []string
		if tasks, err := kernel.ListTasks(vaultDir); err == nil {
			for _, t := range tasks {
				if t.Status == "completed" || t.Status == "cancelled" || t.Status == "done" || t.Status == "archived" {
					completedSlugs = append(completedSlugs, t.TaskSlug)
				}
			}
		}
		rep, err := vault.AuditMemory(vaultDir, completedSlugs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory status failed: %v\n", err)
			os.Exit(1)
		}
		budgetBadge := "OK"
		if rep.OverBudget {
			budgetBadge = "OVER_BUDGET"
		}
		fmt.Printf("Memory Roadmap Index (MEMORY.md): %d/%d lines, %d/%d bytes (~%d tokens) [%s]\n",
			rep.MemoryLines, rep.MemoryMaxLines, rep.MemoryBytes, rep.MemoryMaxBytes, rep.EstimatedTokens, budgetBadge)
		if rep.UserBytes > rep.UserMaxBytes {
			fmt.Printf("USER.md Advisory:                 %d / %d bytes [OVER_ADVISORY]\n", rep.UserBytes, rep.UserMaxBytes)
		}
		fmt.Printf("Knowledge Wiki Topics (topics/):  %d page(s), %d bytes total\n", len(rep.Topics), rep.TopicsTotalBytes)
		for _, tp := range rep.Topics {
			extra := ""
			if tp.PlaybookOwner != "" {
				extra = fmt.Sprintf(" [playbook:%s]", tp.PlaybookOwner)
			}
			fmt.Printf("  - [%s] %s (%d lines, %dB)%s: %s\n", tp.Slug, tp.Title, tp.LineCount, tp.ByteSize, extra, tp.Summary)
		}
		fmt.Printf("Entity Dossiers (entities/):      %d dossier(s) (0 SessionStart tokens; on-demand lookup)\n", rep.EntityCount)
		fmt.Printf("Bi-Temporal Facts (facts.jsonl):  %d active, %d expired, %d overdue-TTL (%d raw lines)\n",
			rep.ActiveFactCount, rep.ExpiredFactCount, rep.OverdueTTLFactCount, rep.RawFactLines)
		if len(rep.DuplicatePlaybookOwners) > 0 {
			fmt.Println("Duplicate Playbook-Owners Detected:")
			for _, d := range rep.DuplicatePlaybookOwners {
				fmt.Printf("  ! %s\n", d)
			}
		}
		if len(rep.InvalidTopicHeaders) > 0 {
			fmt.Println("Invalid Topic Headers:")
			for _, ih := range rep.InvalidTopicHeaders {
				fmt.Printf("  ! %s\n", ih)
			}
		}
		if len(rep.BrokenTopicLinks) > 0 {
			fmt.Println("Broken Topic Links:")
			for _, bl := range rep.BrokenTopicLinks {
				fmt.Printf("  ! %s\n", bl)
			}
		}
		if len(rep.NearDuplicateTopics) > 0 {
			fmt.Println("Near-Duplicate Topics:")
			for _, nd := range rep.NearDuplicateTopics {
				fmt.Printf("  ! %s\n", nd)
			}
		}
		if len(rep.MissingTTLEventFacts) > 0 {
			fmt.Println("Dated Facts Missing --expires TTL:")
			for _, mt := range rep.MissingTTLEventFacts {
				fmt.Printf("  ! %s\n", mt)
			}
		}
		if len(rep.StaleReferences) > 0 {
			fmt.Println("Stale References Detected:")
			for _, sr := range rep.StaleReferences {
				fmt.Printf("  ! %s\n", sr)
			}
		}

	case "topics":
		topics, err := vault.LoadTopicIndex(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory topics failed: %v\n", err)
			os.Exit(1)
		}
		for _, tp := range topics {
			fmt.Printf("- %s (%s): %s [%s]\n", tp.Slug, tp.Title, tp.Summary, tp.Path)
		}

	case "search":
		posQ, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("memory search", flag.ExitOnError)
		qFlag := fs.String("query", "", "Search query (BM25 lexical + CJK bigram search)")
		scopeFlag := fs.String("scope", "", "Optional scope filter (e.g. repo:agy-hq, domain:investing)")
		layersFlag := fs.String("layers", "", "Comma-separated layers (facts,topics,entities,notes,journal,progress)")
		includeArchive := fs.Bool("archive", false, "Include archived topics, entities, journals, and expired facts")
		limit := fs.Int("limit", 10, "Maximum results to return")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if posQ == "" {
			posQ = *qFlag
		}
		if strings.TrimSpace(posQ) == "" {
			fmt.Fprintln(os.Stderr, "usage: hq memory search <query> [--scope=...] [--layers=facts,topics,entities,notes,journal,progress] [--archive] [--limit=10] [--json]")
			os.Exit(1)
		}
		var layers []string
		if strings.TrimSpace(*layersFlag) != "" {
			layers = strings.Split(*layersFlag, ",")
		}
		hits, err := vault.SearchMemory(vaultDir, vault.SearchOptions{
			Query:          posQ,
			Scope:          *scopeFlag,
			Layers:         layers,
			IncludeArchive: *includeArchive,
			Limit:          *limit,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory search failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSONUnescaped(hits)
			return
		}
		if len(hits) == 0 {
			fmt.Printf("No memory hits matched %q.\n", posQ)
			return
		}
		for i, h := range hits {
			fmt.Printf("%d. [%s · score=%.2f] %s\n   %s\n   %s\n", i+1, h.Layer, h.Score, h.Title, h.Provenance, h.Snippet)
		}

	case "dream-plan":
		fs := flag.NewFlagSet("memory dream-plan", flag.ExitOnError)
		precheck := fs.Bool("precheck", false, "Emit empty stdout when idle or concise 1-line summary when delta exists (gatekeeper exit_zero_nonempty)")
		sinceStr := fs.String("since", "", "Optional lookback duration (e.g. 24h, 72h) or RFC3339 timestamp")
		limit := fs.Int("limit", 15, "Maximum salient conversations to include")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)

		since, sErr := parseSinceFlag(*sinceStr, time.Now().UTC())
		if sErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", sErr)
			os.Exit(1)
		}
		plan, err := vault.BuildDreamPlan(vaultDir, vault.DreamPlanOptions{
			Since: since,
			Now:   time.Now().UTC(),
			Limit: *limit,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory dream-plan failed: %v\n", err)
			os.Exit(1)
		}
		if *precheck {
			if line := plan.PrecheckLine(); line != "" {
				fmt.Println(line)
			}
			return
		}
		if *asJSON {
			printJSONUnescaped(plan)
			return
		}
		fmt.Printf("Dream Plan (since %s) — hasDelta=%v\n", plan.Since, plan.HasDelta)
		fmt.Printf("  Salient conversations: %d (skipped tainted turns: %d, ephemeral convs: %d)\n",
			len(plan.SalientConversations), plan.SkippedTaintedTurns, plan.SkippedEphemeralConv)
		fmt.Printf("  MEMORY.md: %d lines, %d bytes (overBudget=%v) · overdueFacts=%d · pendingArchiveDays=%d\n",
			plan.MemoryLines, plan.MemoryBytes, plan.MemoryOverBudget, plan.OverdueTTLFactCount, plan.PendingArchiveDays)
		if len(plan.WikiAlerts) > 0 {
			fmt.Printf("  Wiki alerts: %s\n", strings.Join(plan.WikiAlerts, ", "))
		}
		if len(plan.NegativeConstraints) > 0 {
			fmt.Printf("  Negative constraints (recent rejections): %d\n", len(plan.NegativeConstraints))
		}

	case "eval":
		fs := flag.NewFlagSet("memory eval", flag.ExitOnError)
		sinceStr := fs.String("since", "14d", "Telemetry lookback window (e.g. 7d, 14d, 72h, or RFC3339)")
		runFixtures := fs.Bool("fixtures", false, "Run deterministic F1-F4 synthetic fixture suite")
		runReplay := fs.Bool("replay", false, "Replay historical conversations through SessionStart hook")
		baselineBin := fs.String("baseline-bin", "", "Optional baseline hq binary path for side-by-side --replay comparison")
		baselineVault := fs.String("baseline-vault", "", "Optional baseline Vault copy path for --replay comparison")
		limit := fs.Int("limit", 50, "Maximum historical conversations to sample during --replay")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)

		since, sErr := parseSinceFlag(*sinceStr, time.Now().UTC())
		if sErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", sErr)
			os.Exit(1)
		}

		rep, err := vault.EvaluateMemory(vaultDir, vault.EvalOptions{
			Since:    since,
			Now:      time.Now().UTC(),
			RepoRoot: rootDir,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory eval failed: %v\n", err)
			os.Exit(1)
		}
		if *runFixtures {
			fRep, fErr := vault.RunEvalFixtures()
			if fErr != nil {
				fmt.Fprintf(os.Stderr, "hq memory eval --fixtures failed: %v\n", fErr)
				os.Exit(1)
			}
			rep.Fixtures = fRep
		}
		if *runReplay {
			selfBin, _ := os.Executable()
			rRep, rErr := vault.RunHistoricalReplay(vault.ReplayOptions{
				CandidateBin:   selfBin,
				CandidateVault: vaultDir,
				BaselineBin:    *baselineBin,
				BaselineVault:  *baselineVault,
				Since:          since,
				SampleLimit:    *limit,
			})
			if rErr != nil {
				fmt.Fprintf(os.Stderr, "hq memory eval --replay failed: %v\n", rErr)
				os.Exit(1)
			}
			rep.Replay = rRep
		}
		if *asJSON {
			printJSONUnescaped(rep)
			if rep.Fixtures != nil && !rep.Fixtures.AllPassed {
				os.Exit(1)
			}
			return
		}
		fmt.Printf("Memory v2 Evaluation Report (since %s)\n", rep.Since)
		fmt.Printf("  [M1 Efficiency]  MEMORY.md=%dL/%dB (overBudget=%v) · USER.md=%dB · CrossLayerOverlap=%.1f%%\n",
			rep.MemoryLines, rep.MemoryBytes, rep.MemoryOverBudget, rep.UserBytes, rep.CrossLayerOverlapPct)
		fmt.Printf("  [M2 Freshness]   ActiveFacts=%d (scoped=%d, global=%d) · ScopedPlaybooks=%d · StaleOrMissingTTL=%.1f%% (overdue=%d, undated=%d)\n",
			rep.ActiveFactCount, rep.ScopedFactCount, rep.GlobalFactCount, rep.ScopedPlaybookCount, rep.StaleOrMissingTTLPct, rep.OverdueTTLFactCount, rep.MissingTTLEventFactsCnt)
		fmt.Printf("  [M3 Friction]    Turns=%d · ToolErrors/100t=%.1f · PreToolDenials/100t=%.1f · RejectRate=%.1f%% (%d/%d) · WikiReads=%d\n",
			rep.JournalTurns, rep.ToolErrorsPer100Turns, rep.PreToolDenialsPer100Turns, rep.InterventionRejectionRatePct, rep.InterventionsRejected, rep.InterventionsApproved+rep.InterventionsRejected, rep.WikiReadCount)
		fmt.Printf("  [M4 Wiki Health] Topics=%d (%dB) · InvalidHeaders=%d · BrokenLinks=%d · DupPlaybookOwners=%d · NearDups=%d · StaleRefs=%d\n",
			rep.TopicsCount, rep.TopicsTotalBytes, rep.InvalidTopicHeaders, rep.BrokenTopicLinks, rep.DuplicatePlaybookOwners, rep.NearDuplicateTopics, rep.StaleReferences)
		if rep.DomainCompressionApplicable {
			fmt.Printf("                   Notes=%dB · DomainCompressionRatio=%.2fx\n", rep.NotesBytes, rep.DomainCompressionRatio)
		}
		if rep.Fixtures != nil {
			fmt.Printf("  [Layer-1 Fixtures] %d/%d passed (allPassed=%v)\n", rep.Fixtures.PassedCount, rep.Fixtures.TotalCount, rep.Fixtures.AllPassed)
			for _, fr := range rep.Fixtures.Results {
				status := "PASS"
				if !fr.Passed {
					status = "FAIL"
				}
				fmt.Printf("    [%s] %s: %s\n", status, fr.ID, fr.Details)
			}
		}
		if rep.Replay != nil {
			c := rep.Replay.Candidate
			fmt.Printf("  [Layer-1 Replay] Sampled=%d · Candidate avg=%.0fB p50=%dB p95=%dB max=%dB · fullOver=%d leanOver=%d leanStarved=%d playbooks=%d\n",
				rep.Replay.SampledConversations, c.AvgContextBytes, c.P50ContextBytes, c.P95ContextBytes, c.MaxContextBytes,
				c.FullOverBudgetCount, c.LeanAutoOverBudgetCnt, c.LeanAutoStarvedCount, c.PlaybookInjectedCount)
			if b := rep.Replay.Baseline; b != nil {
				fmt.Printf("                   Baseline  avg=%.0fB p50=%dB p95=%dB max=%dB · fullOver=%d leanOver=%d leanStarved=%d\n",
					b.AvgContextBytes, b.P50ContextBytes, b.P95ContextBytes, b.MaxContextBytes,
					b.FullOverBudgetCount, b.LeanAutoOverBudgetCnt, b.LeanAutoStarvedCount)
				fmt.Printf("                   Delta     avg=%.1f%% · p95=%.1f%%\n", rep.Replay.AvgByteDeltaPct, rep.Replay.P95ByteDeltaPct)
			}
		}
		if rep.Fixtures != nil && !rep.Fixtures.AllPassed {
			os.Exit(1)
		}

	case "add-fact":
		// Block unattended [Auto] workers except sleep-consolidate from mutating durable facts directly.
		if callerConv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")); callerConv != "" {
			if t, ok := kernel.FindTaskByConversationID(vaultDir, callerConv); ok && t.EffectiveKind() == "auto" {
				lowerMeta := strings.ToLower(t.TaskSlug + " " + t.Title)
				if !strings.Contains(lowerMeta, "sleep-consolidate") && !strings.Contains(lowerMeta, "sleep_consolidate") {
					fmt.Fprintln(os.Stderr, "hq memory add-fact denied: unattended [Auto] workers cannot write durable facts directly; route durable lessons through nightly sleep-consolidate")
					os.Exit(1)
				}
			}
		}
		fs := flag.NewFlagSet("memory add-fact", flag.ExitOnError)
		id := fs.String("id", "", "Optional explicit fact ID")
		topic := fs.String("topic", "", "Fact topic category (e.g., architecture, workflow, investing)")
		statement := fs.String("statement", "", "Concise durable fact statement (<= 240 runes)")
		sourceConv := fs.String("source", "", "Optional source conversation ID")
		verified := fs.Bool("verified", false, "Mark fact as ground-truth transcript or human verified")
		scopeStr := fs.String("scope", "", "Optional comma-separated scope tags (e.g., global, repo:agy-hq, domain:investing)")
		expiresStr := fs.String("expires", "", "Optional TTL expiry (YYYY-MM-DD, RFC3339, +14d, +72h)")
		pinned := fs.Bool("pinned", false, "Pin high-priority fact for guaranteed SessionStart inclusion")
		_ = fs.Parse(rest)
		var scopes []string
		if strings.TrimSpace(*scopeStr) != "" {
			scopes = vault.ParseScopeList(*scopeStr)
		}
		fact, warning, err := vault.AddFactWithOptions(vaultDir, vault.AddFactOptions{
			ID:         *id,
			Topic:      *topic,
			Statement:  *statement,
			SourceConv: *sourceConv,
			Verified:   *verified,
			Scopes:     scopes,
			ExpiresAt:  *expiresStr,
			Pinned:     *pinned,
			Now:        time.Now().UTC(),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory add-fact failed: %v\n", err)
			os.Exit(1)
		}
		if warning != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
		}
		fmt.Printf("Added fact %s [%s]: %s\n", fact.ID, fact.Topic, fact.Statement)

	case "prune-fact":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq memory prune-fact <fact-id> [--superseded-by=<new-id>]")
			os.Exit(1)
		}
		factID := rest[0]
		fs := flag.NewFlagSet("memory prune-fact", flag.ExitOnError)
		supersededBy := fs.String("superseded-by", "", "Optional ID of the new fact that supersedes this one")
		_ = fs.Parse(rest[1:])
		pruned, err := vault.PruneFact(vaultDir, factID, *supersededBy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory prune-fact failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Pruned fact %s (valid_to=%s)\n", pruned.ID, *pruned.ValidTo)

	case "compact":
		fs := flag.NewFlagSet("memory compact", flag.ExitOnError)
		noSidebar := fs.Bool("no-sidebar", false, "Skip Antigravity sidebar archive sync")
		_ = fs.Parse(rest)
		now := time.Now().UTC()
		if res, tErr := kernel.CompactTasks(vaultDir, kernel.CompactTasksOptions{NoSidebar: *noSidebar}); tErr == nil {
			var rotated []string
			rotated = append(rotated, res.AutoArchivedSlugs...)
			rotated = append(rotated, res.RotatedColdSlugs...)
			if len(rotated) > 0 {
				_ = vault.ArchiveProgressBatch(vaultDir, rotated, now)
			}
			fmt.Printf("Compacted tasks.json: %d -> %d hot tasks (rotated %d to tasks_archive.jsonl, total cold: %d)\n",
				res.HotBefore, res.HotAfter, len(res.RotatedColdSlugs), res.TotalColdAfter)
		}
		var activeSlugs []string
		if tasks, err := kernel.ListTasks(vaultDir); err == nil {
			for _, t := range tasks {
				st := strings.ToLower(strings.TrimSpace(t.Status))
				if st != "done" && st != "archived" && strings.TrimSpace(t.TaskSlug) != "" {
					activeSlugs = append(activeSlugs, t.TaskSlug)
				}
			}
		}
		memRep, err := vault.CompactMemory(vaultDir, activeSlugs, now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory compact failed: %v\n", err)
			os.Exit(1)
		}
		_ = webui.SyncMarkdownConsole(vaultDir)
		fmt.Printf("Compacted facts.jsonl: %d -> %d canonical lines\n", memRep.FactsBefore, memRep.FactsAfter)
		fmt.Printf("Lifecycle archival:    progress=%d, journals=%d, briefs=%d\n",
			len(memRep.ArchivedProgress), memRep.ArchivedJournals, memRep.ArchivedBriefs)

	case "verify-transcript":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: hq memory verify-transcript <conv-id-or-path>")
			os.Exit(1)
		}
		digest, err := hooks.VerifyTranscript(rest[0], vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory verify-transcript failed: %v\n", err)
			os.Exit(1)
		}
		printJSONUnescaped(digest)

	case "forget":
		posQ, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("memory forget", flag.ExitOnError)
		query := fs.String("query", "", "Substring or fact ID to scrub across facts, MEMORY.md, topics, entities, notes, journals, progress, obs, and rsi")
		topicSlug := fs.String("topic", "", "Optional topic wiki slug to delete (local/memory/topics/<slug>.md)")
		entitySlug := fs.String("entity", "", "Optional entity dossier slug to delete (local/memory/entities/<slug>.md)")
		blockPII := fs.Bool("block-pii", false, "Also append --query to local/private/pii-denylist.txt")
		dryRun := fs.Bool("dry-run", false, "Preview cascading deletions without mutating files")
		asJSON := fs.Bool("json", false, "Output JSON report")
		_ = fs.Parse(flagArgs)
		if *query == "" {
			*query = posQ
		}
		rep, err := vault.ForgetMemory(vaultDir, *query, *topicSlug, *entitySlug, *blockPII, *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory forget failed: %v\n", err)
			os.Exit(1)
		}
		if !*dryRun {
			_ = webui.SyncMarkdownConsole(vaultDir)
		}
		if *asJSON {
			printJSON(rep)
			return
		}
		mode := "Executed"
		if *dryRun {
			mode = "[DRY-RUN] Previewed"
		}
		fmt.Printf("%s surgical forget: expiredFacts=%d, memoryLinesRemoved=%d, deletedFiles=%d, scrubbedFiles=%d, sourceConvs=%d, denylistUpdated=%v\n",
			mode, len(rep.ExpiredFactIDs), rep.MemoryLinesRemoved, len(rep.DeletedFiles), len(rep.ScrubbedFiles), len(rep.SourceConvIDs), rep.DenylistUpdated)

	case "entities":
		fs := flag.NewFlagSet("memory entities", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		list, err := vault.ListEntities(vaultDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory entities failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(list)
			return
		}
		for _, d := range list {
			fmt.Printf("- [%s] %s (%s): %s\n", d.Slug, d.Name, d.Kind, d.Context)
		}

	case "add-entity":
		fs := flag.NewFlagSet("memory add-entity", flag.ExitOnError)
		slug := fs.String("slug", "", "Entity slug (defaults to kebab-case of --name)")
		name := fs.String("name", "", "Display name")
		kind := fs.String("kind", "person", "Entity kind (person, organization, project)")
		email := fs.String("email", "", "Comma-separated email(s)")
		handle := fs.String("handle", "", "Comma-separated handle(s)")
		space := fs.String("space", "", "Comma-separated Google Chat space ID(s)")
		ctxStr := fs.String("context", "", "1-line relationship / role summary")
		note := fs.String("note", "", "Optional interaction or working-style note to append")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		var emails, handles, spaces []string
		if *email != "" {
			emails = []string{*email}
		}
		if *handle != "" {
			handles = []string{*handle}
		}
		if *space != "" {
			spaces = []string{*space}
		}
		ent, err := vault.UpsertEntity(vaultDir, vault.EntityDossier{
			Slug:    *slug,
			Name:    *name,
			Kind:    *kind,
			Emails:  emails,
			Handles: handles,
			Spaces:  spaces,
			Context: *ctxStr,
		}, *note)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory add-entity failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(ent)
			return
		}
		fmt.Printf("Saved entity dossier %s (%s) at %s\n", ent.Slug, ent.Name, ent.Path)

	case "lookup":
		posQ, flagArgs := splitFirstPositionalAndFlags(rest)
		fs := flag.NewFlagSet("memory lookup", flag.ExitOnError)
		qFlag := fs.String("query", "", "Query string (name, email, handle, space ID, or context)")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(flagArgs)
		if posQ == "" {
			posQ = *qFlag
		}
		hits, err := vault.LookupEntities(vaultDir, posQ)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory lookup failed: %v\n", err)
			os.Exit(1)
		}
		if _, isTainted := checkCallerTaintedEnv(vaultDir); isTainted {
			for i := range hits {
				hits[i].Context = ""
				hits[i].Notes = nil
				hits[i].Spaces = nil
				hits[i].Path = ""
			}
		}
		if *asJSON {
			printJSON(hits)
			return
		}
		if len(hits) == 0 {
			fmt.Printf("No entity dossiers matched %q.\n", posQ)
			return
		}
		for _, d := range hits {
			fmt.Printf("- [%s] %s (%s): %s (emails=%s, spaces=%s)\n",
				d.Slug, d.Name, d.Kind, d.Context, strings.Join(d.Emails, ","), strings.Join(d.Spaces, ","))
			for _, n := range d.Notes {
				fmt.Printf("    * %s\n", n)
			}
		}

	case "commit":
		fs := flag.NewFlagSet("memory commit", flag.ExitOnError)
		msg := fs.String("message", "", "Optional commit message for the private Vault git repository")
		msgShort := fs.String("m", "", "Alias for --message")
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(rest)
		m := strings.TrimSpace(*msg)
		if m == "" {
			m = strings.TrimSpace(*msgShort)
		}
		// Advance dream cursor strictly inside `hq memory commit` (before git commit so working tree stays clean).
		_ = vault.AdvanceDreamCursor(vaultDir, strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")), time.Now().UTC())
		committed, err := vault.CommitVault(vaultDir, m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq memory commit failed: %v\n", err)
			os.Exit(1)
		}
		sha := ""
		if out, sErr := vault.SanitizedGitCommand(vaultDir, "rev-parse", "--short=12", "HEAD").Output(); sErr == nil {
			sha = strings.TrimSpace(string(out))
		}
		if *asJSON {
			printJSON(map[string]interface{}{
				"vaultDir":  vaultDir,
				"committed": committed,
				"commit":    sha,
			})
			return
		}
		if committed {
			fmt.Printf("Committed private Vault changes (%s) in %s\n", sha, vaultDir)
		} else {
			fmt.Printf("Private Vault working tree clean (HEAD %s) in %s\n", sha, vaultDir)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown memory subcommand: %s (expected status, topics, search, dream-plan, eval, add-fact, prune-fact, compact, verify-transcript, forget, entities, add-entity, lookup, commit)\n", sub)
		os.Exit(1)
	}
}

func runSkillCLI(rootDir, vaultDir string, args []string) {
	sub, args := splitSubcommand(args, "list")
	templateDir := resolveTemplateDir(rootDir)

	switch sub {
	case "list", "ls":
		fs := flag.NewFlagSet("skill list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "Output JSON")
		_ = fs.Parse(args)
		items, err := vault.ListSkills(rootDir, vaultDir, templateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hq skill list failed: %v\n", err)
			os.Exit(1)
		}
		if *asJSON {
			printJSON(items)
			return
		}
		var core, installed, available []vault.SkillInfo
		for _, it := range items {
			switch it.Tier {
			case "core":
				core = append(core, it)
			case "starter-available":
				available = append(available, it)
			default:
				installed = append(installed, it)
			}
		}
		fmt.Printf("Core OS Skills (%d):\n", len(core))
		for _, it := range core {
			overlayNote := ""
			if it.ActiveRSIRules > 0 {
				overlayNote = fmt.Sprintf(" [overlay: %d rule(s)]", it.ActiveRSIRules)
			}
			fmt.Printf("  - %-20s%s\n", it.Name, overlayNote)
		}
		fmt.Printf("\nInstalled Vault User-Space Skills (%s/skills, %d):\n", vaultDir, len(installed))
		if len(installed) == 0 {
			fmt.Println("  (none installed yet — install a starter with `hq skill add <slug>` or create <vault>/skills/<name>/SKILL.md)")
		}
		for _, it := range installed {
			status := it.Tier
			if it.Modified {
				status += ", modified"
			}
			if it.ActiveRSIRules > 0 {
				status += fmt.Sprintf(", %d rsi rule(s)", it.ActiveRSIRules)
			}
			fmt.Printf("  - %-20s (%s)\n", it.Name, status)
		}
		fmt.Printf("\nAvailable Starter Templates (`hq skill add <slug|--all>`, %d):\n", len(available))
		if len(available) == 0 {
			fmt.Println("  (all starter templates installed)")
		}
		for _, it := range available {
			fmt.Printf("  - %-20s %s\n", it.Name, truncateDisplay(it.Description, 68))
		}

	case "add", "install":
		fs := flag.NewFlagSet("skill add", flag.ExitOnError)
		all := fs.Bool("all", false, "Install all starter skill templates into <vault>/skills/")
		force := fs.Bool("force", false, "Overwrite base template markdown while preserving existing RSI rules")
		asJSON := fs.Bool("json", false, "Output JSON")
		slugs, flagArgs := splitPositionalAndFlags(args)
		_ = fs.Parse(flagArgs)
		for _, a := range fs.Args() {
			slugs = append(slugs, a)
		}
		if *all {
			slugs = append([]string(nil), vault.StarterSkillSlugs...)
		}
		if len(slugs) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq skill add <slug...> [--all] [--force] [--json]")
			fmt.Fprintf(os.Stderr, "available starters: %s\n", strings.Join(vault.StarterSkillSlugs, ", "))
			os.Exit(1)
		}
		sort.Strings(slugs)
		type addResult struct {
			Slug    string `json:"slug"`
			Path    string `json:"path"`
			Changed bool   `json:"changed"`
		}
		var results []addResult
		for _, slug := range slugs {
			changed, dstPath, err := vault.AddStarterSkill(vaultDir, templateDir, slug, *force)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hq skill add %q failed: %v\n", slug, err)
				os.Exit(1)
			}
			results = append(results, addResult{Slug: slug, Path: dstPath, Changed: changed})
		}
		if *asJSON {
			printJSON(results)
			return
		}
		for _, r := range results {
			state := "already installed"
			if r.Changed {
				state = "installed"
			}
			fmt.Printf("Vault skill %-16s: %s (%s)\n", r.Slug, state, r.Path)
		}

	case "remove", "rm", "uninstall":
		fs := flag.NewFlagSet("skill remove", flag.ExitOnError)
		_ = fs.Parse(args)
		if len(fs.Args()) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hq skill remove <slug>")
			os.Exit(1)
		}
		for _, slug := range fs.Args() {
			if err := vault.RemoveVaultSkill(vaultDir, slug); err != nil {
				fmt.Fprintf(os.Stderr, "hq skill remove %q failed: %v\n", slug, err)
				os.Exit(1)
			}
			fmt.Printf("Removed Vault skill %q (pre-removal Vault snapshot recorded if git-initialized)\n", slug)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown skill subcommand: %s (expected list, add, remove)\n", sub)
		os.Exit(1)
	}
}
