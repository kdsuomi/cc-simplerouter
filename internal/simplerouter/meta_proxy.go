package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// The Meta Model API (api.meta.ai) serves an Anthropic-compatible
// /v1/messages endpoint, so unlike the translating proxies this one is a thin
// passthrough: it only strips the request fields Meta rejects with HTTP 400
// (stop_sequences, top_k), pins the picked model, and relays everything else
// — including the native Anthropic SSE stream — byte for byte.

func startMetaProxy(upstreamBase, model string, httpClient *http.Client, disableThinking bool) (baseURL string, stop func(), err error) {
	p := newMetaProxy(upstreamBase, model, httpClient, disableThinking)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: p}
	go server.Serve(listener)
	return fmt.Sprintf("http://%s", listener.Addr().String()), func() { _ = server.Close() }, nil
}

type metaProxy struct {
	upstreamBase    string
	model           string
	httpClient      *http.Client
	disableThinking bool
}

func newMetaProxy(upstreamBase, model string, httpClient *http.Client, disableThinking bool) *metaProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &metaProxy{
		upstreamBase:    strings.TrimRight(upstreamBase, "/"),
		model:           model,
		httpClient:      httpClient,
		disableThinking: disableThinking,
	}
}

func (p *metaProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		p.forward(w, r, "/messages")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		// Meta supports count_tokens natively; the body needs the same
		// sanitization as /messages (same schema, same rejections).
		p.forward(w, r, "/messages/count_tokens")
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": p.model, "type": "model"}}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		writeJSON(w, http.StatusOK, map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1/models/"), "type": "model"})
	default:
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown route "+r.URL.Path)
	}
}

func (p *metaProxy) forward(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	payload, err := sanitizeMetaRequest(body, p.model, p.disableThinking)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstreamBase+upstreamPath, bytes.NewReader(payload))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+apiKeyFromRequest(r))
	for _, name := range []string{"anthropic-version", "anthropic-beta"} {
		if v := r.Header.Get(name); v != "" {
			upReq.Header.Set(name, v)
		}
	}

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "Meta request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayCompatUpstreamError(w, resp, "Meta")
		return
	}
	// Upstream already speaks Anthropic (JSON and SSE alike): relay the body
	// verbatim, flushing per chunk so streamed events reach Claude Code live.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// sanitizeMetaRequest rewrites only what Meta requires and keeps every other
// field byte-identical (metadata, cache_control breakpoints, betas, ... are
// all natively supported upstream).
func sanitizeMetaRequest(body []byte, model string, disableThinking bool) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	// Meta rejects these with HTTP 400; Claude Code sends stop_sequences on
	// some utility calls, so dropping them beats hard-failing the request.
	delete(req, "stop_sequences")
	delete(req, "top_k")

	pinned, err := json.Marshal(metaModelID(model))
	if err != nil {
		return nil, err
	}
	req["model"] = pinned

	if disableThinking {
		req["thinking"] = json.RawMessage(`{"type":"disabled"}`)
	} else if rawThinking, ok := req["thinking"]; ok {
		// Meta enforces budget_tokens < max_tokens (Anthropic semantics);
		// clamp defensively rather than letting the request 400.
		var th anthropicThinking
		if json.Unmarshal(rawThinking, &th) == nil && th.Type == "enabled" && th.BudgetTokens > 0 {
			var maxTokens int
			if rawMax, ok := req["max_tokens"]; ok {
				_ = json.Unmarshal(rawMax, &maxTokens)
			}
			if maxTokens > 1 && th.BudgetTokens >= maxTokens {
				th.BudgetTokens = maxTokens - 1
				clamped, err := json.Marshal(th)
				if err != nil {
					return nil, err
				}
				req["thinking"] = clamped
			}
		}
	}
	return json.Marshal(req)
}

// metaModelID strips the "[1m]" long-context suffix claudeCodeModel appends
// (Muse Spark has a 1M context, so the suffix is always present).
func metaModelID(model string) string {
	return strings.TrimSuffix(strings.TrimSpace(model), "[1m]")
}
