package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

// scanUsage aggregates the usage records of path starting at offset into rows (created when nil) and returns
// the offset just past the last complete line read. Lines are decoded in parallel. With partialTail set, a
// trailing line without newline is left for a later call, because a writer may still be appending it.
func scanUsage(path string, location *time.Location, schedule rateSchedule, rows map[aggregateKey]*aggregate, offset int64, partialTail bool) (map[aggregateKey]*aggregate, int64, error) {
	if rows == nil {
		rows = make(map[aggregateKey]*aggregate)
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return rows, offset, nil
	}
	if err != nil {
		return nil, offset, fmt.Errorf("open usage data file: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seek usage data: %w", err)
	}

	type chunk struct {
		data      []byte
		firstLine int
	}
	workers := max(1, runtime.GOMAXPROCS(0))
	chunks := make(chan chunk, workers)
	partials := make([]map[aggregateKey]*aggregate, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		partials[w] = make(map[aggregateKey]*aggregate)
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for c := range chunks {
				if errs[w] != nil {
					continue
				}
				line := c.firstLine
				for data := c.data; len(data) > 0; line++ {
					end := bytes.IndexByte(data, '\n')
					if end < 0 {
						end = len(data)
					}
					raw := bytes.TrimSpace(data[:end])
					data = data[min(end+1, len(data)):]
					if len(raw) == 0 {
						continue
					}
					var item storedUsage
					if err := json.Unmarshal(raw, &item); err != nil {
						errs[w] = fmt.Errorf("decode usage data line %d: %w", line, err)
						break
					}
					addUsage(partials[w], item, location, schedule)
				}
			}
		}(w)
	}

	reader := bufio.NewReaderSize(file, 1<<20)
	const chunkSize = 4 << 20
	lineNo, consumed := 1, offset
	var readErr error
	for {
		buf := make([]byte, chunkSize)
		n, err := io.ReadFull(reader, buf)
		buf = buf[:n]
		atEOF := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !atEOF {
			readErr = fmt.Errorf("read usage data: %w", err)
			break
		}
		if !atEOF {
			// Extend to the end of the current line so chunks never split a record.
			rest, errLine := reader.ReadBytes('\n')
			buf = append(buf, rest...)
			atEOF = errLine == io.EOF
			if errLine != nil && !atEOF {
				readErr = fmt.Errorf("read usage data: %w", errLine)
				break
			}
		}
		if atEOF && partialTail {
			if last := bytes.LastIndexByte(buf, '\n'); last < len(buf)-1 {
				buf = buf[:last+1]
			}
		}
		if len(buf) > 0 {
			chunks <- chunk{data: buf, firstLine: lineNo}
			lineNo += bytes.Count(buf, []byte{'\n'})
			consumed += int64(len(buf))
		}
		if atEOF {
			break
		}
	}
	close(chunks)
	wg.Wait()
	if readErr != nil {
		return nil, offset, readErr
	}
	for _, err := range errs {
		if err != nil {
			return nil, offset, err
		}
	}
	for _, partial := range partials {
		mergeRows(rows, partial)
	}
	return rows, consumed, nil
}

func mergeRows(dst, src map[aggregateKey]*aggregate) {
	for key, row := range src {
		existing := dst[key]
		if existing == nil {
			dst[key] = row
			continue
		}
		if existing.Provider == "" {
			existing.Provider = row.Provider
		}
		if existing.AuthType == "" {
			existing.AuthType = row.AuthType
		}
		if row.LastUsed.After(existing.LastUsed) {
			existing.LastUsed = row.LastUsed
		}
		existing.Requests += row.Requests
		existing.Failures += row.Failures
		existing.InputTokens += row.InputTokens
		existing.OutputTokens += row.OutputTokens
		existing.CacheReadTokens += row.CacheReadTokens
		existing.CacheCreationTokens += row.CacheCreationTokens
	}
}

// applySettingsChange builds new settings under the lock and commits them. When the bucket schedule changes,
// the data file is re-read without holding the lock, so recording live usage is never blocked by the rescan;
// records appended meanwhile are read under the lock afterwards. build runs again if settings changed during
// the rescan.
func applySettingsChange(build func() (accountSettings, rateSchedule, error)) error {
	for attempt := 0; attempt < 3; attempt++ {
		state.Lock()
		settings, schedule, err := build()
		if err != nil {
			state.Unlock()
			return err
		}
		if schedule.key() == state.schedule.key() || state.dataFile == "" {
			err = commitSettingsLocked(settings, schedule, state.rows)
			state.Unlock()
			return err
		}
		generation, path, location := state.settingsGeneration, state.dataFile, state.location
		state.Unlock()

		rows, offset, err := scanUsage(path, location, schedule, nil, 0, true)
		if err != nil {
			return err
		}
		state.Lock()
		if state.settingsGeneration != generation || state.dataFile != path || state.location != location {
			state.Unlock()
			continue
		}
		if rows, _, err = scanUsage(path, location, schedule, rows, offset, false); err == nil {
			err = commitSettingsLocked(settings, schedule, rows)
		}
		state.Unlock()
		return err
	}
	return fmt.Errorf("设置正在被其他操作修改，请稍后重试")
}

// commitSettingsLocked saves settings and installs them with their usage buckets. The caller holds the lock.
func commitSettingsLocked(settings accountSettings, schedule rateSchedule, rows map[aggregateKey]*aggregate) error {
	if err := saveAccountSettingsLocked(settings); err != nil {
		return err
	}
	state.rows = rows
	state.schedule = schedule
	state.accounts = settings
	state.settingsGeneration++
	state.version++
	return nil
}
