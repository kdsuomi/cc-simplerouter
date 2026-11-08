package simplerouter

import (
	"encoding/json"
	"fmt"
	"strings"
)

// limitResponsesTools runs after namespace flattening, since providers count
// individual functions. Retain explicitly selected tools, core coding tools,
// and recently called tools before filling remaining slots in original order.
func limitResponsesTools(request map[string]json.RawMessage, limit int) error {
	if limit <= 0 {
		return nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(request["tools"], &tools) != nil || len(tools) <= limit {
		return nil
	}
	type identity struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	identities := make([]identity, len(tools))
	for i, raw := range tools {
		if err := json.Unmarshal(raw, &identities[i]); err != nil {
			return fmt.Errorf("parse tool for provider limit: %w", err)
		}
	}
	selected := make([]bool, len(tools))
	count := 0
	selectAt := func(i int) {
		if !selected[i] && count < limit {
			selected[i] = true
			count++
		}
	}
	var choice struct {
		identity
		Tools []identity `json:"tools"`
	}
	if json.Unmarshal(request["tool_choice"], &choice) == nil {
		required := append(choice.Tools, choice.identity)
		for i, tool := range identities {
			for _, requiredTool := range required {
				if tool == requiredTool {
					if !selected[i] && count == limit {
						return fmt.Errorf("tool_choice requires more than the provider's %d-tool limit", limit)
					}
					selectAt(i)
					break
				}
			}
		}
	}
	for i, tool := range identities {
		if !strings.HasPrefix(tool.Name, "mcp__") {
			selectAt(i)
		}
	}
	var input []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(request["input"], &input) == nil {
		for i := len(input) - 1; i >= 0 && count < limit; i-- {
			if input[i].Type != "function_call" && input[i].Type != "custom_tool_call" {
				continue
			}
			for j, tool := range identities {
				if tool.Name == input[i].Name {
					selectAt(j)
				}
			}
		}
	}
	for i := range tools {
		selectAt(i)
	}
	kept := make([]json.RawMessage, 0, limit)
	for i, tool := range tools {
		if selected[i] {
			kept = append(kept, tool)
		}
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	request["tools"] = encoded
	var instructions string
	_ = json.Unmarshal(request["instructions"], &instructions)
	instructions += fmt.Sprintf("\n\nThe selected provider accepts at most %d tools. %d additional tools were omitted from this request. Use only the tools present in the request; if a needed integration is unavailable, explain that fewer enabled plugins or MCP tools are needed for this model.", limit, len(tools)-len(kept))
	request["instructions"], err = json.Marshal(instructions)
	return err
}
