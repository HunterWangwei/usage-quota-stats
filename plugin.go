package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	Provider    string      `json:"Provider"`
	AuthType    string      `json:"AuthType"`
	Model       string      `json:"Model"`
	Alias       string      `json:"Alias"`
	AuthID      string      `json:"AuthID"`
	AuthIndex   string      `json:"AuthIndex"`
	RequestedAt time.Time   `json:"RequestedAt"`
	Failed      bool        `json:"Failed"`
	Detail      usageDetail `json:"Detail"`
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
}

type aggregateKey struct {
	AuthID string
	Model  string
}

type aggregate struct {
	Provider            string
	AuthType            string
	AuthID              string
	Model               string
	Requests            int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

var state = struct {
	sync.RWMutex
	config   pluginConfig
	dataFile string
	rows     map[aggregateKey]*aggregate
}{rows: make(map[aggregateKey]*aggregate)}

type hostCaller func(method string, payload []byte) ([]byte, error)

type authFileEntry struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Provider  string `json:"provider"`
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
				"Version":          "1.0.18",
				"Author":           "CLIProxyAPI",
				"GitHubRepository": "https://github.com/router-for-me/CLIProxyAPI",
				"ConfigFields": []map[string]any{
					{"Name": "currency", "Type": "string", "Description": "显示货币，例如 USD 或 CNY。"},
					{"Name": "data-file", "Type": "string", "Description": "使用记录 JSONL 文件路径。"},
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
		if !record.Failed {
			if err := recordUsage(record); err != nil {
				return nil, err
			}
		}
		return okEnvelope(struct{}{})
	case "management.register":
		return okEnvelope(map[string]any{
			"resources": []map[string]string{{
				"Path": "/dashboard", "Menu": "配置额度统计", "Description": "按配置和模型查看 token、费用及缓存命中率。",
			}},
		})
	case "management.handle":
		var req struct {
			Query map[string][]string `json:"Query"`
		}
		_ = json.Unmarshal(request, &req)
		if values := req.Query["prices_parse"]; len(values) > 0 {
			prices, errParse := parseImportedPrices(values[0])
			if errParse != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": errParse.Error()})
			}
			return jsonResponse(http.StatusOK, map[string]any{"prices": prices})
		}
		if values := req.Query["prices"]; len(values) > 0 {
			if err := updatePrices(values[0]); err != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
		}
		if len(caller) > 0 {
			refreshAuthChannels(caller[0])
		}
		body, err := renderDashboard()
		if err != nil {
			return nil, err
		}
		return okEnvelope(map[string]any{
			"StatusCode": http.StatusOK,
			"Headers":    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
			"Body":       body,
		})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

var authChannels = struct {
	sync.RWMutex
	byID map[string]string
}{byID: make(map[string]string)}

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
	m := make(map[string]string)
	for _, e := range env.Result.Files {
		channel := usageChannel(e.Provider, e.Type, e.ID+" "+e.Name)
		for _, key := range []string{e.ID, e.AuthIndex, e.Name} {
			if strings.TrimSpace(key) != "" {
				m[key] = channel
			}
		}
	}
	authChannels.Lock()
	authChannels.byID = m
	authChannels.Unlock()
}

func applyConfig(raw []byte) error {
	cfg := pluginConfig{Currency: "USD", DataFile: "usage-quota-stats.jsonl", Prices: make(map[string]modelPrices)}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("parse plugin config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.Currency) == "" {
		cfg.Currency = "USD"
	}
	if strings.TrimSpace(cfg.DataFile) == "" {
		cfg.DataFile = "usage-quota-stats.jsonl"
	}
	path, err := filepath.Abs(cfg.DataFile)
	if err != nil {
		return fmt.Errorf("resolve data file: %w", err)
	}

	state.Lock()
	defer state.Unlock()
	if state.dataFile != path {
		rows, errLoad := loadUsage(path)
		if errLoad != nil {
			return errLoad
		}
		state.rows = rows
		state.dataFile = path
	}
	if saved, errRead := os.ReadFile(path + ".prices.json"); errRead == nil {
		_ = json.Unmarshal(saved, &cfg.Prices)
	}
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
	item := storedUsage{
		Provider: record.Provider, AuthType: record.AuthType, Model: model, AuthID: authID, RequestedAt: record.RequestedAt,
		InputTokens: record.Detail.InputTokens, OutputTokens: record.Detail.OutputTokens,
		CacheReadTokens: cacheRead, CacheCreationTokens: record.Detail.CacheCreationTokens,
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
	addUsage(state.rows, item)
	return nil
}

func loadUsage(path string) (map[aggregateKey]*aggregate, error) {
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
		addUsage(rows, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read usage data: %w", err)
	}
	return rows, nil
}

func addUsage(rows map[aggregateKey]*aggregate, item storedUsage) {
	key := aggregateKey{AuthID: item.AuthID, Model: item.Model}
	row := rows[key]
	if row == nil {
		row = &aggregate{Provider: item.Provider, AuthType: item.AuthType, AuthID: item.AuthID, Model: item.Model}
		rows[key] = row
	}
	row.Requests++
	if row.Provider == "" {
		row.Provider = item.Provider
	}
	if row.AuthType == "" {
		row.AuthType = item.AuthType
	}
	row.InputTokens += item.InputTokens
	row.OutputTokens += item.OutputTokens
	row.CacheReadTokens += item.CacheReadTokens
	row.CacheCreationTokens += item.CacheCreationTokens
}

type dashboardRow struct {
	AuthID, Channel, Model, Requests, Input, Output, CacheRead, CacheWrite, HitRate, Cost, PriceState string
}
type dashboardGroup struct {
	AuthID, Requests, Input, Output, CacheRead, CacheWrite, HitRate, Cost string
	Rows                                                                  []dashboardRow
}

type dashboardData struct {
	Currency, TotalCost, TotalRequests, OverallHitRate, DataFile string
	Groups                                                       []dashboardGroup
	Rows                                                         []dashboardRow
	Prices                                                       []priceRow
	Unpriced                                                     []unpricedRow
	EmptyPrice                                                   priceRow
}

type priceRow struct {
	Model, Mode, Input, Output, CacheRead, CacheWrite, Request, Second string
	Matches                                                            []priceMatch
	ShownMatches                                                       []priceMatch
	HiddenMatches                                                      int
	MatchTitle                                                         string
	MatchRequests                                                      int64
}

type priceMatch struct {
	Model    string
	Requests int64
}

type unpricedRow struct {
	Model    string
	Requests int64
	Tokens   int64
}

func renderDashboard() ([]byte, error) {
	state.RLock()
	cfg := state.config
	dataFile := state.dataFile
	aggregates := make([]aggregate, 0, len(state.rows))
	for _, row := range state.rows {
		aggregates = append(aggregates, *row)
	}
	state.RUnlock()
	sort.Slice(aggregates, func(i, j int) bool {
		if aggregates[i].AuthID == aggregates[j].AuthID {
			return aggregates[i].Model < aggregates[j].Model
		}
		return aggregates[i].AuthID < aggregates[j].AuthID
	})
	data := dashboardData{Currency: cfg.Currency, DataFile: dataFile, EmptyPrice: priceRow{Mode: "token", Input: "0", Output: "0", CacheRead: "0", CacheWrite: "0", Request: "0", Second: "0"}}
	groupMap := make(map[string][]dashboardRow)
	var totalCost float64
	var totalRequests, totalRead, totalEligible int64
	for _, item := range aggregates {
		price, priced := lookupPrice(cfg.Prices, item.Model)
		regularInput := regularInputTokens(item.Provider, item.InputTokens, item.CacheReadTokens, item.CacheCreationTokens)
		eligible := regularInput + item.CacheReadTokens + item.CacheCreationTokens
		cost := calculateCost(price, item.Requests, regularInput, item.OutputTokens, item.CacheReadTokens, item.CacheCreationTokens)
		priceState := "已定价"
		costText := fmt.Sprintf("%.6f", cost)
		if !priced {
			priceState, costText = "未定价", "—"
		} else {
			totalCost += cost
		}
		row := dashboardRow{
			AuthID: item.AuthID, Channel: channelForAuth(item.Provider, item.AuthType, item.AuthID), Model: item.Model, Requests: formatInt(item.Requests), Input: formatInt(regularInput), Output: formatInt(item.OutputTokens),
			CacheRead: formatInt(item.CacheReadTokens), CacheWrite: formatInt(item.CacheCreationTokens), HitRate: formatRate(item.CacheReadTokens, eligible), Cost: costText, PriceState: priceState,
		}
		groupMap[item.AuthID] = append(groupMap[item.AuthID], row)
		data.Rows = append(data.Rows, row)
		totalRequests += item.Requests
		totalRead += item.CacheReadTokens
		totalEligible += eligible
	}
	for authID, rows := range groupMap {
		group := dashboardGroup{AuthID: authID, Rows: rows}
		for _, row := range rows {
			group.Requests = formatInt(parseInt(group.Requests) + parseInt(row.Requests))
			group.Input = formatInt(parseInt(group.Input) + parseInt(row.Input))
			group.Output = formatInt(parseInt(group.Output) + parseInt(row.Output))
			group.CacheRead = formatInt(parseInt(group.CacheRead) + parseInt(row.CacheRead))
			group.CacheWrite = formatInt(parseInt(group.CacheWrite) + parseInt(row.CacheWrite))
		}
		group.HitRate = formatRate(parseInt(group.CacheRead), parseInt(group.Input)+parseInt(group.CacheRead)+parseInt(group.CacheWrite))
		group.Cost = "0"
		for _, row := range rows {
			if row.PriceState != "未定价" {
				group.Cost = fmt.Sprintf("%.6f", parseFloat(group.Cost)+parseFloat(row.Cost))
			}
		}
		data.Groups = append(data.Groups, group)
	}
	sort.Slice(data.Groups, func(i, j int) bool { return parseFloat(data.Groups[i].Cost) > parseFloat(data.Groups[j].Cost) })
	data.TotalCost = fmt.Sprintf("%.6f", totalCost)
	data.TotalRequests = formatInt(totalRequests)
	data.OverallHitRate = formatRate(totalRead, totalEligible)
	// Configured rules list the used models they cover; used models that match no rule are listed separately
	// so they are never saved as zero-priced exact rules that would shadow prefix rules.
	matches := make(map[string]map[string]int64, len(cfg.Prices))
	unpriced := make(map[string]*unpricedRow)
	for _, item := range aggregates {
		if pattern, ok := matchPattern(cfg.Prices, item.Model); ok {
			if matches[pattern] == nil {
				matches[pattern] = make(map[string]int64)
			}
			matches[pattern][item.Model] += item.Requests
			continue
		}
		row := unpriced[item.Model]
		if row == nil {
			row = &unpricedRow{Model: item.Model}
			unpriced[item.Model] = row
		}
		row.Requests += item.Requests
		row.Tokens += item.InputTokens + item.OutputTokens + item.CacheReadTokens + item.CacheCreationTokens
	}
	models := make([]string, 0, len(cfg.Prices))
	for model := range cfg.Prices {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		price := cfg.Prices[model]
		row := priceRow{Model: model, Mode: normalizeMode(price.Mode), Input: formatPrice(price.Input), Output: formatPrice(price.Output), CacheRead: formatPrice(price.CacheRead), CacheWrite: formatPrice(price.CacheWrite), Request: formatPrice(price.Request), Second: formatPrice(price.Second)}
		for matched, requests := range matches[model] {
			row.Matches = append(row.Matches, priceMatch{Model: matched, Requests: requests})
			row.MatchRequests += requests
		}
		sort.Slice(row.Matches, func(i, j int) bool {
			if row.Matches[i].Requests == row.Matches[j].Requests {
				return row.Matches[i].Model < row.Matches[j].Model
			}
			return row.Matches[i].Requests > row.Matches[j].Requests
		})
		titles := make([]string, 0, len(row.Matches))
		for _, match := range row.Matches {
			titles = append(titles, fmt.Sprintf("%s：%d 次", match.Model, match.Requests))
		}
		row.MatchTitle = strings.Join(titles, "\n")
		row.ShownMatches = row.Matches[:min(len(row.Matches), 4)]
		row.HiddenMatches = len(row.Matches) - len(row.ShownMatches)
		data.Prices = append(data.Prices, row)
	}
	for _, row := range unpriced {
		data.Unpriced = append(data.Unpriced, *row)
	}
	sort.Slice(data.Unpriced, func(i, j int) bool {
		if data.Unpriced[i].Requests == data.Unpriced[j].Requests {
			return data.Unpriced[i].Model < data.Unpriced[j].Model
		}
		return data.Unpriced[i].Requests > data.Unpriced[j].Requests
	})
	var out bytes.Buffer
	if err := dashboardTemplate.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render dashboard: %w", err)
	}
	return out.Bytes(), nil
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
	authChannels.RLock()
	channel := authChannels.byID[authID]
	authChannels.RUnlock()
	if channel != "" {
		return channel
	}
	return usageChannel(provider, authType, authID)
}

func formatRate(hit, eligible int64) string {
	if eligible <= 0 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", float64(hit)*100/float64(eligible))
}

func formatInt(value int64) string     { return fmt.Sprintf("%d", value) }
func parseInt(value string) int64      { var n int64; _, _ = fmt.Sscan(value, &n); return n }
func parseFloat(value string) float64  { var n float64; _, _ = fmt.Sscan(value, &n); return n }
func formatPrice(value float64) string { return fmt.Sprintf("%.6g", value) }

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

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>配置额度统计</title><style>
:root{color-scheme:light dark;--bg:#f6f7fb;--card:#fff;--text:#172033;--muted:#667085;--line:#e5e7eb;--accent:#2563eb;--input:#fff} @media(prefers-color-scheme:dark){:root{--bg:#111827;--card:#1f2937;--text:#f3f4f6;--muted:#9ca3af;--line:#374151;--accent:#60a5fa;--input:#111827}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}.page{padding:24px;max-width:1600px;margin:auto}h1{margin:0 0 6px;font-size:25px}.hint{color:var(--muted);margin-bottom:20px}.cards{display:grid;grid-template-columns:repeat(3,minmax(180px,1fr));gap:14px;margin-bottom:18px}.card,.panel{background:var(--card);border:1px solid var(--line);border-radius:12px}.card{padding:16px}.label{color:var(--muted)}.value{font-size:24px;font-weight:700;margin-top:5px}.panel{padding:16px;margin-top:16px;overflow:auto}h2{font-size:17px;margin:0 0 12px}table{border-collapse:collapse;width:100%;white-space:nowrap}th,td{text-align:right;padding:10px;border-bottom:1px solid var(--line)}th:first-child,td:first-child,th:nth-child(2),td:nth-child(2){text-align:left}th{color:var(--muted);font-weight:600}.unpriced{color:#d97706}.empty{text-align:center!important;color:var(--muted);padding:28px}code{font-size:12px}input{width:100%;min-width:100px;height:36px;padding:7px 10px;border:1px solid var(--line);border-radius:8px;background:var(--input);color:var(--text);font:inherit;outline:none;transition:border-color .15s,box-shadow .15s}input:focus{border-color:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 18%,transparent)}button{height:36px;padding:0 13px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--text);font:inherit;cursor:pointer;transition:background .15s,border-color .15s}button:hover{border-color:var(--accent);background:color-mix(in srgb,var(--accent) 8%,var(--card))}@media(max-width:750px){.cards{grid-template-columns:1fr}.page{padding:14px}}
</style></head><body><main class="page"><h1>配置额度统计</h1><div class="hint">价格单位：{{.Currency}} / 100 万 token。缓存命中率 = 缓存读取 ÷（普通输入 + 缓存读取 + 缓存写入）。</div>
<section class="cards"><div class="card"><div class="label">已定价费用</div><div class="value">{{.Currency}} {{.TotalCost}}</div>{{if .Unpriced}}<div class="card-note">另有 {{len .Unpriced}} 个模型未定价，未计入</div>{{end}}</div><div class="card"><div class="label">请求数</div><div class="value">{{.TotalRequests}}</div></div><div class="card"><div class="label">缓存命中率</div><div class="value">{{.OverallHitRate}}</div></div></section>
<section class="panel"><h2>按配置 / 模型</h2><table><thead><tr><th>配置（Auth ID）</th><th>模型</th><th>请求</th><th>普通输入</th><th>输出</th><th>缓存读</th><th>缓存写</th><th>命中率</th><th>费用 ({{.Currency}})</th></tr></thead><tbody>{{range .Rows}}<tr data-channel="{{.Channel}}"><td>{{.AuthID}}</td><td>{{.Model}}</td><td>{{.Requests}}</td><td>{{.Input}}</td><td>{{.Output}}</td><td>{{.CacheRead}}</td><td>{{.CacheWrite}}</td><td>{{.HitRate}}</td><td class="{{if eq .PriceState "未定价"}}unpriced{{end}}">{{.Cost}} {{if eq .PriceState "未定价"}}(未定价){{end}}</td></tr>{{else}}<tr><td colspan="9" class="empty">暂无成功请求记录</td></tr>{{end}}</tbody></table></section>
{{define "priceRow"}}<tr class="price-row" data-mode="{{.Mode}}"><td><input class="model" value="{{.Model}}" placeholder="模型名或前缀，如 claude-*"></td><td><select class="mode"><option value="token"{{if eq .Mode "token"}} selected{{end}}>按 Token</option><option value="request"{{if eq .Mode "request"}} selected{{end}}>按次 / 张</option><option value="second"{{if eq .Mode "second"}} selected{{end}}>按秒（暂不计费）</option></select></td><td><div class="fields"><label class="f-token">输入<input type="number" min="0" step="any" data-field="input" value="{{.Input}}"></label><label class="f-token">输出<input type="number" min="0" step="any" data-field="output" value="{{.Output}}"></label><label class="f-token">缓存读<input type="number" min="0" step="any" data-field="cache_read" value="{{.CacheRead}}"></label><label class="f-token">缓存写<input type="number" min="0" step="any" data-field="cache_write" value="{{.CacheWrite}}"></label><label class="f-request">每次 / 张<input type="number" min="0" step="any" data-field="request" value="{{.Request}}"></label><label class="f-second">每秒<input type="number" min="0" step="any" data-field="second" value="{{.Second}}"></label></div></td><td class="matches">{{if .Matches}}<div class="match-count" title="{{.MatchTitle}}">{{len .Matches}} 个模型 · {{.MatchRequests}} 次请求</div><div class="match-list" title="{{.MatchTitle}}">{{range .ShownMatches}}<code>{{.Model}}</code>{{end}}{{if .HiddenMatches}}<span class="more">+{{.HiddenMatches}}</span>{{end}}</div>{{else}}<span class="muted">暂无用量</span>{{end}}</td><td><button type="button" class="del" title="删除规则">删除</button></td></tr>{{end}}
<section class="panel" id="pricePanel"><div class="price-head"><h2>模型价格</h2><div class="price-actions"><button type="button" onclick="openImport()">导入</button><button type="button" onclick="exportPrices()">导出</button><button type="button" onclick="addPrice()">+ 添加规则</button></div></div><p class="hint">按 Token 计费的单位是 {{.Currency}} / 100 万 Token，按次计费按请求（图片）次数计。模型名以 * 结尾表示前缀规则（如 <code>claude-*</code>），精确模型名优先，其次匹配最长前缀。</p>
{{if .Unpriced}}<div class="unpriced-box"><h3>待定价：{{len .Unpriced}} 个模型有用量但没有命中任何规则，费用未计入合计</h3><table class="plain"><thead><tr><th>模型</th><th>请求</th><th>Token</th><th></th></tr></thead><tbody>{{range .Unpriced}}<tr><td><code>{{.Model}}</code></td><td>{{.Requests}}</td><td>{{.Tokens}}</td><td><button type="button" class="add-rule" data-model="{{.Model}}">设置价格</button></td></tr>{{end}}</tbody></table></div>{{end}}
<table id="prices"><thead><tr><th>模型规则</th><th>计费模式</th><th>价格</th><th>命中模型</th><th></th></tr></thead><tbody>{{range .Prices}}{{template "priceRow" .}}{{else}}<tr class="empty-row"><td colspan="5" class="empty">还没有价格规则。点击“添加规则”，或用“导入”粘贴已有配置。</td></tr>{{end}}</tbody></table>
<div class="price-foot"><button type="button" class="primary" onclick="savePrices()">保存价格</button><span id="saved"></span></div><p class="hint">数据文件：<code>{{.DataFile}}</code></p></section>
<template id="priceRowTpl">{{template "priceRow" .EmptyPrice}}</template>
<dialog id="importDialog"><h2>导入价格</h2><p class="hint">粘贴导出的 JSON，或 config.yaml 里的 <code>prices:</code> 配置段。同名规则会被覆盖，其余规则保留；导入后需点击“保存价格”才生效。</p><textarea spellcheck="false" placeholder="claude-*:&#10;  input: 3&#10;  output: 15&#10;  cache-read: 0.3&#10;  cache-write: 3.75"></textarea><div class="import-error"></div><div class="dialog-actions"><button type="button" onclick="this.closest('dialog').close()">取消</button><button type="button" class="primary" onclick="applyImport()">合并到列表</button></div></dialog>
<style>.card-note{color:#d97706;font-size:12px;margin-top:4px}.price-head{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-bottom:4px}.price-head h2{margin:0}.price-actions{margin-left:auto;display:flex;gap:8px}#pricePanel .hint{margin-bottom:14px}#pricePanel th,#pricePanel td{text-align:left!important;vertical-align:middle}#prices td{padding:10px 8px}#prices .model{min-width:200px}#prices select{height:36px;padding:0 8px;border:1px solid var(--line);border-radius:8px;background:var(--input);color:var(--text);font:inherit}.fields{display:flex;gap:8px}.fields label{display:flex;flex-direction:column;gap:3px;color:var(--muted);font-size:12px}.fields input{width:96px;min-width:96px}.price-row[data-mode=token] .f-request,.price-row[data-mode=token] .f-second,.price-row[data-mode=request] .f-token,.price-row[data-mode=request] .f-second,.price-row[data-mode=second] .f-token,.price-row[data-mode=second] .f-request{display:none}.matches{white-space:normal;min-width:180px;max-width:320px}.match-count{font-size:12px;color:var(--muted);margin-bottom:4px}.match-list{display:flex;flex-wrap:wrap;gap:4px}.match-list code,.unpriced-box code{padding:1px 6px;border-radius:6px;background:color-mix(in srgb,var(--accent) 10%,transparent)}.more,.muted{color:var(--muted);font-size:12px}.unpriced-box{border:1px solid color-mix(in srgb,#d97706 45%,var(--line));background:color-mix(in srgb,#d97706 7%,var(--card));border-radius:10px;padding:12px 14px;margin-bottom:16px}.unpriced-box h3{margin:0 0 8px;font-size:14px;color:#d97706}.unpriced-box td,.unpriced-box th{padding:6px 10px}.price-foot{display:flex;align-items:center;gap:10px;margin-top:14px}button.primary{background:var(--accent);border-color:var(--accent);color:#fff}button.primary:hover{background:color-mix(in srgb,var(--accent) 85%,#000)}#saved.ok{color:#16a34a}#saved.warn{color:#d97706}#saved.error{color:#dc2626}#importDialog{width:min(640px,94vw);border:1px solid var(--line);border-radius:14px;background:var(--card);color:var(--text);padding:20px}#importDialog::backdrop{background:#0008}#importDialog h2{margin:0 0 6px}#importDialog textarea{width:100%;height:260px;padding:10px;border:1px solid var(--line);border-radius:8px;background:var(--input);color:var(--text);font:13px/1.5 ui-monospace,Consolas,monospace;resize:vertical}.import-error{color:#dc2626;min-height:20px;margin:6px 0}.dialog-actions{display:flex;justify-content:flex-end;gap:8px}@media(max-width:900px){#prices thead{display:none}#prices,#prices tbody,#prices tr,#prices td{display:block;width:100%}#prices .price-row{border:1px solid var(--line);border-radius:10px;padding:6px;margin-bottom:10px}#prices td{border:0;padding:6px}#prices .model{min-width:0}.fields{flex-wrap:wrap}.fields label{flex:1 1 40%}.fields input{width:100%;min-width:0}.matches{max-width:none}.price-actions{margin-left:0}}</style>
</main><script>
let pricesDirty=false;
function setPriceStatus(text,cls){let s=document.querySelector('#saved');if(s){s.textContent=text;s.className=cls||''}}
function markPricesDirty(){pricesDirty=true;setPriceStatus('有未保存的修改','warn')}
function fillPriceRow(row,price){let mode=row.querySelector('.mode');mode.value=price.mode||'token';row.dataset.mode=mode.value;row.querySelectorAll('[data-field]').forEach(i=>i.value=price[i.dataset.field]??0)}
function newPriceRow(model,price){let row=document.querySelector('#priceRowTpl').content.firstElementChild.cloneNode(true);row.querySelector('.model').value=model||'';if(price)fillPriceRow(row,price);row.querySelector('.matches').innerHTML='<span class="muted">保存后显示</span>';document.querySelector('#prices .empty-row')?.remove();document.querySelector('#prices tbody').append(row);return row}
function addPrice(model){let row=newPriceRow(model),input=row.querySelector(model?'[data-field=input]':'.model');markPricesDirty();row.scrollIntoView({block:'nearest'});input.focus();input.select()}
function collectPrices(){let prices={},error='';document.querySelectorAll('#prices .price-row').forEach(row=>{let model=row.querySelector('.model').value.trim();if(!model||error)return;if(prices[model]){error='规则重复：'+model;return}let price={mode:row.querySelector('.mode').value};row.querySelectorAll('[data-field]').forEach(i=>{let v=+i.value||0;if(v<0)error=model+' 的价格不能为负数';price[i.dataset.field]=v});prices[model]=price});return{prices,error}}
async function savePrices(){let{prices,error}=collectPrices();if(error){setPriceStatus(error,'error');return}setPriceStatus('保存中…','');try{let response=await fetch(location.pathname+'?prices='+encodeURIComponent(JSON.stringify(prices)),{cache:'no-store'}),text=await response.text();if(!response.ok){let message='';try{message=JSON.parse(text).error}catch(e){}throw new Error(message||'HTTP '+response.status)}let doc=new DOMParser().parseFromString(text,'text/html'),panel=doc.querySelector('#pricePanel');if(panel)document.querySelector('#pricePanel').replaceWith(document.importNode(panel,true));pricesDirty=false;setPriceStatus('已保存 '+Object.keys(prices).length+' 条规则','ok');refreshDashboard()}catch(e){setPriceStatus('保存失败：'+e.message,'error')}}
function exportPrices(){let{prices,error}=collectPrices();if(error){setPriceStatus(error,'error');return}let text=JSON.stringify(prices,null,2)+'\n',link=document.createElement('a');link.href=URL.createObjectURL(new Blob([text],{type:'application/json'}));link.download='usage-quota-prices.json';link.click();setTimeout(()=>URL.revokeObjectURL(link.href),1000);navigator.clipboard?.writeText(text).then(()=>setPriceStatus('已导出，并复制到剪贴板','ok'),()=>{})}
function openImport(){let dialog=document.querySelector('#importDialog');dialog.querySelector('textarea').value='';dialog.querySelector('.import-error').textContent='';dialog.showModal();dialog.querySelector('textarea').focus()}
async function applyImport(){let dialog=document.querySelector('#importDialog'),text=dialog.querySelector('textarea').value.trim(),errorBox=dialog.querySelector('.import-error');if(!text)return;try{let response=await fetch(location.pathname+'?prices_parse='+encodeURIComponent(text),{cache:'no-store'}),data=await response.json();if(!response.ok)throw new Error(data.error||'HTTP '+response.status);let count=0;Object.entries(data.prices||{}).forEach(([model,price])=>{let row=Array.from(document.querySelectorAll('#prices .price-row')).find(r=>r.querySelector('.model').value.trim()===model);if(row)fillPriceRow(row,price);else newPriceRow(model,price);count++});dialog.close();markPricesDirty();setPriceStatus('已导入 '+count+' 条规则，点击“保存价格”后生效','warn')}catch(e){errorBox.textContent='导入失败：'+e.message}}
document.addEventListener('input',e=>{if(e.target.closest('#prices'))markPricesDirty()});
document.addEventListener('change',e=>{if(e.target.matches('#prices .mode')){e.target.closest('tr').dataset.mode=e.target.value;markPricesDirty()}});
document.addEventListener('click',e=>{let button=e.target.closest('button');if(!button)return;if(button.matches('#prices .del')){button.closest('tr').remove();markPricesDirty()}else if(button.matches('.add-rule')){let row=button.closest('tr'),body=row.parentElement;addPrice(button.dataset.model);row.remove();if(!body.rows.length)body.closest('.unpriced-box').remove()}});
window.addEventListener('beforeunload',e=>{if(pricesDirty){e.preventDefault();e.returnValue=''}});
function esc(v){return v.replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function closeUsageModal(){document.querySelectorAll('.usage-modal').forEach(m=>m.classList.remove('open'));localStorage.setItem('usage-quota-modal','')}
function openUsageModal(id){closeUsageModal();let m=Array.from(document.querySelectorAll('.usage-modal')).find(x=>x.dataset.id===id);if(m){m.classList.add('open');localStorage.setItem('usage-quota-modal',id)}}
function buildCredentialCards(){let panel=document.querySelector('.panel'),table=panel&&panel.querySelector('table');if(!table||!table.tBodies.length)return;let groups={};Array.from(table.tBodies[0].rows).forEach(r=>{let c=r.cells;if(c.length<9)return;let id=c[0].textContent.trim();(groups[id]??=[]).push(Array.from(c).slice(1).map(x=>x.textContent.trim()))});let wrap=document.createElement('div');wrap.id='credentialView';wrap.innerHTML='<input id="credentialSearch" placeholder="搜索凭证"> <div class="credential-grid"></div>';let grid=wrap.querySelector('.credential-grid');Object.entries(groups).map(([id,rows])=>{let req=rows.reduce((n,x)=>n+(+x[1]||0),0),read=rows.reduce((n,x)=>n+(+x[4]||0),0),eligible=rows.reduce((n,x)=>n+(+x[2]||0)+(+x[4]||0)+(+x[5]||0),0),cost=rows.reduce((n,x)=>n+(parseFloat(x[7])||0),0);return{id,rows,req,hit:eligible?read*100/eligible:0,cost}}).sort((a,b)=>b.cost-a.cost).forEach(g=>{let card=document.createElement('button');card.className='credential-card';card.dataset.id=g.id;card.innerHTML='<strong>'+esc(g.id)+'</strong><div><span>已使用金额<b>'+g.cost.toFixed(2)+'</b></span><span>请求数<b>'+g.req+'</b></span><span>缓存命中率<b>'+g.hit.toFixed(2)+'%</b></span></div><em>查看模型详情 →</em>';card.onclick=()=>openUsageModal(g.id);grid.append(card);let modal=document.createElement('div');modal.className='usage-modal';modal.dataset.id=g.id;modal.onclick=e=>{if(e.target===modal)closeUsageModal()};modal.innerHTML='<div class="modal-box"><div class="modal-head"><h2>'+esc(g.id)+'</h2><button onclick="closeUsageModal()">×</button></div><div class="modal-body"><table><thead><tr><th>模型</th><th>请求</th><th>普通输入</th><th>输出</th><th>缓存读</th><th>缓存写</th><th>命中率</th><th>费用</th></tr></thead><tbody>'+g.rows.map(x=>'<tr>'+x.map(v=>'<td>'+esc(v)+'</td>').join('')+'</tr>').join('')+'</tbody></table></div></div>';document.body.append(modal)});table.replaceWith(wrap);let s=document.querySelector('#credentialSearch');s.oninput=()=>{let q=s.value.toLowerCase();document.querySelectorAll('.credential-card').forEach(c=>c.hidden=!c.dataset.id.toLowerCase().includes(q))};let active=localStorage.getItem('usage-quota-modal');if(active)openUsageModal(active)}
document.addEventListener('keydown',e=>{if(e.key==='Escape')closeUsageModal()});document.head.insertAdjacentHTML('beforeend','<style>#credentialSearch{max-width:320px;margin-bottom:15px}.credential-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:14px}.credential-card{height:auto;text-align:left;padding:17px;border-radius:12px}.credential-card strong{display:block;overflow:hidden;text-overflow:ellipsis;margin-bottom:15px}.credential-card div{display:grid;grid-template-columns:repeat(3,1fr);gap:8px}.credential-card span{color:var(--muted);font-size:12px}.credential-card b{display:block;color:var(--text);font-size:16px;margin-top:4px}.credential-card em{display:block;text-align:right;color:var(--accent);font-style:normal;margin-top:15px}.usage-modal{display:none;position:fixed;inset:0;background:#0008;z-index:99;padding:5vh 4vw}.usage-modal.open{display:flex;align-items:center;justify-content:center}.modal-box{background:var(--card);border-radius:14px;width:min(1200px,96vw);max-height:90vh;overflow:hidden;box-shadow:0 24px 70px #0006}.modal-head{display:flex;align-items:center;padding:16px 20px;border-bottom:1px solid var(--line)}.modal-head h2{margin:0;overflow:hidden;text-overflow:ellipsis}.modal-head button{margin-left:auto;font-size:24px}.modal-body{overflow:auto;max-height:calc(90vh - 70px);padding:12px 20px 20px}@media(max-width:700px){.credential-card div{grid-template-columns:1fr}.usage-modal{padding:2vh 2vw}}</style>');let usageChannels={};document.querySelectorAll('.panel table tbody tr[data-channel]').forEach(row=>{let id=row.cells[0]?.textContent.trim();if(id)usageChannels[id]=row.dataset.channel});buildCredentialCards();document.querySelectorAll('.credential-card').forEach(card=>card.dataset.channel=usageChannels[card.dataset.id]||'');
document.head.insertAdjacentHTML('beforeend','<style>.credential-grid{gap:18px}.credential-card{position:relative;min-height:210px;padding:0!important;border:1px solid color-mix(in srgb,var(--accent) 16%,var(--line))!important;border-radius:16px!important;background:linear-gradient(145deg,color-mix(in srgb,var(--accent) 5%,var(--card)),var(--card) 55%)!important;box-shadow:0 2px 8px #0f172a0a;overflow:hidden;transition:transform .18s ease,box-shadow .18s ease,border-color .18s ease}.credential-card:before{content:"";position:absolute;inset:0 auto 0 0;width:4px;background:linear-gradient(180deg,var(--accent),color-mix(in srgb,var(--accent) 45%,#8b5cf6))}.credential-card:hover{transform:translateY(-3px);border-color:color-mix(in srgb,var(--accent) 42%,var(--line))!important;box-shadow:0 12px 28px #0f172a18}.credential-card>strong{display:flex!important;align-items:center;height:64px;margin:0!important;padding:0 20px 0 24px;border-bottom:1px solid var(--line);font-size:15px;min-width:0}.credential-card>strong:before{content:"";width:10px;height:10px;margin-right:10px;border-radius:50%;background:#22c55e;box-shadow:0 0 0 4px #22c55e1f;flex:none}.credential-card .credential-email{display:block;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.credential-card>div{display:grid!important;grid-template-columns:1.35fr 1fr 1fr!important;gap:0!important;padding:18px 20px 14px 24px}.credential-card>div span{padding:0 12px;border-right:1px solid var(--line)}.credential-card>div span:first-child{padding-left:0}.credential-card>div span:last-child{border:0;padding-right:0}.credential-card>div b{font-size:17px!important;white-space:nowrap}.credential-card>div span:first-child b{color:var(--accent);font-size:19px!important}.credential-card>em{margin:0!important;padding:0 20px 16px 24px;font-size:13px;font-weight:600}.credential-card:hover>em{transform:translateX(2px)}@media(max-width:700px){.credential-card>div{grid-template-columns:1fr 1fr!important;row-gap:15px!important}.credential-card>div span{border:0!important;padding:0!important}}</style>');
function credentialEmail(id){let name=id.replace(/\.(json|ya?ml)$/i,'').replace(/^codex-[a-f0-9]+-/i,'').replace(/^xai-/i,'').replace(/-pro$/i,'');let match=name.match(/[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,24}$/i);return match?match[0]:name}
document.querySelectorAll('.credential-card').forEach(card=>{let email=credentialEmail(card.dataset.id),title=card.querySelector('strong');title.title=email;title.innerHTML='<span class="credential-email">'+esc(email)+'</span>'});
let nativeCredentialChannel=credentialChannel;credentialChannel=function(id){let card=Array.from(document.querySelectorAll('.credential-card')).find(item=>item.dataset.id===id);return card&&card.dataset.channel?card.dataset.channel:nativeCredentialChannel(id)};
let activeChannel='all';function credentialChannel(id){return /(^gemini[-_]cli-|-gemini[-_]cli\.json$)/i.test(id)?'gemini':/^codex-/i.test(id)?'codex':/^xai-/i.test(id)?'xai':'antigravity'}function applyCredentialFilters(){let q=(document.querySelector('#credentialSearch')?.value||'').toLowerCase();document.querySelectorAll('.credential-card').forEach(card=>{let channel=credentialChannel(card.dataset.id),email=credentialEmail(card.dataset.id).toLowerCase();card.hidden=!email.includes(q)||(activeChannel!=='all'&&channel!==activeChannel)})}let filterBar=document.createElement('div');filterBar.className='channel-filters';filterBar.innerHTML='<span>凭证渠道</span><button class="active" data-channel="all">全部</button><button data-channel="antigravity">Antigravity</button><button data-channel="gemini">Gemini CLI</button><button data-channel="codex">Codex</button><button data-channel="xai">xAI</button>';document.querySelector('#credentialView')?.prepend(filterBar);filterBar.querySelectorAll('button').forEach(button=>button.onclick=()=>{activeChannel=button.dataset.channel;filterBar.querySelectorAll('button').forEach(x=>x.classList.toggle('active',x===button));applyCredentialFilters()});let credentialSearch=document.querySelector('#credentialSearch');if(credentialSearch)credentialSearch.oninput=applyCredentialFilters;document.head.insertAdjacentHTML('beforeend','<style>.channel-filters{display:flex;align-items:center;gap:8px;margin-bottom:14px;flex-wrap:wrap}.channel-filters span{color:var(--muted);margin-right:4px}.channel-filters button{height:34px;border-radius:999px;padding:0 15px}.channel-filters button.active{color:#fff;border-color:var(--accent);background:var(--accent);box-shadow:0 4px 12px color-mix(in srgb,var(--accent) 24%,transparent)}</style>');
filterBar.querySelectorAll('button').forEach(button=>button.addEventListener('click',()=>localStorage.setItem('usage-quota-channel',button.dataset.channel)));let savedChannel=localStorage.getItem('usage-quota-channel')||'all',savedButton=filterBar.querySelector('[data-channel="'+savedChannel+'"]');if(savedButton)savedButton.click();
let refreshing=false;async function refreshDashboard(){if(refreshing||pricesDirty||document.hidden)return;refreshing=true;try{let response=await fetch(location.pathname+'?dashboard_refresh='+Date.now(),{cache:'no-store'});if(!response.ok)return;let doc=new DOMParser().parseFromString(await response.text(),'text/html'),summary=doc.querySelector('.cards'),currentSummary=document.querySelector('.cards');if(summary&&currentSummary)currentSummary.innerHTML=summary.innerHTML;let table=doc.querySelector('.panel table');if(!table||!table.tBodies.length)return;let groups={};Array.from(table.tBodies[0].rows).forEach(row=>{let cells=row.cells;if(cells.length<9)return;let id=cells[0].textContent.trim();(groups[id]??=[]).push(Array.from(cells).slice(1).map(cell=>cell.textContent.trim()))});Object.entries(groups).forEach(([id,rows])=>{let card=Array.from(document.querySelectorAll('.credential-card')).find(item=>item.dataset.id===id);if(card){let req=rows.reduce((n,x)=>n+(+x[1]||0),0),read=rows.reduce((n,x)=>n+(+x[4]||0),0),eligible=rows.reduce((n,x)=>n+(+x[2]||0)+(+x[4]||0)+(+x[5]||0),0),cost=rows.reduce((n,x)=>n+(parseFloat(x[7])||0),0),values=card.querySelectorAll('b');if(values[0])values[0].textContent=cost.toFixed(2);if(values[1])values[1].textContent=req;if(values[2])values[2].textContent=(eligible?read*100/eligible:0).toFixed(2)+'%'}let modal=Array.from(document.querySelectorAll('.usage-modal')).find(item=>item.dataset.id===id),body=modal&&modal.querySelector('tbody');if(body)body.innerHTML=rows.map(x=>'<tr>'+x.map(v=>'<td>'+esc(v)+'</td>').join('')+'</tr>').join('')});applyCredentialFilters()}catch(error){}finally{refreshing=false}}setInterval(refreshDashboard,5000);document.addEventListener('visibilitychange',()=>{if(!document.hidden)refreshDashboard()});
</script></body></html>`))
