package simplerouter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeminiModelsFiltersAndMaps(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		io.WriteString(w, `{"models": [
			{"name": "models/gemini-2.5-flash", "displayName": "Gemini 2.5 Flash", "inputTokenLimit": 1048576, "outputTokenLimit": 65536, "supportedGenerationMethods": ["generateContent", "countTokens"]},
			{"name": "models/gemini-2.0-flash", "displayName": "Gemini 2.0 Flash", "inputTokenLimit": 1048576, "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemini-embedding-001", "displayName": "Embedding", "inputTokenLimit": 2048, "supportedGenerationMethods": ["embedContent"]},
			{"name": "models/gemini-2.5-flash-preview-tts", "displayName": "TTS", "inputTokenLimit": 8192, "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/imagen-4.0-generate-001", "displayName": "Imagen", "inputTokenLimit": 480, "supportedGenerationMethods": ["predict"]},
			{"name": "models/gemini-2.5-flash-image", "displayName": "Image", "inputTokenLimit": 32768, "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/veo-3.1-generate-preview", "displayName": "Veo", "inputTokenLimit": 480, "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/gemma-4-31b-it", "displayName": "Gemma", "inputTokenLimit": 262144, "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/lyria-3-pro-preview", "displayName": "Lyria", "inputTokenLimit": 1048576, "supportedGenerationMethods": ["generateContent"]},
			{"name": "models/deep-research-preview-04-2026", "displayName": "Deep Research", "inputTokenLimit": 131072, "supportedGenerationMethods": ["generateContent"]}
		]}`)
	}))
	defer srv.Close()

	client := newGeminiClient(nil, srv.URL)
	models, err := client.models(context.Background(), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "test-key" {
		t.Errorf("x-goog-api-key = %q", gotKey)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want only 2.5-flash and 2.0-flash", models)
	}
	m := models[0]
	if m.ID != "gemini-2.5-flash" || m.Name != "Gemini 2.5 Flash" || m.ContextLength != 1048576 {
		t.Errorf("model = %+v", m)
	}
	if !supportsParameter(m, "tools") || !supportsParameter(m, "reasoning") {
		t.Errorf("2.5 model should synthesize tools+reasoning: %v", m.SupportedParameters)
	}
	if supportsParameter(models[1], "reasoning") {
		t.Errorf("2.0 model should not get the reasoning tag: %v", models[1].SupportedParameters)
	}
}

func TestGeminiModelsPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			io.WriteString(w, `{"models": [{"name": "models/gemini-2.5-flash", "displayName": "A", "inputTokenLimit": 1, "supportedGenerationMethods": ["generateContent"]}], "nextPageToken": "page2"}`)
			return
		}
		io.WriteString(w, `{"models": [{"name": "models/gemini-2.5-pro", "displayName": "B", "inputTokenLimit": 1, "supportedGenerationMethods": ["generateContent"]}]}`)
	}))
	defer srv.Close()

	models, err := newGeminiClient(nil, srv.URL).models(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[1].ID != "gemini-2.5-pro" {
		t.Errorf("paginated models = %+v", models)
	}
}

func TestGeminiValidateKeyStatuses(t *testing.T) {
	for _, tc := range []struct {
		status   int
		rejected bool
		ok       bool
	}{
		{200, false, true},
		{400, true, false}, // Google uses 400 INVALID_ARGUMENT for bad keys
		{401, true, false},
		{403, true, false},
		{429, false, false},
		{500, false, false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, `{}`)
			}))
			defer srv.Close()
			err := newGeminiClient(nil, srv.URL).validateKey(context.Background(), "k")
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v", err)
			}
			if tc.rejected != errors.Is(err, errGeminiKeyRejected) {
				t.Errorf("rejected = %v, want %v (err %v)", errors.Is(err, errGeminiKeyRejected), tc.rejected, err)
			}
		})
	}
}

func TestGeminiRecommendedOrdering(t *testing.T) {
	models := []Model{
		{ID: "gemini-2.0-flash"},
		{ID: "gemini-2.5-flash"},
		{ID: "gemini-3.1-pro-preview"},
		{ID: "gemini-flash-latest"},
	}
	ordered := orderModelsForPicker(models)
	if ordered[0].ID != "gemini-3.1-pro-preview" || ordered[1].ID != "gemini-2.5-flash" {
		t.Errorf("ordered = %+v", ordered)
	}
	// Non-recommended keep incoming relative order.
	if ordered[2].ID != "gemini-2.0-flash" || ordered[3].ID != "gemini-flash-latest" {
		t.Errorf("tail = %+v", ordered[2:])
	}
	if !isRecommendedModel("gemini-2.5-flash") || isRecommendedModel("gemini-2.0-flash") {
		t.Error("isRecommendedModel wrong for gemini ids")
	}
	// OpenRouter recommendations must still rank above everything.
	if !isRecommendedModel("z-ai/glm-5.2") || recommendedRank("z-ai/glm-5.2") >= recommendedRank("gemini-3.1-pro-preview") {
		t.Error("OpenRouter recommendation ranking broken")
	}
}
