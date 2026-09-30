package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dvcrn/antigravity-oauth-proxy/internal/antigravity"
	"github.com/dvcrn/antigravity-oauth-proxy/internal/env"
	"github.com/dvcrn/antigravity-oauth-proxy/internal/logger"
)

// UsageEntry is one recorded LLM request with its token counts.
// Token counts come from upstream usageMetadata; a zero value means
// upstream did not report that field for the request.
type UsageEntry struct {
	Timestamp        time.Time `json:"timestamp"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"` // "chat_completions" | "generateContent" | "streamGenerateContent"
	Stream           bool      `json:"stream"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	ThoughtTokens    int       `json:"thought_tokens,omitempty"`
	TotalTokens      int       `json:"total_tokens"`
}

// ModelUsage aggregates token counts for a single model.
type ModelUsage struct {
	Requests         int `json:"requests"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ThoughtTokens    int `json:"thought_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`
}

// UsageSummary is the response shape of GET /admin/usage.
type UsageSummary struct {
	TotalRequests    int                    `json:"total_requests"`
	PromptTokens     int                    `json:"prompt_tokens"`
	CompletionTokens int                    `json:"completion_tokens"`
	ThoughtTokens    int                    `json:"thought_tokens,omitempty"`
	TotalTokens      int                    `json:"total_tokens"`
	ByModel          map[string]*ModelUsage `json:"by_model"`
	ByDay            map[string]*ModelUsage `json:"by_day"`
	// Tier is the account's current upstream plan (e.g. "Antigravity (free-tier)").
	Tier string `json:"tier,omitempty"`
	// Quota is Google's per-model allowance: fraction remaining in the
	// current window plus reset time. Absent when upstream is unreachable.
	Quota map[string]antigravity.QuotaStatus `json:"quota,omitempty"`
}

// UsageTracker records per-request token usage to a JSONL file and serves
// in-memory aggregates. It is safe for concurrent use.
type UsageTracker struct {
	mu      sync.Mutex
	path    string
	entries []UsageEntry
}

// NewUsageTracker loads prior entries (best effort) and returns the tracker.
// Path comes from USAGE_LOG_PATH, defaulting to
// ~/.config/antigravity-oauth-proxy/usage.jsonl. An empty USAGE_LOG_PATH
// disables file persistence (memory only).
func NewUsageTracker() *UsageTracker {
	path := env.GetOrDefault("USAGE_LOG_PATH", "")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".config", "antigravity-oauth-proxy", "usage.jsonl")
		}
	}
	t := &UsageTracker{path: path}
	if path != "" {
		t.load()
	}
	return t
}

func (t *UsageTracker) load() {
	f, err := os.Open(t.path)
	if err != nil {
		return // no history yet; not an error
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e UsageEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		t.entries = append(t.entries, e)
	}
	if len(t.entries) > 0 {
		logger.Get().Info().Str("path", t.path).Int("entries", len(t.entries)).Msg("Loaded usage history")
	}
}

// Record appends one entry to memory and to the JSONL log.
// Failures to persist are logged but never fail the request.
func (t *UsageTracker) Record(e UsageEntry) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	t.mu.Lock()
	t.entries = append(t.entries, e)
	t.mu.Unlock()

	if t.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		logger.Get().Warn().Err(err).Msg("Usage log dir creation failed")
		return
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		logger.Get().Warn().Err(err).Msg("Usage log write failed")
		return
	}
	defer f.Close()
	if b, err := json.Marshal(e); err == nil {
		_, _ = f.Write(append(b, '\n'))
	}
}

func addUsage(dst *ModelUsage, e UsageEntry) {
	dst.Requests++
	dst.PromptTokens += e.PromptTokens
	dst.CompletionTokens += e.CompletionTokens
	dst.ThoughtTokens += e.ThoughtTokens
	dst.TotalTokens += e.TotalTokens
}

// Summary aggregates all recorded entries.
func (t *UsageTracker) Summary() UsageSummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := UsageSummary{
		ByModel: map[string]*ModelUsage{},
		ByDay:   map[string]*ModelUsage{},
	}
	for _, e := range t.entries {
		s.TotalRequests++
		s.PromptTokens += e.PromptTokens
		s.CompletionTokens += e.CompletionTokens
		s.ThoughtTokens += e.ThoughtTokens
		s.TotalTokens += e.TotalTokens

		m := s.ByModel[e.Model]
		if m == nil {
			m = &ModelUsage{}
			s.ByModel[e.Model] = m
		}
		addUsage(m, e)

		day := s.ByDay[e.Timestamp.Format("2006-01-02")]
		if day == nil {
			day = &ModelUsage{}
			s.ByDay[e.Timestamp.Format("2006-01-02")] = day
		}
		addUsage(day, e)
	}
	return s
}

// usageHandler serves GET /admin/usage: locally recorded token consumption
// plus Google's per-model quota (fraction remaining + reset time) and the
// account tier. Pass ?refresh=1 to force a fresh upstream quota fetch
// instead of the 5-minute cached catalogue.
func (s *Server) usageHandler(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		http.Error(w, "Usage tracking not initialized", http.StatusInternalServerError)
		return
	}
	summary := s.usage.Summary()

	var models *antigravity.FetchAvailableModelsResponse
	var err error
	if r.URL.Query().Get("refresh") == "1" {
		models, err = s.antigravityClient.FetchAvailableModelsForce(r.Context())
	} else {
		models, err = s.antigravityClient.FetchAvailableModels(r.Context())
	}
	if err == nil && models != nil {
		quota := make(map[string]antigravity.QuotaStatus, len(models.Models))
		for id, m := range models.Models {
			if q, ok := m.ParsedQuota(); ok {
				quota[id] = q
			}
		}
		summary.Quota = quota
	} else {
		logger.Get().Warn().Err(err).Msg("Usage: upstream quota fetch failed")
	}

	if ca, err := s.antigravityClient.LoadCodeAssist(); err == nil {
		summary.Tier = ca.CurrentTier.Name + " (" + ca.CurrentTier.ID + ")"
	} else {
		logger.Get().Warn().Err(err).Msg("Usage: LoadCodeAssist failed")
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summary)
}

// toInt converts JSON-decoded numbers (float64/int/json.Number) to int.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

// parseUsageMetadata extracts token counts from an upstream usageMetadata map.
// Missing fields yield zeros.
func parseUsageMetadata(um map[string]interface{}) (prompt, completion, thoughts, total int) {
	if um == nil {
		return 0, 0, 0, 0
	}
	prompt = toInt(um["promptTokenCount"])
	completion = toInt(um["candidatesTokenCount"])
	thoughts = toInt(um["thoughtsTokenCount"])
	total = toInt(um["totalTokenCount"])
	if total == 0 {
		total = prompt + completion
	}
	return prompt, completion, thoughts, total
}
