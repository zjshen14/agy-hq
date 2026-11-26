package obs

import (
	"sort"
	"time"
)

const (
	maxWindowModels    = 14
	maxWindowRootTrees = 12
	maxTreeChildren    = 8
)

// HourlyBurnPoint is a 1-hour summary for the 24h hourly trend chart in the Console Tokens tab.
type HourlyBurnPoint struct {
	Hour            string  `json:"hour"`
	Label           string  `json:"label"`
	UncachedTokens  int64   `json:"uncachedTokens"`
	CacheReadTokens int64   `json:"cacheReadTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	GrossTokens     int64   `json:"grossTokens"`
	CacheHitRatio   float64 `json:"cacheHitRatio"`
	EstCostUSD      float64 `json:"estCostUsd"`
}

// ModelBurnRow summarizes token usage, prefix-cache hit ratio, and API-equivalent USD cost for one model or family.
type ModelBurnRow struct {
	Model           string  `json:"model"`
	Family          string  `json:"family"`
	Tier            string  `json:"tier"`
	ConvCount       int     `json:"convCount"`
	TurnCount       int     `json:"turnCount"`
	InputTokens     int64   `json:"inputTokens"`
	CacheReadTokens int64   `json:"cacheReadTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	UncachedTokens  int64   `json:"uncachedTokens"`
	GrossTokens     int64   `json:"grossTokens"`
	CacheHitRatio   float64 `json:"cacheHitRatio"`
	EstCostUSD      float64 `json:"estCostUsd"`
	CacheSavedUSD   float64 `json:"cacheSavedUsd"`
	SharePct        float64 `json:"sharePct"`
}

// TaskTreeChildRow summarizes a direct child subagent subtree inside a root task tree within a window.
type TaskTreeChildRow struct {
	ConversationID  string  `json:"conversationId"`
	Title           string  `json:"title"`
	Kind            string  `json:"kind"`
	Model           string  `json:"model"`
	TurnCount       int     `json:"turnCount"`
	UncachedTokens  int64   `json:"uncachedTokens"`
	CacheReadTokens int64   `json:"cacheReadTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	GrossTokens     int64   `json:"grossTokens"`
	CacheHitRatio   float64 `json:"cacheHitRatio"`
	EstCostUSD      float64 `json:"estCostUsd"`
}

// TaskTreeBurnRow summarizes a disjoint root conversation tree (Front Desk, Task, or Auto) within a window.
type TaskTreeBurnRow struct {
	ConversationID   string             `json:"conversationId"`
	TaskSlug         string             `json:"taskSlug,omitempty"`
	Title            string             `json:"title"`
	Kind             string             `json:"kind"`
	Scope            string             `json:"scope,omitempty"`
	Model            string             `json:"model"`
	TurnCount        int                `json:"turnCount"`
	DescendantCount  int                `json:"descendantCount"`
	SelfUncached     int64              `json:"selfUncachedTokens"`
	SubagentUncached int64              `json:"subagentUncachedTokens"`
	InputTokens      int64              `json:"inputTokens"`
	CacheReadTokens  int64              `json:"cacheReadTokens"`
	OutputTokens     int64              `json:"outputTokens"`
	UncachedTokens   int64              `json:"uncachedTokens"`
	GrossTokens      int64              `json:"grossTokens"`
	CacheHitRatio    float64            `json:"cacheHitRatio"`
	PeakContext      int64              `json:"peakContextTokens"`
	EstCostUSD       float64            `json:"estCostUsd"`
	CacheSavedUSD    float64            `json:"cacheSavedUsd"`
	Children         []TaskTreeChildRow `json:"children,omitempty"`
}

// WindowBreakdown pre-aggregates token usage for a selectable Console window ("today", "7d", "30d", "all").
type WindowBreakdown struct {
	Window          string            `json:"window"`
	TurnCount       int               `json:"turnCount"`
	ConvCount       int               `json:"convCount"`
	InputTokens     int64             `json:"inputTokens"`
	CacheReadTokens int64             `json:"cacheReadTokens"`
	OutputTokens    int64             `json:"outputTokens"`
	UncachedTokens  int64             `json:"uncachedTokens"`
	GrossTokens     int64             `json:"grossTokens"`
	DeepUncached    int64             `json:"deepUncachedTokens"`
	CacheHitRatio   float64           `json:"cacheHitRatio"`
	EstCostUSD      float64           `json:"estCostUsd"`
	CacheSavedUSD   float64           `json:"cacheSavedUsd"`
	ColdSpikes      int               `json:"coldSpikes,omitempty"`
	ColdSpikeTokens int64             `json:"coldSpikeTokens,omitempty"`
	ByModel         []ModelBurnRow    `json:"byModel,omitempty"`
	ByFamily        []ModelBurnRow    `json:"byFamily,omitempty"`
	TopRootTrees    []TaskTreeBurnRow `json:"topRootTrees,omitempty"`
}

func resolveDirectChildUnderRoot(idx *ObsIndex, cid, rootID string) string {
	if cid == "" || cid == rootID {
		return ""
	}
	cur := cid
	visited := map[string]bool{cur: true}
	for {
		cm, ok := idx.Conversations[cur]
		if !ok || cm.ParentConversationID == "" {
			return cur
		}
		if cm.ParentConversationID == rootID {
			return cur
		}
		if visited[cm.ParentConversationID] {
			return cur
		}
		visited[cm.ParentConversationID] = true
		cur = cm.ParentConversationID
	}
}

func populateDashboardWindows(sum *DashboardSummary, idx *ObsIndex, now time.Time) {
	if sum == nil || idx == nil {
		return
	}
	loc := now.Location()

	// 1. Build 24h HourlyTrend (24 hourly buckets ending at current local hour)
	curHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, loc)
	hourMap := make(map[string]*HourlyBurnPoint, 24)
	hourOrder := make([]string, 0, 24)
	for h := 23; h >= 0; h-- {
		ht := curHour.Add(-time.Duration(h) * time.Hour)
		key := ht.Format("2006-01-02 15:00")
		hourOrder = append(hourOrder, key)
		hourMap[key] = &HourlyBurnPoint{
			Hour:  key,
			Label: ht.Format("15:04"),
		}
	}
	for _, cm := range idx.Conversations {
		for _, t := range cm.Turns {
			hk := turnLocalHourStr(t, loc)
			if hp, ok := hourMap[hk]; ok {
				hp.UncachedTokens += t.UncachedTokens
				hp.CacheReadTokens += t.CacheReadTokens
				hp.OutputTokens += t.OutputTokens
				hp.GrossTokens += t.GrossTokens
				hp.EstCostUSD += t.EstCostUSD
			}
		}
	}
	sum.HourlyTrend = make([]HourlyBurnPoint, 0, 24)
	for _, hk := range hourOrder {
		hp := hourMap[hk]
		inTok := hp.UncachedTokens - hp.OutputTokens
		if denom := inTok + hp.CacheReadTokens; denom > 0 {
			hp.CacheHitRatio = float64(hp.CacheReadTokens) / float64(denom)
		}
		sum.HourlyTrend = append(sum.HourlyTrend, *hp)
	}

	// 2. Build selectable windows ("today", "7d", "30d", "all")
	windowNames := []string{"today", "7d", "30d", "all"}
	sum.Windows = make(map[string]WindowBreakdown, len(windowNames))
	for _, wName := range windowNames {
		cutoff := parseSinceCutoff(wName, now)
		sum.Windows[wName] = buildWindowBreakdown(idx, wName, cutoff)
	}
}

func buildWindowBreakdown(idx *ObsIndex, wName string, cutoff time.Time) WindowBreakdown {
	wb := WindowBreakdown{Window: wName}
	convsSeen := make(map[string]bool)

	type modelAcc struct {
		row   ModelBurnRow
		convs map[string]bool
	}
	byModel := make(map[string]*modelAcc)
	byFamily := make(map[string]*modelAcc)

	type childAcc struct {
		row   TaskTreeChildRow
		input int64
	}
	type treeAcc struct {
		row      TaskTreeBurnRow
		children map[string]*childAcc
	}
	trees := make(map[string]*treeAcc)

	for cid, cm := range idx.Conversations {
		rootID := cm.RootConversationID
		if rootID == "" {
			rootID = cid
		}
		if _, rootExists := idx.Conversations[rootID]; !rootExists {
			rootID = cid
		}
		rootCM := idx.Conversations[rootID]
		directChildID := resolveDirectChildUnderRoot(idx, cid, rootID)

		for _, t := range cm.Turns {
			if !turnMatchesCutoff(t, cutoff) {
				continue
			}
			convsSeen[cid] = true
			wb.TurnCount++
			wb.InputTokens += t.InputTokens
			wb.CacheReadTokens += t.CacheReadTokens
			wb.OutputTokens += t.OutputTokens
			wb.UncachedTokens += t.UncachedTokens
			wb.GrossTokens += t.GrossTokens
			wb.EstCostUSD += t.EstCostUSD
			wb.CacheSavedUSD += t.CacheSavedUSD
			if t.ColdStartInputTokens >= 50_000 {
				wb.ColdSpikes++
				wb.ColdSpikeTokens += t.ColdStartInputTokens
			}

			tModel := turnModel(t, cm)
			tTier := turnModelTier(t, cm)
			tFam := ModelFamily(tModel)
			if tTier == "deep" {
				wb.DeepUncached += t.UncachedTokens
			}

			// Model rollup
			ma := byModel[tModel]
			if ma == nil {
				ma = &modelAcc{
					row: ModelBurnRow{
						Model:  tModel,
						Family: tFam,
						Tier:   tTier,
					},
					convs: make(map[string]bool),
				}
				byModel[tModel] = ma
			}
			if !ma.convs[cid] {
				ma.convs[cid] = true
				ma.row.ConvCount++
			}
			ma.row.TurnCount++
			ma.row.InputTokens += t.InputTokens
			ma.row.CacheReadTokens += t.CacheReadTokens
			ma.row.OutputTokens += t.OutputTokens
			ma.row.UncachedTokens += t.UncachedTokens
			ma.row.GrossTokens += t.GrossTokens
			ma.row.EstCostUSD += t.EstCostUSD
			ma.row.CacheSavedUSD += t.CacheSavedUSD

			// Family rollup
			fa := byFamily[tFam]
			if fa == nil {
				fa = &modelAcc{
					row: ModelBurnRow{
						Model:  tFam,
						Family: tFam,
						Tier:   tTier,
					},
					convs: make(map[string]bool),
				}
				byFamily[tFam] = fa
			}
			if !fa.convs[cid] {
				fa.convs[cid] = true
				fa.row.ConvCount++
			}
			fa.row.TurnCount++
			fa.row.InputTokens += t.InputTokens
			fa.row.CacheReadTokens += t.CacheReadTokens
			fa.row.OutputTokens += t.OutputTokens
			fa.row.UncachedTokens += t.UncachedTokens
			fa.row.GrossTokens += t.GrossTokens
			fa.row.EstCostUSD += t.EstCostUSD
			fa.row.CacheSavedUSD += t.CacheSavedUSD

			// Disjoint Root Tree rollup
			ta := trees[rootID]
			if ta == nil {
				ta = &treeAcc{
					row: TaskTreeBurnRow{
						ConversationID:  rootID,
						TaskSlug:        rootCM.TaskSlug,
						Title:           rootCM.Title,
						Kind:            rootCM.Kind,
						Scope:           rootCM.Scope,
						Model:           rootCM.Model,
						DescendantCount: rootCM.DescendantCount,
					},
					children: make(map[string]*childAcc),
				}
				trees[rootID] = ta
			}
			ta.row.TurnCount++
			ta.row.InputTokens += t.InputTokens
			ta.row.CacheReadTokens += t.CacheReadTokens
			ta.row.OutputTokens += t.OutputTokens
			ta.row.UncachedTokens += t.UncachedTokens
			ta.row.GrossTokens += t.GrossTokens
			ta.row.EstCostUSD += t.EstCostUSD
			ta.row.CacheSavedUSD += t.CacheSavedUSD
			if t.PeakContextTokens > ta.row.PeakContext {
				ta.row.PeakContext = t.PeakContextTokens
			}
			if cid == rootID {
				ta.row.SelfUncached += t.UncachedTokens
			} else {
				ta.row.SubagentUncached += t.UncachedTokens
				if directChildID != "" {
					chCM := idx.Conversations[directChildID]
					ca := ta.children[directChildID]
					if ca == nil {
						ca = &childAcc{
							row: TaskTreeChildRow{
								ConversationID: directChildID,
								Title:          chCM.Title,
								Kind:           chCM.Kind,
								Model:          chCM.Model,
							},
						}
						ta.children[directChildID] = ca
					}
					ca.input += t.InputTokens
					ca.row.TurnCount++
					ca.row.UncachedTokens += t.UncachedTokens
					ca.row.CacheReadTokens += t.CacheReadTokens
					ca.row.OutputTokens += t.OutputTokens
					ca.row.GrossTokens += t.GrossTokens
					ca.row.EstCostUSD += t.EstCostUSD
				}
			}
		}
	}

	wb.ConvCount = len(convsSeen)
	if denom := wb.InputTokens + wb.CacheReadTokens; denom > 0 {
		wb.CacheHitRatio = float64(wb.CacheReadTokens) / float64(denom)
	}

	// Finalize ByModel
	models := make([]ModelBurnRow, 0, len(byModel))
	for _, ma := range byModel {
		r := ma.row
		if denom := r.InputTokens + r.CacheReadTokens; denom > 0 {
			r.CacheHitRatio = float64(r.CacheReadTokens) / float64(denom)
		}
		if wb.UncachedTokens > 0 {
			r.SharePct = (float64(r.UncachedTokens) / float64(wb.UncachedTokens)) * 100.0
		}
		models = append(models, r)
	}
	sort.Slice(models, func(i, j int) bool {
		if !floatApproxEqual(models[i].EstCostUSD, models[j].EstCostUSD) {
			return models[i].EstCostUSD > models[j].EstCostUSD
		}
		if models[i].UncachedTokens != models[j].UncachedTokens {
			return models[i].UncachedTokens > models[j].UncachedTokens
		}
		return models[i].Model < models[j].Model
	})
	if len(models) > maxWindowModels {
		tail := ModelBurnRow{
			Model:  "other-models",
			Family: "other",
			Tier:   "standard",
		}
		for _, m := range models[maxWindowModels-1:] {
			tail.ConvCount += m.ConvCount
			tail.TurnCount += m.TurnCount
			tail.InputTokens += m.InputTokens
			tail.CacheReadTokens += m.CacheReadTokens
			tail.OutputTokens += m.OutputTokens
			tail.UncachedTokens += m.UncachedTokens
			tail.GrossTokens += m.GrossTokens
			tail.EstCostUSD += m.EstCostUSD
			tail.CacheSavedUSD += m.CacheSavedUSD
		}
		if denom := tail.InputTokens + tail.CacheReadTokens; denom > 0 {
			tail.CacheHitRatio = float64(tail.CacheReadTokens) / float64(denom)
		}
		if wb.UncachedTokens > 0 {
			tail.SharePct = (float64(tail.UncachedTokens) / float64(wb.UncachedTokens)) * 100.0
		}
		models = append(models[:maxWindowModels-1], tail)
	}
	wb.ByModel = models

	// Finalize ByFamily
	families := make([]ModelBurnRow, 0, len(byFamily))
	for _, fa := range byFamily {
		r := fa.row
		if denom := r.InputTokens + r.CacheReadTokens; denom > 0 {
			r.CacheHitRatio = float64(r.CacheReadTokens) / float64(denom)
		}
		if wb.UncachedTokens > 0 {
			r.SharePct = (float64(r.UncachedTokens) / float64(wb.UncachedTokens)) * 100.0
		}
		families = append(families, r)
	}
	sort.Slice(families, func(i, j int) bool {
		if !floatApproxEqual(families[i].EstCostUSD, families[j].EstCostUSD) {
			return families[i].EstCostUSD > families[j].EstCostUSD
		}
		if families[i].UncachedTokens != families[j].UncachedTokens {
			return families[i].UncachedTokens > families[j].UncachedTokens
		}
		return families[i].Family < families[j].Family
	})
	wb.ByFamily = families

	// Finalize TopRootTrees
	rootRows := make([]TaskTreeBurnRow, 0, len(trees))
	for _, ta := range trees {
		r := ta.row
		if denom := r.InputTokens + r.CacheReadTokens; denom > 0 {
			r.CacheHitRatio = float64(r.CacheReadTokens) / float64(denom)
		}
		if len(ta.children) > 0 {
			kids := make([]TaskTreeChildRow, 0, len(ta.children))
			for _, ca := range ta.children {
				kr := ca.row
				if denom := ca.input + kr.CacheReadTokens; denom > 0 {
					kr.CacheHitRatio = float64(kr.CacheReadTokens) / float64(denom)
				}
				kids = append(kids, kr)
			}
			sort.Slice(kids, func(i, j int) bool {
				if kids[i].UncachedTokens != kids[j].UncachedTokens {
					return kids[i].UncachedTokens > kids[j].UncachedTokens
				}
				return kids[i].ConversationID < kids[j].ConversationID
			})
			if len(kids) > maxTreeChildren {
				kids = kids[:maxTreeChildren]
			}
			r.Children = kids
		}
		rootRows = append(rootRows, r)
	}
	sort.Slice(rootRows, func(i, j int) bool {
		if rootRows[i].UncachedTokens != rootRows[j].UncachedTokens {
			return rootRows[i].UncachedTokens > rootRows[j].UncachedTokens
		}
		if !floatApproxEqual(rootRows[i].EstCostUSD, rootRows[j].EstCostUSD) {
			return rootRows[i].EstCostUSD > rootRows[j].EstCostUSD
		}
		return rootRows[i].ConversationID < rootRows[j].ConversationID
	})
	if len(rootRows) > maxWindowRootTrees {
		rootRows = rootRows[:maxWindowRootTrees]
	}
	wb.TopRootTrees = rootRows

	return wb
}
