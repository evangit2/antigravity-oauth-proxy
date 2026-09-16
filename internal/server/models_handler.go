package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dvcrn/antigravity-oauth-proxy/internal/antigravity"
	"github.com/dvcrn/antigravity-oauth-proxy/internal/logger"
)

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Name    string `json:"name,omitempty"`
}

type openAIModelsListResponse struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

type apiErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *Server) modelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, err := s.fetchModelsForListing(r)
	if err != nil {
		logger.Get().Error().Err(err).Msg("Failed to fetch available models")
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// One timestamp for the whole listing: a per-model time.Now() lets entries in
	// the same response straddle a second boundary and report different values.
	created := time.Now().Unix()

	models := make([]openAIModel, 0, len(data.Models))
	for modelID := range data.Models {
		family := modelFamily(modelID)
		if !isSupportedFamily(family) {
			continue
		}
		models = append(models, newOpenAIModel(modelID, family, created))
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})

	// Handle request for a single model, e.g., /v1/models/gemini-2.5-pro
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) > 2 {
		requestedModelID := pathParts[2]
		for _, m := range models {
			if m.ID == requestedModelID {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(m)
				return
			}
		}
		http.NotFound(w, r)
		return
	}

	resp := openAIModelsListResponse{
		Object: "list",
		Data:   models,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// modelMissRefreshInterval bounds how often an unknown model ID may trigger an
// extra upstream catalogue fetch, so repeated bad IDs cannot hammer the backend.
const modelMissRefreshInterval = 30 * time.Second

// fetchModelsForListing returns the upstream model catalogue, honoring
// ?refresh=1 to bypass the local cache. Useful to pick up brand-new Google
// models the moment they appear, without waiting out the cache TTL.
func (s *Server) fetchModelsForListing(r *http.Request) (*antigravity.FetchAvailableModelsResponse, error) {
	if wantsRefresh(r) {
		return s.antigravityClient.FetchAvailableModelsForce(r.Context())
	}
	return s.antigravityClient.FetchAvailableModels(r.Context())
}

// wantsRefresh reports whether the caller asked to bypass the model cache.
func wantsRefresh(r *http.Request) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("refresh")))
	return v == "1" || v == "true" || v == "yes"
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var resp apiErrorResponse
	resp.Type = "error"
	resp.Error.Type = "api_error"
	resp.Error.Message = message
	_ = json.NewEncoder(w).Encode(resp)
}

// newOpenAIModel builds the listing entry for a single upstream model ID.
//
// Name echoes the model ID; see AvailableModel.DisplayName for why upstream's
// label is not used.
func newOpenAIModel(modelID, family string, created int64) openAIModel {
	ownedBy := "google"
	if family == "claude" {
		ownedBy = "anthropic"
	}
	return openAIModel{
		ID:      modelID,
		Object:  "model",
		Created: created,
		OwnedBy: ownedBy,
		Name:    modelID,
	}
}

// catalogOffersModel reports whether the catalog contains the given model ID
// (nil-safe, so callers can pass a catalog straight from a failed fetch).
func catalogOffersModel(catalog *antigravity.FetchAvailableModelsResponse, modelID string) bool {
	if catalog == nil || modelID == "" {
		return false
	}
	_, ok := catalog.Models[modelID]
	return ok
}

// fallbackModelCandidates are tried in order when a request names a model the
// backend does not offer. A short ordered list (rather than one hardcoded ID)
// keeps the fallback working as older models get retired.
var fallbackModelCandidates = []string{
	"gemini-3.8-flash-high",
	"gemini-3.7-flash-high",
	"gemini-3.5-flash-extra-low",
}

// newestGeminiFlashRegex matches the versioned Gemini Flash chat variants, e.g.
// "gemini-3.8-flash-high". Non-chat Gemini IDs (-image, -agent, bare previews)
// are excluded so the fallback never lands on a non-conversational model.
var newestGeminiFlashRegex = regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)-flash-(extra-low|low|medium|high|tiered|lite)$`)

// pickFallbackModel chooses the model to substitute for an unknown request.
//
// Preference order: the backend's own default agent model, then the known-good
// candidate list, then the newest versioned Gemini Flash variant actually
// present in the catalog — so the substitution tracks upstream releases instead
// of pinning a model that may already be gone.
func pickFallbackModel(catalog *antigravity.FetchAvailableModelsResponse) string {
	if catalog != nil {
		if catalogOffersModel(catalog, catalog.DefaultAgentModelID) {
			return catalog.DefaultAgentModelID
		}
		for _, candidate := range fallbackModelCandidates {
			if catalogOffersModel(catalog, candidate) {
				return candidate
			}
		}
		if newest := newestGeminiFlashModel(catalog); newest != "" {
			return newest
		}
	}
	return fallbackModelCandidates[0]
}

// geminiFlashVariantRank orders the Flash thinking levels for tie-breaking
// within one version. "high" wins because the fallback should be a capable
// general-purpose model; the rest follow from most to least capable.
var geminiFlashVariantRank = map[string]int{
	"high":      0,
	"tiered":    1,
	"medium":    2,
	"low":       3,
	"extra-low": 4,
	"lite":      5,
}

// newestGeminiFlashModel returns the highest-versioned Gemini Flash chat model
// in the catalog, or "" when the catalog has none.
//
// Versions compare numerically component-wise, not lexically: gemini-3.10 must
// outrank gemini-3.8, and a future gemini-4.x must outrank every 3.x. Ties
// within a version resolve by variant rank, then by ID, so the choice is
// deterministic despite iterating a map.
func newestGeminiFlashModel(catalog *antigravity.FetchAvailableModelsResponse) string {
	bestID := ""
	bestMajor, bestMinor, bestVariantRank := -1, -1, 1<<30
	for modelID := range catalog.Models {
		matches := newestGeminiFlashRegex.FindStringSubmatch(modelID)
		if matches == nil {
			continue
		}
		major, minor := parseModelVersion(matches[1])
		if major < 0 {
			continue
		}
		variantRank := geminiFlashVariantRank[matches[2]]
		better := major > bestMajor ||
			(major == bestMajor && minor > bestMinor) ||
			(major == bestMajor && minor == bestMinor && variantRank < bestVariantRank) ||
			(major == bestMajor && minor == bestMinor && variantRank == bestVariantRank && modelID > bestID)
		if better {
			bestID, bestMajor, bestMinor, bestVariantRank = modelID, major, minor, variantRank
		}
	}
	return bestID
}

// parseModelVersion splits a model version such as "3.10" into its numeric
// components. Unparseable components become -1 so they never win a comparison.
func parseModelVersion(version string) (major, minor int) {
	parts := strings.SplitN(version, ".", 2)
	v, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1, -1
	}
	major = v
	if len(parts) == 1 {
		return major, 0
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return -1, -1
	}
	return major, minor
}

func isSupportedModel(modelID string) bool {
	return isSupportedFamily(modelFamily(modelID))
}

func isSupportedFamily(family string) bool {
	return family == "claude" || family == "gemini" || family == "gpt" || family == "openai"
}

func modelFamily(modelID string) string {
	lower := strings.ToLower(modelID)
	if strings.Contains(lower, "claude") {
		return "claude"
	}
	if strings.Contains(lower, "gemini") {
		return "gemini"
	}
	if strings.Contains(lower, "gpt") || strings.Contains(lower, "openai") {
		return "gpt"
	}
	return "unknown"
}
