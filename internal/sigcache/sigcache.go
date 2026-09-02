// Package sigcache remembers Gemini thought signatures emitted by the model so
// they can be re-attached when a client replays tool calls with bare IDs.
//
// Background: Gemini 3 requires every functionCall part that originally carried
// a thoughtSignature to include it again when the conversation is replayed, or
// the request fails with 400 "Function call is missing a thought_signature".
// The OpenAI protocol has no field for thought signatures, so clients
// round-tripping through it lose them. The proxy encodes signatures into the
// tool-call ID (call_<uuid>|<sig>), but many harnesses strip or normalize IDs,
// so this server-side cache is the reliable fallback.
package sigcache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type entry struct {
	Signature string `json:"signature"`
	CreatedAt int64  `json:"created_at"`
}

type Cache struct {
	mu      sync.Mutex
	entries map[string]entry
	path    string
}

func cachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "antigravity-oauth-proxy", "thought_signatures.json")
}

// global is the process-wide cache, loaded lazily from disk on first use.
var global = func() *Cache {
	c := &Cache{entries: make(map[string]entry), path: cachePath()}
	if c.path == "" {
		return c
	}
	if data, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(data, &c.entries)
	}
	return c
}()

// Store records the signature for a bare call ID (no |sig suffix).
func Store(callID, signature string) {
	if callID == "" || signature == "" {
		return
	}
	c := global
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[callID] = entry{Signature: signature, CreatedAt: time.Now().Unix()}
	// prune: keep at most 5000 entries, drop ones older than 48h first
	if len(c.entries) > 5000 {
		cutoff := time.Now().Add(-48 * time.Hour).Unix()
		for k, v := range c.entries {
			if v.CreatedAt < cutoff {
				delete(c.entries, k)
			}
		}
	}
	if c.path == "" {
		return
	}
	if data, err := json.Marshal(c.entries); err == nil {
		_ = os.MkdirAll(filepath.Dir(c.path), 0700)
		_ = os.WriteFile(c.path, data, 0600)
	}
}

// Lookup returns the cached signature for a bare call ID, or "".
func Lookup(callID string) string {
	if callID == "" {
		return ""
	}
	c := global
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[callID]; ok {
		return e.Signature
	}
	return ""
}
