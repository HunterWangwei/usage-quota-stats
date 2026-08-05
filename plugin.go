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
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache-read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache-write" json:"cache_write"`
}

type usageRecord struct {
	Provider    string      `json:"Provider"`
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

func handleMethod(method string, request []byte) ([]byte, error) {
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
				"Version":          "1.0.8",
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
		if values := req.Query["prices"]; len(values) > 0 {
			if err := updatePrices(values[0]); err != nil {
				return nil, err
			}
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
		Provider: record.Provider, Model: model, AuthID: authID, RequestedAt: record.RequestedAt,
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
		row = &aggregate{Provider: item.Provider, AuthID: item.AuthID, Model: item.Model}
		rows[key] = row
	}
	row.Requests++
	row.InputTokens += item.InputTokens
	row.OutputTokens += item.OutputTokens
	row.CacheReadTokens += item.CacheReadTokens
	row.CacheCreationTokens += item.CacheCreationTokens
}

type dashboardRow struct {
	AuthID, Model, Requests, Input, Output, CacheRead, CacheWrite, HitRate, Cost, PriceState string
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
}

type priceRow struct{ Model, Input, Output, CacheRead, CacheWrite string }

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
	data := dashboardData{Currency: cfg.Currency, DataFile: dataFile}
	groupMap := make(map[string][]dashboardRow)
	var totalCost float64
	var totalRequests, totalRead, totalEligible int64
	for _, item := range aggregates {
		price, priced := lookupPrice(cfg.Prices, item.Model)
		regularInput := regularInputTokens(item.Provider, item.InputTokens, item.CacheReadTokens, item.CacheCreationTokens)
		eligible := regularInput + item.CacheReadTokens + item.CacheCreationTokens
		cost := (float64(regularInput)*price.Input + float64(item.OutputTokens)*price.Output + float64(item.CacheReadTokens)*price.CacheRead + float64(item.CacheCreationTokens)*price.CacheWrite) / 1_000_000
		priceState := "已定价"
		costText := fmt.Sprintf("%.6f", cost)
		if !priced {
			priceState, costText = "未定价", "—"
		} else {
			totalCost += cost
		}
		row := dashboardRow{
			AuthID: item.AuthID, Model: item.Model, Requests: formatInt(item.Requests), Input: formatInt(regularInput), Output: formatInt(item.OutputTokens),
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
	models := make([]string, 0, len(cfg.Prices))
	for model := range cfg.Prices {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		price := cfg.Prices[model]
		data.Prices = append(data.Prices, priceRow{model, formatPrice(price.Input), formatPrice(price.Output), formatPrice(price.CacheRead), formatPrice(price.CacheWrite)})
	}
	var out bytes.Buffer
	if err := dashboardTemplate.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render dashboard: %w", err)
	}
	return out.Bytes(), nil
}

func lookupPrice(prices map[string]modelPrices, model string) (modelPrices, bool) {
	if price, ok := prices[model]; ok {
		return price, true
	}
	bestPrefix := ""
	var bestPrice modelPrices
	for pattern, price := range prices {
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(model, prefix) && len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			bestPrice = price
		}
	}
	if bestPrefix != "" {
		return bestPrice, true
	}
	return modelPrices{}, false
}

func regularInputTokens(provider string, input, cacheRead, cacheWrite int64) int64 {
	provider = strings.ToLower(provider)
	if strings.Contains(provider, "claude") || strings.Contains(provider, "anthropic") {
		return max(input, 0)
	}
	return max(input-cacheRead-cacheWrite, 0)
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
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: map[string]string{"code": code, "message": message}})
	return raw
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>配置额度统计</title><style>
:root{color-scheme:light dark;--bg:#f6f7fb;--card:#fff;--text:#172033;--muted:#667085;--line:#e5e7eb;--accent:#2563eb;--input:#fff} @media(prefers-color-scheme:dark){:root{--bg:#111827;--card:#1f2937;--text:#f3f4f6;--muted:#9ca3af;--line:#374151;--accent:#60a5fa;--input:#111827}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}.page{padding:24px;max-width:1600px;margin:auto}h1{margin:0 0 6px;font-size:25px}.hint{color:var(--muted);margin-bottom:20px}.cards{display:grid;grid-template-columns:repeat(3,minmax(180px,1fr));gap:14px;margin-bottom:18px}.card,.panel{background:var(--card);border:1px solid var(--line);border-radius:12px}.card{padding:16px}.label{color:var(--muted)}.value{font-size:24px;font-weight:700;margin-top:5px}.panel{padding:16px;margin-top:16px;overflow:auto}h2{font-size:17px;margin:0 0 12px}table{border-collapse:collapse;width:100%;white-space:nowrap}th,td{text-align:right;padding:10px;border-bottom:1px solid var(--line)}th:first-child,td:first-child,th:nth-child(2),td:nth-child(2){text-align:left}th{color:var(--muted);font-weight:600}#prices th{text-align:center!important}.unpriced{color:#d97706}.empty{text-align:center!important;color:var(--muted);padding:28px}code{font-size:12px}input{width:100%;min-width:100px;height:36px;padding:7px 10px;border:1px solid var(--line);border-radius:8px;background:var(--input);color:var(--text);font:inherit;outline:none;transition:border-color .15s,box-shadow .15s}input:focus{border-color:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 18%,transparent)}button{height:36px;padding:0 13px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--text);font:inherit;cursor:pointer;transition:background .15s,border-color .15s}button:hover{border-color:var(--accent);background:color-mix(in srgb,var(--accent) 8%,var(--card))}#prices td:last-child{padding-left:4px}#saved{color:#16a34a;margin-left:8px}@media(max-width:750px){.cards{grid-template-columns:1fr}.page{padding:14px}}
</style></head><body><main class="page"><h1>配置额度统计</h1><div class="hint">价格单位：{{.Currency}} / 100 万 token。缓存命中率 = 缓存读取 ÷（普通输入 + 缓存读取 + 缓存写入）。</div>
<section class="cards"><div class="card"><div class="label">已定价费用</div><div class="value">{{.Currency}} {{.TotalCost}}</div></div><div class="card"><div class="label">请求数</div><div class="value">{{.TotalRequests}}</div></div><div class="card"><div class="label">缓存命中率</div><div class="value">{{.OverallHitRate}}</div></div></section>
<section class="panel"><h2>按配置 / 模型</h2><table><thead><tr><th>配置（Auth ID）</th><th>模型</th><th>请求</th><th>普通输入</th><th>输出</th><th>缓存读</th><th>缓存写</th><th>命中率</th><th>费用 ({{.Currency}})</th></tr></thead><tbody>{{range .Rows}}<tr><td>{{.AuthID}}</td><td>{{.Model}}</td><td>{{.Requests}}</td><td>{{.Input}}</td><td>{{.Output}}</td><td>{{.CacheRead}}</td><td>{{.CacheWrite}}</td><td>{{.HitRate}}</td><td class="{{if eq .PriceState "未定价"}}unpriced{{end}}">{{.Cost}} {{if eq .PriceState "未定价"}}(未定价){{end}}</td></tr>{{else}}<tr><td colspan="9" class="empty">暂无成功请求记录</td></tr>{{end}}</tbody></table></section>
<section class="panel"><h2>模型价格（可直接编辑）</h2><p class="hint">单位：{{.Currency}} / 100 万 Token。支持模型前缀，例如 <code>claude-*</code>。保存后写入数据文件旁的 <code>.prices.json</code>。</p><table id="prices"><thead><tr><th>模型（支持前缀 *）</th><th>输入</th><th>输出</th><th>缓存读</th><th>缓存写</th><th></th></tr></thead><tbody>{{range .Prices}}<tr><td><input value="{{.Model}}"></td><td><input type="number" step="any" value="{{.Input}}"></td><td><input type="number" step="any" value="{{.Output}}"></td><td><input type="number" step="any" value="{{.CacheRead}}"></td><td><input type="number" step="any" value="{{.CacheWrite}}"></td><td><button type="button" onclick="this.closest('tr').remove()">删除</button></td></tr>{{end}}</tbody></table><button type="button" onclick="addPrice()">添加模型</button> <button type="button" onclick="savePrices()">保存价格</button><span id="saved"></span><p class="hint">数据文件：<code>{{.DataFile}}</code></p></section></main><script>
function addPrice(){document.querySelector('#prices tbody').insertAdjacentHTML('beforeend','<tr><td><input></td><td><input type="number" step="any" value="0"></td><td><input type="number" step="any" value="0"></td><td><input type="number" step="any" value="0"></td><td><input type="number" step="any" value="0"></td><td><button type="button" onclick="this.closest(\'tr\').remove()">删除</button></td></tr>')}
let pricesDirty=false;document.querySelector('#prices').addEventListener('input',()=>{pricesDirty=true});setInterval(()=>{if(!pricesDirty&&!document.querySelector('#prices input:focus')){let m=document.querySelector('.usage-modal.open');localStorage.setItem('usage-quota-modal',m?m.dataset.id:'');location.reload()}},5000);
function savePrices(){let p={};document.querySelectorAll('#prices tbody tr').forEach(r=>{let i=r.querySelectorAll('input'),m=i[0].value.trim();if(m)p[m]={input:+i[1].value||0,output:+i[2].value||0,cache_read:+i[3].value||0,cache_write:+i[4].value||0}});pricesDirty=false;location.href=location.pathname+'?prices='+encodeURIComponent(JSON.stringify(p))}
function esc(v){return v.replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function closeUsageModal(){document.querySelectorAll('.usage-modal').forEach(m=>m.classList.remove('open'));localStorage.setItem('usage-quota-modal','')}
function openUsageModal(id){closeUsageModal();let m=Array.from(document.querySelectorAll('.usage-modal')).find(x=>x.dataset.id===id);if(m){m.classList.add('open');localStorage.setItem('usage-quota-modal',id)}}
function buildCredentialCards(){let panel=document.querySelector('.panel'),table=panel&&panel.querySelector('table');if(!table||!table.tBodies.length)return;let groups={};Array.from(table.tBodies[0].rows).forEach(r=>{let c=r.cells;if(c.length<9)return;let id=c[0].textContent.trim();(groups[id]??=[]).push(Array.from(c).slice(1).map(x=>x.textContent.trim()))});let wrap=document.createElement('div');wrap.id='credentialView';wrap.innerHTML='<input id="credentialSearch" placeholder="搜索凭证"> <div class="credential-grid"></div>';let grid=wrap.querySelector('.credential-grid');Object.entries(groups).map(([id,rows])=>{let req=rows.reduce((n,x)=>n+(+x[1]||0),0),read=rows.reduce((n,x)=>n+(+x[4]||0),0),eligible=rows.reduce((n,x)=>n+(+x[2]||0)+(+x[4]||0)+(+x[5]||0),0),cost=rows.reduce((n,x)=>n+(parseFloat(x[7])||0),0);return{id,rows,req,hit:eligible?read*100/eligible:0,cost}}).sort((a,b)=>b.cost-a.cost).forEach(g=>{let card=document.createElement('button');card.className='credential-card';card.dataset.id=g.id;card.innerHTML='<strong>'+esc(g.id)+'</strong><div><span>已使用金额<b>'+g.cost.toFixed(6)+' {{.Currency}}</b></span><span>请求数<b>'+g.req+'</b></span><span>缓存命中率<b>'+g.hit.toFixed(2)+'%</b></span></div><em>查看模型详情 →</em>';card.onclick=()=>openUsageModal(g.id);grid.append(card);let modal=document.createElement('div');modal.className='usage-modal';modal.dataset.id=g.id;modal.onclick=e=>{if(e.target===modal)closeUsageModal()};modal.innerHTML='<div class="modal-box"><div class="modal-head"><h2>'+esc(g.id)+'</h2><button onclick="closeUsageModal()">×</button></div><div class="modal-body"><table><thead><tr><th>模型</th><th>请求</th><th>普通输入</th><th>输出</th><th>缓存读</th><th>缓存写</th><th>命中率</th><th>费用</th></tr></thead><tbody>'+g.rows.map(x=>'<tr>'+x.map(v=>'<td>'+esc(v)+'</td>').join('')+'</tr>').join('')+'</tbody></table></div></div>';document.body.append(modal)});table.replaceWith(wrap);let s=document.querySelector('#credentialSearch');s.oninput=()=>{let q=s.value.toLowerCase();document.querySelectorAll('.credential-card').forEach(c=>c.hidden=!c.dataset.id.toLowerCase().includes(q))};let active=localStorage.getItem('usage-quota-modal');if(active)openUsageModal(active)}
document.addEventListener('keydown',e=>{if(e.key==='Escape')closeUsageModal()});document.head.insertAdjacentHTML('beforeend','<style>#credentialSearch{max-width:320px;margin-bottom:15px}.credential-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:14px}.credential-card{height:auto;text-align:left;padding:17px;border-radius:12px}.credential-card strong{display:block;overflow:hidden;text-overflow:ellipsis;margin-bottom:15px}.credential-card div{display:grid;grid-template-columns:repeat(3,1fr);gap:8px}.credential-card span{color:var(--muted);font-size:12px}.credential-card b{display:block;color:var(--text);font-size:16px;margin-top:4px}.credential-card em{display:block;text-align:right;color:var(--accent);font-style:normal;margin-top:15px}.usage-modal{display:none;position:fixed;inset:0;background:#0008;z-index:99;padding:5vh 4vw}.usage-modal.open{display:flex;align-items:center;justify-content:center}.modal-box{background:var(--card);border-radius:14px;width:min(1200px,96vw);max-height:90vh;overflow:hidden;box-shadow:0 24px 70px #0006}.modal-head{display:flex;align-items:center;padding:16px 20px;border-bottom:1px solid var(--line)}.modal-head h2{margin:0;overflow:hidden;text-overflow:ellipsis}.modal-head button{margin-left:auto;font-size:24px}.modal-body{overflow:auto;max-height:calc(90vh - 70px);padding:12px 20px 20px}@media(max-width:700px){.credential-card div{grid-template-columns:1fr}.usage-modal{padding:2vh 2vw}}</style>');buildCredentialCards();
document.head.insertAdjacentHTML('beforeend','<style>.credential-grid{gap:18px}.credential-card{position:relative;min-height:210px;padding:0!important;border:1px solid color-mix(in srgb,var(--accent) 16%,var(--line))!important;border-radius:16px!important;background:linear-gradient(145deg,color-mix(in srgb,var(--accent) 5%,var(--card)),var(--card) 55%)!important;box-shadow:0 2px 8px #0f172a0a;overflow:hidden;transition:transform .18s ease,box-shadow .18s ease,border-color .18s ease}.credential-card:before{content:"";position:absolute;inset:0 auto 0 0;width:4px;background:linear-gradient(180deg,var(--accent),color-mix(in srgb,var(--accent) 45%,#8b5cf6))}.credential-card:hover{transform:translateY(-3px);border-color:color-mix(in srgb,var(--accent) 42%,var(--line))!important;box-shadow:0 12px 28px #0f172a18}.credential-card>strong{display:flex!important;align-items:center;height:64px;margin:0!important;padding:0 20px 0 24px;border-bottom:1px solid var(--line);font-size:15px}.credential-card>strong:before{content:"";width:10px;height:10px;margin-right:10px;border-radius:50%;background:#22c55e;box-shadow:0 0 0 4px #22c55e1f;flex:none}.credential-card>div{display:grid!important;grid-template-columns:1.35fr 1fr 1fr!important;gap:0!important;padding:18px 20px 14px 24px}.credential-card>div span{padding:0 12px;border-right:1px solid var(--line)}.credential-card>div span:first-child{padding-left:0}.credential-card>div span:last-child{border:0;padding-right:0}.credential-card>div b{font-size:17px!important;white-space:nowrap}.credential-card>div span:first-child b{color:var(--accent);font-size:19px!important}.credential-card>em{margin:0!important;padding:0 20px 16px 24px;font-size:13px;font-weight:600}.credential-card:hover>em{transform:translateX(2px)}@media(max-width:700px){.credential-card>div{grid-template-columns:1fr 1fr!important;row-gap:15px!important}.credential-card>div span{border:0!important;padding:0!important}}</style>');
</script></body></html>`))
