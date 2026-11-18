package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	dailyBackupEnabled = false // background zips would race with t.TempDir cleanup
	os.Exit(m.Run())
}

// Retiring a credential freezes its profit at the retirement moment, exact within the day.
func TestRetiredAccountProfitFreezes(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	retire := time.Date(2026, 11, 10, 15, 0, 0, 0, shanghai)
	setupDashboard(t, "prices:\n  '*':\n    input: 1\n",
		use("codex", "a.json", retire.Add(-2*time.Hour)),
		use("codex", "a.json", retire.Add(time.Hour)), // same day, after retirement
	)
	if err := enterNewAccounts(`[{"id":"a.json","channel":"codex","cost":25}]`, retire); err != nil {
		t.Fatal(err)
	}
	settings := mustSettingsJSON(t, map[string][]rateChange{"codex": {{Multiplier: 0.3}}})
	settings = strings.Replace(settings, `"batch":"`, `"retiredAt":"2026-11-10T07:00:00Z","batch":"`, 1)
	if err := updateAccountSettings(settings); err != nil {
		t.Fatal(err)
	}
	a := findAccount(t, buildDashboard("all", retire.Add(48*time.Hour)), "a.json")
	if a.RetiredAt == nil || !near(a.Total.Cost, 100) || !near(a.Total.Revenue, 30) || !near(*a.Profit, 5) {
		t.Fatalf("retired account total = %#v, profit %v", a.Total, *a.Profit)
	}
	if a.AfterRetired.Requests != 1 || !near(a.AfterRetired.Cost, 100) || a.Range.Requests != 2 || len(a.PeriodCost) != 2 || a.PeriodCost[1] != 0 {
		t.Fatalf("after retirement = %#v, range %#v, period cost %v", a.AfterRetired, a.Range, a.PeriodCost)
	}
	if err := updateAccountSettings(strings.Replace(settings, "2026-11-10T07:00:00Z", "soon", 1)); err == nil {
		t.Fatal("bad retirement time should be rejected")
	}
}

func TestSettingsVersionsAndRestore(t *testing.T) {
	path := setupDashboard(t, "")
	if err := updatePrices(`{"gpt":{"input":1}}`); err != nil {
		t.Fatal(err)
	}
	if err := updatePrices(`{"gpt":{"input":2}}`); err != nil { // backs up the input:1 version
		t.Fatal(err)
	}
	info := listBackups()
	var version string
	for _, file := range info.Files {
		if file.Kind == "prices" && file.Restores {
			version = file.Name
		}
	}
	if version == "" || info.Dir != filepath.Join(filepath.Dir(path), "usage-quota-stats-backups") {
		t.Fatalf("backups = %#v", info)
	}
	if err := restoreSettingsBackup(version); err != nil {
		t.Fatal(err)
	}
	if state.config.Prices["gpt"].Input != 1 {
		t.Fatalf("restored prices = %#v", state.config.Prices)
	}
	for _, bad := range []string{"../usage.jsonl", "daily-20261118.zip", "prices-x.json"} {
		if err := restoreSettingsBackup(bad); err == nil {
			t.Fatalf("restoring %q should be refused", bad)
		}
	}
	if _, _, err := readBackup("../../etc/passwd"); err == nil {
		t.Fatal("path traversal must be refused")
	}
}

func TestFullBackupContainsDataAndSettings(t *testing.T) {
	setupDashboard(t, "prices:\n  '*':\n    input: 1\n", use("codex", "a.json", time.Now()))
	if err := enterNewAccounts(`[{"id":"a.json","channel":"codex","cost":25}]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := updatePrices(`{"*":{"input":1}}`); err != nil {
		t.Fatal(err)
	}
	name, err := createManualBackup(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, fileName, err := readBackup(name)
	if err != nil || fileName != name {
		t.Fatalf("read backup: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, file := range reader.File {
		f, _ := file.Open()
		raw, _ := io.ReadAll(f)
		_ = f.Close()
		contents[file.Name] = string(raw)
	}
	if !strings.Contains(contents["usage.jsonl"], `"auth_id":"a.json"`) || !strings.Contains(contents["usage.jsonl.accounts.json"], `"cost": 25`) || contents["usage.jsonl.prices.json"] == "" {
		t.Fatalf("backup contents = %v", contents)
	}
	if now, _, err := readBackup("now"); err != nil || len(now) == 0 {
		t.Fatalf("download now: %v", err)
	}
}

func TestDailyBackupOncePerDayAndPruned(t *testing.T) {
	path := setupDashboard(t, "", use("codex", "a.json", time.Now()))
	dir := backupDirFor(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 9; day++ { // older daily backups beyond the kept number get pruned
		_ = os.WriteFile(filepath.Join(dir, "daily-202611"+twoDigits(day)+".zip"), []byte("x"), 0o600)
	}
	dailyBackupEnabled = true
	defer func() { dailyBackupEnabled = false }()
	lastDailyBackup.Store("")
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	maybeDailyBackup(now)
	for i := 0; i < 100 && dailyBackupBusy.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	maybeDailyBackup(now) // same day: no second backup
	var daily []string
	for _, file := range listBackups().Files {
		if file.Kind == "daily" {
			daily = append(daily, file.Name)
		}
	}
	if len(daily) != dailyBackupsKept || !fileExists(filepath.Join(dir, "daily-"+now.In(state.location).Format("20060102")+".zip")) || fileExists(filepath.Join(dir, "daily-20261101.zip")) {
		t.Fatalf("daily backups = %v", daily)
	}
}

func twoDigits(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }
