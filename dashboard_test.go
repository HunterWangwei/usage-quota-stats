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

// The user's scenario: batch 1 bought at ¥25, later batch 2 at ¥26 while the selling multiplier rises from
// 0.3 to 0.35 for everyone. Each request is valued at the multiplier in effect when it was made, and each
// account keeps its own batch cost.
func TestRateTimelineAppliesToAllAccountsByRequestTime(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	change := time.Date(2026, 11, 10, 15, 0, 0, 0, shanghai)
	path := setupDashboard(t, "prices:\n  '*':\n    input: 1\n",
		codexUse("old.json", change.Add(-2*time.Hour)), // same day, before the change
	)
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"第一批","cost":25}},"defaultBatch":{"codex":"b1"},"rates":[{"from":"","multiplier":0.3}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod("management.handle", []byte(`{"Query":{"api":["1"]}}`)); err != nil {
		t.Fatal(err)
	}
	// The multiplier change is entered after the fact; requests already recorded after it must be revalued.
	recordAll(t, codexUse("old.json", change.Add(time.Hour)))
	if err := updateAccountSettings(`{"batches":{"b1":{"name":"第一批","cost":25},"b2":{"name":"第二批","cost":26}},"defaultBatch":{"codex":"b2"},
		"rates":[{"from":"2026-11-10T07:00:00Z","multiplier":0.35,"note":"第二批进货调价"},{"from":"","multiplier":0.3}]}`); err != nil {
		t.Fatal(err)
	}
	recordAll(t, codexUse("old.json", change.Add(24*time.Hour)), codexUse("new.json", change.Add(24*time.Hour)))
	now := change.Add(48 * time.Hour)
	payload := buildDashboard("all", now)
	old, fresh := findAccount(t, payload, "old.json"), findAccount(t, payload, "new.json")
	// old: ¥100 before the change at 0.3, ¥200 after at 0.35 → 30 + 70 = 100 revenue, cost 25.
	if old.Batch != "b1" || *old.Cost != 25 || !near(old.Total.Cost, 300) || !near(old.Total.Revenue, 100) || !near(*old.Profit, 75) {
		t.Fatalf("old account = cost %v revenue %v profit %v batch %s", *old.Cost, old.Total.Revenue, *old.Profit, old.Batch)
	}
	// new: ¥100 after the change at 0.35, cost 26.
	if fresh.Batch != "b2" || *fresh.Cost != 26 || !near(fresh.Total.Revenue, 35) || !near(*fresh.Profit, 9) {
		t.Fatalf("new account = cost %v revenue %v profit %v batch %s", *fresh.Cost, fresh.Total.Revenue, *fresh.Profit, fresh.Batch)
	}
	if len(payload.Rates) != 2 || !near(payload.Rates[0].Revenue, 30) || !near(payload.Rates[1].Revenue, 105) || !payload.Rates[1].Current || payload.Summary.Multiplier != 0.35 {
		t.Fatalf("rates = %#v, current multiplier %v", payload.Rates, payload.Summary.Multiplier)
	}
	if payload.Rates[1].From != "2026-11-10T15:00:00+08:00" || payload.Rates[0].To != payload.Rates[1].From {
		t.Fatalf("rate times should be normalized to the configured timezone: %#v", payload.Rates)
	}
	// Today's chart point mixes nothing up: the change day holds 100×0.3 + 100×0.35.
	for _, point := range buildDashboard("30d", now).Daily {
		if point.Day == "2026-11-10" && !near(point.Revenue, 65) {
			t.Fatalf("change-day revenue = %v, want 65", point.Revenue)
		}
	}
	// The timeline survives a reload, and the buckets are rebuilt from the raw records.
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\nprices:\n  '*':\n    input: 1\n")); err != nil {
		t.Fatal(err)
	}
	if again := findAccount(t, buildDashboard("all", now), "old.json"); !near(again.Total.Revenue, 100) {
		t.Fatalf("revenue after reload = %v", again.Total.Revenue)
	}
	for _, bad := range []string{
		`{"rates":[{"from":"2026-11-10T07:00:00Z","multiplier":0.35}]}`,
		`{"rates":[{"from":"","multiplier":0.3},{"from":"","multiplier":0.4}]}`,
		`{"rates":[{"from":"","multiplier":0.3},{"from":"tomorrow","multiplier":0.4}]}`,
		`{"rates":[{"from":"","multiplier":-1}]}`,
		`{"rates":[{"from":"","multiplier":0.3},{"from":"2026-11-10T07:00:00Z","multiplier":0.4},{"from":"2026-11-10T15:00:00+08:00","multiplier":0.5}]}`,
	} {
		if err := updateAccountSettings(bad); err == nil {
			t.Fatalf("expected %s to be rejected", bad)
		}
	}
}

func TestBatchMultipliersMigrateToRateTimeline(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	v120 := `{"batches":{"b1":{"name":"第一批","date":"2026-11-01","cost":25,"multiplier":0.3},"b2":{"name":"第二批","date":"2026-11-10","cost":26,"multiplier":0.35}},"accounts":{"a.json":{"batch":"b1","multiplier":0.9}}}`
	if err := os.WriteFile(path+".accounts.json", []byte(v120), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\n")); err != nil {
		t.Fatal(err)
	}
	rates := state.accounts.Rates
	if len(rates) != 2 || rates[0].From != "" || rates[0].Multiplier != 0.3 || rates[1].From != "2026-11-10T00:00:00+08:00" || rates[1].Multiplier != 0.35 {
		t.Fatalf("migrated rates = %#v", rates)
	}
	if state.accounts.Batches["b1"].LegacyMultiplier != nil || state.accounts.Accounts["a.json"].LegacyMultiplier != nil {
		t.Fatal("legacy multipliers should be cleared")
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
	// 1.1.0: codex-y inherited channel cost 50 and global ×0.5; codex-x had its own cost 20.
	if y.Batch != "migrated-codex" || *y.Cost != 50 || !near(*y.Profit, 0) {
		t.Fatalf("migrated account = %#v", y)
	}
	if x.Batch != "migrated-codex" || *x.Cost != 20 || x.CostSource != "account" || !near(*x.Profit, 30) {
		t.Fatalf("pre-configured account = %#v", x)
	}
	if payload.Summary.Multiplier != 0.5 || payload.Settings.LegacyChannels != nil || payload.Settings.LegacyMultiplier != nil {
		t.Fatalf("legacy fields should be migrated: %#v", payload.Settings)
	}
}
