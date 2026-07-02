package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const defaultGeminiAPIBase = "https://generativelanguage.googleapis.com/v1beta"

// startGeminiProxy launches a localhost proxy that translates the Anthropic
// Messages API (spoken by Claude Code) to the Gemini generateContent API.
// Unlike the OpenRouter provider proxy this is a full translator, not a
// reverse proxy: request bodies, response bodies, and SSE streams are all
// re-materialized. It returns the base URL to use as ANTHROPIC_BASE_URL and a
// stop func to shut it down.
//
// The Gemini API key is not stored here: Claude Code sends it back as
// Authorization: Bearer (from ANTHROPIC_AUTH_TOKEN) and the proxy forwards it
// as x-goog-api-key. The signature store lives for the proxy's lifetime — one
// Claude Code session.
func startGeminiProxy(upstreamBase, model string, httpClient *http.Client) (baseURL string, stop func(), err error) {
	p := newGeminiProxy(upstreamBase, model, httpClient)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: p}
	go server.Serve(listener)
	return fmt.Sprintf("http://%s", listener.Addr().String()), func() { _ = server.Close() }, nil
}

type geminiProxy struct {
	upstreamBase string
	model        string // for /v1/models responses only; request bodies carry the real model
	httpClient   *http.Client
	sigs         *signatureStore
	newID        func() string
}

func newGeminiProxy(upstreamBase, model string, httpClient *http.Client) *geminiProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &geminiProxy{
		upstreamBase: strings.TrimSuffix(upstreamBase, "/"),
		model:        model,
		httpClient:   httpClient,
		sigs:         newSignatureStore(),
		newID:        newToolUseID,
	}
}

func (p *geminiProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		p.handleMessages(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		p.handleCountTokens(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{{"id": p.model, "type": "model"}},
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		writeJSON(w, http.StatusOK, map[string]any{
			"id": strings.TrimPrefix(r.URL.Path, "/v1/models/"), "type": "model",
		})
	default:
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown route "+r.URL.Path)
	}
}

func (p *geminiProxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "parse body: "+err.Error())
		return
	}

	gemReq, err := anthropicToGemini(&req, p.sigs)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	payload, err := json.Marshal(gemReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	endpoint := "generateContent"
	query := ""
	if req.Stream {
		endpoint = "streamGenerateContent"
		query = "?alt=sse"
	}
	upstreamURL := fmt.Sprintf("%s/models/%s:%s%s", p.upstreamBase, url.PathEscape(geminiModelID(req.Model)), endpoint, query)

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("x-goog-api-key", apiKeyFromRequest(r))

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "gemini request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.relayUpstreamError(w, resp, payload)
		return
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		tr := newGeminiStreamTranslator(w, flusher, req.Model, p.sigs, p.newID)
		if err := readGeminiSSE(resp.Body, tr.onChunk); err == nil {
			tr.finishStream()
		}
		return
	}

	var gemResp geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gemResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode gemini response: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, geminiToAnthropic(&gemResp, req.Model, p.sigs, p.newID))
}

func (p *geminiProxy) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Token counting must never block the session: any upstream failure falls
	// back to a rough bytes/4 estimate.
	estimate := len(body) / 4
	gemReq, err := anthropicToGemini(&req, p.sigs)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
		return
	}
	payload, err := json.Marshal(map[string]any{"contents": gemReq.Contents})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
		return
	}
	upstreamURL := fmt.Sprintf("%s/models/%s:countTokens", p.upstreamBase, url.PathEscape(geminiModelID(req.Model)))
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("x-goog-api-key", apiKeyFromRequest(r))
	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
		return
	}
	defer resp.Body.Close()
	var counted geminiCountTokensResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&counted) != nil {
		writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimate})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": counted.TotalTokens})
}

func (p *geminiProxy) relayUpstreamError(w http.ResponseWriter, resp *http.Response, sentPayload []byte) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	var wrapped struct {
		Error *geminiError `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error != nil {
		message = wrapped.Error.Message
	}
	if resp.StatusCode == http.StatusBadRequest && os.Getenv("SIMPLEROUTER_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "simplerouter: gemini 400: %s\nrequest body: %s\n", message, sentPayload)
	}
	status, errType := anthropicErrorForStatus(resp.StatusCode)
	writeAnthropicError(w, status, errType, message)
}

// geminiModelID normalizes the model string Claude Code echoes back in request
// bodies: an optional "models/" prefix and the "[1m]" long-context suffix that
// claudeCodeModel appends must both be stripped before building the URL.
func geminiModelID(model string) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	model = strings.TrimSuffix(model, "[1m]")
	return model
}

// apiKeyFromRequest extracts the Gemini key that buildClaudeEnv passed to
// Claude Code as ANTHROPIC_AUTH_TOKEN (or ANTHROPIC_API_KEY).
func apiKeyFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func anthropicErrorForStatus(code int) (httpStatus int, errType string) {
	switch code {
	case http.StatusBadRequest:
		return http.StatusBadRequest, "invalid_request_error"
	case http.StatusUnauthorized:
		return http.StatusUnauthorized, "authentication_error"
	case http.StatusForbidden:
		return http.StatusForbidden, "permission_error"
	case http.StatusNotFound:
		return http.StatusNotFound, "not_found_error"
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return 529, "overloaded_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"encode response"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}
