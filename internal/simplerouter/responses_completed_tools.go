package simplerouter

import "encoding/json"

// Some compatible endpoints put a forced tool call only in response.completed.
// Codex executes calls from output_item.done, so recover missing item events
// from that final output while retaining calls already streamed by the server.
type completedToolCallRecovery struct {
	enabled bool
	added   map[string]bool
	done    map[string]bool
}

func newCompletedToolCallRecovery(enabled bool) *completedToolCallRecovery {
	return &completedToolCallRecovery{enabled: enabled, added: map[string]bool{}, done: map[string]bool{}}
}

func (r *completedToolCallRecovery) processBlock(block []byte) [][]byte {
	if !r.enabled {
		return [][]byte{block}
	}
	payload, ok := ssePayload(block)
	if !ok {
		return [][]byte{block}
	}
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return [][]byte{block}
	}
	if event.Type == "response.output_item.added" || event.Type == "response.output_item.done" {
		if key := completedToolCallKey(event.Item); key != "" {
			if event.Type == "response.output_item.added" {
				r.added[key] = true
			} else {
				r.done[key] = true
			}
		}
	}
	if event.Type != "response.completed" {
		return [][]byte{block}
	}
	var blocks [][]byte
	for index, raw := range event.Response.Output {
		key := completedToolCallKey(raw)
		if key == "" || r.done[key] {
			continue
		}
		if !r.added[key] {
			blocks = append(blocks, completedToolItemEvent("response.output_item.added", index, raw))
			r.added[key] = true
		}
		blocks = append(blocks, completedToolItemEvent("response.output_item.done", index, raw))
		r.done[key] = true
	}
	return append(blocks, block)
}

func completedToolCallKey(raw json.RawMessage) string {
	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(raw, &item) != nil || (item.Type != "function_call" && item.Type != "custom_tool_call") {
		return ""
	}
	return item.CallID
}

func completedToolItemEvent(eventType string, index int, item json.RawMessage) []byte {
	payload, _ := json.Marshal(map[string]any{"type": eventType, "output_index": index, "item": item})
	return []byte("event: " + eventType + "\ndata: " + string(payload) + "\n\n")
}
