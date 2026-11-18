package main

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func setupDashboard(t *testing.T, yamlConfig string, records ...usageRecord) string {
	t.Helper()
	path := t.TempDir() + "/usage.jsonl"
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\n" + yamlConfig)); err != nil {
		t.Fatal(err)
	}
	authInfo.Lock()
	authInfo.entries, authInfo.byKey = nil, map[string]authFileEntry{}
	authInfo.Unlock()
	for _, record := range records {
		raw, _ := json.Marshal(record)
		if _, err := handleMethod("usage.handle", raw); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func findAccount(t *testing.T, payload dashboardPayload, id string) accountView {
	t.Helper()
	for _, account := range payload.Accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %q not found in %#v", id, payload.Accounts)
	return accountView{}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDashboardRangesFailuresAndCosts(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -10)
	setupDashboard(t, "currency: CNY\nprices:\n  gpt-5.4:\n    input: 2\n    output: 8\n    cache-read: 1\n    cache-write: 3\n",
		usageRecord{Provider: "openai", Model: "gpt-5.4", AuthID: "config-a", RequestedAt: now, Detail: usageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, CacheCreationTokens: 10}},
		usageRecord{Provider: "openai", Model: "gpt-5.4", AuthID: "config-a", RequestedAt: old, Detail: usageDetail{InputTokens: 1_000_000}},
		usageRecord{Provider: "openai", Model: "gpt-5.4", AuthID: "config-a", RequestedAt: now, Failed: true},
		usageRecord{Provider: "openai", Model: "gpt-x", AuthID: "config-a", RequestedAt: now, Detail: usageDetail{InputTokens: 5}},
	)
	week := buildDashboard("7d", now)
	a := findAccount(t, week, "config-a")
	if a.Range.Requests != 2 || a.Range.Failures != 1 || a.Range.UnpricedRequests != 1 {
		t.Fatalf("7d stats = %#v", a.Range)
	}
	wantCost := (50*2 + 20*8 + 40*1 + 10*3) / 1_000_000.0
	if !near(a.Range.Cost, wantCost) || !near(a.Range.Revenue, wantCost) {
		t.Fatalf("7d cost = %v revenue = %v, want %v", a.Range.Cost, a.Range.Revenue, wantCost)
	}
	if !near(a.Total.Cost, wantCost+2) || a.Total.Requests != 3 {
		t.Fatalf("total = %#v", a.Total)
	}
	if len(week.Daily) != 7 || week.Daily[6].Day != "2026-11-15" || week.Daily[6].Failures != 1 {
		t.Fatalf("daily = %#v", week.Daily)
	}
	if all := findAccount(t, buildDashboard("all", now), "config-a"); all.Range.Requests != 3 {
		t.Fatalf("all-time requests = %d", all.Range.Requests)
	}
	if a.Cost != nil || a.Profit != nil || len(week.Unpriced) != 1 || week.Unpriced[0].Model != "gpt-x" {
		t.Fatalf("unexpected cost %v / unpriced %#v", a.Cost, week.Unpriced)
	}
}

func TestUnusedListedCredentialsAppear(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	setupDashboard(t, "")
	authInfo.Lock()
	entry := authFileEntry{ID: "xai-new.json", Name: "xai-new.json", Provider: "xai", Email: "new@example.com", Disabled: true}
	authInfo.entries, authInfo.byKey = []authFileEntry{entry}, map[string]authFileEntry{"xai-new.json": entry}
	authInfo.Unlock()
	payload := buildDashboard("today", now)
	a := findAccount(t, payload, "xai-new.json")
	if a.Models == nil {
		t.Fatal("models must be an empty list, not null")
	}
	if !a.Listed || a.Email != "new@example.com" || !a.Disabled || a.Channel != "xai" || a.LastUsed != nil {
		t.Fatalf("listed account = %#v", a)
	}
	if payload.Currency != "CNY" {
		t.Fatalf("default currency = %q, want CNY", payload.Currency)
	}
	if len(payload.Daily) != 7 {
		t.Fatalf("today chart should cover 7 days, got %d", len(payload.Daily))
	}
}

func TestDashboardPageEmbedsEscapedData(t *testing.T) {
	setupDashboard(t, "", usageRecord{Model: "</script><b>", AuthID: "config-a", RequestedAt: time.Now()})
	page, err := renderDashboardPage("all", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if strings.Contains(html, "</script><b>") || strings.Contains(html, dashboardDataMarkerString) || !strings.Contains(html, `</script>`) {
		t.Fatal("initial data must be embedded and HTML-safe")
	}
	raw, err := handleMethod("management.handle", []byte(`{"Query":{"api":["1"],"range":["30d"]}}`))
	if err != nil || !strings.Contains(string(raw), `"StatusCode":200`) {
		t.Fatalf("api response = %s, %v", raw, err)
	}
}

const dashboardDataMarkerString = "/*INITIAL_DATA*/"

func TestLegacyRecordsLoadIntoDayBuckets(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	legacy := `{"provider":"openai","model":"m","auth_id":"a","requested_at":"2026-11-14T20:00:00Z","input_tokens":5}` + "\n" +
		`{"provider":"openai","model":"m","auth_id":"a","requested_at":"0001-01-01T00:00:00Z","input_tokens":5}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\n")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if a := findAccount(t, buildDashboard("today", now), "a"); a.Range.Requests != 1 || a.Total.Requests != 2 {
		t.Fatalf("legacy buckets: range %d total %d", a.Range.Requests, a.Total.Requests)
	}
}

func codexUse(id string, at time.Time) usageRecord {
	return usageRecord{Provider: "codex", Model: "gpt-5.4", AuthID: id, RequestedAt: at, Detail: usageDetail{InputTokens: 100_000_000}}
}

func recordAll(t *testing.T, records ...usageRecord) {
	t.Helper()
	for _, record := range records {
		raw, _ := json.Marshal(record)
		if _, err := handleMethod("usage.handle", raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewBatchDoesNotChangeEarlierAccounts(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	path := setupDashboard(t, "prices:\n  '*':\n    input: 1\n", codexUse("codex-old.json", now))
	// Batch 1 becomes the codex default: the already-seen, unassigned credential joins it on the next page load.
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"第一批","cost":30,"multiplier":0.3}},"defaultBatch":{"codex":"b1"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod("management.handle", []byte(`{"Query":{"api":["1"]}}`)); err != nil {
		t.Fatal(err)
	}
	// Batch 2 with higher cost and multiplier becomes the default; only credentials seen afterwards use it.
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"第一批","cost":30,"multiplier":0.3},"b2":{"name":"第二批","cost":60,"multiplier":0.8}},"defaultBatch":{"codex":"b2"},"accounts":{"codex-old.json":{"batch":"b1"}}}`); err != nil {
		t.Fatal(err)
	}
	recordAll(t, codexUse("codex-new.json", now))
	payload := buildDashboard("all", now)
	old, fresh := findAccount(t, payload, "codex-old.json"), findAccount(t, payload, "codex-new.json")
	if old.Batch != "b1" || *old.Cost != 30 || old.Multiplier != 0.3 || !near(*old.Profit, 0) || old.BatchName != "第一批" {
		t.Fatalf("old account changed: %#v", old)
	}
	if fresh.Batch != "b2" || *fresh.Cost != 60 || !near(fresh.Total.Revenue, 80) || !near(*fresh.Profit, 20) {
		t.Fatalf("new account = %#v", fresh)
	}
	// A per-account override wins over its batch.
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"第一批","cost":30,"multiplier":0.3},"b2":{"name":"第二批","cost":60,"multiplier":0.8}},"defaultBatch":{"codex":"b2"},"accounts":{"codex-old.json":{"batch":"b1","cost":10}}}`); err != nil {
		t.Fatal(err)
	}
	if old = findAccount(t, buildDashboard("all", now), "codex-old.json"); *old.Cost != 10 || old.CostSource != "account" || old.MultiplierSource != "batch" {
		t.Fatalf("override = %#v", old)
	}
	// The page did not send codex-new.json, but its assignment must survive the save and a reload.
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\n")); err != nil {
		t.Fatal(err)
	}
	if got := state.accounts.Accounts["codex-new.json"].Batch; got != "b2" {
		t.Fatalf("new account batch after reload = %q", got)
	}
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"","cost":1}}}`); err == nil {
		t.Fatal("empty batch name should be rejected")
	}
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"x","cost":-1}}}`); err == nil {
		t.Fatal("negative batch cost should be rejected")
	}
}

func TestUnassignedAccountStaysOutOfLaterDefault(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	setupDashboard(t, "", codexUse("codex-a.json", now))
	// The user explicitly keeps codex-a.json out of any batch.
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"第一批","cost":30}},"defaultBatch":{"codex":"b1"},"accounts":{"codex-a.json":{}}}`); err != nil {
		t.Fatal(err)
	}
	recordAll(t, codexUse("codex-a.json", now))
	if a := findAccount(t, buildDashboard("all", now), "codex-a.json"); a.Batch != "" || a.Cost != nil {
		t.Fatalf("explicitly unassigned account was moved: %#v", a)
	}
}

func TestLegacyChannelDefaultsMigrateToBatches(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/usage.jsonl"
	legacy := `{"multiplier":0.5,"channels":{"codex":{"cost":50}},"accounts":{"codex-x.json":{"cost":20}}}`
	if err := os.WriteFile(path+".accounts.json", []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig([]byte("data-file: " + path + "\nprices:\n  '*':\n    input: 1\n")); err != nil {
		t.Fatal(err)
	}
	recordAll(t, codexUse("codex-y.json", now), codexUse("codex-x.json", now))
	payload := buildDashboard("all", now)
	x, y := findAccount(t, payload, "codex-x.json"), findAccount(t, payload, "codex-y.json")
	// codex-y inherited channel cost 50 and global ×0.5 in 1.1.0; codex-x had its own cost 20 on top of that.
	if y.Batch != "migrated-codex" || *y.Cost != 50 || y.Multiplier != 0.5 || !near(*y.Profit, 0) {
		t.Fatalf("migrated account = %#v", y)
	}
	if x.Batch != "migrated-codex" || *x.Cost != 20 || x.CostSource != "account" || x.Multiplier != 0.5 || !near(*x.Profit, 30) {
		t.Fatalf("pre-configured account = %#v", x)
	}
	if payload.Settings.LegacyChannels != nil || payload.Settings.LegacyMultiplier != nil || len(payload.Settings.LegacyAccounts) != 0 {
		t.Fatalf("legacy fields should be cleared after migration: %#v", payload.Settings)
	}
}
