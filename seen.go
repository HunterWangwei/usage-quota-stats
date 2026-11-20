package main

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// Credentials' added time: the first moment the plugin saw each credential, either in the host's auth list or
// in a usage record. It is stored once and never moved later, because the host reports the auth file's
// modification time, which changes whenever a token refresh rewrites the file.

func seenFile(dataFile string) string { return dataFile + ".seen.json" }

func loadSeen(path string) map[string]time.Time {
	seen := make(map[string]time.Time)
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &seen)
	}
	return seen
}

func saveSeenLocked() {
	if state.dataFile == "" {
		return
	}
	if raw, err := json.MarshalIndent(state.seen, "", "  "); err == nil {
		_ = os.WriteFile(seenFile(state.dataFile), append(raw, '\n'), 0o600)
	}
}

// noteSeenLocked records at as id's added time unless an earlier time is already known. The caller holds
// state's write lock and calls saveSeenLocked when it returns true.
func noteSeenLocked(id string, at time.Time) bool {
	id = strings.TrimSpace(id)
	if id == "" || at.IsZero() {
		return false
	}
	if known, ok := state.seen[id]; ok && !at.Before(known) {
		return false
	}
	state.seen[id] = at.UTC()
	return true
}

// backfillSeenLocked fills the added time of credentials that only appear in older usage data with their
// first request. The caller holds state's write lock.
func backfillSeenLocked() {
	changed := false
	for _, row := range state.rows {
		if noteSeenLocked(row.AuthID, row.FirstUsed) {
			changed = true
		}
	}
	if changed {
		saveSeenLocked()
	}
}

// noteListedAccounts records credentials from the host's auth list, using the creation time the host reports
// when it is earlier than now.
func noteListedAccounts(entries []authFileEntry) {
	now := time.Now()
	state.Lock()
	defer state.Unlock()
	changed := false
	for _, entry := range entries {
		id := entry.ID
		if id == "" {
			id = entry.Name
		}
		at := now
		if !entry.CreatedAt.IsZero() && entry.CreatedAt.Before(now) {
			at = entry.CreatedAt
		}
		if _, known := state.seen[id]; !known || at.Before(state.seen[id]) {
			if noteSeenLocked(id, at) {
				changed = true
			}
		}
	}
	if changed {
		saveSeenLocked()
	}
}
