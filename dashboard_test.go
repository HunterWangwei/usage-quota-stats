package main

import (
	"encoding/json"
	"fmt"
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
	payload := buildDashboard("today", now, true)
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

func use(provider, id string, at time.Time) usageRecord {
	return usageRecord{Provider: provider, Model: "m", AuthID: id, RequestedAt: at, Detail: usageDetail{InputTokens: 100_000_000}}
}

func mustSettingsJSON(t *testing.T, rates map[string][]rateChange) string {
	t.Helper()
	state.RLock()
	settings := state.accounts
	state.RUnlock()
	settings.ChannelRates = rates
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The user's scenario: Codex accounts bought at 25, a later Codex purchase at 26 while the Codex multiplier
// rises from 0.3 to 0.35. Antigravity keeps its own multiplier. Each request is valued at its channel's
// multiplier in effect when it was made, and each account keeps the cost it was entered with.
func TestChannelTimelinesAndNewAccountEntry(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	change := time.Date(2026, 11, 10, 15, 0, 0, 0, shanghai)
	path := setupDashboard(t, "prices:\n  '*':\n    input: 1\n",
		use("codex", "old.json", change.Add(-2*time.Hour)), // same day, before the change
		use("antigravity", "ag.json", change.Add(-2*time.Hour)),
	)
	payload := buildDashboard("all", change)
	if payload.Summary.Pending != 2 || !findAccount(t, payload, "old.json").Pending {
		t.Fatalf("pending = %d", payload.Summary.Pending)
	}
	if err := enterNewAccounts(`[{"id":"old.json","channel":"codex","cost":25},{"id":"ag.json","channel":"antigravity","cost":null,"note":"自用"}]`, change); err != nil {
		t.Fatal(err)
	}
	if err := updateAccountSettings(mustSettingsJSON(t, map[string][]rateChange{
		"codex":       {{Multiplier: 0.3}},
		"antigravity": {{Multiplier: 0.5}},
	})); err != nil {
		t.Fatal(err)
	}
	// The Codex change is entered after the fact; requests already recorded after it must be revalued.
	recordAll(t, use("codex", "old.json", change.Add(time.Hour)), use("antigravity", "ag.json", change.Add(time.Hour)))
	if err := updateAccountSettings(mustSettingsJSON(t, map[string][]rateChange{
		"codex":       {{From: "2026-11-10T07:00:00Z", Multiplier: 0.35, Note: "第二批进货调价"}, {Multiplier: 0.3}},
		"antigravity": {{Multiplier: 0.5}},
	})); err != nil {
		t.Fatal(err)
	}
	recordAll(t, use("codex", "old.json", change.Add(24*time.Hour)), use("codex", "new.json", change.Add(24*time.Hour)))
	if err := enterNewAccounts(`[{"id":"new.json","channel":"codex","cost":26}]`, change.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := change.Add(48 * time.Hour)
	payload = buildDashboard("all", now)
	old, fresh, ag := findAccount(t, payload, "old.json"), findAccount(t, payload, "new.json"), findAccount(t, payload, "ag.json")
	// old: 100 before the change at 0.3, 200 after at 0.35 -> 30 + 70 = 100 revenue, cost 25.
	if *old.Cost != 25 || !near(old.Total.Revenue, 100) || !near(*old.Profit, 75) || old.Pending {
		t.Fatalf("old account = cost %v revenue %v profit %v", *old.Cost, old.Total.Revenue, *old.Profit)
	}
	// new: 100 after the change at 0.35, cost 26, in its own purchase record.
	if *fresh.Cost != 26 || !near(fresh.Total.Revenue, 35) || !near(*fresh.Profit, 9) || fresh.Batch == old.Batch || !strings.Contains(fresh.BatchName, "Codex 11-11 进货 ¥26") {
		t.Fatalf("new account = cost %v revenue %v batch %q", *fresh.Cost, fresh.Total.Revenue, fresh.BatchName)
	}
	// Antigravity is unaffected by the Codex change: 200 at 0.5, no cost.
	if !near(ag.Total.Revenue, 100) || ag.Cost != nil || ag.Note != "自用" || ag.Pending {
		t.Fatalf("antigravity account = revenue %v cost %v", ag.Total.Revenue, ag.Cost)
	}
	codex := payload.Rates["codex"]
	if len(codex) != 2 || !near(codex[0].Revenue, 30) || !near(codex[1].Revenue, 105) || !codex[1].Current || payload.Summary.Multipliers["codex"] != 0.35 || payload.Summary.Multipliers["antigravity"] != 0.5 {
		t.Fatalf("codex rates = %#v, multipliers %v", codex, payload.Summary.Multipliers)
	}
	if codex[1].From != "2026-11-10T15:00:00+08:00" || len(payload.PeriodStarts) != 2 || len(old.PeriodCost) != 2 || !near(old.PeriodCost[1], 200) {
		t.Fatalf("periods = %v, old period cost %v", payload.PeriodStarts, old.PeriodCost)
	}
	for _, point := range buildDashboard("30d", now).Daily {
		if point.Day == "2026-11-10" && !near(point.Revenue, 30+35+50+50) {
			t.Fatalf("change-day revenue = %v", point.Revenue)
		}
	}
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\nprices:\n  '*':\n    input: 1\n")); err != nil {
		t.Fatal(err)
	}
	if again := findAccount(t, buildDashboard("all", now), "old.json"); !near(again.Total.Revenue, 100) || *again.Cost != 25 {
		t.Fatalf("after reload = revenue %v cost %v", again.Total.Revenue, *again.Cost)
	}
	for _, bad := range []map[string][]rateChange{
		{"codex": {{From: "2026-11-10T07:00:00Z", Multiplier: 0.35}}},
		{"codex": {{Multiplier: 0.3}, {Multiplier: 0.4}}},
		{"codex": {{Multiplier: 0.3}, {From: "tomorrow", Multiplier: 0.4}}},
		{"codex": {{Multiplier: -1}}},
		{"codex": {{Multiplier: 0.3}, {From: "2026-11-10T07:00:00Z", Multiplier: 0.4}, {From: "2026-11-10T15:00:00+08:00", Multiplier: 0.5}}},
	} {
		if err := updateAccountSettings(mustSettingsJSON(t, bad)); err == nil {
			t.Fatalf("expected %v to be rejected", bad)
		}
	}
	if err := enterNewAccounts(`[{"id":"x.json","channel":"codex","cost":-1}]`, now); err == nil {
		t.Fatal("negative cost should be rejected")
	}
}

func TestV130SettingsMigrate(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/usage.jsonl"
	v130 := `{"batches":{"b1":{"name":"第一批","cost":25}},"defaultBatch":{"codex":"b1"},"accounts":{"codex-a.json":{"batch":"b1"}},
		"rates":[{"from":"","multiplier":0.3},{"from":"2026-11-10T15:00:00+08:00","multiplier":0.35}]}`
	if err := os.WriteFile(path+".accounts.json", []byte(v130), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\n")); err != nil {
		t.Fatal(err)
	}
	recordAll(t, codexUse("codex-a.json", now), codexUse("codex-b.json", now))
	payload := buildDashboard("all", now)
	// The global timeline is copied to every channel; the old default batch no longer captures new accounts.
	if len(payload.Settings.ChannelRates) != len(knownChannels) || payload.Summary.Multipliers["xai"] != 0.35 || payload.Settings.LegacyRates != nil || payload.Settings.LegacyDefaultBatch != nil {
		t.Fatalf("settings = %#v", payload.Settings)
	}
	if a, b := findAccount(t, payload, "codex-a.json"), findAccount(t, payload, "codex-b.json"); a.Pending || *a.Cost != 25 || !b.Pending || b.Cost != nil {
		t.Fatalf("a = %#v, b = %#v", a, b)
	}
}

func TestV120BatchMultipliersMigrate(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	v120 := `{"batches":{"b1":{"name":"第一批","date":"2026-11-01","cost":25,"multiplier":0.3},"b2":{"name":"第二批","date":"2026-11-10","cost":26,"multiplier":0.35}},"accounts":{"a.json":{"batch":"b1","multiplier":0.9}}}`
	if err := os.WriteFile(path+".accounts.json", []byte(v120), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig([]byte("data-file: " + path + "\ntimezone: Asia/Shanghai\n")); err != nil {
		t.Fatal(err)
	}
	rates := state.accounts.ChannelRates["codex"]
	if len(rates) != 2 || rates[0].Multiplier != 0.3 || rates[1].From != "2026-11-10T00:00:00+08:00" || rates[1].Multiplier != 0.35 {
		t.Fatalf("migrated rates = %#v", rates)
	}
	if state.accounts.Batches["b1"].LegacyMultiplier != nil || state.accounts.Accounts["a.json"].LegacyMultiplier != nil {
		t.Fatal("legacy multipliers should be cleared")
	}
}

func TestV110SettingsMigrate(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	path := t.TempDir() + "/usage.jsonl"
	legacy := `{"multiplier":0.5,"channels":{"codex":{"cost":50,"multiplier":0.3}},"accounts":{"codex-x.json":{"cost":20}}}`
	if err := os.WriteFile(path+".accounts.json", []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig([]byte("data-file: " + path + "\nprices:\n  '*':\n    input: 1\n")); err != nil {
		t.Fatal(err)
	}
	recordAll(t, codexUse("codex-x.json", now), codexUse("codex-y.json", now))
	payload := buildDashboard("all", now)
	if payload.Summary.Multipliers["codex"] != 0.3 || payload.Summary.Multipliers["xai"] != 0.5 {
		t.Fatalf("multipliers = %v", payload.Summary.Multipliers)
	}
	x, y := findAccount(t, payload, "codex-x.json"), findAccount(t, payload, "codex-y.json")
	if *x.Cost != 20 || x.Pending || !near(x.Total.Revenue, 30) || !y.Pending {
		t.Fatalf("x = %#v, y pending %v", x, y.Pending)
	}
	if _, ok := payload.Settings.Batches["migrated-codex"]; !ok {
		t.Fatal("channel default cost should become a purchase record")
	}
}

func TestRefreshSkipsUnchangedAndModelsOnDemand(t *testing.T) {
	setupDashboard(t, "prices:\n  '*':\n    input: 1\n", use("codex", "a.json", time.Now()))
	first := buildDashboard("all", time.Now())
	if first.Accounts[0].Models != nil {
		t.Fatal("refresh payload should not carry per-model details")
	}
	query := func(q string) string {
		raw, err := handleMethod("management.handle", []byte(`{"Query":`+q+`}`))
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Result struct{ Body []byte }
		}
		_ = json.Unmarshal(raw, &env)
		return string(env.Result.Body)
	}
	since := fmt.Sprint(first.Version)
	if body := query(`{"api":["1"],"since":["` + since + `"]}`); body != `{"unchanged":true}` {
		t.Fatalf("unchanged refresh = %s", body)
	}
	recordAll(t, use("codex", "a.json", time.Now()))
	if body := query(`{"api":["1"],"since":["` + since + `"]}`); strings.Contains(body, "unchanged") {
		t.Fatal("refresh after new usage must return data")
	}
	if body := query(`{"models":["1"],"account":["a.json"]}`); !strings.Contains(body, `"a.json":[{"model":"m"`) {
		t.Fatalf("models = %s", body)
	}
}

func TestUnpricedVariantsGroupedByBase(t *testing.T) {
	now := time.Now()
	setupDashboard(t, "",
		usageRecord{Model: "gemini-3.8-flash-high", AuthID: "a", RequestedAt: now},
		usageRecord{Model: "gemini-3.8-flash-low", AuthID: "a", RequestedAt: now},
		usageRecord{Model: "gemini-3.8-flash-tiered", AuthID: "a", RequestedAt: now},
	)
	p := buildDashboard("all", now)
	if len(p.Unpriced) != 2 || p.Unpriced[0].Model != "gemini-3.8-flash" || p.Unpriced[0].Requests != 2 || strings.Join(p.Unpriced[0].Variants, ",") != "gemini-3.8-flash-high,gemini-3.8-flash-low" {
		t.Fatalf("unpriced = %#v", p.Unpriced)
	}
	if err := updatePrices(`{"gemini-3.8-flash":{"input":1}}`); err != nil {
		t.Fatal(err)
	}
	p = buildDashboard("all", now)
	if len(p.Unpriced) != 1 || p.Unpriced[0].Model != "gemini-3.8-flash-tiered" || len(p.Prices[0].Matches) != 2 {
		t.Fatalf("after pricing base: unpriced %#v, matches %#v", p.Unpriced, p.Prices[0].Matches)
	}
}

func TestAddedTimeKeepsEarliestAndSurvivesReload(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }
	path := setupDashboard(t, "", use("codex", "old.json", day(3)), use("codex", "old.json", day(1)))
	// Listed by the host with a file time after its first request: the first request wins.
	authInfo.Lock()
	authInfo.entries = nil
	authInfo.Unlock()
	noteListedAccounts([]authFileEntry{{ID: "old.json", CreatedAt: day(5)}, {ID: "new.json", CreatedAt: day(10)}})
	p := buildDashboard("all", day(20))
	if a := findAccount(t, p, "old.json"); a.AddedAt == nil || !a.AddedAt.Equal(day(1)) {
		t.Fatalf("old added = %v", a.AddedAt)
	}
	// A later token refresh rewrites the file; the added time must not move.
	noteListedAccounts([]authFileEntry{{ID: "new.json", CreatedAt: day(15)}})
	if got := state.seen["new.json"]; !got.Equal(day(10)) {
		t.Fatalf("new added = %v", got)
	}
	if err := applyConfig([]byte("data-file: " + path + "\n")); err != nil {
		t.Fatal(err)
	}
	if got := state.seen["new.json"]; !got.Equal(day(10)) || !state.seen["old.json"].Equal(day(1)) {
		t.Fatalf("after reload = %v", state.seen)
	}
	// A credential first seen through a request gets that request's time.
	recordAll(t, use("codex", "fresh.json", day(18)))
	if got := state.seen["fresh.json"]; !got.Equal(day(18)) {
		t.Fatalf("fresh added = %v", got)
	}
}
