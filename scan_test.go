package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func writeRecords(t *testing.T, path string, n int, start time.Time) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for i := 0; i < n; i++ {
		item := storedUsage{Provider: "codex", Model: fmt.Sprintf("m%d", i%3), AuthID: fmt.Sprintf("a%d.json", i%7), RequestedAt: start.Add(time.Duration(i) * time.Minute),
			InputTokens: int64(i), OutputTokens: 1, Failed: i%11 == 0}
		raw, _ := json.Marshal(item)
		if _, err := file.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// The parallel chunked scan must give exactly the same buckets as reading line by line.
func TestScanUsageMatchesSequentialAcrossChunks(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	writeRecords(t, path, 40_000, start) // > 4 MB, several chunks
	schedule := rateSchedule{boundaries: []time.Time{start.Add(10_000 * time.Minute), start.Add(25_000 * time.Minute)}}
	got, offset, err := scanUsage(path, time.UTC, schedule, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[aggregateKey]*aggregate)
	raw, _ := os.ReadFile(path)
	for _, line := range splitLines(raw) {
		var item storedUsage
		_ = json.Unmarshal(line, &item)
		addUsage(want, item, time.UTC, schedule)
	}
	if stat, _ := os.Stat(path); offset != stat.Size() || !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel scan differs: %d buckets vs %d, offset %d", len(got), len(want), offset)
	}
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	for len(raw) > 0 {
		i := 0
		for i < len(raw) && raw[i] != '\n' {
			i++
		}
		if i > 0 {
			lines = append(lines, raw[:i])
		}
		raw = raw[min(i+1, len(raw)):]
	}
	return lines
}

func TestScanUsageLeavesPartialTail(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	writeRecords(t, path, 10, start)
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = file.WriteString(`{"provider":"codex","model":"m0","auth_id":"a0.j`)
	_ = file.Close()
	rows, offset, err := scanUsage(path, time.UTC, rateSchedule{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	file, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = file.WriteString(`son","requested_at":"2026-11-01T00:00:00Z","input_tokens":1}` + "\n")
	_ = file.Close()
	if rows, _, err = scanUsage(path, time.UTC, rateSchedule{}, rows, offset, false); err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, row := range rows {
		requests += row.Requests + row.Failures
	}
	if requests != 11 {
		t.Fatalf("requests = %d, want 11", requests)
	}
}

// Usage recorded while a settings change rescans the file must neither block nor get lost.
func TestRescanDoesNotLoseConcurrentUsage(t *testing.T) {
	path := setupDashboard(t, "prices:\n  '*':\n    input: 1\n")
	start := time.Now().Add(-48 * time.Hour)
	writeRecords(t, path, 60_000, start)
	if err := applyConfig([]byte("data-file: " + path + "\nprices:\n  '*':\n    input: 1\n")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			recordAll(t, use("codex", "live.json", time.Now()))
		}
	}()
	settings := mustSettingsJSON(t, map[string][]rateChange{"codex": {{Multiplier: 0.3}, {From: start.Add(time.Hour).Format(time.RFC3339), Multiplier: 0.35}}})
	if err := updateAccountSettings(settings); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var requests int64
	state.RLock()
	for _, row := range state.rows {
		requests += row.Requests + row.Failures
	}
	periods := state.schedule.periods()
	state.RUnlock()
	if requests != 60_500 || periods != 2 {
		t.Fatalf("requests = %d (want 60500), periods = %d", requests, periods)
	}
}
