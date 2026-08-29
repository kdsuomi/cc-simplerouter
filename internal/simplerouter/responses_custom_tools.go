package simplerouter

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

var freeformFunctionParameters = json.RawMessage(`{
  "type":"object",
  "properties":{"input":{"type":"string","description":"Raw freeform tool input."}},
  "required":["input"],
  "additionalProperties":false
}`)

// responsesToolTranslation holds reverse mappings used when replaying the
// upstream Responses stream back to Codex after request-side tool rewrites.
type responsesToolTranslation struct {
	CustomTools map[string]struct{}
	// Registry maps flattened function names (namespace__child) back to Codex
	// identities when FlattenNamespaces is enabled.
	Registry *responseToolRegistry
}

func translateResponsesTools(request map[string]json.RawMessage, options responsesPassthroughOptions) (responsesToolTranslation, error) {
	out := responsesToolTranslation{CustomTools: map[string]struct{}{}}
	if options.TranslateCustomTools {
		custom, err := translateResponsesCustomTools(request)
		if err != nil {
			return out, err
		}
		out.CustomTools = custom
	}
	if options.FlattenNamespaces {
		registry, err := flattenResponsesNamespaceTools(request)
		if err != nil {
			return out, err
		}
		out.Registry = registry
		if err := rewriteNamespacedFunctionCallsInInput(request, registry); err != nil {
			return out, err
		}
	}
	if len(options.AllowedToolTypes) > 0 {
		if err := filterResponsesToolsByType(request, options.AllowedToolTypes); err != nil {
			return out, err
		}
	}
	return out, nil
}

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

// flattenResponsesNamespaceTools rewrites Codex multi-agent namespace tools into
// top-level function tools named namespace__child, matching the Chat adapter.
// Returns the registry used for reverse mapping on the response stream.
func flattenResponsesNamespaceTools(request map[string]json.RawMessage) (*responseToolRegistry, error) {
	var tools []json.RawMessage
	if err := json.Unmarshal(request["tools"], &tools); err != nil {
		return nil, nil
	}
	registry := newResponseToolRegistry()
	out := make([]json.RawMessage, 0, len(tools))
	changed := false
	for _, raw := range tools {
		var tool rawResponseTool
		if err := json.Unmarshal(raw, &tool); err != nil {
			out = append(out, raw)
			continue
		}
		if tool.Type != "namespace" {
			out = append(out, raw)
			continue
		}
		changed = true
		for _, childRaw := range tool.Tools {
			var child rawResponseTool
			if err := json.Unmarshal(childRaw, &child); err != nil {
				return nil, err
			}
			if child.Type != "function" && child.Type != "custom" {
				continue
			}
			chatName := registry.register(responseToolIdentity{
				Name:      child.Name,
				Namespace: tool.Name,
				Custom:    child.Type == "custom",
			})
			description := strings.TrimSpace(child.Description)
			if tool.Description != "" {
				description = strings.TrimSpace(strings.TrimSpace(tool.Description) + "\n\n" + description)
			}
			parameters := child.Parameters
			if child.Type == "custom" {
				parameters = freeformParametersWithFormat(child.Format)
			} else if len(parameters) == 0 || string(parameters) == "null" {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			function := map[string]any{
				"type":       "function",
				"name":       chatName,
				"parameters": json.RawMessage(parameters),
				"strict":     false,
			}
			if description != "" {
				function["description"] = description
			}
			encoded, err := json.Marshal(function)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		}
	}
	if !changed {
		return registry, nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	request["tools"] = encoded
	return registry, nil
}

func rewriteNamespacedFunctionCallsInInput(request map[string]json.RawMessage, registry *responseToolRegistry) error {
	if registry == nil {
		return nil
	}
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
		case "function_call", "custom_tool_call":
			var namespace, name string
			_ = json.Unmarshal(item["namespace"], &namespace)
			_ = json.Unmarshal(item["name"], &name)
			if namespace == "" || name == "" {
				continue
			}
			chatName := registry.chatName(namespace, name)
			encodedName, err := json.Marshal(chatName)
			if err != nil {
				return err
			}
			item["name"] = encodedName
			delete(item, "namespace")
			// Upstream only sees function tools after custom/namespace rewrites.
			if itemType == "custom_tool_call" {
				// custom_tool_call should already have been rewritten; keep defensive.
				item["type"] = json.RawMessage(`"function_call"`)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				return err
			}
			input[index] = encoded
			changed = true
		}
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

func filterResponsesToolsByType(request map[string]json.RawMessage, allowed []string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, t := range allowed {
		allow[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	rawTools, found := request["tools"]
	if !found {
		delete(request, "tool_choice")
		return nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return nil
	}
	out := make([]json.RawMessage, 0, len(tools))
	changed := false
	for _, raw := range tools {
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tool); err != nil {
			out = append(out, raw)
			continue
		}
		var toolType string
		_ = json.Unmarshal(tool["type"], &toolType)
		// OpenAI compatibility alias.
		if toolType == "web_search_preview" {
			if _, ok := allow["web_search"]; ok {
				tool["type"] = json.RawMessage(`"web_search"`)
				if sanitizeWebSearchToolForStrictProviders(tool) {
					// already mutated
				}
				encoded, err := json.Marshal(tool)
				if err != nil {
					return err
				}
				out = append(out, encoded)
				changed = true
				continue
			}
		}
		if _, ok := allow[strings.ToLower(toolType)]; !ok {
			changed = true
			continue
		}
		// Strip Codex-only web_search fields that providers such as xAI reject
		// (e.g. external_web_access, search_content_types).
		if toolType == "web_search" {
			if sanitizeWebSearchToolForStrictProviders(tool) {
				encoded, err := json.Marshal(tool)
				if err != nil {
					return err
				}
				out = append(out, encoded)
				changed = true
				continue
			}
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		delete(request, "tool_choice")
	}
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	request["tools"] = encoded
	return nil
}

// sanitizeWebSearchToolForStrictProviders removes Codex-only web_search fields
// that OpenAI accepts but providers like xAI reject with 400.
// Keeps type and any remaining provider-neutral options.
func sanitizeWebSearchToolForStrictProviders(tool map[string]json.RawMessage) bool {
	changed := false
	for _, key := range []string{
		"external_web_access",
		"search_content_types",
		// Codex sometimes attaches filters/user_location that mirror OpenAI's
		// web_search_preview shape; drop them when filtering for strict APIs.
		"filters",
		"user_location",
	} {
		if _, found := tool[key]; found {
			delete(tool, key)
			changed = true
		}
	}
	return changed
}

func normalizeMetaResponsesTool(tool map[string]json.RawMessage) bool {
	var toolType string
	if json.Unmarshal(tool["type"], &toolType) != nil {
		return false
	}
	switch toolType {
	case "function":
		changed := false
		var strict bool
		if json.Unmarshal(tool["strict"], &strict) == nil && strict {
			tool["strict"] = json.RawMessage(`false`)
			changed = true
		}
		return removeRecursiveMetaFunctionSchemaRefs(tool) || changed
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

// removeRecursiveMetaFunctionSchemaRefs cuts only local $ref edges that close
// a cycle. Meta accepts referenced function schemas, but rejects recursive
// schemas such as the Gmail app's nested MIME-part declaration. Keeping the
// first reference and replacing the back-edge with an unconstrained schema
// preserves useful argument guidance without dropping the tool.
func removeRecursiveMetaFunctionSchemaRefs(tool map[string]json.RawMessage) bool {
	var schema any
	if err := json.Unmarshal(tool["parameters"], &schema); err != nil || schema == nil {
		return false
	}
	if !cutRecursiveLocalSchemaRefs(schema) {
		return false
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return false
	}
	tool["parameters"] = encoded
	return true
}

func cutRecursiveLocalSchemaRefs(root any) bool {
	visited := map[string]bool{}
	var walk func(any, map[string]bool) bool
	walk = func(node any, active map[string]bool) bool {
		changed := false
		switch value := node.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok {
				if active[ref] {
					// An empty schema accepts the recursive value while preventing
					// the provider from following the cycle again.
					delete(value, "$ref")
					changed = true
				} else if !visited[ref] {
					if target, found := resolveLocalSchemaRef(root, ref); found {
						active[ref] = true
						changed = walk(target, active) || changed
						delete(active, ref)
						visited[ref] = true
					}
				}
			}
			for key, child := range value {
				if key == "$ref" {
					continue
				}
				changed = walk(child, active) || changed
			}
		case []any:
			for _, child := range value {
				changed = walk(child, active) || changed
			}
		}
		return changed
	}
	return walk(root, map[string]bool{})
}

func resolveLocalSchemaRef(root any, ref string) (any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	path, err := url.PathUnescape(strings.TrimPrefix(ref, "#"))
	if err != nil {
		return nil, false
	}
	current := root
	for _, encoded := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		switch value := current.(type) {
		case map[string]any:
			var found bool
			current, found = value[part]
			if !found {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(value) {
				return nil, false
			}
			current = value[index]
		default:
			return nil, false
		}
	}
	return current, true
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

type responsesToolStreamTranslator struct {
	customTools    map[string]struct{}
	registry       *responseToolRegistry
	itemIDs        map[string]struct{}
	callIDs        map[string]struct{}
	metricStreamed map[string]struct{}
}

func newResponsesToolStreamTranslator(translation responsesToolTranslation) *responsesToolStreamTranslator {
	return &responsesToolStreamTranslator{
		customTools:    translation.CustomTools,
		registry:       translation.Registry,
		itemIDs:        map[string]struct{}{},
		callIDs:        map[string]struct{}{},
		metricStreamed: map[string]struct{}{},
	}
}

func (t *responsesToolStreamTranslator) needsTranslation() bool {
	return len(t.customTools) > 0 || (t.registry != nil && len(t.registry.byChatName) > 0)
}

func (t *responsesToolStreamTranslator) processBlock(block []byte) [][]byte {
	if !t.needsTranslation() {
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
		item, changed, itemID, callID := t.translateFunctionCallItem(fields["item"])
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
			// Suppressed custom-tool deltas still represent live generation:
			// forward them under a synthetic id so the client counts their
			// characters for the rate display without any consumer matching
			// the real call. The actual input arrives via output_item.done.
			return t.metricOnlyArgumentDelta(block, fields, /*markStreamed*/ true)
		}
		blocks := [][]byte{block}
		if name, ok := t.restoreEventName(fields); ok {
			fields["name"] = name
			blocks = [][]byte{rebuildDataBlock(block, fields)}
		}
		// Plain function-call argument deltas are ignored by the client's SSE
		// parser; append a metric-only copy so their generation is visible to
		// the rate display too.
		return append(blocks, t.metricOnlyArgumentDelta(block, fields, /*markStreamed*/ false)...)
	case "response.function_call_arguments.done":
		if t.isCustomEvent(fields) {
			if _, streamed := t.metricStreamed[t.argumentEventKey(fields)]; streamed {
				// The argument characters were already counted through the
				// live metric deltas; a terminal blob here would count them
				// twice.
				return nil
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
		}
		if name, ok := t.restoreEventName(fields); ok {
			fields["name"] = name
			return [][]byte{rebuildDataBlock(block, fields)}
		}
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		response, changed := t.translateResponseObject(fields["response"])
		if changed {
			fields["response"] = response
			return [][]byte{rebuildDataBlock(block, fields)}
		}
	}
	return [][]byte{block}
}

func (t *responsesToolStreamTranslator) isCustomEvent(fields map[string]json.RawMessage) bool {
	if len(t.customTools) == 0 {
		return false
	}
	var itemID, callID string
	_ = json.Unmarshal(fields["item_id"], &itemID)
	_ = json.Unmarshal(fields["call_id"], &callID)
	_, itemFound := t.itemIDs[itemID]
	_, callFound := t.callIDs[callID]
	return itemFound || callFound
}

// argumentEventKey identifies the tool call an argument event belongs to,
// preferring an id the translator already tracks as custom.
func (t *responsesToolStreamTranslator) argumentEventKey(fields map[string]json.RawMessage) string {
	var itemID, callID string
	_ = json.Unmarshal(fields["item_id"], &itemID)
	_ = json.Unmarshal(fields["call_id"], &callID)
	if _, ok := t.itemIDs[itemID]; ok {
		return itemID
	}
	if _, ok := t.callIDs[callID]; ok {
		return callID
	}
	if itemID != "" {
		return itemID
	}
	return callID
}

// metricOnlyArgumentDelta rewrites a function-call argument delta into a
// custom_tool_call_input.delta under a synthetic id. The client counts the
// delta's characters for the live generation rate, but the synthetic id
// matches no real item or call, so no content consumer ever sees it.
func (t *responsesToolStreamTranslator) metricOnlyArgumentDelta(
	block []byte,
	fields map[string]json.RawMessage,
	markStreamed bool,
) [][]byte {
	var delta string
	if json.Unmarshal(fields["delta"], &delta) != nil || delta == "" {
		return nil
	}
	key := t.argumentEventKey(fields)
	if key == "" {
		return nil
	}
	if markStreamed {
		t.metricStreamed[key] = struct{}{}
	}
	syntheticID, err := json.Marshal("metrics_" + key)
	if err != nil {
		return nil
	}
	fields["type"] = json.RawMessage(`"response.custom_tool_call_input.delta"`)
	fields["item_id"] = syntheticID
	fields["call_id"] = syntheticID
	delete(fields, "name")
	return [][]byte{rebuildSSEEventBlock(block, "response.custom_tool_call_input.delta", fields)}
}

func (t *responsesToolStreamTranslator) restoreEventName(fields map[string]json.RawMessage) (json.RawMessage, bool) {
	if t.registry == nil {
		return nil, false
	}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil || name == "" {
		return nil, false
	}
	identity, ok := t.registry.byChatName[name]
	if !ok || identity.Namespace == "" {
		return nil, false
	}
	encoded, err := json.Marshal(identity.Name)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func (t *responsesToolStreamTranslator) translateFunctionCallItem(raw json.RawMessage) (json.RawMessage, bool, string, string) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw, false, "", ""
	}
	var itemType, name string
	if json.Unmarshal(item["type"], &itemType) != nil || itemType != "function_call" ||
		json.Unmarshal(item["name"], &name) != nil {
		return raw, false, "", ""
	}
	var itemID, callID string
	_ = json.Unmarshal(item["id"], &itemID)
	_ = json.Unmarshal(item["call_id"], &callID)

	// Restore Codex multi-agent namespace before custom-tool rewrite so custom
	// tools that lived inside a namespace keep their identity.
	changed := false
	if t.registry != nil {
		if identity, ok := t.registry.byChatName[name]; ok && identity.Namespace != "" {
			encodedName, err := json.Marshal(identity.Name)
			if err != nil {
				return raw, false, "", ""
			}
			item["name"] = encodedName
			item["namespace"], _ = json.Marshal(identity.Namespace)
			name = identity.Name
			changed = true
			if identity.Custom {
				var arguments string
				_ = json.Unmarshal(item["arguments"], &arguments)
				item["type"] = json.RawMessage(`"custom_tool_call"`)
				item["input"], _ = json.Marshal(customToolInput(arguments))
				delete(item, "arguments")
				encoded, err := json.Marshal(item)
				if err != nil {
					return raw, false, "", ""
				}
				return encoded, true, itemID, callID
			}
		}
	}

	if _, found := t.customTools[name]; found {
		var arguments string
		_ = json.Unmarshal(item["arguments"], &arguments)
		item["type"] = json.RawMessage(`"custom_tool_call"`)
		item["input"], _ = json.Marshal(customToolInput(arguments))
		delete(item, "arguments")
		changed = true
		encoded, err := json.Marshal(item)
		if err != nil {
			return raw, false, "", ""
		}
		return encoded, true, itemID, callID
	}
	if !changed {
		return raw, false, "", ""
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return raw, false, "", ""
	}
	return encoded, true, itemID, callID
}

func (t *responsesToolStreamTranslator) translateResponseObject(raw json.RawMessage) (json.RawMessage, bool) {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return raw, false
	}
	changed := false
	var output []json.RawMessage
	if json.Unmarshal(response["output"], &output) == nil {
		for index, item := range output {
			translated, itemChanged, _, _ := t.translateFunctionCallItem(item)
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
			// Prefer registry reverse for flattened namespace tools.
			if t.registry != nil {
				if identity, ok := t.registry.byChatName[name]; ok && identity.Namespace != "" {
					// Rebuild as namespace is complex; leave function form with original child name
					// and drop the flattened name for Codex's tool list display.
					tool["name"], _ = json.Marshal(identity.Name)
					if identity.Custom {
						tool["type"] = json.RawMessage(`"custom"`)
						delete(tool, "parameters")
					}
					tools[index], _ = json.Marshal(tool)
					toolsChanged = true
					continue
				}
			}
			if _, found := t.customTools[name]; !found {
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
