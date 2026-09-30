package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type FetchAvailableModelsResponse struct {
	Models              map[string]AvailableModel `json:"models"`
	DefaultAgentModelID string                    `json:"defaultAgentModelId,omitempty"`
}

type AvailableModel struct {
	// DisplayName is upstream's human label. Do not surface it to clients: it is
	// neither unique nor accurate. Four distinct IDs (gemini-2.5-flash,
	// -flash-lite, -flash-thinking, gemini-3.1-flash-lite) all report "Gemini 3.1
	// Flash Lite" and two report "Gemini 3.1 Pro (High)", so clients keying off
	// the label see duplicates; the legacy gemini-2.5-flash* aliases are labelled
	// for a different model entirely; and the *-flash-tiered IDs have no label at
	// all. Report the model ID instead — it is unique and is what callers must
	// pass back to us anyway.
	DisplayName string          `json:"displayName"`
	QuotaInfo   json.RawMessage `json:"quotaInfo,omitempty"`
}

// QuotaStatus is Google's per-model allowance: how much of the current
// window is left and when it resets.
type QuotaStatus struct {
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time,omitempty"`
}

// ParsedQuota decodes the upstream quotaInfo blob. ok is false when upstream
// sent nothing usable.
func (m AvailableModel) ParsedQuota() (q QuotaStatus, ok bool) {
	if len(m.QuotaInfo) == 0 {
		return QuotaStatus{}, false
	}
	var raw struct {
		RemainingFraction *float64 `json:"remainingFraction"`
		ResetTime         string   `json:"resetTime"`
	}
	if err := json.Unmarshal(m.QuotaInfo, &raw); err != nil || raw.RemainingFraction == nil {
		return QuotaStatus{}, false
	}
	return QuotaStatus{RemainingFraction: *raw.RemainingFraction, ResetTime: raw.ResetTime}, true
}

func (c *Client) FetchAvailableModels(ctx context.Context) (*FetchAvailableModelsResponse, error) {
	return c.fetchAvailableModels(ctx, false)
}

// FetchAvailableModelsForce bypasses the cache and queries the upstream
// backend, so newly released models show up immediately.
func (c *Client) FetchAvailableModelsForce(ctx context.Context) (*FetchAvailableModelsResponse, error) {
	return c.fetchAvailableModels(ctx, true)
}

// modelsCacheTTL bounds how stale the model listing can get. Kept short on
// purpose: Google adds new models server-side at any time and the proxy must
// surface them without a binary update or restart.
const modelsCacheTTL = 5 * time.Minute

// ModelsCacheAge reports how long ago the cached catalogue was fetched. It
// returns a huge duration when nothing is cached, so callers can treat "never
// fetched" as maximally stale.
func (c *Client) ModelsCacheAge() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.modelsCache == nil {
		return time.Duration(1 << 62)
	}
	return time.Since(c.modelsCacheTime)
}

func (c *Client) fetchAvailableModels(ctx context.Context, force bool) (*FetchAvailableModelsResponse, error) {
	c.mu.RLock()
	cached := c.modelsCache
	fresh := c.modelsCache != nil && time.Since(c.modelsCacheTime) < modelsCacheTTL
	c.mu.RUnlock()
	if fresh && !force {
		return cached, nil
	}

	bodyBytes, err := json.Marshal(map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	var lastErr error
	for _, endpoint := range Endpoints {
		url := fmt.Sprintf("%s/v1internal:fetchAvailableModels", endpoint)
		resp, err := c.doRequest(ctx, http.MethodPost, url, bodyBytes, "application/json")
		if err != nil {
			lastErr = err
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("could not read response body: %w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("fetchAvailableModels failed with status %d: %s", resp.StatusCode, string(respBody))
			continue
		}

		var result FetchAvailableModelsResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("could not unmarshal response body: %w", err)
		}

		c.mu.Lock()
		c.modelsCache = &result
		c.modelsCacheTime = time.Now()
		c.mu.Unlock()

		return &result, nil
	}

	if lastErr != nil {
		// Serve the last-known listing rather than failing: a transient
		// upstream blip must not hide (new) models from clients.
		c.mu.RLock()
		stale := c.modelsCache
		c.mu.RUnlock()
		if stale != nil {
			return stale, nil
		}
		return nil, lastErr
	}
	return nil, fmt.Errorf("fetchAvailableModels failed with no endpoints available")
}
