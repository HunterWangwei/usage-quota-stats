package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

var dashboardDataMarker = []byte("/*INITIAL_DATA*/null")

type rangeInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	From  string `json:"from"` // inclusive YYYY-MM-DD, empty for all time
	To    string `json:"to"`
}

type usageStats struct {
	Requests         int64   `json:"requests"`
	Failures         int64   `json:"failures"`
	Input            int64   `json:"input"` // regular (non-cache) input tokens
	Output           int64   `json:"output"`
	CacheRead        int64   `json:"cacheRead"`
	CacheWrite       int64   `json:"cacheWrite"`
	Cost             float64 `json:"cost"`    // API-equivalent cost at the configured model prices
	Revenue          float64 `json:"revenue"` // cost multiplied by the account's resale multiplier
	UnpricedRequests int64   `json:"unpricedRequests"`
}

type modelView struct {
	Model string `json:"model"`
	usageStats
	Priced bool `json:"priced"`
}

type accountView struct {
	ID               string      `json:"id"`
	Email            string      `json:"email"`
	Label            string      `json:"label"`
	Channel          string      `json:"channel"`
	Status           string      `json:"status"`
	StatusMessage    string      `json:"statusMessage"`
	Disabled         bool        `json:"disabled"`
	Unavailable      bool        `json:"unavailable"`
	Listed           bool        `json:"listed"`
	LastUsed         *time.Time  `json:"lastUsed"`
	Range            usageStats  `json:"range"`
	Total            usageStats  `json:"total"`
	Models           []modelView `json:"models"`
	Cost             *float64    `json:"cost"`
	CostSource       string      `json:"costSource"`
	Multiplier       float64     `json:"multiplier"`
	MultiplierSource string      `json:"multiplierSource"`
	Profit           *float64    `json:"profit"`
	Payback          *float64    `json:"payback"`
	Note             string      `json:"note"`
	Batch            string      `json:"batch"`
	BatchName        string      `json:"batchName"`
}

type dailyPoint struct {
	Day      string  `json:"day"`
	Requests int64   `json:"requests"`
	Failures int64   `json:"failures"`
	Cost     float64 `json:"cost"`
	Revenue  float64 `json:"revenue"`
}

type summaryView struct {
	Range          usageStats `json:"range"`
	TotalRevenue   float64    `json:"totalRevenue"`
	AccountCost    float64    `json:"accountCost"`
	Profit         float64    `json:"profit"`
	CostedAccounts int        `json:"costedAccounts"`
	Accounts       int        `json:"accounts"`
}

type priceMatch struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

type priceView struct {
	Model string `json:"model"`
	modelPrices
	Matches []priceMatch `json:"matches"`
}

type unpricedRow struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

type dashboardPayload struct {
	Currency    string          `json:"currency"`
	Timezone    string          `json:"timezone"`
	Today       string          `json:"today"`
	GeneratedAt time.Time       `json:"generatedAt"`
	DataFile    string          `json:"dataFile"`
	Range       rangeInfo       `json:"range"`
	Summary     summaryView     `json:"summary"`
	Daily       []dailyPoint    `json:"daily"`
	Accounts    []accountView   `json:"accounts"`
	Prices      []priceView     `json:"prices"`
	Unpriced    []unpricedRow   `json:"unpriced"`
	Settings    accountSettings `json:"settings"`
}

// chartDays caps the trend chart for long ranges so bars stay readable.
const chartDays = 90

func resolveRange(key string, now time.Time) rangeInfo {
	today := now.Format(time.DateOnly)
	daysBack := func(n int) string { return now.AddDate(0, 0, -(n - 1)).Format(time.DateOnly) }
	switch key {
	case "today":
		return rangeInfo{Key: key, Label: "今天", From: today, To: today}
	case "7d":
		return rangeInfo{Key: key, Label: "近 7 天", From: daysBack(7), To: today}
	case "30d":
		return rangeInfo{Key: key, Label: "近 30 天", From: daysBack(30), To: today}
	case "month":
		return rangeInfo{Key: key, Label: "本月", From: now.Format("2006-01") + "-01", To: today}
	default:
		return rangeInfo{Key: "all", Label: "全部", To: today}
	}
}

func (r rangeInfo) contains(day string) bool {
	if r.From == "" {
		return true
	}
	return day != "" && day >= r.From && day <= r.To
}

func (s *usageStats) add(item aggregate, regularInput int64, cost, multiplier float64, priced bool) {
	s.Requests += item.Requests
	s.Failures += item.Failures
	s.Input += regularInput
	s.Output += item.OutputTokens
	s.CacheRead += item.CacheReadTokens
	s.CacheWrite += item.CacheCreationTokens
	if priced {
		s.Cost += cost
		s.Revenue += cost * multiplier
	} else {
		s.UnpricedRequests += item.Requests
	}
}

func buildDashboard(rangeKey string, now time.Time) dashboardPayload {
	state.RLock()
	cfg := state.config
	location := state.location
	dataFile := state.dataFile
	settings := state.accounts
	aggregates := make([]aggregate, 0, len(state.rows))
	for _, row := range state.rows {
		aggregates = append(aggregates, *row)
	}
	state.RUnlock()

	now = now.In(location)
	window := resolveRange(rangeKey, now)
	payload := dashboardPayload{
		Currency: cfg.Currency, Timezone: location.String(), Today: window.To, GeneratedAt: now, DataFile: dataFile,
		Range: window, Settings: settings,
	}

	auths := authSnapshot()
	chartFrom := now.AddDate(0, 0, -(chartDays - 1)).Format(time.DateOnly)
	accounts := make(map[string]*accountView)
	models := make(map[string]map[string]*modelView)
	daily := make(map[string]*dailyPoint)
	matches := make(map[string]map[string]int64)
	unpriced := make(map[string]*unpricedRow)
	ensureAccount := func(id, provider, authType string) *accountView {
		view := accounts[id]
		if view == nil {
			view = &accountView{ID: id, Channel: channelForAuth(provider, authType, id)}
			if entry, ok := auths.byKey[id]; ok {
				applyAuthEntry(view, entry)
			}
			terms := settings.effectiveAccount(id)
			view.Batch, view.BatchName, view.Note = terms.Batch, terms.BatchName, terms.Note
			view.Cost, view.CostSource, view.Multiplier, view.MultiplierSource = terms.Cost, terms.CostSource, terms.Multiplier, terms.MultiplierSource
			accounts[id] = view
			models[id] = make(map[string]*modelView)
		}
		return view
	}

	for _, item := range aggregates {
		view := ensureAccount(item.AuthID, item.Provider, item.AuthType)
		price, priced := lookupPrice(cfg.Prices, item.Model)
		regularInput := regularInputTokens(item.Provider, item.InputTokens, item.CacheReadTokens, item.CacheCreationTokens)
		cost := calculateCost(price, item.Requests, regularInput, item.OutputTokens, item.CacheReadTokens, item.CacheCreationTokens)
		view.Total.add(item, regularInput, cost, view.Multiplier, priced)
		if !item.LastUsed.IsZero() && (view.LastUsed == nil || item.LastUsed.After(*view.LastUsed)) {
			lastUsed := item.LastUsed.In(location)
			view.LastUsed = &lastUsed
		}
		if item.Requests > 0 {
			if pattern, ok := matchPattern(cfg.Prices, item.Model); ok {
				if matches[pattern] == nil {
					matches[pattern] = make(map[string]int64)
				}
				matches[pattern][item.Model] += item.Requests
			} else {
				row := unpriced[item.Model]
				if row == nil {
					row = &unpricedRow{Model: item.Model}
					unpriced[item.Model] = row
				}
				row.Requests += item.Requests
				row.Tokens += item.InputTokens + item.OutputTokens + item.CacheReadTokens + item.CacheCreationTokens
			}
		}
		if item.Day != "" && item.Day >= chartFrom && item.Day <= window.To {
			point := daily[item.Day]
			if point == nil {
				point = &dailyPoint{Day: item.Day}
				daily[item.Day] = point
			}
			point.Requests += item.Requests
			point.Failures += item.Failures
			if priced {
				point.Cost += cost
				point.Revenue += cost * view.Multiplier
			}
		}
		if !window.contains(item.Day) {
			continue
		}
		view.Range.add(item, regularInput, cost, view.Multiplier, priced)
		model := models[item.AuthID][item.Model]
		if model == nil {
			model = &modelView{Model: item.Model, Priced: priced}
			models[item.AuthID][item.Model] = model
		}
		model.add(item, regularInput, cost, view.Multiplier, priced)
	}

	// Credentials known to the host but never used still show up so their purchase cost counts.
	for _, entry := range auths.entries {
		id := entry.ID
		if id == "" {
			id = entry.Name
		}
		if id == "" || accounts[id] != nil || accounts[entry.Name] != nil || accounts[entry.AuthIndex] != nil {
			continue
		}
		ensureAccount(id, entry.Provider, entry.Type)
	}

	for id, view := range accounts {
		view.Models = []modelView{}
		for _, model := range models[id] {
			view.Models = append(view.Models, *model)
		}
		sort.Slice(view.Models, func(i, j int) bool {
			if view.Models[i].Cost == view.Models[j].Cost {
				return view.Models[i].Requests > view.Models[j].Requests
			}
			return view.Models[i].Cost > view.Models[j].Cost
		})
		if view.Cost != nil {
			profit := view.Total.Revenue - *view.Cost
			view.Profit = &profit
			if *view.Cost > 0 {
				payback := view.Total.Revenue / *view.Cost
				view.Payback = &payback
			}
			payload.Summary.AccountCost += *view.Cost
			payload.Summary.Profit += profit
			payload.Summary.CostedAccounts++
		}
		payload.Summary.TotalRevenue += view.Total.Revenue
		addStats(&payload.Summary.Range, view.Range)
		payload.Accounts = append(payload.Accounts, *view)
	}
	payload.Summary.Accounts = len(payload.Accounts)
	sort.Slice(payload.Accounts, func(i, j int) bool {
		a, b := payload.Accounts[i], payload.Accounts[j]
		if a.Range.Cost != b.Range.Cost {
			return a.Range.Cost > b.Range.Cost
		}
		if a.Range.Requests != b.Range.Requests {
			return a.Range.Requests > b.Range.Requests
		}
		return a.ID < b.ID
	})

	payload.Daily = fillDays(daily, window, now)

	patterns := make([]string, 0, len(cfg.Prices))
	for pattern := range cfg.Prices {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, pattern := range patterns {
		price := cfg.Prices[pattern]
		price.Mode = normalizeMode(price.Mode)
		view := priceView{Model: pattern, modelPrices: price, Matches: []priceMatch{}}
		for model, requests := range matches[pattern] {
			view.Matches = append(view.Matches, priceMatch{Model: model, Requests: requests})
		}
		sort.Slice(view.Matches, func(i, j int) bool {
			if view.Matches[i].Requests == view.Matches[j].Requests {
				return view.Matches[i].Model < view.Matches[j].Model
			}
			return view.Matches[i].Requests > view.Matches[j].Requests
		})
		payload.Prices = append(payload.Prices, view)
	}
	for _, row := range unpriced {
		payload.Unpriced = append(payload.Unpriced, *row)
	}
	sort.Slice(payload.Unpriced, func(i, j int) bool {
		if payload.Unpriced[i].Requests == payload.Unpriced[j].Requests {
			return payload.Unpriced[i].Model < payload.Unpriced[j].Model
		}
		return payload.Unpriced[i].Requests > payload.Unpriced[j].Requests
	})
	if payload.Accounts == nil {
		payload.Accounts = []accountView{}
	}
	if payload.Prices == nil {
		payload.Prices = []priceView{}
	}
	if payload.Unpriced == nil {
		payload.Unpriced = []unpricedRow{}
	}
	return payload
}

func addStats(dst *usageStats, src usageStats) {
	dst.Requests += src.Requests
	dst.Failures += src.Failures
	dst.Input += src.Input
	dst.Output += src.Output
	dst.CacheRead += src.CacheRead
	dst.CacheWrite += src.CacheWrite
	dst.Cost += src.Cost
	dst.Revenue += src.Revenue
	dst.UnpricedRequests += src.UnpricedRequests
}

// fillDays returns one point per calendar day of the chart window, including days without usage.
func fillDays(daily map[string]*dailyPoint, window rangeInfo, now time.Time) []dailyPoint {
	from := window.From
	switch {
	case window.Key == "today":
		from = now.AddDate(0, 0, -6).Format(time.DateOnly)
	case from == "":
		from = now.AddDate(0, 0, -(chartDays - 1)).Format(time.DateOnly)
		earliest := ""
		for day := range daily {
			if earliest == "" || day < earliest {
				earliest = day
			}
		}
		if earliest != "" && earliest > from {
			from = earliest
		}
	}
	start, err := time.ParseInLocation(time.DateOnly, from, now.Location())
	if err != nil {
		return []dailyPoint{}
	}
	points := []dailyPoint{}
	for day := start; day.Format(time.DateOnly) <= window.To; day = day.AddDate(0, 0, 1) {
		key := day.Format(time.DateOnly)
		if point := daily[key]; point != nil {
			points = append(points, *point)
		} else {
			points = append(points, dailyPoint{Day: key})
		}
	}
	return points
}

func applyAuthEntry(view *accountView, entry authFileEntry) {
	view.Listed = true
	view.Email = strings.TrimSpace(entry.Email)
	view.Label = strings.TrimSpace(entry.Label)
	view.Status = strings.TrimSpace(entry.Status)
	view.StatusMessage = strings.TrimSpace(entry.StatusMessage)
	view.Disabled = entry.Disabled
	view.Unavailable = entry.Unavailable
}

func renderDashboardPage(rangeKey string, now time.Time) ([]byte, error) {
	data, err := json.Marshal(buildDashboard(rangeKey, now)) // json.Marshal escapes <, > and & so the data is safe inside <script>
	if err != nil {
		return nil, fmt.Errorf("encode dashboard: %w", err)
	}
	return bytes.Replace(dashboardHTML, dashboardDataMarker, data, 1), nil
}
