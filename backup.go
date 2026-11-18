package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Backups live next to the data file: versioned copies of the settings files taken before every save, and a
// daily zip of the usage records plus all settings.
const (
	settingsVersionsKept = 30
	dailyBackupsKept     = 7
)

var (
	backupNamePattern  = regexp.MustCompile(`^(accounts|prices)-\d{8}-\d{6}(-\d+)?\.json$|^(daily|manual)-\d{8}(-\d{6})?\.zip$`)
	dailyBackupBusy    atomic.Bool
	lastDailyBackup    atomic.Value // string YYYYMMDD
	dailyBackupEnabled = true
)

type backupEntry struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"` // accounts, prices, daily, manual
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	Restores bool      `json:"restores"` // settings versions can be restored from the page
}

type backupInfo struct {
	Dir     string        `json:"dir"`
	Files   []backupEntry `json:"files"`
	Sources []backupEntry `json:"sources"` // the live files that a full backup contains
}

func backupDirFor(dataFile string) string {
	return filepath.Join(filepath.Dir(dataFile), "usage-quota-stats-backups")
}

func settingsFiles(dataFile string) map[string]string {
	return map[string]string{"accounts": dataFile + ".accounts.json", "prices": dataFile + ".prices.json"}
}

// versionSettingsLocked copies a settings file into the backup directory before it is overwritten. The caller
// must hold state's write lock. Failing to back up never blocks the save.
func versionSettingsLocked(kind string) {
	if state.dataFile == "" {
		return
	}
	source := settingsFiles(state.dataFile)[kind]
	data, err := os.ReadFile(source)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return
	}
	dir := backupDirFor(state.dataFile)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	stamp := time.Now().In(state.location).Format("20060102-150405")
	name := kind + "-" + stamp + ".json"
	for i := 1; fileExists(filepath.Join(dir, name)); i++ {
		name = fmt.Sprintf("%s-%s-%d.json", kind, stamp, i)
	}
	if os.WriteFile(filepath.Join(dir, name), data, 0o600) == nil {
		pruneBackups(dir, kind+"-", settingsVersionsKept)
	}
}

// maybeDailyBackup writes one zip per calendar day in the background. Usage records are copied up to the size
// measured under the lock: the file is append-only, so that prefix ends on a complete line.
func maybeDailyBackup(now time.Time) {
	if !dailyBackupEnabled {
		return
	}
	state.RLock()
	dataFile, location := state.dataFile, state.location
	state.RUnlock()
	if dataFile == "" {
		return
	}
	day := now.In(location).Format("20060102")
	if last, _ := lastDailyBackup.Load().(string); last == day {
		return
	}
	dir := backupDirFor(dataFile)
	name := "daily-" + day + ".zip"
	if fileExists(filepath.Join(dir, name)) {
		lastDailyBackup.Store(day)
		return
	}
	if !dailyBackupBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer dailyBackupBusy.Store(false)
		if writeBackupZip(filepath.Join(dir, name)) == nil {
			lastDailyBackup.Store(day)
			pruneBackups(dir, "daily-", dailyBackupsKept)
		}
	}()
}

// createManualBackup writes a full backup now and returns its file name.
func createManualBackup(now time.Time) (string, error) {
	state.RLock()
	dataFile, location := state.dataFile, state.location
	state.RUnlock()
	dir := backupDirFor(dataFile)
	name := "manual-" + now.In(location).Format("20060102-150405") + ".zip"
	if err := writeBackupZip(filepath.Join(dir, name)); err != nil {
		return "", err
	}
	pruneBackups(dir, "manual-", dailyBackupsKept)
	return name, nil
}

func writeBackupZip(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	var buffer bytes.Buffer
	if err := buildBackupZip(&buffer); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, buffer.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	return os.Rename(temp, path)
}

// buildBackupZip writes the usage records and settings files into a zip archive.
func buildBackupZip(out io.Writer) error {
	state.RLock()
	dataFile := state.dataFile
	var dataSize int64
	if info, err := os.Stat(dataFile); err == nil {
		dataSize = info.Size()
	}
	settings := make(map[string][]byte)
	for _, path := range settingsFiles(dataFile) {
		if data, err := os.ReadFile(path); err == nil {
			settings[filepath.Base(path)] = data
		}
	}
	state.RUnlock()

	archive := zip.NewWriter(out)
	if dataSize > 0 {
		file, err := os.Open(dataFile)
		if err != nil {
			return fmt.Errorf("open usage data: %w", err)
		}
		defer func() { _ = file.Close() }()
		entry, err := archive.Create(filepath.Base(dataFile))
		if err != nil {
			return err
		}
		if _, err := io.CopyN(entry, file, dataSize); err != nil {
			return fmt.Errorf("copy usage data: %w", err)
		}
	}
	names := make([]string, 0, len(settings))
	for name := range settings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := archive.Create(name)
		if err != nil {
			return err
		}
		if _, err := entry.Write(settings[name]); err != nil {
			return err
		}
	}
	return archive.Close()
}

func listBackups() backupInfo {
	state.RLock()
	dataFile := state.dataFile
	state.RUnlock()
	info := backupInfo{Dir: backupDirFor(dataFile), Files: []backupEntry{}, Sources: []backupEntry{}}
	for _, path := range []string{dataFile, dataFile + ".accounts.json", dataFile + ".prices.json"} {
		if stat, err := os.Stat(path); err == nil {
			info.Sources = append(info.Sources, backupEntry{Name: path, Size: stat.Size(), Created: stat.ModTime()})
		}
	}
	entries, err := os.ReadDir(info.Dir)
	if err != nil {
		return info
	}
	for _, entry := range entries {
		if entry.IsDir() || !backupNamePattern.MatchString(entry.Name()) {
			continue
		}
		stat, err := entry.Info()
		if err != nil {
			continue
		}
		kind := entry.Name()[:strings.Index(entry.Name(), "-")]
		info.Files = append(info.Files, backupEntry{Name: entry.Name(), Kind: kind, Size: stat.Size(), Created: stat.ModTime(), Restores: kind == "accounts" || kind == "prices"})
	}
	sort.Slice(info.Files, func(i, j int) bool { return info.Files[i].Created.After(info.Files[j].Created) })
	return info
}

// readBackup returns the bytes of a backup file, or a fresh full backup when name is "now".
func readBackup(name string) ([]byte, string, error) {
	if name == "now" {
		var buffer bytes.Buffer
		if err := buildBackupZip(&buffer); err != nil {
			return nil, "", err
		}
		state.RLock()
		stamp := time.Now().In(state.location).Format("20060102-150405")
		state.RUnlock()
		return buffer.Bytes(), "usage-quota-stats-" + stamp + ".zip", nil
	}
	if !backupNamePattern.MatchString(name) {
		return nil, "", fmt.Errorf("备份文件名不正确")
	}
	state.RLock()
	dir := backupDirFor(state.dataFile)
	state.RUnlock()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, "", fmt.Errorf("读取备份失败：%w", err)
	}
	return data, name, nil
}

// restoreSettingsBackup puts a versioned settings file back in place (backing up the current one first) and
// reloads it.
func restoreSettingsBackup(name string) error {
	if !backupNamePattern.MatchString(name) || !(strings.HasPrefix(name, "accounts-") || strings.HasPrefix(name, "prices-")) {
		return fmt.Errorf("只能恢复成本或价格设置的备份")
	}
	state.Lock()
	defer state.Unlock()
	data, err := os.ReadFile(filepath.Join(backupDirFor(state.dataFile), name))
	if err != nil {
		return fmt.Errorf("读取备份失败：%w", err)
	}
	if !json.Valid(data) {
		return fmt.Errorf("备份文件已损坏")
	}
	kind := name[:strings.Index(name, "-")]
	if kind == "prices" {
		var prices map[string]modelPrices
		if err := json.Unmarshal(data, &prices); err != nil {
			return fmt.Errorf("备份文件已损坏：%w", err)
		}
		versionSettingsLocked("prices")
		if err := os.WriteFile(state.dataFile+".prices.json", data, 0o600); err != nil {
			return err
		}
		state.config.Prices = prices
		return nil
	}
	temp := state.dataFile + ".accounts.restore.tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp) }()
	settings, schedule := loadAccountSettings(temp, state.location)
	return commitAccountSettingsLocked(settings, schedule)
}

func pruneBackups(dir, prefix string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && backupNamePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names) // timestamps sort chronologically
	for len(names) > keep {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
