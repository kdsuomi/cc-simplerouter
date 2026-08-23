package simplerouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestLMStudioModelsUsesLoadedMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"models":[
			{"type":"llm","key":"qwen/qwen3.8-27b","display_name":"Qwen3.8 27B","max_context_length":262144,
			 "capabilities":{"trained_for_tool_use":true,"reasoning":{"allowed_options":["off","low","medium","xhigh","on"],"default":"xhigh"}},
			 "loaded_instances":[{"config":{"context_length":162048}}]},
			{"type":"llm","key":"plain/model","display_name":"Plain Model","max_context_length":32768,
			 "capabilities":{"trained_for_tool_use":false,"reasoning":false},"loaded_instances":[]},
			{"type":"embedding","key":"embed/model","display_name":"Embed","max_context_length":8192}
		]}`)
	}))
	defer server.Close()

	models, err := lmStudioModels(context.Background(), server.Client(), server.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	qwen := models[0]
	if qwen.ID != "qwen/qwen3.8-27b" || qwen.Name != "Qwen3.8 27B" || qwen.ContextLength != 162048 {
		t.Fatalf("Qwen metadata = %+v", qwen)
	}
	if !slices.Equal(qwen.SupportedParameters, []string{"tools", "reasoning"}) {
		t.Fatalf("parameters = %v", qwen.SupportedParameters)
	}
	if !slices.Equal(qwen.SupportedReasoningEfforts, []string{"none", "low", "medium", "xhigh"}) {
		t.Fatalf("reasoning efforts = %v", qwen.SupportedReasoningEfforts)
	}
	if qwen.DefaultReasoningEffort != "xhigh" || qwen.DefaultReasoningSummary != "auto" {
		t.Fatalf("reasoning defaults = %q/%q", qwen.DefaultReasoningEffort, qwen.DefaultReasoningSummary)
	}
	if qwen.AutoCompactTokenLimit != 129638 {
		t.Fatalf("auto compact = %d", qwen.AutoCompactTokenLimit)
	}
	if models[1].ContextLength != 32768 || len(models[1].SupportedParameters) != 0 {
		t.Fatalf("plain model = %+v", models[1])
	}
}

func TestLMStudioModelsReportsServerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "local server unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := lmStudioModels(context.Background(), server.Client(), server.URL+"/v1")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "local server unavailable") {
		t.Fatalf("error = %v", err)
	}
}
