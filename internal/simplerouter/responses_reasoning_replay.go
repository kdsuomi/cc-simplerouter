package simplerouter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const responsesReasoningReplayPrefix = "simplerouter-responses-reasoning-v1:"

type responsesReasoningReplayState struct {
	Signature        string          `json:"signature"`
	Format           json.RawMessage `json:"format,omitempty"`
	EncryptedContent json.RawMessage `json:"encrypted_content,omitempty"`
}

func preserveResponsesReasoningSignatures(block []byte) []byte {
	payload, ok := ssePayload(block)
	if !ok {
		return block
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return block
	}
	var eventType string
	if json.Unmarshal(fields["type"], &eventType) != nil {
		return block
	}
	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		item, changed := preserveResponsesReasoningItem(fields["item"])
		if !changed {
			return block
		}
		fields["item"] = item
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		var response map[string]json.RawMessage
		if json.Unmarshal(fields["response"], &response) != nil {
			return block
		}
		var output []json.RawMessage
		if json.Unmarshal(response["output"], &output) != nil {
			return block
		}
		changedAny := false
		for index, raw := range output {
			if item, changed := preserveResponsesReasoningItem(raw); changed {
				output[index] = item
				changedAny = true
			}
		}
		if !changedAny {
			return block
		}
		response["output"], _ = json.Marshal(output)
		fields["response"], _ = json.Marshal(response)
	default:
		return block
	}
	return rebuildDataBlock(block, fields)
}

func preserveResponsesReasoningItem(raw json.RawMessage) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw, false
	}
	var itemType, signature string
	if json.Unmarshal(item["type"], &itemType) != nil || itemType != "reasoning" ||
		json.Unmarshal(item["signature"], &signature) != nil || signature == "" {
		return raw, false
	}
	state := responsesReasoningReplayState{
		Signature:        signature,
		Format:           item["format"],
		EncryptedContent: item["encrypted_content"],
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return raw, false
	}
	item["encrypted_content"], _ = json.Marshal(responsesReasoningReplayPrefix + base64.RawStdEncoding.EncodeToString(encoded))
	delete(item, "signature")
	delete(item, "format")
	return rebuildJSONObject(item), true
}

func restoreResponsesReasoningSignatures(request map[string]json.RawMessage) error {
	var input []json.RawMessage
	if json.Unmarshal(request["input"], &input) != nil {
		return nil
	}
	changed := false
	for index, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		var itemType, encrypted string
		if json.Unmarshal(item["type"], &itemType) != nil || itemType != "reasoning" ||
			json.Unmarshal(item["encrypted_content"], &encrypted) != nil ||
			!strings.HasPrefix(encrypted, responsesReasoningReplayPrefix) {
			continue
		}
		decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encrypted, responsesReasoningReplayPrefix))
		var state responsesReasoningReplayState
		if err != nil || json.Unmarshal(decoded, &state) != nil || state.Signature == "" {
			return fmt.Errorf("invalid reasoning replay state at input[%d]", index)
		}
		item["signature"], _ = json.Marshal(state.Signature)
		if len(state.Format) > 0 {
			item["format"] = state.Format
		}
		delete(item, "encrypted_content")
		if len(state.EncryptedContent) > 0 {
			item["encrypted_content"] = state.EncryptedContent
		}
		input[index] = rebuildJSONObject(item)
		changed = true
	}
	if changed {
		request["input"], _ = json.Marshal(input)
	}
	return nil
}
