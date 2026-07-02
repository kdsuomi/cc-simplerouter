package simplerouter

import "encoding/json"

// scrubJSONSchema converts an Anthropic tool input_schema (JSON Schema) into
// the OpenAPI subset Gemini accepts for functionDeclarations.parameters.
// Gemini rejects requests containing unknown schema keywords, so this strips
// or rewrites everything outside the accepted subset. Returns nil when the
// schema is empty or unusable — callers should omit parameters entirely then
// (Gemini rejects OBJECT schemas with empty properties).
func scrubJSONSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	defs := collectSchemaDefs(root)
	scrubbed, ok := scrubSchemaNode(root, defs, 0).(map[string]any)
	if !ok || emptyObjectSchema(scrubbed) {
		return nil
	}
	out, err := json.Marshal(scrubbed)
	if err != nil {
		return nil
	}
	return out
}

// Keywords Gemini's Schema proto accepts (v1beta). Anything else causes a
// hard 400 "Unknown name ... Cannot find field", so scrubbing works as an
// allow-list — Claude Code's real tool schemas carry draft-2020-12 keywords
// (propertyNames, additionalProperties, ...) Gemini has never heard of.
var geminiAllowedSchemaKeys = map[string]bool{
	"type":             true,
	"format":           true,
	"title":            true,
	"description":      true,
	"nullable":         true,
	"default":          true,
	"items":            true,
	"minItems":         true,
	"maxItems":         true,
	"enum":             true,
	"properties":       true,
	"required":         true,
	"minProperties":    true,
	"maxProperties":    true,
	"minimum":          true,
	"maximum":          true,
	"minLength":        true,
	"maxLength":        true,
	"pattern":          true,
	"example":          true,
	"anyOf":            true,
	"propertyOrdering": true,
}

// Formats Gemini accepts, by schema type. Everything else (uri, uuid, ...) is dropped.
var geminiAllowedFormats = map[string]map[string]bool{
	"string":  {"enum": true, "date-time": true},
	"integer": {"int32": true, "int64": true},
	"number":  {"float": true, "double": true},
}

const maxSchemaRefDepth = 8

// collectSchemaDefs gathers $defs/definitions from the root schema so $ref
// nodes can be inlined (Gemini has no reference support).
func collectSchemaDefs(root map[string]any) map[string]map[string]any {
	defs := map[string]map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		section, _ := root[key].(map[string]any)
		for name, node := range section {
			if m, ok := node.(map[string]any); ok {
				defs["#/"+key+"/"+name] = m
			}
		}
	}
	return defs
}

// scrubSchemaNode recursively rewrites one schema node. Non-map nodes (e.g.
// boolean schemas) have no Gemini equivalent and collapse to nil.
func scrubSchemaNode(node any, defs map[string]map[string]any, depth int) any {
	if depth > maxSchemaRefDepth {
		return map[string]any{"type": "object"}
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}

	// Inline $ref by scrubbing the referenced definition in its place.
	if ref, ok := m["$ref"].(string); ok {
		if target, ok := defs[ref]; ok {
			return scrubSchemaNode(target, defs, depth+1)
		}
		return map[string]any{"type": "object"}
	}

	// Rewrites that map draft-2020-12 constructs onto Gemini equivalents run
	// on a copy first, then the allow-list filter drops everything else.
	out := map[string]any{}
	for key, value := range m {
		out[key] = value
	}

	// oneOf has no Gemini equivalent; anyOf does.
	if oneOf, ok := out["oneOf"]; ok {
		delete(out, "oneOf")
		if _, exists := out["anyOf"]; !exists {
			out["anyOf"] = oneOf
		}
	}
	// const -> single-value enum.
	if c, ok := out["const"]; ok {
		delete(out, "const")
		if _, exists := out["enum"]; !exists {
			out["enum"] = []any{c}
		}
	}
	// type: ["string","null"] -> first non-null type + nullable.
	if types, ok := out["type"].([]any); ok {
		var picked string
		nullable := false
		for _, t := range types {
			s, _ := t.(string)
			if s == "null" {
				nullable = true
			} else if picked == "" {
				picked = s
			}
		}
		if picked == "" {
			picked = "object"
		}
		out["type"] = picked
		if nullable {
			out["nullable"] = true
		}
	}
	// Drop formats Gemini rejects for the node's type.
	if format, ok := out["format"].(string); ok {
		typ, _ := out["type"].(string)
		if !geminiAllowedFormats[typ][format] {
			delete(out, "format")
		}
	}

	// Flatten allOf: merge element keys into this node (existing keys win).
	if allOf, ok := out["allOf"].([]any); ok {
		delete(out, "allOf")
		for _, elem := range allOf {
			merged, ok := scrubSchemaNode(elem, defs, depth+1).(map[string]any)
			if !ok {
				continue
			}
			for key, value := range merged {
				switch key {
				case "properties":
					props, _ := out["properties"].(map[string]any)
					if props == nil {
						props = map[string]any{}
					}
					for name, p := range value.(map[string]any) {
						if _, exists := props[name]; !exists {
							props[name] = p
						}
					}
					out["properties"] = props
				case "required":
					out["required"] = unionStringSlices(out["required"], value)
				default:
					if _, exists := out[key]; !exists {
						out[key] = value
					}
				}
			}
		}
	}

	// Allow-list filter: drop every keyword Gemini's Schema proto lacks.
	for key := range out {
		if !geminiAllowedSchemaKeys[key] {
			delete(out, key)
		}
	}

	// Recurse.
	if props, ok := out["properties"].(map[string]any); ok {
		scrubbed := map[string]any{}
		for name, p := range props {
			if s := scrubSchemaNode(p, defs, depth+1); s != nil {
				scrubbed[name] = s
			} else {
				scrubbed[name] = map[string]any{"type": "object"}
			}
		}
		out["properties"] = scrubbed
	}
	if items, ok := out["items"]; ok {
		if s := scrubSchemaNode(items, defs, depth+1); s != nil {
			out["items"] = s
		} else {
			delete(out, "items")
		}
	}
	if anyOf, ok := out["anyOf"].([]any); ok {
		scrubbed := make([]any, 0, len(anyOf))
		for _, elem := range anyOf {
			if s := scrubSchemaNode(elem, defs, depth+1); s != nil {
				scrubbed = append(scrubbed, s)
			}
		}
		if len(scrubbed) > 0 {
			out["anyOf"] = scrubbed
		} else {
			delete(out, "anyOf")
		}
	}

	return out
}

func unionStringSlices(a, b any) []any {
	seen := map[string]bool{}
	var out []any
	for _, list := range []any{a, b} {
		items, _ := list.([]any)
		for _, item := range items {
			s, ok := item.(string)
			if ok && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// emptyObjectSchema reports whether a scrubbed schema carries no constraints
// worth sending (Gemini rejects OBJECT schemas with an empty properties map).
func emptyObjectSchema(m map[string]any) bool {
	if len(m) == 0 {
		return true
	}
	typ, _ := m["type"].(string)
	if typ != "" && typ != "object" {
		return false
	}
	props, hasProps := m["properties"].(map[string]any)
	if hasProps && len(props) > 0 {
		return false
	}
	if _, hasAnyOf := m["anyOf"]; hasAnyOf {
		return false
	}
	// Only type/nullable/description-level keys remain: nothing to constrain.
	for key := range m {
		switch key {
		case "type", "properties", "description", "title", "nullable", "required":
		default:
			return false
		}
	}
	return true
}
