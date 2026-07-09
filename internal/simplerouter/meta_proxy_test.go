package simplerouter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSanitizeMetaRequestStripsRejectedFields(t *testing.T) {
	body := []byte(`{
		"model": "muse-spark-1.1[1m]",
		"max_tokens": 100,
		"stop_sequences": ["foo"],
		"top_k": 40,
		"metadata": {"user_id": "u1"},
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	out, err := sanitizeMetaRequest(body, "muse-spark-1.1[1m]", false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["stop_sequences"]; ok {
		t.Error("stop_sequences not stripped")
	}
	if _, ok := got["top_k"]; ok {
		t.Error("top_k not stripped")
	}
	if string(got["model"]) != `"muse-spark-1.1"` {
		t.Errorf("model = %s", got["model"])
	}
	// Fields the shared anthropicRequest struct would drop must survive.
	if string(got["metadata"]) != `{"user_id":"u1"}` {
		t.Errorf("metadata not preserved: %s", got["metadata"])
	}
	if _, ok := got["messages"]; !ok {
		t.Error("messages missing")
	}
}

func TestSanitizeMetaRequestClampsThinkingBudget(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":300,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[]}`)
	out, err := sanitizeMetaRequest(body, "muse-spark-1.1", false)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Thinking anthropicThinking `json:"thinking"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != 299 {
		t.Errorf("thinking = %+v", got.Thinking)
	}

	// A budget already under max_tokens passes through untouched.
	body = []byte(`{"model":"m","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`)
	out, err = sanitizeMetaRequest(body, "muse-spark-1.1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking.BudgetTokens != 1024 {
		t.Errorf("budget changed: %+v", got.Thinking)
	}
}

func TestSanitizeMetaRequestDisableThinking(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":50},"messages":[]}`)
	out, err := sanitizeMetaRequest(body, "muse-spark-1.1", true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["thinking"]) != `{"type":"disabled"}` {
		t.Errorf("thinking = %s", got["thinking"])
	}
}

func metaTestUpstream(t *testing.T, handler http.HandlerFunc) (*metaProxy, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	return newMetaProxy(upstream.URL, "muse-spark-1.1", upstream.Client(), false), upstream
}

func TestMetaProxyForwardsMessages(t *testing.T) {
	var gotPath, gotAuth, gotBeta string
	var gotBody []byte
	p, _ := metaTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotBody, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, map[string]any{"type": "message", "content": []any{map[string]any{"type": "text", "text": "pong"}}})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"muse-spark-1.1[1m]","max_tokens":10,"stop_sequences":["x"],"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/messages" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBeta != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q", gotBeta)
	}
	if strings.Contains(string(gotBody), "stop_sequences") {
		t.Errorf("stop_sequences reached upstream: %s", gotBody)
	}
	if !strings.Contains(string(gotBody), `"model":"muse-spark-1.1"`) {
		t.Errorf("model not pinned: %s", gotBody)
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("response not relayed: %s", rec.Body.String())
	}
}

func TestMetaProxyRelaysStreamVerbatim(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	p, _ := metaTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"muse-spark-1.1","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Body.String() != sse {
		t.Errorf("stream altered:\n%s", rec.Body.String())
	}
}

func TestMetaProxyForwardsCountTokens(t *testing.T) {
	var gotPath string
	p, _ := metaTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, map[string]int{"input_tokens": 9})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"muse-spark-1.1[1m]","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || gotPath != "/messages/count_tokens" {
		t.Fatalf("status = %d, path = %q", rec.Code, gotPath)
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":9`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestMetaProxyRelaysUpstreamError(t *testing.T) {
	p, _ := metaTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":1,"messages":[]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "slow down") || !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestMetaProxyServesLocalModels(t *testing.T) {
	p := newMetaProxy("http://unused", "muse-spark-1.1", nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "muse-spark-1.1") {
		t.Fatalf("models: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/other", nil)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", rec.Code)
	}
}

func TestMetaProxyRejectsBadJSON(t *testing.T) {
	p := newMetaProxy("http://unused", "muse-spark-1.1", nil, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Errorf("body = %s", rec.Body.String())
	}
}
