package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUsageMetadata(t *testing.T) {
	um := map[string]interface{}{
		"promptTokenCount":     float64(100),
		"candidatesTokenCount": float64(25),
		"thoughtsTokenCount":   float64(10),
		"totalTokenCount":      float64(135),
	}
	p, c, th, tot := parseUsageMetadata(um)
	if p != 100 || c != 25 || th != 10 || tot != 135 {
		t.Fatalf("got %d %d %d %d", p, c, th, tot)
	}

	// total falls back to prompt+completion when upstream omits it
	um2 := map[string]interface{}{
		"promptTokenCount":     float64(50),
		"candidatesTokenCount": float64(5),
	}
	p, c, _, tot = parseUsageMetadata(um2)
	if p != 50 || c != 5 || tot != 55 {
		t.Fatalf("fallback total: got %d %d %d", p, c, tot)
	}

	p, c, th, tot = parseUsageMetadata(nil)
	if p+c+th+tot != 0 {
		t.Fatal("nil map should yield zeros")
	}
}

func TestUsageTrackerRecordAndSummary(t *testing.T) {
	t.Setenv("USAGE_LOG_PATH", filepath.Join(t.TempDir(), "usage.jsonl"))
	tr := NewUsageTracker()
	tr.Record(UsageEntry{Model: "gemini-3.8-flash-high", Endpoint: "chat_completions", PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})
	tr.Record(UsageEntry{Model: "gemini-3.8-flash-high", Endpoint: "chat_completions", Stream: true, PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60})
	tr.Record(UsageEntry{Model: "gemini-2.5-pro", Endpoint: "generateContent", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})

	s := tr.Summary()
	if s.TotalRequests != 3 {
		t.Fatalf("requests: got %d", s.TotalRequests)
	}
	if s.PromptTokens != 160 || s.CompletionTokens != 35 || s.TotalTokens != 195 {
		t.Fatalf("totals: %+v", s)
	}
	if s.ByModel["gemini-3.8-flash-high"].Requests != 2 {
		t.Fatalf("by_model: %+v", s.ByModel)
	}
	if len(s.ByDay) != 1 {
		t.Fatalf("by_day: %+v", s.ByDay)
	}

	// Persistence round-trip: a new tracker over the same file reloads entries.
	tr2 := NewUsageTracker()
	if got := tr2.Summary().TotalRequests; got != 3 {
		t.Fatalf("reload: got %d requests", got)
	}

	// Malformed lines are skipped, not fatal.
	f, _ := os.OpenFile(os.Getenv("USAGE_LOG_PATH"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("not json\n")
	_ = f.Close()
	tr3 := NewUsageTracker()
	if got := tr3.Summary().TotalRequests; got != 3 {
		t.Fatalf("malformed-line reload: got %d requests", got)
	}
}
