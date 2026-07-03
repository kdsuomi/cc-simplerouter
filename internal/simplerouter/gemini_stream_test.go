package simplerouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseEvent is one parsed Anthropic SSE event emitted by the translator.
type sseEvent struct {
	Name string
	Data map[string]any
}

func parseSSE(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, chunk := range strings.Split(raw, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(chunk, "\n") {
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				ev.Name = name
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				if err := json.Unmarshal([]byte(data), &ev.Data); err != nil {
					t.Fatalf("bad event data %q: %v", data, err)
				}
			}
		}
		events = append(events, ev)
	}
	return events
}

// runStream feeds a canned Gemini SSE transcript through the translator.
func runStream(t *testing.T, transcript string, sigs *signatureStore, ids ...string) []sseEvent {
	t.Helper()
	i := 0
	newID := func() string {
		if i < len(ids) {
			id := ids[i]
			i++
			return id
		}
		return newToolUseID()
	}
	rec := httptest.NewRecorder()
	tr := newGeminiStreamTranslator(rec, rec, "gemini-2.5-flash", sigs, newID)
	err := readGeminiSSE(strings.NewReader(transcript), tr.onChunk)
	if err != nil && !errors.Is(err, errStreamAborted) {
		t.Fatalf("readGeminiSSE: %v", err)
	}
	tr.finishStream()
	return parseSSE(t, rec.Body.String())
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = ev.Name
	}
	return names
}

func geminiChunk(parts string, extra string) string {
	return fmt.Sprintf(`data: {"candidates":[{"content":{"role":"model","parts":[%s]}%s}]}`, parts, extra) + "\n\n"
}

func TestStreamThinkingTextToolTransition(t *testing.T) {
	sigs := newSignatureStore()
	transcript := geminiChunk(`{"text":"let me ","thought":true}`, "") +
		geminiChunk(`{"text":"think","thought":true,"thoughtSignature":"dGhpbmtzaWc="}`, "") +
		geminiChunk(`{"text":"I will call a tool."}`, "") +
		geminiChunk(`{"functionCall":{"name":"get_weather","args":{"city":"London"}},"thoughtSignature":"Y2FsbHNpZw=="}`, `,"finishReason":"STOP"`) +
		"data: {\"usageMetadata\":{\"promptTokenCount\":50,\"candidatesTokenCount\":15,\"thoughtsTokenCount\":47}}\n\n"

	events := runStream(t, transcript, sigs, "toolu_1")
	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking_delta "let me "
		"content_block_delta", // thinking_delta "think"
		"content_block_delta", // signature_delta
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta", // text_delta
		"content_block_stop",
		"content_block_start", // tool_use
		"content_block_delta", // input_json_delta
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	got := eventNames(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v\nwant %v", got, want)
	}

	// Signature latched from the second thinking chunk, emitted at block close.
	sigDelta := events[4].Data["delta"].(map[string]any)
	if sigDelta["type"] != "signature_delta" || sigDelta["signature"] != "dGhpbmtzaWc=" {
		t.Errorf("signature_delta = %+v", sigDelta)
	}
	// Block indexes: thinking=0, text=1, tool_use=2.
	if events[9].Data["index"].(float64) != 2 {
		t.Errorf("tool_use index = %v", events[9].Data["index"])
	}
	toolBlock := events[9].Data["content_block"].(map[string]any)
	if toolBlock["id"] != "toolu_1" || toolBlock["name"] != "get_weather" {
		t.Errorf("tool_use block = %+v", toolBlock)
	}
	argsDelta := events[10].Data["delta"].(map[string]any)
	if argsDelta["partial_json"] != `{"city":"London"}` {
		t.Errorf("input_json_delta = %+v", argsDelta)
	}
	// message_delta: stop_reason tool_use (despite STOP) + usage math.
	md := events[12].Data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", md["delta"])
	}
	usage := md["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 50 || usage["output_tokens"].(float64) != 62 {
		t.Errorf("usage = %+v", usage)
	}
	// Store captured the functionCall signature.
	if rec, _ := sigs.lookup("toolu_1"); rec.signature != "Y2FsbHNpZw==" {
		t.Errorf("store = %+v", rec)
	}
}

func TestStreamSignatureLatchFirstWins(t *testing.T) {
	transcript := geminiChunk(`{"text":"a","thought":true,"thoughtSignature":"Zmlyc3Q="}`, "") +
		geminiChunk(`{"text":"b","thought":true}`, "") +
		geminiChunk(`{"text":"c","thought":true,"thoughtSignature":"c2Vjb25k"}`, `,"finishReason":"STOP"`)
	events := runStream(t, transcript, newSignatureStore())
	var sigDeltas []string
	for _, ev := range events {
		if ev.Name != "content_block_delta" {
			continue
		}
		delta := ev.Data["delta"].(map[string]any)
		if delta["type"] == "signature_delta" {
			sigDeltas = append(sigDeltas, delta["signature"].(string))
		}
	}
	if len(sigDeltas) != 1 || sigDeltas[0] != "Zmlyc3Q=" {
		t.Errorf("signature deltas = %v, want single first-seen value", sigDeltas)
	}
}

func TestStreamLateToolSignatureLatch(t *testing.T) {
	sigs := newSignatureStore()
	transcript := geminiChunk(`{"functionCall":{"name":"get_time","args":{}}}`, "") +
		geminiChunk(`{"thoughtSignature":"bGF0ZXNpZw=="}`, `,"finishReason":"STOP"`)
	runStream(t, transcript, sigs, "toolu_late")
	if rec, _ := sigs.lookup("toolu_late"); rec.signature != "bGF0ZXNpZw==" {
		t.Errorf("late signature not latched into store: %+v", rec)
	}
}

func TestStreamParallelToolCalls(t *testing.T) {
	sigs := newSignatureStore()
	transcript := geminiChunk(
		`{"functionCall":{"name":"get_weather","args":{"city":"London"}},"thoughtSignature":"c2lnMQ=="},{"functionCall":{"name":"get_time","args":{}}}`,
		`,"finishReason":"STOP"`)
	events := runStream(t, transcript, sigs, "toolu_a", "toolu_b")

	var toolStarts int
	for _, ev := range events {
		if ev.Name == "content_block_start" && ev.Data["content_block"].(map[string]any)["type"] == "tool_use" {
			toolStarts++
		}
	}
	if toolStarts != 2 {
		t.Errorf("tool_use blocks = %d, want 2", toolStarts)
	}
	if rec, _ := sigs.lookup("toolu_a"); rec.signature != "c2lnMQ==" {
		t.Errorf("first call = %+v", rec)
	}
	if rec, ok := sigs.lookup("toolu_b"); !ok || rec.signature != "" {
		t.Errorf("second call = %+v, want recorded with empty signature", rec)
	}
}

func TestStreamMidStreamError(t *testing.T) {
	transcript := geminiChunk(`{"text":"partial"}`, "") +
		"data: {\"error\":{\"code\":429,\"message\":\"quota exceeded\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n" +
		geminiChunk(`{"text":"never seen"}`, "")
	events := runStream(t, transcript, newSignatureStore())

	last := events[len(events)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %q, want error (no message_stop after abort)", last.Name)
	}
	errObj := last.Data["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" || errObj["message"] != "quota exceeded" {
		t.Errorf("error = %+v", errObj)
	}
	for _, ev := range events {
		if ev.Name == "content_block_delta" {
			if d := ev.Data["delta"].(map[string]any); d["type"] == "text_delta" && d["text"] == "never seen" {
				t.Error("stream should stop consuming after error chunk")
			}
		}
	}
}

func TestStreamEmptyYieldsValidMessage(t *testing.T) {
	events := runStream(t, "", newSignatureStore())
	got := eventNames(events)
	want := []string{"message_start", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v", got)
	}
}

func TestReadGeminiSSEOversizedLine(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	transcript := geminiChunk(fmt.Sprintf(`{"text":%q}`, big), `,"finishReason":"STOP"`)
	var texts []string
	err := readGeminiSSE(strings.NewReader(transcript), func(resp *geminiResponse) error {
		for _, p := range resp.Candidates[0].Content.Parts {
			texts = append(texts, p.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != 1 || len(texts[0]) != 100_000 {
		t.Errorf("oversized data line not parsed, got %d texts", len(texts))
	}
}

func TestReadGeminiSSEMalformedPayload(t *testing.T) {
	transcript := geminiChunk(`{"text":"ok"}`, "") +
		"data: <html>not json</html>\n\n" +
		geminiChunk(`{"text":"never reached"}`, "")
	count := 0
	err := readGeminiSSE(strings.NewReader(transcript), func(*geminiResponse) error {
		count++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "malformed gemini SSE payload") {
		t.Fatalf("err = %v, want malformed payload error", err)
	}
	if count != 1 {
		t.Errorf("emit count = %d, want 1 (only the chunk before the bad one)", count)
	}
}

func TestReadGeminiSSEFinalEventWithoutTrailingBlank(t *testing.T) {
	// Some servers end the body right after the last data line.
	transcript := `data: {"candidates":[{"content":{"parts":[{"text":"end"}]},"finishReason":"STOP"}]}`
	count := 0
	err := readGeminiSSE(strings.NewReader(transcript), func(*geminiResponse) error {
		count++
		return nil
	})
	if err != nil || count != 1 {
		t.Errorf("err=%v count=%d", err, count)
	}
}
