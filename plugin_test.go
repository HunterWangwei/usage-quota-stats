package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegularInputTokens(t *testing.T) {
	if got := regularInputTokens("openai", 100, 30, 20); got != 50 {
		t.Fatalf("OpenAI regular input = %d, want 50", got)
	}
	if got := regularInputTokens("claude", 100, 30, 20); got != 100 {
		t.Fatalf("Claude regular input = %d, want 100", got)
	}
}

func TestUsageChannelPrefersUsageMetadata(t *testing.T) {
	tests := []struct {
		provider string
		authType string
		authID   string
		want     string
	}{
		{provider: "codex", authType: "oauth", authID: "custom.json", want: "codex"},
		{provider: "xai", authType: "oauth", authID: "uuid.json", want: "xai"},
		{provider: "antigravity", authType: "oauth", authID: "custom.json", want: "antigravity"},
		{provider: "gemini", authType: "gemini-cli", authID: "custom.json", want: "gemini"},
		{authID: "gemini-cli-account.json", want: "gemini"},
		{authID: "expodev0-gmail.com-gemini-cli.json", want: "gemini"},
		{authID: "codex-account.json", want: "codex"},
	}
	for _, test := range tests {
		if got := usageChannel(test.provider, test.authType, test.authID); got != test.want {
			t.Fatalf("usageChannel(%q, %q, %q) = %q, want %q", test.provider, test.authType, test.authID, got, test.want)
		}
	}
}

func TestLookupPriceSupportsPrefix(t *testing.T) {
	prices := map[string]modelPrices{"gpt-*": {Input: 1}, "gpt-5.*": {Input: 1.5}, "gpt-5.4": {Input: 2}}
	if got, ok := lookupPrice(prices, "gpt-5.4"); !ok || got.Input != 2 {
		t.Fatalf("exact price = %#v, %t", got, ok)
	}
	if got, ok := lookupPrice(prices, "gpt-5.3"); !ok || got.Input != 1.5 {
		t.Fatalf("longest prefix price = %#v, %t", got, ok)
	}
}

func TestUsageHandleAggregatesAndRenders(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	if err := applyConfig([]byte("currency: CNY\ndata-file: " + path + "\nprices:\n  gpt-5.4:\n    input: 2\n    output: 8\n    cache-read: 1\n    cache-write: 3\n")); err != nil {
		t.Fatal(err)
	}
	record := usageRecord{Provider: "openai", Model: "gpt-5.4", AuthID: "config-a", Detail: usageDetail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 40, CacheCreationTokens: 10}}
	raw, _ := json.Marshal(record)
	if _, err := handleMethod("usage.handle", raw); err != nil {
		t.Fatal(err)
	}
	html, err := renderDashboard()
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{"config-a", "gpt-5.4", "40.00%", "CNY"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}

func TestUnpricedModelsAreNotListedAsRules(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	if err := applyConfig([]byte("data-file: " + path + "\nprices:\n  claude-*:\n    input: 3\n    output: 15\n")); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"claude-sonnet-4", "claude-sonnet-4", "gpt-5.4"} {
		raw, _ := json.Marshal(usageRecord{Provider: "claude", Model: model, AuthID: "config-a", Detail: usageDetail{InputTokens: 10}})
		if _, err := handleMethod("usage.handle", raw); err != nil {
			t.Fatal(err)
		}
	}
	html, err := renderDashboard()
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	if strings.Contains(page, `class="model" value="claude-sonnet-4"`) || strings.Contains(page, `class="model" value="gpt-5.4"`) {
		t.Fatal("used models must not be rendered as editable price rules")
	}
	for _, want := range []string{`class="model" value="claude-*"`, "1 个模型 · 2 次请求", `data-model="gpt-5.4"`, "1 个模型未定价"} {
		if !strings.Contains(page, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}

func TestParseImportedPrices(t *testing.T) {
	yamlText := "plugins:\n  configs:\n    usage-quota-stats:\n      prices:\n        claude-*:\n          input: 3\n          cache-read: 0.3\n        dall-e:\n          mode: image\n          request: \"0.04\"\n"
	prices, err := parseImportedPrices(yamlText)
	if err != nil {
		t.Fatal(err)
	}
	if got := prices["claude-*"]; got.Input != 3 || got.CacheRead != 0.3 || got.Mode != "token" {
		t.Fatalf("claude-* = %#v", got)
	}
	if got := prices["dall-e"]; got.Mode != "request" || got.Request != 0.04 {
		t.Fatalf("dall-e = %#v", got)
	}
	exported := `{"gpt-5.4":{"mode":"token","input":1.25,"output":10,"cache_read":0.125,"cache_write":0,"request":0,"second":0}}`
	if prices, err = parseImportedPrices(exported); err != nil || prices["gpt-5.4"].CacheRead != 0.125 {
		t.Fatalf("exported JSON = %#v, %v", prices, err)
	}
	if _, err = parseImportedPrices("gpt:\n  input: -1\n"); err == nil {
		t.Fatal("negative price should be rejected")
	}
}

func TestUpdatePricesRejectsNegativeAndDropsBlankModels(t *testing.T) {
	path := t.TempDir() + "/usage.jsonl"
	if err := applyConfig([]byte("data-file: " + path + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := updatePrices(`{"gpt":{"input":-1}}`); err == nil {
		t.Fatal("negative price should be rejected")
	}
	if err := updatePrices(`{" ":{"input":1},"gpt-*":{"mode":"image","request":0.1}}`); err != nil {
		t.Fatal(err)
	}
	if len(state.config.Prices) != 1 || state.config.Prices["gpt-*"].Mode != "request" {
		t.Fatalf("prices = %#v", state.config.Prices)
	}
}
