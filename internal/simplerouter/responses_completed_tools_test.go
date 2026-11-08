package simplerouter

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCompletedToolRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, prior string
		enabled     bool
		want        []string
	}{
		{"missing events", "", true, []string{"response.output_item.added", "response.output_item.done", "response.completed"}},
		{"added only", "response.output_item.added", true, []string{"response.output_item.done", "response.completed"}},
		{"done already", "response.output_item.done", true, []string{"response.completed"}},
		{"disabled", "", false, []string{"response.completed"}},
	} {
		for _, kind := range []string{"function_call", "custom_tool_call"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				recovery := newCompletedToolCallRecovery(tc.enabled)
				item := map[string]any{"type": kind, "id": "tool_1", "call_id": "call_1", "name": "word", "arguments": "{}", "input": "patch"}
				event := func(typ string) []byte {
					payload, _ := json.Marshal(map[string]any{"type": typ, "item": item})
					return []byte("data: " + string(payload) + "\n\n")
				}
				if tc.prior != "" {
					// A changed item ID in the final response must not duplicate
					// an already delivered call with the same call_id.
					recovery.processBlock(event(tc.prior))
					item["id"] = "different_final_id"
				}
				payload, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{
					map[string]any{"type": "reasoning", "id": "reasoning_1"}, item,
				}}})
				block := []byte("data: " + string(payload) + "\n\n")
				blocks := recovery.processBlock(block)
				events := decodeTestSSE(t, string(bytes.Join(blocks, nil)))
				if len(events) != len(tc.want) {
					t.Fatalf("events = %#v", events)
				}
				for i, want := range tc.want {
					if events[i]["type"] != want {
						t.Fatalf("event %d = %#v, want %s", i, events[i], want)
					}
					if want != "response.completed" && events[i]["output_index"] != float64(1) {
						t.Fatal("recovered call must retain its output index")
					}
				}
				if string(blocks[len(blocks)-1]) != string(block) {
					t.Fatal("completion event must remain unchanged")
				}
				if tc.enabled && len(recovery.processBlock(block)) != 1 {
					t.Fatal("repeated completion must not duplicate a delivered tool call")
				}
			})
		}
	}
}

func TestAbliterationRecoversTranslatedCompletedToolCall(t *testing.T) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"tools":[{"type":"custom","name":"apply_patch"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	translation, err := translateResponsesTools(request, abliterationResponsesOptions("abliterated-model"))
	if err != nil {
		t.Fatal(err)
	}
	translator := newResponsesToolStreamTranslator(translation)
	recovery := newCompletedToolCallRecovery(true)
	// Reproduce the base model's forced-tool stream: the call is present only
	// in the final response, while Codex consumes output_item.done events.
	block := []byte(`data: {"type":"response.completed","response":{"output":[{"type":"function_call","name":"apply_patch","call_id":"c1","id":"fc1","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}]}}` + "\n\n")
	var result []byte
	for _, translated := range translator.processBlock(block) {
		result = append(result, bytes.Join(recovery.processBlock(translated), nil)...)
	}
	events := decodeTestSSE(t, string(result))
	if len(events) != 3 || events[1]["type"] != "response.output_item.done" {
		t.Fatalf("missing recovered call: %s", result)
	}
	item := events[1]["item"].(map[string]any)
	if item["type"] != "custom_tool_call" || item["input"] != "*** Begin Patch\n*** End Patch" || item["name"] != "apply_patch" {
		t.Fatalf("restored custom call = %#v", item)
	}
}
