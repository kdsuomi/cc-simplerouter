package simplerouter

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestProbeClaudeCodeThinkingStreaming(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM")) == "" {
		t.Skip("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM not set")
	}
	probeClaudeCodeThinkingStreaming(t, false, false, "")
}

func TestProbeClaudeCodeThinkingStreamingWithRepeatedSignatures(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM")) == "" {
		t.Skip("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM not set")
	}
	probeClaudeCodeThinkingStreaming(t, true, false, "")
}

func TestProbeClaudeCodeThinkingStreamingWithSplitBlocks(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM")) == "" {
		t.Skip("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM not set")
	}
	probeClaudeCodeThinkingStreaming(t, false, true, "")
}

func TestProbeClaudeCodeThinkingStreamingWithSummarizedDisplay(t *testing.T) {
	if strings.TrimSpace(os.Getenv("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM")) == "" {
		t.Skip("SIMPLEROUTER_PROBE_CLAUDE_THINKING_STREAM not set")
	}
	probeClaudeCodeThinkingStreaming(t, false, false, "summarized")
}

func probeClaudeCodeThinkingStreaming(t *testing.T, repeatSignatures, splitBlocks bool, thinkingDisplay string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			writeJSON(w, http.StatusOK, map[string]int{"input_tokens": 42})
		case "/v1/models":
			writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{"id": "probe-model", "type": "model"}}})
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, _ := w.(http.Flusher)
			writeSSE := func(event, data string) {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
				if flusher != nil {
					flusher.Flush()
				}
			}
			writeSSE("message_start", `{"type":"message_start","message":{"id":"msg_probe","type":"message","role":"assistant","model":"probe-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
			blockIndex := 0
			if !splitBlocks {
				writeSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
			}
			for i, chunk := range []string{"alpha ", "bravo ", "charlie "} {
				time.Sleep(750 * time.Millisecond)
				if splitBlocks {
					blockIndex = i
					writeSSE("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, blockIndex))
				}
				writeSSE("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%q}}`, blockIndex, chunk))
				if repeatSignatures {
					writeSSE("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":"c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="}}`, blockIndex))
				}
				if splitBlocks {
					writeSSE("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":"c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="}}`, blockIndex))
					writeSSE("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, blockIndex))
				}
			}
			time.Sleep(750 * time.Millisecond)
			if !repeatSignatures && !splitBlocks {
				writeSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="}}`)
			}
			if !splitBlocks {
				writeSSE("content_block_stop", `{"type":"content_block_stop","index":0}`)
				blockIndex = 0
			}
			textIndex := blockIndex + 1
			writeSSE("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, textIndex))
			writeSSE("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":"done"}}`, textIndex))
			writeSSE("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, textIndex))
			writeSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":1,"output_tokens":4}}`)
			writeSSE("message_stop", `{"type":"message_stop"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	claudePath, err := findClaude()
	if err != nil {
		t.Fatal(err)
	}
	debugFile := t.TempDir() + "/claude-debug.log"
	args := []string{
		"--model", "probe-model",
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--debug-file", debugFile,
		"--no-session-persistence",
	}
	if thinkingDisplay != "" {
		args = append(args, "--thinking-display", thinkingDisplay)
	}
	args = append(args, "trigger slow thinking")
	cmd := exec.Command(claudePath, args...)
	cmd.Dir = "."
	cmd.Env = append(buildClaudeEnvWithEffort(os.Environ(), srv.URL, "probe-key", "probe-model", 200_000, false, ""), "CLAUDE_CODE_EAGER_FLUSH=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, stderr)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		t.Logf("%6dms stdout: %s", time.Since(start).Milliseconds(), scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		debug, _ := os.ReadFile(debugFile)
		t.Fatalf("claude failed: %v\ndebug:\n%s", err, debug)
	}
	debug, _ := os.ReadFile(debugFile)
	for _, line := range strings.Split(string(debug), "\n") {
		if strings.Contains(line, "thinking_delta") || strings.Contains(line, "content_block_start") || strings.Contains(line, "signature_delta") {
			t.Logf("debug: %s", line)
		}
	}
}
