package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

const pluginID = "usage-quota-stats"

type envelope struct {
	OK     bool `json:"ok"`
	Result any  `json:"result,omitempty"`
	Error  any  `json:"error,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	Currency string                 `yaml:"currency"`
	DataFile string                 `yaml:"data-file"`
	Timezone string                 `yaml:"timezone"`
	Prices   map[string]modelPrices `yaml:"prices"`
}

type modelPrices struct {
	Mode       string  `yaml:"mode" json:"mode"` // token, request, second
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache-read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache-write" json:"cache_write"`
	Request    float64 `yaml:"request" json:"request"`
	Second     float64 `yaml:"second" json:"second"`
}

type usageRecord struct {
	Provider    string    `json:"Provider"`
	AuthType    string    `json:"AuthType"`
	Model       string    `json:"Model"`
	Alias       string    `json:"Alias"`
	AuthID      string    `json:"AuthID"`
	AuthIndex   string    `json:"AuthIndex"`
	RequestedAt time.Time `json:"RequestedAt"`
	Failed      bool      `json:"Failed"`
	Failure     struct {
		StatusCode int `json:"StatusCode"`
	} `json:"Failure"`
	Detail usageDetail `json:"Detail"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
}

type storedUsage struct {
	Provider            string    `json:"provider"`
	AuthType            string    `json:"auth_type,omitempty"`
	Model               string    `json:"model"`
	AuthID              string    `json:"auth_id"`
	RequestedAt         time.Time `json:"requested_at"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	Failed              bool      `json:"failed,omitempty"`
	StatusCode          int       `json:"status_code,omitempty"`
}

// aggregateKey buckets usage per credential, model, calendar day (in the configured timezone) and resale
// rate period, so a day containing a multiplier change is split exactly at the change.
type aggregateKey struct {
	AuthID string
	Model  string
	Day    string
	Period int
}

type aggregate struct {
	Provider            string
	AuthType            string
	AuthID              string
	Model               string
	Day                 string
	Period              int
	Requests            int64
	Failures            int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	LastUsed            time.Time
}

var state = struct {
	sync.RWMutex
	config   pluginConfig
	location *time.Location
	dataFile string
	rows     map[aggregateKey]*aggregate
	accounts accountSettings
	schedule rateSchedule
}{rows: make(map[aggregateKey]*aggregate), location: time.Local, accounts: defaultAccountSettings()}

type hostCaller func(method string, payload []byte) ([]byte, error)

type authFileEntry struct {
	ID            string `json:"id"`
	AuthIndex     string `json:"auth_index"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Provider      string `json:"provider"`
	Label         string `json:"label"`
	Email         string `json:"email"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
	Disabled      bool   `json:"disabled"`
	Unavailable   bool   `json:"unavailable"`
}

func handleMethod(method string, request []byte, caller ...hostCaller) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		if err := applyConfig(req.ConfigYAML); err != nil {
			return nil, err
		}
		return okEnvelope(map[string]any{
			"schema_version": 1,
			"metadata": map[string]any{
				"Name":             "配置额度统计",
				"Version":          "1.5.2",
				"Author":           "CLIProxyAPI",
				"GitHubRepository": "https://github.com/router-for-me/CLIProxyAPI",
				"ConfigFields": []map[string]any{
					{"Name": "currency", "Type": "string", "Description": "显示货币，默认 CNY（人民币，模型价格按 1 美元 = 1 元填写）。"},
					{"Name": "data-file", "Type": "string", "Description": "使用记录 JSONL 文件路径。"},
					{"Name": "timezone", "Type": "string", "Description": "按天统计使用的时区，例如 Asia/Shanghai；默认使用服务器时区。"},
					{"Name": "prices", "Type": "object", "Description": "按模型填写每 100 万 token 的输入、输出、缓存读和缓存写价格。"},
				},
			},
			"capabilities": map[string]any{"usage_plugin": true, "management_api": true},
		})
	case "usage.handle":
		var record usageRecord
		if err := json.Unmarshal(request, &record); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		if err := recordUsage(record); err != nil {
			return nil, err
		}
		maybeDailyBackup(time.Now())
		return okEnvelope(struct{}{})
	case "management.register":
		return okEnvelope(map[string]any{
			"resources": []map[string]string{{
				"Path": "/dashboard", "Menu": "配置额度统计", "Description": "按配置和模型查看 token、费用、账号成本与利润。",
			}},
		})
	case "management.handle":
		var req struct {
			Query map[string][]string `json:"Query"`
		}
		_ = json.Unmarshal(request, &req)
		query := func(key string) (string, bool) {
			values := req.Query[key]
			if len(values) == 0 {
				return "", false
			}
			return values[0], true
		}
		if text, ok := query("prices_parse"); ok {
			prices, errParse := parseImportedPrices(text)
			if errParse != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": errParse.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]any{"prices": prices})
		}
		if raw, ok := query("prices"); ok {
			if err := updatePrices(raw); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
		}
		if _, ok := query("backups"); ok {
			return jsonResponse(http.StatusOK, listBackups())
		}
		if _, ok := query("backup_now"); ok {
			name, err := createManualBackup(time.Now())
			if err != nil {
				return jsonResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]string{"name": name})
		}
		if name, ok := query("backup_download"); ok {
			data, fileName, err := readBackup(name)
			if err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			contentType := "application/zip"
			if strings.HasSuffix(fileName, ".json") {
				contentType = "application/octet-stream"
			}
			return okEnvelope(map[string]any{
				"StatusCode": http.StatusOK,
				"Headers": map[string][]string{"Content-Type": {contentType}, "Cache-Control": {"no-store"},
					"Content-Disposition": {`attachment; filename="` + fileName + `"`}},
				"Body": data,
			})
		}
		if name, ok := query("backup_restore"); ok {
			if err := restoreSettingsBackup(name); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
		}
		if id, ok := query("retire"); ok {
			at, _ := query("at")
			if err := setRetirement(id, at); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
		}
		if raw, ok := query("new_accounts"); ok {
			if err := enterNewAccounts(raw, time.Now()); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
		}
		if raw, ok := query("accounts"); ok {
			if err := updateAccountSettings(raw); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
		}
		if len(caller) > 0 {
			refreshAuthChannels(caller[0])
		}
		maybeDailyBackup(time.Now())
		rangeKey, _ := query("range")
		if _, ok := query("api"); ok {
			return jsonResponse(http.StatusOK, buildDashboard(rangeKey, time.Now()))
		}
		body, err := renderDashboardPage(rangeKey, time.Now())
		if err != nil {
			return nil, err
		}
		return okEnvelope(map[string]any{
			"StatusCode": http.StatusOK,
			"Headers":    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}},
			"Body":       body,
		})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

var authInfo = struct {
	sync.RWMutex
	entries []authFileEntry
	byKey   map[string]authFileEntry
}{byKey: make(map[string]authFileEntry)}

type authView struct {
	entries []authFileEntry
	byKey   map[string]authFileEntry
}

func authSnapshot() authView {
	authInfo.RLock()
	defer authInfo.RUnlock()
	return authView{entries: authInfo.entries, byKey: authInfo.byKey}
}

func refreshAuthChannels(caller hostCaller) {
	if caller == nil {
		return
	}
	raw, err := caller("host.auth.list", []byte(`{}`))
	if err != nil {
		return
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Files []authFileEntry `json:"files"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return
	}
	m := make(map[string]authFileEntry)
	for _, e := range env.Result.Files {
		for _, key := range []string{e.ID, e.AuthIndex, e.Name} {
			if strings.TrimSpace(key) != "" {
				m[key] = e
			}
		}
	}
	authInfo.Lock()
	authInfo.entries = env.Result.Files
	authInfo.byKey = m
	authInfo.Unlock()
}

func applyConfig(raw []byte) error {
	cfg := pluginConfig{Currency: "CNY", DataFile: "usage-quota-stats.jsonl", Prices: make(map[string]modelPrices)}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("parse plugin config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.Currency) == "" {
		cfg.Currency = "CNY"
	}
	if strings.TrimSpace(cfg.DataFile) == "" {
		cfg.DataFile = "usage-quota-stats.jsonl"
	}
	location := time.Local
	if name := strings.TrimSpace(cfg.Timezone); name != "" {
		loaded, errLocation := time.LoadLocation(name)
		if errLocation != nil {
			return fmt.Errorf("load timezone %q: %w", name, errLocation)
		}
		location = loaded
	}
	path, err := filepath.Abs(cfg.DataFile)
	if err != nil {
		return fmt.Errorf("resolve data file: %w", err)
	}

	accounts, schedule := loadAccountSettings(path+".accounts.json", location)

	state.Lock()
	defer state.Unlock()
	if state.dataFile != path || state.location.String() != location.String() || state.schedule.key() != schedule.key() {
		rows, errLoad := loadUsage(path, location, schedule)
		if errLoad != nil {
			return errLoad
		}
		state.rows = rows
		state.dataFile = path
		state.location = location
	}
	if saved, errRead := os.ReadFile(path + ".prices.json"); errRead == nil {
		_ = json.Unmarshal(saved, &cfg.Prices)
	}
	state.accounts = accounts
	state.schedule = schedule
	state.config = cfg
	return nil
}

func updatePrices(raw string) error {
	var prices map[string]modelPrices
	if err := json.Unmarshal([]byte(raw), &prices); err != nil {
		return fmt.Errorf("parse prices: %w", err)
	}
	cleaned := make(map[string]modelPrices, len(prices))
	for model, price := range prices {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		for _, value := range []float64{price.Input, price.Output, price.CacheRead, price.CacheWrite, price.Request, price.Second} {
			if value < 0 {
				return fmt.Errorf("price for %s must not be negative", model)
			}
		}
		price.Mode = normalizeMode(price.Mode)
		cleaned[model] = price
	}
	prices = cleaned
	state.Lock()
	defer state.Unlock()
	data, err := json.MarshalIndent(prices, "", "  ")
	if err != nil {
		return err
	}
	versionSettingsLocked("prices")
	if err := os.WriteFile(state.dataFile+".prices.json", append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save prices: %w", err)
	}
	state.config.Prices = prices
	return nil
}

// parseImportedPrices accepts the exported JSON, a bare YAML/JSON price map, or a pasted config.yaml fragment
// (plugins.configs.usage-quota-stats.prices). Field names may use "cache-read" or "cache_read".
func parseImportedPrices(text string) (map[string]modelPrices, error) {
	var root any
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, fmt.Errorf("无法解析导入内容：%w", err)
	}
	for _, path := range [][]string{{"plugins", "configs", pluginID, "prices"}, {"configs", pluginID, "prices"}, {pluginID, "prices"}, {"prices"}} {
		if node, ok := lookupNode(root, path); ok {
			root = node
			break
		}
	}
	entries, ok := root.(map[string]any)
	if !ok || len(entries) == 0 {
		return nil, fmt.Errorf("导入内容里没有找到模型价格")
	}
	prices := make(map[string]modelPrices, len(entries))
	for model, rawFields := range entries {
		fields, ok := rawFields.(map[string]any)
		model = strings.TrimSpace(model)
		if !ok || model == "" {
			return nil, fmt.Errorf("模型 %q 的价格格式不正确", model)
		}
		var price modelPrices
		for key, value := range fields {
			key = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "_", "-")
			if key == "mode" {
				price.Mode = fmt.Sprint(value)
				continue
			}
			number, okNumber := toFloat(value)
			if !okNumber || number < 0 {
				return nil, fmt.Errorf("模型 %s 的 %s 不是有效的非负数", model, key)
			}
			switch key {
			case "input":
				price.Input = number
			case "output":
				price.Output = number
			case "cache-read":
				price.CacheRead = number
			case "cache-write":
				price.CacheWrite = number
			case "request":
				price.Request = number
			case "second":
				price.Second = number
			}
		}
		price.Mode = normalizeMode(price.Mode)
		prices[model] = price
	}
	return prices, nil
}

func lookupNode(root any, path []string) (any, bool) {
	node := root
	for _, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[key]; !ok {
			return nil, false
		}
	}
	return node, true
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, true
	case string:
		var n float64
		if _, err := fmt.Sscan(strings.TrimSpace(v), &n); err != nil {
			return 0, false
		}
		return n, true
	case nil:
		return 0, true
	}
	return 0, false
}

func recordUsage(record usageRecord) error {
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = strings.TrimSpace(record.Alias)
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(record.AuthIndex)
	}
	if authID == "" {
		authID = "unknown"
	}
	cacheRead := record.Detail.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = record.Detail.CachedTokens
	}
	requestedAt := record.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}
	item := storedUsage{
		Provider: record.Provider, AuthType: record.AuthType, Model: model, AuthID: authID, RequestedAt: requestedAt,
		InputTokens: record.Detail.InputTokens, OutputTokens: record.Detail.OutputTokens,
		CacheReadTokens: cacheRead, CacheCreationTokens: record.Detail.CacheCreationTokens,
		Failed: record.Failed, StatusCode: record.Failure.StatusCode,
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode usage record: %w", err)
	}

	state.Lock()
	defer state.Unlock()
	if errMkdir := os.MkdirAll(filepath.Dir(state.dataFile), 0o755); errMkdir != nil {
		return fmt.Errorf("create usage data directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(state.dataFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open usage data file: %w", errOpen)
	}
	defer func() { _ = file.Close() }()
	if _, errWrite := file.Write(append(raw, '\n')); errWrite != nil {
		return fmt.Errorf("append usage data: %w", errWrite)
	}
	addUsage(state.rows, item, state.location, state.schedule)
	return nil
}

func loadUsage(path string, location *time.Location, schedule rateSchedule) (map[aggregateKey]*aggregate, error) {
	rows := make(map[aggregateKey]*aggregate)
	file, errOpen := os.Open(path)
	if os.IsNotExist(errOpen) {
		return rows, nil
	}
	if errOpen != nil {
		return nil, fmt.Errorf("open usage data file: %w", errOpen)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var item storedUsage
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, fmt.Errorf("decode usage data line %d: %w", line, err)
		}
		addUsage(rows, item, location, schedule)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read usage data: %w", err)
	}
	return rows, nil
}

func addUsage(rows map[aggregateKey]*aggregate, item storedUsage, location *time.Location, schedule rateSchedule) {
	day := ""
	if !item.RequestedAt.IsZero() {
		day = item.RequestedAt.In(location).Format(time.DateOnly)
	}
	period := schedule.period(item.RequestedAt)
	key := aggregateKey{AuthID: item.AuthID, Model: item.Model, Day: day, Period: period}
	row := rows[key]
	if row == nil {
		row = &aggregate{Provider: item.Provider, AuthType: item.AuthType, AuthID: item.AuthID, Model: item.Model, Day: day, Period: period}
		rows[key] = row
	}
	if row.Provider == "" {
		row.Provider = item.Provider
	}
	if row.AuthType == "" {
		row.AuthType = item.AuthType
	}
	if item.RequestedAt.After(row.LastUsed) {
		row.LastUsed = item.RequestedAt
	}
	if item.Failed {
		row.Failures++
		return
	}
	row.Requests++
	row.InputTokens += item.InputTokens
	row.OutputTokens += item.OutputTokens
	row.CacheReadTokens += item.CacheReadTokens
	row.CacheCreationTokens += item.CacheCreationTokens
}

func lookupPrice(prices map[string]modelPrices, model string) (modelPrices, bool) {
	pattern, ok := matchPattern(prices, model)
	if !ok {
		return modelPrices{}, false
	}
	return prices[pattern], true
}

// matchPattern returns the price rule key used for model: an exact name wins, otherwise the longest "prefix*" rule.
func matchPattern(prices map[string]modelPrices, model string) (string, bool) {
	if _, ok := prices[model]; ok {
		return model, true
	}
	best, bestPrefix := "", ""
	for pattern := range prices {
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(model, prefix) && (best == "" || len(prefix) > len(bestPrefix)) {
			best, bestPrefix = pattern, prefix
		}
	}
	return best, best != ""
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "request", "image", "image-request":
		return "request"
	case "second", "seconds", "video":
		return "second"
	default:
		return "token"
	}
}

func calculateCost(price modelPrices, requests, input, output, cacheRead, cacheWrite int64) float64 {
	switch strings.ToLower(strings.TrimSpace(price.Mode)) {
	case "request", "image", "image-request":
		return float64(requests) * price.Request
	case "second", "seconds", "video":
		return 0 // Duration is not exposed by the current UsageRecord ABI.
	default:
		return (float64(input)*price.Input + float64(output)*price.Output + float64(cacheRead)*price.CacheRead + float64(cacheWrite)*price.CacheWrite) / 1_000_000
	}
}

func regularInputTokens(provider string, input, cacheRead, cacheWrite int64) int64 {
	provider = strings.ToLower(provider)
	if strings.Contains(provider, "claude") || strings.Contains(provider, "anthropic") {
		return max(input, 0)
	}
	return max(input-cacheRead-cacheWrite, 0)
}

func usageChannel(provider, authType, authID string) string {
	value := strings.ToLower(strings.Join([]string{provider, authType}, " "))
	switch {
	case strings.Contains(value, "antigravity"):
		return "antigravity"
	case strings.Contains(value, "gemini-cli") || strings.Contains(value, "gemini_cli"):
		return "gemini"
	case strings.Contains(value, "codex") || strings.Contains(value, "openai"):
		return "codex"
	case strings.Contains(value, "xai") || strings.Contains(value, "grok"):
		return "xai"
	}
	authID = strings.ToLower(strings.TrimSpace(authID))
	switch {
	case strings.HasPrefix(authID, "gemini-cli-") || strings.HasPrefix(authID, "gemini_cli-") ||
		strings.HasSuffix(authID, "-gemini-cli.json") || strings.HasSuffix(authID, "-gemini_cli.json"):
		return "gemini"
	case strings.HasPrefix(authID, "codex-"):
		return "codex"
	case strings.HasPrefix(authID, "xai-"):
		return "xai"
	default:
		return "antigravity"
	}
}

func channelForAuth(provider, authType, authID string) string {
	authInfo.RLock()
	entry, ok := authInfo.byKey[authID]
	authInfo.RUnlock()
	if ok {
		return usageChannel(entry.Provider, entry.Type, entry.ID+" "+entry.Name)
	}
	return usageChannel(provider, authType, authID)
}

func okEnvelope(result any) ([]byte, error) { return json.Marshal(envelope{OK: true, Result: result}) }

func jsonResponse(status int, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{
		"StatusCode": status,
		"Headers":    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
		"Body":       raw,
	})
}
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: map[string]string{"code": code, "message": message}})
	return raw
}
