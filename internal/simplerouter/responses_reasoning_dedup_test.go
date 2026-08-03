package simplerouter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func deltaBlock(itemID string, delta string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type":          "response.reasoning_text.delta",
		"item_id":       itemID,
		"content_index": 0,
		"delta":         delta,
	})
	return []byte("event: response.reasoning_text.delta\ndata: " + string(payload) + "\n\n")
}

func reasoningDoneBlock(itemID, text string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type":          "response.reasoning_text.done",
		"item_id":       itemID,
		"content_index": 0,
		"text":          text,
	})
	return []byte("event: response.reasoning_text.done\ndata: " + string(payload) + "\n\n")
}

func reasoningItemDoneBlock(itemID, text string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]any{
			"type":    "reasoning",
			"id":      itemID,
			"summary": []any{},
			"content": []any{map[string]any{"type": "reasoning_text", "text": text}},
		},
	})
	return []byte("event: response.output_item.done\ndata: " + string(payload) + "\n\n")
}

func filterOutput(t *testing.T, blocks ...[]byte) string {
	t.Helper()
	filter := newReasoningReplayFilter()
	var out strings.Builder
	for _, block := range blocks {
		for _, emitted := range filter.processBlock(block) {
			out.Write(emitted)
		}
	}
	for _, emitted := range filter.finish() {
		out.Write(emitted)
	}
	return out.String()
}

func reasoningDeltaTexts(t *testing.T, stream string) []string {
	t.Helper()
	var deltas []string
	for _, line := range strings.Split(stream, "\n") {
		payload, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("parse forwarded event %q: %v", line, err)
		}
		if event.Type == "response.reasoning_text.delta" {
			deltas = append(deltas, event.Delta)
		}
	}
	return deltas
}

// The observed wrapper behavior: many small deltas, then one delta replaying
// the entire text, then doubled terminal artifacts.
func TestReasoningReplayFilterDropsFullReplayAndHalvesArtifacts(t *testing.T) {
	text := "The user asks a quick math question: is 391 prime? 391 = 17 * 23, so it is composite."
	var blocks [][]byte
	for _, chunk := range splitChunks(text, 7) {
		blocks = append(blocks, deltaBlock("rs_1", chunk))
	}
	blocks = append(blocks,
		deltaBlock("rs_1", text),
		reasoningDoneBlock("rs_1", text+text),
		reasoningItemDoneBlock("rs_1", text+text),
	)

	out := filterOutput(t, blocks...)
	if joined := strings.Join(reasoningDeltaTexts(t, out), ""); joined != text {
		t.Fatalf("forwarded deltas = %q", joined)
	}
	if strings.Contains(out, jsonEscape(text+text)) {
		t.Fatalf("doubled text leaked into output:\n%s", out)
	}
	if !strings.Contains(out, `"type":"response.output_item.done"`) {
		t.Fatalf("item done event missing:\n%s", out)
	}
	if got := strings.Count(out, jsonEscape(text)); got != 2 {
		// Once in reasoning_text.done, once in output_item.done content.
		t.Fatalf("expected the halved text twice in terminal artifacts, got %d:\n%s", got, out)
	}
}

func TestReasoningReplayFilterDropsChunkedReplay(t *testing.T) {
	text := "Consider the factorization of 391 into primes; testing 17 gives 17*23 exactly."
	var blocks [][]byte
	for _, chunk := range splitChunks(text, 9) {
		blocks = append(blocks, deltaBlock("rs_1", chunk))
	}
	for _, chunk := range splitChunks(text, 13) {
		blocks = append(blocks, deltaBlock("rs_1", chunk))
	}
	blocks = append(blocks, reasoningDoneBlock("rs_1", text+text))

	out := filterOutput(t, blocks...)
	if joined := strings.Join(reasoningDeltaTexts(t, out), ""); joined != text {
		t.Fatalf("forwarded deltas = %q", joined)
	}
}

func TestReasoningReplayFilterFlushesFalseSuspicionWithoutLoss(t *testing.T) {
	blocks := [][]byte{
		deltaBlock("rs_1", "The plan requires care. We proceed step by step here."),
		// Legitimately repeats the opening words, then diverges.
		deltaBlock("rs_1", "The plan"),
		deltaBlock("rs_1", " is now different."),
		reasoningDoneBlock("rs_1", "The plan requires care. We proceed step by step here.The plan is now different."),
	}
	out := filterOutput(t, blocks...)
	joined := strings.Join(reasoningDeltaTexts(t, out), "")
	want := "The plan requires care. We proceed step by step here.The plan is now different."
	if joined != want {
		t.Fatalf("forwarded deltas = %q, want %q", joined, want)
	}
}

func TestReasoningReplayFilterPassesCleanStreamBytesUnchanged(t *testing.T) {
	blocks := [][]byte{
		[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"),
		deltaBlock("rs_1", "Short thought over thirty-two characters long."),
		deltaBlock("rs_1", " More fresh text that never replays."),
		reasoningDoneBlock("rs_1", "Short thought over thirty-two characters long. More fresh text that never replays."),
		[]byte("data: [DONE]\n\n"),
	}
	var want strings.Builder
	for _, block := range blocks {
		want.Write(block)
	}
	if out := filterOutput(t, blocks...); out != want.String() {
		t.Fatalf("clean stream altered:\n got %q\nwant %q", out, want.String())
	}
}

func TestReasoningReplayFilterEmitsFreshRemainderAfterReplay(t *testing.T) {
	text := "First half of the reasoning, long enough to qualify for replay detection."
	blocks := [][]byte{
		deltaBlock("rs_1", text),
		deltaBlock("rs_1", text+" And a brand new conclusion."),
		reasoningDoneBlock("rs_1", text+text+" And a brand new conclusion."),
	}
	out := filterOutput(t, blocks...)
	joined := strings.Join(reasoningDeltaTexts(t, out), "")
	want := text + " And a brand new conclusion."
	if joined != want {
		t.Fatalf("forwarded deltas = %q, want %q", joined, want)
	}
}

func TestReasoningReplayFilterHalvesCompletedResponseOutput(t *testing.T) {
	text := "Doubled reasoning recorded in the terminal response payload artifact."
	payload, _ := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":    "resp_1",
			"usage": map[string]any{"output_tokens": 42, "cost": 0.2234493},
			"output": []any{
				map[string]any{
					"type":    "reasoning",
					"id":      "rs_1",
					"content": []any{map[string]any{"type": "reasoning_text", "text": text + text}},
				},
				map[string]any{
					"type":    "message",
					"id":      "msg_1",
					"content": []any{map[string]any{"type": "output_text", "text": "unchanged answer"}},
				},
			},
		},
	})
	block := []byte("event: response.completed\ndata: " + string(payload) + "\n\n")

	out := filterOutput(t, block)
	if strings.Contains(out, jsonEscape(text+text)) {
		t.Fatalf("doubled reasoning survived response.completed:\n%s", out)
	}
	for _, needle := range []string{jsonEscape(text), "unchanged answer", "0.2234493", `"output_tokens":42`} {
		if !strings.Contains(out, needle) {
			t.Fatalf("missing %q in rewritten response.completed:\n%s", needle, out)
		}
	}
}

func splitChunks(text string, size int) []string {
	var chunks []string
	for len(text) > size {
		chunks = append(chunks, text[:size])
		text = text[size:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func jsonEscape(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("marshal %q: %v", s, err))
	}
	return strings.Trim(string(encoded), `"`)
}
