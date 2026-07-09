package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCaptureClaudeCodeRequest(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_CAPTURE_CLAUDE")) == "" {
		t.Skip("SIMPLEROUTER_CAPTURE_CLAUDE not set")
	}
	captured, out := captureClaudeCodeRequest(t)
	logCapturedClaudeCodeRequest(t, captured, out)
}

func TestCaptureClaudeCodeRequestWithThinkingDisplay(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_CAPTURE_CLAUDE")) == "" {
		t.Skip("SIMPLEROUTER_CAPTURE_CLAUDE not set")
	}
	captured, out := captureClaudeCodeRequestWithArgs(t, "--thinking-display", "summarized")
	logCapturedClaudeCodeRequest(t, captured, out)
}

func logCapturedClaudeCodeRequest(t *testing.T, captured, out []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, captured, "", "  "); err != nil {
		t.Logf("raw captured request:\n%s", captured)
	} else {
		t.Logf("captured request:\n%s", pretty.String())
	}
	t.Logf("claude output:\n%s", out)
}

func captureClaudeCodeRequest(t *testing.T) ([]byte, []byte) {
	t.Helper()
	return captureClaudeCodeRequestWithArgs(t)
}

func captureClaudeCodeRequestWithArgs(t *testing.T, extraArgs ...string) ([]byte, []byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			writeJSON(w, http.StatusOK, map[string]int{"input_tokens": 42})
		case "/v1/messages":
			body, _ := io.ReadAll(r.Body)
			captured = append([]byte(nil), body...)
			var req anthropicRequest
			_ = json.Unmarshal(body, &req)
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_capture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"%s\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n", req.Model)
				fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n")
				fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
				fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n")
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				return
			}
			writeJSON(w, http.StatusOK, &anthropicMessageResponse{
				ID:           "msg_capture",
				Type:         "message",
				Role:         "assistant",
				Model:        req.Model,
				Content:      []map[string]any{{"type": "text", "text": "pong"}},
				StopReason:   "end_turn",
				StopSequence: nil,
				Usage:        anthropicUsage{InputTokens: 1, OutputTokens: 1},
			})
		case "/v1/models":
			writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": "swe-1-7-lightning", "type": "model"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	claudePath, err := findClaude()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--model", "swe-1-7-lightning", "-p", "--output-format", "json"}
	args = append(args, extraArgs...)
	args = append(args, "Reply with exactly: pong")
	cmd := exec.Command(claudePath, args...)
	cmd.Dir = "."
	cmd.Env = buildClaudeEnvWithEffort(os.Environ(), srv.URL, "capture-key", "swe-1-7-lightning", 200_000, false, "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("claude failed: %v\n%s", err, out)
	}
	if len(captured) == 0 {
		t.Fatalf("no /v1/messages request captured; claude output:\n%s", out)
	}
	return captured, out
}
