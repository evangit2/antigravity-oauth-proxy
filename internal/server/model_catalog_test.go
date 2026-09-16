package server

import (
	"net/http"
	"testing"

	"github.com/dvcrn/antigravity-oauth-proxy/internal/antigravity"
)

func catalogWith(ids ...string) *antigravity.FetchAvailableModelsResponse {
	models := make(map[string]antigravity.AvailableModel, len(ids))
	for _, id := range ids {
		models[id] = antigravity.AvailableModel{DisplayName: id}
	}
	return &antigravity.FetchAvailableModelsResponse{Models: models}
}

func TestWantsRefresh(t *testing.T) {
	testCases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"refresh=1", true},
		{"refresh=true", true},
		{"refresh=TRUE", true},
		{"refresh=yes", true},
		{"refresh=0", false},
		{"refresh=false", false},
		{"refresh=", false},
		{"other=1", false},
		{"refresh=1&other=2", true},
	}

	for _, tc := range testCases {
		t.Run(tc.query, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/v1/models?"+tc.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := wantsRefresh(r); got != tc.want {
				t.Errorf("wantsRefresh(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestCatalogOffersModel(t *testing.T) {
	catalog := catalogWith("gemini-3.8-flash-high", "claude-sonnet-4-6")

	if !catalogOffersModel(catalog, "gemini-3.8-flash-high") {
		t.Error("expected catalog to offer gemini-3.8-flash-high")
	}
	if catalogOffersModel(catalog, "gemini-4-pro") {
		t.Error("catalog should not offer a model it lacks")
	}
	if catalogOffersModel(nil, "gemini-3.8-flash-high") {
		t.Error("nil catalog must not report a model as offered")
	}
	if catalogOffersModel(catalog, "") {
		t.Error("empty model ID must not be reported as offered")
	}
}

func TestPickFallbackModelPrefersBackendDefault(t *testing.T) {
	catalog := catalogWith("gemini-3.8-flash-high", "gemini-pro-agent")
	catalog.DefaultAgentModelID = "gemini-pro-agent"

	if got := pickFallbackModel(catalog); got != "gemini-pro-agent" {
		t.Errorf("pickFallbackModel() = %q, want the backend default %q", got, "gemini-pro-agent")
	}
}

func TestPickFallbackModelPrefersNewestAvailableCandidate(t *testing.T) {
	// Newest candidate absent: must fall through to the next one present.
	catalog := catalogWith("gemini-3.5-flash-extra-low", "claude-sonnet-4-6")

	if got := pickFallbackModel(catalog); got != "gemini-3.5-flash-extra-low" {
		t.Errorf("pickFallbackModel() = %q, want %q", got, "gemini-3.5-flash-extra-low")
	}
}

func TestPickFallbackModelTracksNewestReleasedModel(t *testing.T) {
	// None of the pinned candidates exist: a model released after this binary
	// was built must still be chosen over the hardcoded default.
	catalog := catalogWith("gemini-4.2-flash-high", "gemini-4.2-flash-low")

	if got := pickFallbackModel(catalog); got != "gemini-4.2-flash-high" {
		t.Errorf("pickFallbackModel() = %q, want %q", got, "gemini-4.2-flash-high")
	}
}

func TestPickFallbackModelNilCatalog(t *testing.T) {
	if got := pickFallbackModel(nil); got != fallbackModelCandidates[0] {
		t.Errorf("pickFallbackModel(nil) = %q, want %q", got, fallbackModelCandidates[0])
	}
}

func TestNewestGeminiFlashModel(t *testing.T) {
	testCases := []struct {
		name string
		ids  []string
		want string
	}{
		{
			name: "highest version wins",
			ids:  []string{"gemini-3.7-flash-high", "gemini-3.8-flash-high", "gemini-3.8-flash-low"},
			want: "gemini-3.8-flash-high",
		},
		{
			name: "double digit minor compares numerically",
			ids:  []string{"gemini-3.8-flash-high", "gemini-3.10-flash-high"},
			want: "gemini-3.10-flash-high",
		},
		{
			name: "next major wins",
			ids:  []string{"gemini-3.10-flash-high", "gemini-4-flash-high"},
			want: "gemini-4-flash-high",
		},
		{
			name: "non chat gemini ids are ignored",
			ids:  []string{"gemini-4-flash-image", "gemini-pro-agent", "gemini-3-flash-agent"},
			want: "",
		},
		{
			name: "claude and gpt ids are ignored",
			ids:  []string{"claude-opus-4-6-thinking", "gpt-oss-120b-medium"},
			want: "",
		},
		{
			name: "newer version beats better variant",
			ids:  []string{"gemini-3.8-flash-high", "gemini-3.9-flash-lite"},
			want: "gemini-3.9-flash-lite",
		},
		{
			name: "empty catalog",
			ids:  nil,
			want: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := newestGeminiFlashModel(catalogWith(tc.ids...))
			if got != tc.want {
				t.Errorf("newestGeminiFlashModel(%v) = %q, want %q", tc.ids, got, tc.want)
			}
		})
	}
}

func TestParseModelVersion(t *testing.T) {
	testCases := []struct {
		version      string
		major, minor int
	}{
		{"3.8", 3, 8},
		{"3.10", 3, 10},
		{"4", 4, 0},
		{"4.0", 4, 0},
		{"x", -1, -1},
	}

	for _, tc := range testCases {
		t.Run(tc.version, func(t *testing.T) {
			major, minor := parseModelVersion(tc.version)
			if major != tc.major || minor != tc.minor {
				t.Errorf("parseModelVersion(%q) = (%d, %d), want (%d, %d)", tc.version, major, minor, tc.major, tc.minor)
			}
		})
	}
}
