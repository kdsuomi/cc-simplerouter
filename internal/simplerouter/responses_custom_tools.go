package simplerouter

import (
	"bytes"
	"encoding/json"
	"strings"
)

var freeformFunctionParameters = json.RawMessage(`{
  "type":"object",
  "properties":{"input":{"type":"string","description":"Raw freeform tool input."}},
  "required":["input"],
  "additionalProperties":false
}`)

func translateResponsesCustomTools(request map[string]json.RawMessage) (map[string]struct{}, error) {
	customTools := map[string]struct{}{}
	var tools []json.RawMessage
	if err := json.Unmarshal(request["tools"], &tools); err != nil {
		return customTools, nil
	}
	changed := false
	for index, raw := range tools {
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tool); err != nil {
			continue
		}
		toolChanged := normalizeMetaResponsesTool(tool)
		var toolType, name string
		_ = json.Unmarshal(tool["type"], &toolType)
		if toolType == "custom" && json.Unmarshal(tool["name"], &name) == nil && name != "" {
			customTools[name] = struct{}{}
			tool["type"] = json.RawMessage(`"function"`)
			tool["parameters"] = freeformParametersWithFormat(tool["format"])
			delete(tool, "format")
			toolChanged = true
		}
		if !toolChanged {
			continue
		}
		encoded, err := json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		tools[index] = encoded
		changed = true
	}
	if changed {
		encoded, err := json.Marshal(tools)
		if err != nil {
			return nil, err
		}
		request["tools"] = encoded
	}
	if err := translateResponsesCustomToolInput(request); err != nil {
		return nil, err
	}
	return customTools, nil
}

func normalizeMetaResponsesTool(tool map[string]json.RawMessage) bool {
	var toolType string
	if json.Unmarshal(tool["type"], &toolType) != nil {
		return false
	}
	switch toolType {
	case "function":
		var strict bool
		if json.Unmarshal(tool["strict"], &strict) == nil && strict {
			tool["strict"] = json.RawMessage(`false`)
			return true
		}
	case "namespace":
		var children []json.RawMessage
		if json.Unmarshal(tool["tools"], &children) != nil {
			return false
		}
		changed := false
		for index, raw := range children {
			var child map[string]json.RawMessage
			if json.Unmarshal(raw, &child) != nil || !normalizeMetaResponsesTool(child) {
				continue
			}
			encoded, err := json.Marshal(child)
			if err != nil {
				continue
			}
			children[index] = encoded
			changed = true
		}
		if changed {
			tool["tools"], _ = json.Marshal(children)
		}
		return changed
	case "tool_search":
		return omitOptionalToolSearchParameters(tool)
	case "web_search", "web_search_preview":
		if _, found := tool["search_content_types"]; found {
			delete(tool, "search_content_types")
			return true
		}
	}
	return false
}

func omitOptionalToolSearchParameters(tool map[string]json.RawMessage) bool {
	var parameters map[string]json.RawMessage
	if json.Unmarshal(tool["parameters"], &parameters) != nil {
		return false
	}
	var properties map[string]json.RawMessage
	if json.Unmarshal(parameters["properties"], &properties) != nil {
		return false
	}
	var required []string
	if json.Unmarshal(parameters["required"], &required) != nil {
		return false
	}
	requiredSet := make(map[string]struct{}, len(required))
	for _, name := range required {
		requiredSet[name] = struct{}{}
	}
	changed := false
	for name := range properties {
		if _, found := requiredSet[name]; found {
			continue
		}
		delete(properties, name)
		changed = true
	}
	if !changed {
		return false
	}
	parameters["properties"], _ = json.Marshal(properties)
	tool["parameters"], _ = json.Marshal(parameters)
	return true
}

func freeformParametersWithFormat(format json.RawMessage) json.RawMessage {
	if len(format) == 0 || bytes.Equal(bytes.TrimSpace(format), []byte("null")) {
		return append(json.RawMessage(nil), freeformFunctionParameters...)
	}
	description := "Raw freeform tool input.\n\nThe input must follow this format exactly:\n" + string(format)
	var parsed struct {
		Definition string `json:"definition"`
	}
	if json.Unmarshal(format, &parsed) == nil && strings.TrimSpace(parsed.Definition) != "" {
		description = "Raw freeform tool input.\n\nThe input must follow this grammar exactly:\n" + parsed.Definition
	}
	encoded, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string", "description": description},
		},
		"required":             []string{"input"},
		"additionalProperties": false,
	})
	if err != nil {
		return append(json.RawMessage(nil), freeformFunctionParameters...)
	}
	return encoded
}

func translateResponsesCustomToolInput(request map[string]json.RawMessage) error {
	var input []json.RawMessage
	if err := json.Unmarshal(request["input"], &input); err != nil {
		return nil
	}
	changed := false
	for index, raw := range input {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		var itemType string
		if json.Unmarshal(item["type"], &itemType) != nil {
			continue
		}
		switch itemType {
		case "custom_tool_call":
			var freeformInput string
			if json.Unmarshal(item["input"], &freeformInput) != nil {
				continue
			}
			arguments, err := json.Marshal(map[string]string{"input": freeformInput})
			if err != nil {
				return err
			}
			item["type"] = json.RawMessage(`"function_call"`)
			item["arguments"], err = json.Marshal(string(arguments))
			if err != nil {
				return err
			}
			delete(item, "input")
			delete(item, "namespace")
			delete(item, "internal_chat_message_metadata_passthrough")
			changed = true
		case "custom_tool_call_output":
			item["type"] = json.RawMessage(`"function_call_output"`)
			delete(item, "name")
			delete(item, "internal_chat_message_metadata_passthrough")
			changed = true
		default:
			continue
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		input[index] = encoded
	}
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request["input"] = encoded
	return nil
}

type responsesCustomToolStreamTranslator struct {
	customTools map[string]struct{}
	itemIDs     map[string]struct{}
	callIDs     map[string]struct{}
}

func newResponsesCustomToolStreamTranslator(customTools map[string]struct{}) *responsesCustomToolStreamTranslator {
	return &responsesCustomToolStreamTranslator{
		customTools: customTools,
		itemIDs:     map[string]struct{}{},
		callIDs:     map[string]struct{}{},
	}
}

func (t *responsesCustomToolStreamTranslator) processBlock(block []byte) [][]byte {
	if len(t.customTools) == 0 {
		return [][]byte{block}
	}
	payload, ok := ssePayload(block)
	if !ok {
		return [][]byte{block}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return [][]byte{block}
	}
	var eventType string
	if json.Unmarshal(fields["type"], &eventType) != nil {
		return [][]byte{block}
	}
	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		item, changed, itemID, callID := translateCustomFunctionCallItem(fields["item"], t.customTools)
		if !changed {
			return [][]byte{block}
		}
		if itemID != "" {
			t.itemIDs[itemID] = struct{}{}
		}
		if callID != "" {
			t.callIDs[callID] = struct{}{}
		}
		fields["item"] = item
		return [][]byte{rebuildDataBlock(block, fields)}
	case "response.function_call_arguments.delta":
		if t.isCustomEvent(fields) {
			return nil
		}
	case "response.function_call_arguments.done":
		if !t.isCustomEvent(fields) {
			return [][]byte{block}
		}
		var arguments string
		if json.Unmarshal(fields["arguments"], &arguments) != nil {
			return nil
		}
		fields["type"] = json.RawMessage(`"response.custom_tool_call_input.delta"`)
		fields["delta"], _ = json.Marshal(customToolInput(arguments))
		delete(fields, "arguments")
		delete(fields, "name")
		return [][]byte{rebuildSSEEventBlock(block, "response.custom_tool_call_input.delta", fields)}
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		response, changed := translateCustomToolsInResponse(fields["response"], t.customTools)
		if changed {
			fields["response"] = response
			return [][]byte{rebuildDataBlock(block, fields)}
		}
	}
	return [][]byte{block}
}

func (t *responsesCustomToolStreamTranslator) isCustomEvent(fields map[string]json.RawMessage) bool {
	var itemID, callID string
	_ = json.Unmarshal(fields["item_id"], &itemID)
	_ = json.Unmarshal(fields["call_id"], &callID)
	_, itemFound := t.itemIDs[itemID]
	_, callFound := t.callIDs[callID]
	return itemFound || callFound
}

func translateCustomFunctionCallItem(raw json.RawMessage, customTools map[string]struct{}) (json.RawMessage, bool, string, string) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw, false, "", ""
	}
	var itemType, name string
	if json.Unmarshal(item["type"], &itemType) != nil || itemType != "function_call" ||
		json.Unmarshal(item["name"], &name) != nil {
		return raw, false, "", ""
	}
	if _, found := customTools[name]; !found {
		return raw, false, "", ""
	}
	var arguments, itemID, callID string
	_ = json.Unmarshal(item["arguments"], &arguments)
	_ = json.Unmarshal(item["id"], &itemID)
	_ = json.Unmarshal(item["call_id"], &callID)
	item["type"] = json.RawMessage(`"custom_tool_call"`)
	item["input"], _ = json.Marshal(customToolInput(arguments))
	delete(item, "arguments")
	encoded, err := json.Marshal(item)
	if err != nil {
		return raw, false, "", ""
	}
	return encoded, true, itemID, callID
}

func translateCustomToolsInResponse(raw json.RawMessage, customTools map[string]struct{}) (json.RawMessage, bool) {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return raw, false
	}
	changed := false
	var output []json.RawMessage
	if json.Unmarshal(response["output"], &output) == nil {
		for index, item := range output {
			translated, itemChanged, _, _ := translateCustomFunctionCallItem(item, customTools)
			if itemChanged {
				output[index] = translated
				changed = true
			}
		}
		if changed {
			response["output"], _ = json.Marshal(output)
		}
	}
	var tools []json.RawMessage
	if json.Unmarshal(response["tools"], &tools) == nil {
		toolsChanged := false
		for index, rawTool := range tools {
			var tool map[string]json.RawMessage
			if json.Unmarshal(rawTool, &tool) != nil {
				continue
			}
			var toolType, name string
			_ = json.Unmarshal(tool["type"], &toolType)
			_ = json.Unmarshal(tool["name"], &name)
			if toolType != "function" {
				continue
			}
			if _, found := customTools[name]; !found {
				continue
			}
			tool["type"] = json.RawMessage(`"custom"`)
			delete(tool, "parameters")
			tools[index], _ = json.Marshal(tool)
			toolsChanged = true
		}
		if toolsChanged {
			response["tools"], _ = json.Marshal(tools)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func rebuildSSEEventBlock(block []byte, eventName string, fields map[string]json.RawMessage) []byte {
	rebuilt := rebuildDataBlock(block, fields)
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(rebuilt, []byte("\n")) {
		trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			out.WriteString("event: ")
			out.WriteString(eventName)
			out.WriteByte('\n')
			continue
		}
		out.Write(line)
	}
	return out.Bytes()
}
