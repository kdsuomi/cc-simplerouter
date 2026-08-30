package simplerouter

import (
	"strings"
	"testing"
)

func TestPlainRouteForwardsFunctionArgumentMetrics(t *testing.T) {
	// No custom tools and no registry: needsTranslation() is false, as on a
	// plain (non-Meta) OpenRouter Responses route.
	translator := newResponsesToolStreamTranslator(responsesToolTranslation{})

	textBlock := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"hello"}` + "\n\n")
	passed := translator.processBlock(textBlock)
	if len(passed) != 1 || string(passed[0]) != string(textBlock) {
		t.Fatalf("text delta was not passed through untouched: %q", passed)
	}

	argBlock := []byte("event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"command\":\"pwd\"}"}` + "\n\n")
	blocks := translator.processBlock(argBlock)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want original plus metric copy:\n%q", len(blocks), blocks)
	}
	if string(blocks[0]) != string(argBlock) {
		t.Fatalf("original argument delta was altered:\n%s", blocks[0])
	}
	metric := string(blocks[1])
	for _, want := range []string{
		"response.custom_tool_call_input.delta",
		`"call_id":"metrics_fc_1"`,
		`"item_id":"metrics_fc_1"`,
		`{\"command\":\"pwd\"}`,
	} {
		if !strings.Contains(metric, want) {
			t.Fatalf("metric copy missing %q:\n%s", want, metric)
		}
	}
}

func TestPlainRouteDoesNotDuplicateNativeCustomToolDeltas(t *testing.T) {
	translator := newResponsesToolStreamTranslator(responsesToolTranslation{})

	// A native custom_tool_call_input.delta is already counted by the Codex
	// parser; duplicating it would double-count.
	block := []byte("event: response.custom_tool_call_input.delta\n" +
		`data: {"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":"*** Begin Patch"}` + "\n\n")
	blocks := translator.processBlock(block)
	if len(blocks) != 1 || string(blocks[0]) != string(block) {
		t.Fatalf("native custom tool delta was not passed through untouched: %q", blocks)
	}
}
