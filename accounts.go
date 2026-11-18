package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// accountSettings stores purchase records, per-credential assignments and one resale multiplier timeline per
// channel. A credential's cost comes from the purchase record it was entered with and never changes when later
// purchases arrive. The multiplier is the channel's selling price over time: every request is valued at its
// channel's multiplier in effect when the request was made.
type accountSettings struct {
	Batches      map[string]batchSettings   `json:"batches"`
	Accounts     map[string]accountOverride `json:"accounts"`
	ChannelRates map[string][]rateChange    `json:"channelRates"`

	// Fields from earlier versions, converted when loaded.
	LegacyRates        []rateChange               `json:"rates,omitempty"`        // v1.3.0 global timeline
	LegacyDefaultBatch map[string]string          `json:"defaultBatch,omitempty"` // v1.2.0/v1.3.0
	LegacyMultiplier   *float64                   `json:"multiplier,omitempty"`   // v1.1.0
	LegacyChannels     map[string]accountOverride `json:"channels,omitempty"`     // v1.1.0
	LegacyAccounts     map[string]accountOverride `json:"legacyAccounts,omitempty"`
}

// batchSettings is a purchase record created when new credentials are entered.
type batchSettings struct {
	Name    string   `json:"name"`
	Channel string   `json:"channel,omitempty"`
	Date    string   `json:"date,omitempty"`
	Cost    *float64 `json:"cost,omitempty"`
	Note    string   `json:"note,omitempty"`
	// LegacyMultiplier is the v1.2.0 per-batch multiplier, converted into rate timelines when loaded.
	LegacyMultiplier *float64 `json:"multiplier,omitempty"`
}

// accountOverride records a credential's purchase. An entry existing at all means the credential has been
// entered (possibly without cost); credentials without an entry are new and wait for their cost.
type accountOverride struct {
	Batch string   `json:"batch,omitempty"`
	Cost  *float64 `json:"cost,omitempty"`
	Note  string   `json:"note,omitempty"`
	// RetiredAt (RFC3339) freezes the credential's profit: requests after it no longer count as its revenue.
	RetiredAt        string   `json:"retiredAt,omitempty"`
	LegacyMultiplier *float64 `json:"multiplier,omitempty"` // v1.1.0/v1.2.0 only; ignored after migration
}

// rateChange sets a channel's resale multiplier from a moment on. The first entry has an empty From and
// applies to everything before the first change.
type rateChange struct {
	From       string  `json:"from"`
	Multiplier float64 `json:"multiplier"`
	Note       string  `json:"note,omitempty"`
}

// channelTimeline is one channel's parsed timeline: multipliers[i] applies from starts[i-1] (or the beginning).
type channelTimeline struct {
	starts      []time.Time
	multipliers []float64
}

func (c channelTimeline) index(at time.Time) int {
	if at.IsZero() {
		return 0
	}
	return sort.Search(len(c.starts), func(i int) bool { return c.starts[i].After(at) })
}

// rateSchedule buckets requests by the union of every channel's change times, so the multiplier of a bucket
// is known for whichever channel its credential belongs to, even if that classification changes later.
type rateSchedule struct {
	boundaries []time.Time
	channels   map[string]channelTimeline
}

func (s rateSchedule) period(at time.Time) int {
	if at.IsZero() {
		return 0
	}
	return sort.Search(len(s.boundaries), func(i int) bool { return s.boundaries[i].After(at) })
}

func (s rateSchedule) periodStart(period int) time.Time {
	if period <= 0 || period > len(s.boundaries) {
		return time.Time{}
	}
	return s.boundaries[period-1]
}

func (s rateSchedule) periods() int { return len(s.boundaries) + 1 }

// channelIndex is the position in the channel's own timeline that covers the given bucket, or -1.
func (s rateSchedule) channelIndex(channel string, period int) int {
	timeline, ok := s.channels[channel]
	if !ok {
		return -1
	}
	return timeline.index(s.periodStart(period))
}

func (s rateSchedule) multiplier(channel string, period int) float64 {
	index := s.channelIndex(channel, period)
	if index < 0 {
		return 1
	}
	return s.channels[channel].multipliers[index]
}

func (s rateSchedule) key() string {
	parts := make([]string, len(s.boundaries))
	for i, start := range s.boundaries {
		parts[i] = start.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join(parts, ",")
}

// parseChannelRates validates one channel's timeline, sorts it and normalizes times to the given location.
func parseChannelRates(rates []rateChange, location *time.Location) ([]rateChange, channelTimeline, error) {
	if len(rates) == 0 {
		rates = []rateChange{{Multiplier: 1}}
	}
	initial := -1
	out := make([]rateChange, 0, len(rates))
	for i, rate := range rates {
		rate.From, rate.Note = strings.TrimSpace(rate.From), strings.TrimSpace(rate.Note)
		if rate.Multiplier < 0 {
			return nil, channelTimeline{}, fmt.Errorf("售价倍率不能为负数")
		}
		if rate.From == "" {
			if initial >= 0 {
				return nil, channelTimeline{}, fmt.Errorf("只能有一个初始倍率")
			}
			initial = i
		} else {
			at, err := time.Parse(time.RFC3339, rate.From)
			if err != nil {
				return nil, channelTimeline{}, fmt.Errorf("倍率生效时间 %q 格式不正确", rate.From)
			}
			rate.From = at.In(location).Format(time.RFC3339)
		}
		out = append(out, rate)
	}
	if initial < 0 {
		return nil, channelTimeline{}, fmt.Errorf("缺少初始倍率")
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].From == "" || out[j].From == "" {
			return out[i].From == ""
		}
		a, _ := time.Parse(time.RFC3339, out[i].From)
		b, _ := time.Parse(time.RFC3339, out[j].From)
		return a.Before(b)
	})
	timeline := channelTimeline{multipliers: []float64{out[0].Multiplier}}
	for _, rate := range out[1:] {
		at, _ := time.Parse(time.RFC3339, rate.From)
		if n := len(timeline.starts); n > 0 && !at.After(timeline.starts[n-1]) {
			return nil, channelTimeline{}, fmt.Errorf("同一渠道两次调价的生效时间不能相同")
		}
		timeline.starts = append(timeline.starts, at)
		timeline.multipliers = append(timeline.multipliers, rate.Multiplier)
	}
	return out, timeline, nil
}

// parseRates builds the bucket schedule from every channel's change times plus the given extra boundaries
// (credential retirement times), so each bucket lies entirely before or after any of them.
func parseRates(all map[string][]rateChange, location *time.Location, extra []time.Time) (map[string][]rateChange, rateSchedule, error) {
	out := make(map[string][]rateChange, len(all))
	schedule := rateSchedule{channels: make(map[string]channelTimeline, len(all))}
	seen := make(map[int64]bool)
	for channel, rates := range all {
		channel = strings.TrimSpace(channel)
		if channel == "" {
			continue
		}
		normalized, timeline, err := parseChannelRates(rates, location)
		if err != nil {
			return nil, rateSchedule{}, fmt.Errorf("%s：%w", channelLabel(channel), err)
		}
		out[channel] = normalized
		schedule.channels[channel] = timeline
		for _, start := range timeline.starts {
			if !seen[start.UnixNano()] {
				seen[start.UnixNano()] = true
				schedule.boundaries = append(schedule.boundaries, start)
			}
		}
	}
	for _, at := range extra {
		if !seen[at.UnixNano()] {
			seen[at.UnixNano()] = true
			schedule.boundaries = append(schedule.boundaries, at)
		}
	}
	sort.Slice(schedule.boundaries, func(i, j int) bool { return schedule.boundaries[i].Before(schedule.boundaries[j]) })
	return out, schedule, nil
}

var knownChannels = []string{"antigravity", "gemini", "codex", "xai"}

func defaultAccountSettings() accountSettings {
	return accountSettings{Batches: map[string]batchSettings{}, Accounts: map[string]accountOverride{}, ChannelRates: map[string][]rateChange{}}
}

func loadAccountSettings(path string, location *time.Location) (accountSettings, rateSchedule) {
	settings := defaultAccountSettings()
	if saved, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(saved, &settings)
	}
	if settings.Batches == nil {
		settings.Batches = map[string]batchSettings{}
	}
	if settings.Accounts == nil {
		settings.Accounts = map[string]accountOverride{}
	}
	if settings.ChannelRates == nil {
		settings.ChannelRates = map[string][]rateChange{}
	}
	migrateLegacySettings(&settings)
	migrateBatchMultipliers(&settings, location)
	migrateGlobalRates(&settings)
	rates, schedule, err := parseRates(settings.ChannelRates, location, retirementTimes(settings.Accounts))
	if err != nil {
		rates, schedule, _ = parseRates(nil, location, retirementTimes(settings.Accounts))
	}
	settings.ChannelRates = rates
	return settings, schedule
}

// migrateLegacySettings turns v1.1.0 channel defaults into purchase records and its multipliers into each
// channel's initial rate. Credentials that only inherited a channel default are asked for their cost again.
func migrateLegacySettings(s *accountSettings) {
	if s.LegacyChannels == nil && s.LegacyMultiplier == nil {
		return
	}
	global := 1.0
	if s.LegacyMultiplier != nil {
		global = *s.LegacyMultiplier
	}
	for _, channel := range knownChannels {
		multiplier := global
		if legacy, ok := s.LegacyChannels[channel]; ok && legacy.LegacyMultiplier != nil {
			multiplier = *legacy.LegacyMultiplier
		}
		if _, ok := s.ChannelRates[channel]; !ok && (multiplier != 1 || s.LegacyChannels[channel].LegacyMultiplier != nil) {
			s.ChannelRates[channel] = []rateChange{{Multiplier: multiplier, Note: "从 1.1.0 迁移"}}
		}
	}
	for channel, legacy := range s.LegacyChannels {
		if legacy.Cost != nil {
			s.Batches["migrated-"+channel] = batchSettings{Name: channelLabel(channel) + " 默认（1.1.0 迁移）", Channel: channel, Cost: legacy.Cost}
		}
	}
	s.LegacyMultiplier, s.LegacyChannels = nil, nil
}

// migrateBatchMultipliers converts v1.2.0 per-batch multipliers into a global timeline (handled further by
// migrateGlobalRates): the earliest batch's multiplier is the initial rate and each later batch with a different
// multiplier starts a new rate at the beginning of its purchase date.
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
	for _, overrides := range []map[string]accountOverride{s.Accounts, s.LegacyAccounts} {
		for id, override := range overrides {
			override.LegacyMultiplier = nil
			overrides[id] = override
		}
	}
	if len(s.LegacyRates) > 0 || len(found) == 0 {
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
	s.LegacyRates = rates
}

// migrateGlobalRates copies the v1.3.0 global timeline to every channel without its own timeline, keeping the
// numbers unchanged until each channel's rates are corrected. Pending default batches and 1.1.0 overrides are
// folded in: from v1.4.0 on, new credentials are entered through the new-account prompt instead.
func migrateGlobalRates(s *accountSettings) {
	if len(s.LegacyRates) > 0 {
		for _, channel := range knownChannels {
			if _, ok := s.ChannelRates[channel]; !ok {
				s.ChannelRates[channel] = append([]rateChange(nil), s.LegacyRates...)
			}
		}
	}
	for id, override := range s.LegacyAccounts {
		if _, ok := s.Accounts[id]; !ok {
			s.Accounts[id] = override
		}
	}
	s.LegacyRates, s.LegacyDefaultBatch, s.LegacyAccounts = nil, nil, nil
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

// effectiveAccount resolves the purchase cost: the account's own cost, otherwise its purchase record's cost.
func (s accountSettings) effectiveAccount(id string) (view accountTerms) {
	override, entered := s.Accounts[id]
	view.Pending = !entered
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
	Pending                            bool
}

func saveAccountSettingsLocked(settings accountSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	versionSettingsLocked("accounts")
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
			return fmt.Errorf("进货记录名称不能为空")
		}
		if negative(batch.Cost) {
			return fmt.Errorf("%s 的成本不能为负数", batch.Name)
		}
		batch.LegacyMultiplier = nil
		batches[id] = batch
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
		if override.RetiredAt = strings.TrimSpace(override.RetiredAt); override.RetiredAt != "" {
			at, errTime := time.Parse(time.RFC3339, override.RetiredAt)
			if errTime != nil {
				return fmt.Errorf("%s 的下架时间格式不正确", id)
			}
			override.RetiredAt = at.Format(time.RFC3339)
		}
		accounts[id] = override
	}
	state.Lock()
	defer state.Unlock()
	// Keep credentials entered since the page was loaded (through the new-account prompt in another tab).
	for id, override := range state.accounts.Accounts {
		if _, ok := accounts[id]; !ok {
			if _, exists := batches[override.Batch]; !exists {
				override.Batch = ""
			}
			accounts[id] = override
		}
	}
	rates, schedule, err := parseRates(settings.ChannelRates, state.location, retirementTimes(accounts))
	if err != nil {
		return err
	}
	settings = accountSettings{Batches: batches, Accounts: accounts, ChannelRates: rates}
	return commitAccountSettingsLocked(settings, schedule)
}

// commitAccountSettingsLocked saves settings and, when the multiplier change times moved, rebuilds the usage
// buckets from the raw records. The caller must hold state's write lock.
func commitAccountSettingsLocked(settings accountSettings, schedule rateSchedule) error {
	rows := state.rows
	if schedule.key() != state.schedule.key() && state.dataFile != "" {
		var err error
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

// newAccountEntry is one credential submitted from the new-account prompt.
type newAccountEntry struct {
	ID      string   `json:"id"`
	Channel string   `json:"channel"`
	Cost    *float64 `json:"cost"` // nil records the credential without cost
	Note    string   `json:"note"`
}

// enterNewAccounts records the purchase of newly detected credentials. Credentials of the same channel and
// cost entered together become one purchase record named after the channel and date.
func enterNewAccounts(raw string, now time.Time) error {
	var entries []newAccountEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return fmt.Errorf("parse new accounts: %w", err)
	}
	state.Lock()
	defer state.Unlock()
	settings := state.accounts
	batches := make(map[string]batchSettings, len(settings.Batches)+1)
	for id, batch := range settings.Batches {
		batches[id] = batch
	}
	accounts := make(map[string]accountOverride, len(settings.Accounts)+len(entries))
	for id, override := range settings.Accounts {
		accounts[id] = override
	}
	day := now.In(state.location)
	groups := make(map[string]string) // channel + cost -> batch ID
	for i, entry := range entries {
		entry.ID, entry.Channel, entry.Note = strings.TrimSpace(entry.ID), strings.TrimSpace(entry.Channel), strings.TrimSpace(entry.Note)
		if entry.ID == "" {
			continue
		}
		if entry.Cost != nil && *entry.Cost < 0 {
			return fmt.Errorf("%s 的成本不能为负数", entry.ID)
		}
		override := accountOverride{Note: entry.Note}
		if entry.Cost != nil {
			key := entry.Channel + "|" + fmt.Sprint(*entry.Cost)
			batchID, ok := groups[key]
			if !ok {
				batchID = fmt.Sprintf("p%s-%d", day.Format("20060102150405"), i)
				cost := *entry.Cost
				batches[batchID] = batchSettings{Name: fmt.Sprintf("%s %s 进货 ¥%s", channelLabel(entry.Channel), day.Format("01-02"), formatAmount(cost)), Channel: entry.Channel, Date: day.Format(time.DateOnly), Cost: &cost}
				groups[key] = batchID
			}
			override.Batch = batchID
		}
		accounts[entry.ID] = override
	}
	settings.Batches, settings.Accounts = batches, accounts
	return commitAccountSettingsLocked(settings, state.schedule)
}

func formatAmount(value float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}

// retirementTimes lists the retirement moments of all credentials.
func retirementTimes(accounts map[string]accountOverride) []time.Time {
	var times []time.Time
	for _, override := range accounts {
		if at, ok := override.retiredTime(); ok {
			times = append(times, at)
		}
	}
	return times
}

func (o accountOverride) retiredTime() (time.Time, bool) {
	if o.RetiredAt == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, o.RetiredAt)
	return at, err == nil
}

// setRetirement retires a credential at the given RFC3339 time, or restores it when at is empty.
func setRetirement(id, at string) error {
	id, at = strings.TrimSpace(id), strings.TrimSpace(at)
	if id == "" {
		return fmt.Errorf("缺少账号")
	}
	if at != "" {
		parsed, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return fmt.Errorf("下架时间格式不正确")
		}
		at = parsed.Format(time.RFC3339)
	}
	state.Lock()
	defer state.Unlock()
	settings := state.accounts
	accounts := make(map[string]accountOverride, len(settings.Accounts)+1)
	for key, override := range settings.Accounts {
		accounts[key] = override
	}
	override := accounts[id]
	override.RetiredAt = at
	accounts[id] = override
	rates, schedule, err := parseRates(settings.ChannelRates, state.location, retirementTimes(accounts))
	if err != nil {
		return err
	}
	settings.Accounts, settings.ChannelRates = accounts, rates
	return commitAccountSettingsLocked(settings, schedule)
}
