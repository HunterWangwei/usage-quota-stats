package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// accountSettings stores purchase batches, per-credential assignments and the resale multiplier timeline.
// A credential's cost comes from the batch it was assigned to and never changes when later batches arrive.
// The multiplier is a global selling price that changes over time: every request is valued at the
// multiplier in effect when it was made, for all credentials alike.
type accountSettings struct {
	Batches      map[string]batchSettings   `json:"batches"`
	DefaultBatch map[string]string          `json:"defaultBatch"` // channel -> batch ID for newly seen credentials
	Accounts     map[string]accountOverride `json:"accounts"`
	Rates        []rateChange               `json:"rates"`

	// Fields from v1.1.0, converted when loaded.
	LegacyMultiplier *float64                   `json:"multiplier,omitempty"`
	LegacyChannels   map[string]accountOverride `json:"channels,omitempty"`
	// LegacyAccounts holds 1.1.0 per-account overrides until the credential is seen again and its channel is known.
	LegacyAccounts map[string]accountOverride `json:"legacyAccounts,omitempty"`
}

type batchSettings struct {
	Name string   `json:"name"`
	Date string   `json:"date,omitempty"`
	Cost *float64 `json:"cost,omitempty"`
	Note string   `json:"note,omitempty"`
	// LegacyMultiplier is the v1.2.0 per-batch multiplier, converted into the rate timeline when loaded.
	LegacyMultiplier *float64 `json:"multiplier,omitempty"`
}

// accountOverride assigns a credential to a batch. An entry existing at all means the credential has been
// assigned (possibly to no batch), so it is never re-assigned to a later default batch.
type accountOverride struct {
	Batch            string   `json:"batch,omitempty"`
	Cost             *float64 `json:"cost,omitempty"`
	Note             string   `json:"note,omitempty"`
	LegacyMultiplier *float64 `json:"multiplier,omitempty"` // v1.1.0/v1.2.0 only; ignored after migration
}

// rateChange sets the resale multiplier from a moment on. The first entry has an empty From and applies to
// everything before the first change.
type rateChange struct {
	From       string  `json:"from"`
	Multiplier float64 `json:"multiplier"`
	Note       string  `json:"note,omitempty"`
}

// rateSchedule is the parsed timeline: period i covers [starts[i-1], starts[i]) with multipliers[i].
type rateSchedule struct {
	starts      []time.Time
	multipliers []float64
}

func (s rateSchedule) period(at time.Time) int {
	if at.IsZero() {
		return 0
	}
	return sort.Search(len(s.starts), func(i int) bool { return s.starts[i].After(at) })
}

func (s rateSchedule) multiplier(period int) float64 {
	if period < 0 || period >= len(s.multipliers) {
		return 1
	}
	return s.multipliers[period]
}

func (s rateSchedule) key() string {
	parts := make([]string, len(s.starts))
	for i, start := range s.starts {
		parts[i] = start.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join(parts, ",")
}

// parseRates validates the timeline, sorts it and normalizes times to RFC3339 in the given location.
func parseRates(rates []rateChange, location *time.Location) ([]rateChange, rateSchedule, error) {
	if len(rates) == 0 {
		rates = []rateChange{{Multiplier: 1}}
	}
	initial := -1
	out := make([]rateChange, 0, len(rates))
	for i, rate := range rates {
		rate.From, rate.Note = strings.TrimSpace(rate.From), strings.TrimSpace(rate.Note)
		if rate.Multiplier < 0 {
			return nil, rateSchedule{}, fmt.Errorf("售价倍率不能为负数")
		}
		if rate.From == "" {
			if initial >= 0 {
				return nil, rateSchedule{}, fmt.Errorf("只能有一个初始倍率")
			}
			initial = i
		} else {
			at, err := time.Parse(time.RFC3339, rate.From)
			if err != nil {
				return nil, rateSchedule{}, fmt.Errorf("倍率生效时间 %q 格式不正确", rate.From)
			}
			rate.From = at.In(location).Format(time.RFC3339)
		}
		out = append(out, rate)
	}
	if initial < 0 {
		return nil, rateSchedule{}, fmt.Errorf("缺少初始倍率")
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].From == "" || out[j].From == "" {
			return out[i].From == ""
		}
		a, _ := time.Parse(time.RFC3339, out[i].From)
		b, _ := time.Parse(time.RFC3339, out[j].From)
		return a.Before(b)
	})
	schedule := rateSchedule{multipliers: []float64{out[0].Multiplier}}
	for _, rate := range out[1:] {
		at, _ := time.Parse(time.RFC3339, rate.From)
		if n := len(schedule.starts); n > 0 && !at.After(schedule.starts[n-1]) {
			return nil, rateSchedule{}, fmt.Errorf("两次调价的生效时间不能相同")
		}
		schedule.starts = append(schedule.starts, at)
		schedule.multipliers = append(schedule.multipliers, rate.Multiplier)
	}
	return out, schedule, nil
}

var knownChannels = []string{"antigravity", "gemini", "codex", "xai"}

func defaultAccountSettings() accountSettings {
	return accountSettings{Batches: map[string]batchSettings{}, DefaultBatch: map[string]string{}, Accounts: map[string]accountOverride{}}
}

func loadAccountSettings(path string, location *time.Location) (accountSettings, rateSchedule) {
	settings := defaultAccountSettings()
	if saved, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(saved, &settings)
	}
	if settings.Batches == nil {
		settings.Batches = map[string]batchSettings{}
	}
	if settings.DefaultBatch == nil {
		settings.DefaultBatch = map[string]string{}
	}
	if settings.Accounts == nil {
		settings.Accounts = map[string]accountOverride{}
	}
	migrateLegacySettings(&settings)
	migrateBatchMultipliers(&settings, location)
	rates, schedule, err := parseRates(settings.Rates, location)
	if err != nil {
		rates, schedule, _ = parseRates(nil, location)
	}
	settings.Rates = rates
	return settings, schedule
}

// migrateLegacySettings turns v1.1.0 channel defaults (and a non-default global multiplier) into batches that
// are the default for their channel, so credentials that inherited them keep the same numbers once assigned.
func migrateLegacySettings(s *accountSettings) {
	global := 1.0
	if s.LegacyMultiplier != nil {
		global = *s.LegacyMultiplier
	}
	if s.LegacyChannels == nil && s.LegacyMultiplier == nil {
		return
	}
	if s.LegacyAccounts == nil {
		s.LegacyAccounts = map[string]accountOverride{}
	}
	for id, override := range s.Accounts {
		if override.Batch == "" {
			s.LegacyAccounts[id] = override
			delete(s.Accounts, id)
		}
	}
	for channel, legacy := range s.LegacyChannels {
		multiplier := legacy.LegacyMultiplier
		if multiplier == nil && global != 1 {
			multiplier = &global
		}
		id := "migrated-" + channel
		s.Batches[id] = batchSettings{Name: channelLabel(channel) + " 默认（1.1.0 迁移）", Cost: legacy.Cost, LegacyMultiplier: multiplier}
		s.DefaultBatch[channel] = id
	}
	if global != 1 {
		for _, channel := range knownChannels {
			if _, ok := s.DefaultBatch[channel]; !ok {
				value := global
				s.Batches["migrated-global"] = batchSettings{Name: "全局倍率（1.1.0 迁移）", LegacyMultiplier: &value}
				s.DefaultBatch[channel] = "migrated-global"
			}
		}
	}
	s.LegacyMultiplier, s.LegacyChannels = nil, nil
}

// migrateBatchMultipliers converts v1.2.0 per-batch multipliers into the global rate timeline: the earliest
// batch's multiplier becomes the initial rate and each later batch with a different multiplier starts a new
// rate at the beginning of its purchase date. Per-account multiplier overrides no longer exist.
func migrateBatchMultipliers(s *accountSettings, location *time.Location) {
	type dated struct {
		date       string
		multiplier float64
	}
	var found []dated
	for id, batch := range s.Batches {
		if batch.LegacyMultiplier != nil {
			found = append(found, dated{batch.Date, *batch.LegacyMultiplier})
			batch.LegacyMultiplier = nil
			s.Batches[id] = batch
		}
	}
	for id, override := range s.Accounts {
		override.LegacyMultiplier = nil
		s.Accounts[id] = override
	}
	for id, override := range s.LegacyAccounts {
		override.LegacyMultiplier = nil
		s.LegacyAccounts[id] = override
	}
	if len(s.Rates) > 0 || len(found) == 0 {
		return
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].date == found[j].date {
			return found[i].multiplier < found[j].multiplier
		}
		return found[i].date < found[j].date
	})
	rates := []rateChange{{Multiplier: found[0].multiplier, Note: "从 1.2.0 批次倍率迁移"}}
	current := found[0].multiplier
	for _, item := range found[1:] {
		day, err := time.ParseInLocation(time.DateOnly, item.date, location)
		if err != nil || item.multiplier == current {
			continue
		}
		rates = append(rates, rateChange{From: day.Format(time.RFC3339), Multiplier: item.multiplier, Note: "从 1.2.0 批次倍率迁移"})
		current = item.multiplier
	}
	s.Rates = rates
}

func channelLabel(channel string) string {
	switch channel {
	case "antigravity":
		return "Antigravity"
	case "gemini":
		return "Gemini CLI"
	case "codex":
		return "Codex"
	case "xai":
		return "xAI"
	}
	return channel
}

// effectiveAccount resolves the purchase cost: the account's own cost, otherwise its batch's cost.
func (s accountSettings) effectiveAccount(id string) (view accountTerms) {
	override := s.Accounts[id]
	view.Batch, view.Note = override.Batch, override.Note
	if batch, ok := s.Batches[override.Batch]; ok {
		view.BatchName = batch.Name
		if batch.Cost != nil {
			view.Cost, view.CostSource = batch.Cost, "batch"
		}
	} else {
		view.Batch = ""
	}
	if override.Cost != nil {
		view.Cost, view.CostSource = override.Cost, "account"
	}
	return view
}

type accountTerms struct {
	Batch, BatchName, CostSource, Note string
	Cost                               *float64
}

// assignNewAccounts puts credentials seen for the first time into their channel's current default batch and
// persists the assignment. The caller must hold state's write lock.
func assignNewAccounts(channels map[string]string) error {
	changed := false
	for id, channel := range channels {
		if _, seen := state.accounts.Accounts[id]; seen || strings.TrimSpace(id) == "" {
			continue
		}
		legacy, isLegacy := state.accounts.LegacyAccounts[id]
		batch := state.accounts.DefaultBatch[channel]
		if _, exists := state.accounts.Batches[batch]; !exists {
			if !isLegacy {
				continue
			}
			batch = ""
		}
		legacy.Batch = batch
		state.accounts.Accounts[id] = legacy
		delete(state.accounts.LegacyAccounts, id)
		changed = true
	}
	if !changed || state.dataFile == "" {
		return nil
	}
	return saveAccountSettingsLocked(state.accounts)
}

func saveAccountSettingsLocked(settings accountSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(state.dataFile+".accounts.json", append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save account settings: %w", err)
	}
	return nil
}

func updateAccountSettings(raw string) error {
	settings := defaultAccountSettings()
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return fmt.Errorf("parse account settings: %w", err)
	}
	negative := func(v *float64) bool { return v != nil && *v < 0 }
	batches := make(map[string]batchSettings, len(settings.Batches))
	for id, batch := range settings.Batches {
		id = strings.TrimSpace(id)
		batch.Name, batch.Date, batch.Note = strings.TrimSpace(batch.Name), strings.TrimSpace(batch.Date), strings.TrimSpace(batch.Note)
		if id == "" {
			continue
		}
		if batch.Name == "" {
			return fmt.Errorf("批次名称不能为空")
		}
		if negative(batch.Cost) {
			return fmt.Errorf("批次 %s 的成本不能为负数", batch.Name)
		}
		batch.LegacyMultiplier = nil
		batches[id] = batch
	}
	defaults := make(map[string]string, len(settings.DefaultBatch))
	for channel, id := range settings.DefaultBatch {
		if _, ok := batches[id]; ok && strings.TrimSpace(channel) != "" {
			defaults[strings.TrimSpace(channel)] = id
		}
	}
	accounts := make(map[string]accountOverride, len(settings.Accounts))
	for id, override := range settings.Accounts {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if negative(override.Cost) {
			return fmt.Errorf("%s 的成本不能为负数", id)
		}
		override.LegacyMultiplier = nil
		if _, ok := batches[override.Batch]; !ok {
			override.Batch = ""
		}
		override.Note = strings.TrimSpace(override.Note)
		accounts[id] = override
	}
	state.Lock()
	defer state.Unlock()
	rates, schedule, err := parseRates(settings.Rates, state.location)
	if err != nil {
		return err
	}
	settings = accountSettings{Batches: batches, DefaultBatch: defaults, Accounts: accounts, Rates: rates}
	// Keep assignments made since the page was loaded (credentials first used meanwhile).
	for id, override := range state.accounts.Accounts {
		if _, ok := settings.Accounts[id]; !ok {
			if _, exists := batches[override.Batch]; !exists {
				override.Batch = ""
			}
			settings.Accounts[id] = override
		}
	}
	for id, override := range state.accounts.LegacyAccounts {
		if _, ok := settings.Accounts[id]; !ok {
			if settings.LegacyAccounts == nil {
				settings.LegacyAccounts = map[string]accountOverride{}
			}
			settings.LegacyAccounts[id] = override
		}
	}
	// Requests are bucketed by rate period, so a changed timeline needs the raw records re-read.
	rows := state.rows
	if schedule.key() != state.schedule.key() && state.dataFile != "" {
		if rows, err = loadUsage(state.dataFile, state.location, schedule); err != nil {
			return err
		}
	}
	if err := saveAccountSettingsLocked(settings); err != nil {
		return err
	}
	state.rows = rows
	state.schedule = schedule
	state.accounts = settings
	return nil
}

// assignKnownAccounts runs first-seen batch assignment for every credential with usage or listed by the host.
func assignKnownAccounts() error {
	channels := make(map[string]string)
	for _, entry := range authSnapshot().entries {
		id := entry.ID
		if id == "" {
			id = entry.Name
		}
		channels[id] = usageChannel(entry.Provider, entry.Type, entry.ID+" "+entry.Name)
	}
	state.Lock()
	defer state.Unlock()
	for _, row := range state.rows {
		if _, ok := channels[row.AuthID]; !ok {
			channels[row.AuthID] = channelForAuth(row.Provider, row.AuthType, row.AuthID)
		}
	}
	return assignNewAccounts(channels)
}
