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

func TestAccountSettingsInheritanceAndProfit(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	path := setupDashboard(t, "prices:\n  '*':\n    input: 1\n",
		usageRecord{Provider: "codex", Model: "gpt-5.4", AuthID: "codex-a.json", RequestedAt: now, Detail: usageDetail{InputTokens: 100_000_000}},
		usageRecord{Provider: "codex", Model: "gpt-5.4", AuthID: "codex-b.json", RequestedAt: now, Detail: usageDetail{InputTokens: 100_000_000}},
	)
	if err := updateAccountSettings(`{"multiplier":0.5,"channels":{"codex":{"cost":30,"multiplier":0.3}},"accounts":{"codex-b.json":{"cost":20,"note":" 闲鱼 "},"empty":{}}}`); err != nil {
		t.Fatal(err)
	}
	payload := buildDashboard("all", now)
	a, b := findAccount(t, payload, "codex-a.json"), findAccount(t, payload, "codex-b.json")
	if a.CostSource != "channel" || *a.Cost != 30 || a.MultiplierSource != "channel" || !near(a.Total.Revenue, 30) || !near(*a.Profit, 0) || !near(*a.Payback, 1) {
		t.Fatalf("account a = %#v", a)
	}
	if b.CostSource != "account" || *b.Cost != 20 || !near(*b.Profit, 10) || b.Note != "闲鱼" {
		t.Fatalf("account b = %#v", b)
	}
	if payload.Summary.CostedAccounts != 2 || !near(payload.Summary.AccountCost, 50) || !near(payload.Summary.Profit, 10) {
		t.Fatalf("summary = %#v", payload.Summary)
	}
	if _, ok := payload.Settings.Accounts["empty"]; ok {
		t.Fatal("empty overrides should be dropped")
	}
	if err := updateAccountSettings(`{"accounts":{"x":{"cost":-1}}}`); err == nil {
		t.Fatal("negative cost should be rejected")
	}
	if err := applyConfig([]byte("data-file: " + path + "\ncurrency: CNY\n")); err != nil {
		t.Fatal(err)
	}
	if got := state.accounts.Accounts["codex-b.json"]; got.Cost == nil || *got.Cost != 20 {
		t.Fatalf("settings after reload = %#v", state.accounts)
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
