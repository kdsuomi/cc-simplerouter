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

type responsesPassthroughOptions struct {
	Label       string
	ProviderTag string
}

type responsesPassthroughProxy struct {
	upstreamBase string
	model        string
	httpClient   *http.Client
	options      responsesPassthroughOptions
}

func startResponsesPassthroughProxy(upstreamBase, model string, httpClient *http.Client, options responsesPassthroughOptions) (baseURL string, stop func(), err error) {
	proxy := newResponsesPassthroughProxy(upstreamBase, model, httpClient, options)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: proxy}
	go func() { _ = server.Serve(listener) }()
	return fmt.Sprintf("http://%s/v1", listener.Addr().String()), func() { _ = server.Close() }, nil
}

func newResponsesPassthroughProxy(upstreamBase, model string, httpClient *http.Client, options responsesPassthroughOptions) *responsesPassthroughProxy {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(options.Label) == "" {
		options.Label = "upstream"
	}
	return &responsesPassthroughProxy{
		upstreamBase: strings.TrimRight(upstreamBase, "/"),
		model:        model,
		httpClient:   httpClient,
		options:      options,
	}
}

func (p *responsesPassthroughProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/responses"):
		p.forwardResponses(w, r)
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{
			"id": p.model, "object": "model",
		}}})
	default:
		writeResponsesError(w, http.StatusNotFound, "not_found", "unknown route "+r.URL.Path)
	}
}

func (p *responsesPassthroughProxy) forwardResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "parse body: "+err.Error())
		return
	}
	model, _ := json.Marshal(p.model)
	request["model"] = model
	if tag := strings.TrimSpace(p.options.ProviderTag); tag != "" {
		var provider map[string]any
		if raw := request["provider"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &provider)
		}
		if provider == nil {
			provider = map[string]any{}
		}
		provider["only"] = []string{tag}
		provider["allow_fallbacks"] = false
		encoded, _ := json.Marshal(provider)
		request["provider"] = encoded
	}
	payload, err := json.Marshal(request)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "encode request: "+err.Error())
		return
	}

	endpoint := p.upstreamBase
	if !strings.HasSuffix(endpoint, "/responses") {
		endpoint += "/responses"
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("Authorization", "Bearer "+apiKeyFromRequest(r))
	upReq.Header.Set("X-Title", "simplerouter")

	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "api_error", p.options.Label+" request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayResponsesUpstreamError(w, resp, p.options.Label)
		return
	}
	for _, name := range []string{
		"Content-Type",
		"Cache-Control",
		"OpenAI-Request-ID",
		"X-Request-ID",
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
	} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}
