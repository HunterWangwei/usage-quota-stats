package main

import (
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
