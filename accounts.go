package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// accountSettings stores purchase batches and per-credential assignments. A credential's cost and resale
// multiplier come from the batch it was assigned to, so creating or switching to a new batch never changes
// the profit of credentials bought earlier.
type accountSettings struct {
	Batches      map[string]batchSettings   `json:"batches"`
	DefaultBatch map[string]string          `json:"defaultBatch"` // channel -> batch ID for newly seen credentials
	Accounts     map[string]accountOverride `json:"accounts"`

	// Fields from v1.1.0, converted into batches when loaded.
	LegacyMultiplier *float64                   `json:"multiplier,omitempty"`
	LegacyChannels   map[string]accountOverride `json:"channels,omitempty"`
	// LegacyAccounts holds 1.1.0 per-account overrides until the credential is seen again and its channel is known.
	LegacyAccounts map[string]accountOverride `json:"legacyAccounts,omitempty"`
}

type batchSettings struct {
	Name       string   `json:"name"`
	Date       string   `json:"date,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// accountOverride assigns a credential to a batch. An entry existing at all means the credential has been
// assigned (possibly to no batch), so it is never re-assigned to a later default batch.
type accountOverride struct {
	Batch      string   `json:"batch,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	Note       string   `json:"note,omitempty"`
}

var knownChannels = []string{"antigravity", "gemini", "codex", "xai"}

func defaultAccountSettings() accountSettings {
	return accountSettings{Batches: map[string]batchSettings{}, DefaultBatch: map[string]string{}, Accounts: map[string]accountOverride{}}
}

func loadAccountSettings(path string) accountSettings {
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
	return settings
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
		multiplier := legacy.Multiplier
		if multiplier == nil && global != 1 {
			multiplier = &global
		}
		id := "migrated-" + channel
		s.Batches[id] = batchSettings{Name: channelLabel(channel) + " 默认（1.1.0 迁移）", Cost: legacy.Cost, Multiplier: multiplier}
		s.DefaultBatch[channel] = id
	}
	if global != 1 {
		for _, channel := range knownChannels {
			if _, ok := s.DefaultBatch[channel]; !ok {
				value := global
				s.Batches["migrated-global"] = batchSettings{Name: "全局倍率（1.1.0 迁移）", Multiplier: &value}
				s.DefaultBatch[channel] = "migrated-global"
			}
		}
	}
	s.LegacyMultiplier, s.LegacyChannels = nil, nil
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

// effectiveAccount resolves cost and multiplier: account override, then the account's batch. Credentials
// without a batch have no cost and sell at the model price (multiplier 1).
func (s accountSettings) effectiveAccount(id string) (view accountTerms) {
	view.Multiplier = 1
	override := s.Accounts[id]
	view.Batch, view.Note = override.Batch, override.Note
	if batch, ok := s.Batches[override.Batch]; ok {
		view.BatchName = batch.Name
		if batch.Cost != nil {
			view.Cost, view.CostSource = batch.Cost, "batch"
		}
		if batch.Multiplier != nil {
			view.Multiplier, view.MultiplierSource = *batch.Multiplier, "batch"
		}
	} else {
		view.Batch = ""
	}
	if override.Cost != nil {
		view.Cost, view.CostSource = override.Cost, "account"
	}
	if override.Multiplier != nil {
		view.Multiplier, view.MultiplierSource = *override.Multiplier, "account"
	}
	return view
}

type accountTerms struct {
	Batch, BatchName, CostSource, MultiplierSource, Note string
	Cost                                                 *float64
	Multiplier                                           float64
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
		if negative(batch.Cost) || negative(batch.Multiplier) {
			return fmt.Errorf("批次 %s 的成本或倍率不能为负数", batch.Name)
		}
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
		if negative(override.Cost) || negative(override.Multiplier) {
			return fmt.Errorf("%s 的成本或倍率不能为负数", id)
		}
		if _, ok := batches[override.Batch]; !ok {
			override.Batch = ""
		}
		override.Note = strings.TrimSpace(override.Note)
		accounts[id] = override
	}
	settings = accountSettings{Batches: batches, DefaultBatch: defaults, Accounts: accounts}

	state.Lock()
	defer state.Unlock()
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
	if err := saveAccountSettingsLocked(settings); err != nil {
		return err
	}
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
