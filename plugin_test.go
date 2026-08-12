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
