package simplerouter

import (
	"encoding/json"
	"reflect"
	"testing"
)

func scrubToMap(t *testing.T, in string) map[string]any {
	t.Helper()
	out := scrubJSONSchema(json.RawMessage(in))
	if out == nil {
		t.Fatalf("scrubJSONSchema returned nil for %s", in)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("scrubbed schema is not valid JSON: %v", err)
	}
	return m
}

func TestScrubJSONSchemaDropsUnsupportedKeys(t *testing.T) {
	// Shaped like a realistic Claude Code tool schema.
	in := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"command": {"type": "string", "description": "The command to run"},
			"timeout": {"type": "number", "exclusiveMinimum": 0}
		},
		"required": ["command"]
	}`
	m := scrubToMap(t, in)
	for _, key := range []string{"$schema", "additionalProperties"} {
		if _, ok := m[key]; ok {
			t.Errorf("key %q should have been dropped", key)
		}
	}
	props := m["properties"].(map[string]any)
	timeout := props["timeout"].(map[string]any)
	if _, ok := timeout["exclusiveMinimum"]; ok {
		t.Error("nested exclusiveMinimum should have been dropped")
	}
	if props["command"].(map[string]any)["description"] != "The command to run" {
		t.Error("description should be preserved")
	}
	if !reflect.DeepEqual(m["required"], []any{"command"}) {
		t.Errorf("required = %v", m["required"])
	}
}

func TestScrubJSONSchemaAllowListsUnknownKeywords(t *testing.T) {
	// Regression: real Claude Code tool schemas carry draft-2020-12 keywords
	// (propertyNames, dependentRequired, ...) that made Gemini 400 with
	// "Unknown name ... Cannot find field". The scrubber must allow-list.
	in := `{
		"type": "object",
		"properties": {
			"env": {"type": "object", "propertyNames": {"pattern": "^[A-Z_]+$"}, "minProperties": 1},
			"mode": {"type": "string", "dependentRequired": {"a": ["b"]}, "contentMediaType": "text/plain"}
		},
		"unevaluatedProperties": false
	}`
	m := scrubToMap(t, in)
	props := m["properties"].(map[string]any)
	env := props["env"].(map[string]any)
	if _, ok := env["propertyNames"]; ok {
		t.Error("propertyNames must be dropped")
	}
	if env["minProperties"] != float64(1) {
		t.Error("minProperties is a supported keyword and must be kept")
	}
	mode := props["mode"].(map[string]any)
	for _, bad := range []string{"dependentRequired", "contentMediaType"} {
		if _, ok := mode[bad]; ok {
			t.Errorf("%s must be dropped", bad)
		}
	}
	if _, ok := m["unevaluatedProperties"]; ok {
		t.Error("unevaluatedProperties must be dropped")
	}
}

func TestScrubJSONSchemaNullableAndConst(t *testing.T) {
	in := `{
		"type": "object",
		"properties": {
			"name": {"type": ["string", "null"]},
			"mode": {"const": "fast"}
		}
	}`
	m := scrubToMap(t, in)
	props := m["properties"].(map[string]any)
	name := props["name"].(map[string]any)
	if name["type"] != "string" || name["nullable"] != true {
		t.Errorf("name = %v, want string + nullable", name)
	}
	mode := props["mode"].(map[string]any)
	if !reflect.DeepEqual(mode["enum"], []any{"fast"}) {
		t.Errorf("const should become enum, got %v", mode)
	}
}

func TestScrubJSONSchemaFormats(t *testing.T) {
	in := `{
		"type": "object",
		"properties": {
			"when": {"type": "string", "format": "date-time"},
			"url": {"type": "string", "format": "uri"},
			"count": {"type": "integer", "format": "int64"}
		}
	}`
	m := scrubToMap(t, in)
	props := m["properties"].(map[string]any)
	if props["when"].(map[string]any)["format"] != "date-time" {
		t.Error("date-time format should be kept")
	}
	if _, ok := props["url"].(map[string]any)["format"]; ok {
		t.Error("uri format should be dropped")
	}
	if props["count"].(map[string]any)["format"] != "int64" {
		t.Error("int64 format should be kept")
	}
}

func TestScrubJSONSchemaOneOfAndRefs(t *testing.T) {
	in := `{
		"type": "object",
		"$defs": {
			"target": {"type": "object", "properties": {"path": {"type": "string"}}, "additionalProperties": false}
		},
		"properties": {
			"choice": {"oneOf": [{"type": "string"}, {"type": "number"}]},
			"target": {"$ref": "#/$defs/target"},
			"items": {"type": "array", "items": {"$ref": "#/$defs/target"}}
		}
	}`
	m := scrubToMap(t, in)
	if _, ok := m["$defs"]; ok {
		t.Error("$defs should be dropped")
	}
	props := m["properties"].(map[string]any)
	choice := props["choice"].(map[string]any)
	if _, ok := choice["oneOf"]; ok {
		t.Error("oneOf should be renamed to anyOf")
	}
	if len(choice["anyOf"].([]any)) != 2 {
		t.Errorf("anyOf = %v", choice["anyOf"])
	}
	target := props["target"].(map[string]any)
	tprops, ok := target["properties"].(map[string]any)
	if !ok || tprops["path"].(map[string]any)["type"] != "string" {
		t.Errorf("$ref should be inlined, got %v", target)
	}
	if _, ok := target["additionalProperties"]; ok {
		t.Error("inlined definition should also be scrubbed")
	}
	arrItems := props["items"].(map[string]any)["items"].(map[string]any)
	if _, ok := arrItems["properties"]; !ok {
		t.Errorf("array items $ref should be inlined, got %v", arrItems)
	}
}

func TestScrubJSONSchemaAllOfMerge(t *testing.T) {
	in := `{
		"type": "object",
		"allOf": [
			{"properties": {"a": {"type": "string"}}, "required": ["a"]},
			{"properties": {"b": {"type": "number"}}, "required": ["b"]}
		],
		"properties": {"c": {"type": "boolean"}},
		"required": ["c"]
	}`
	m := scrubToMap(t, in)
	if _, ok := m["allOf"]; ok {
		t.Error("allOf should be flattened")
	}
	props := m["properties"].(map[string]any)
	for _, name := range []string{"a", "b", "c"} {
		if _, ok := props[name]; !ok {
			t.Errorf("property %q missing after allOf merge", name)
		}
	}
	req := m["required"].([]any)
	if len(req) != 3 {
		t.Errorf("required = %v, want union of 3", req)
	}
}

func TestScrubJSONSchemaEmptyAndInvalid(t *testing.T) {
	for _, in := range []string{"", "{}", `{"type":"object"}`, `{"type":"object","properties":{}}`, "not json", "true"} {
		if out := scrubJSONSchema(json.RawMessage(in)); out != nil {
			t.Errorf("scrubJSONSchema(%q) = %s, want nil", in, out)
		}
	}
	// Unresolvable $ref at the top level degrades to a bare object schema -> nil.
	if out := scrubJSONSchema(json.RawMessage(`{"$ref":"#/$defs/missing"}`)); out != nil {
		t.Errorf("unresolvable ref = %s, want nil", out)
	}
}

func TestScrubJSONSchemaCyclicRefBounded(t *testing.T) {
	in := `{
		"type": "object",
		"$defs": {"node": {"type": "object", "properties": {"next": {"$ref": "#/$defs/node"}}}},
		"properties": {"root": {"$ref": "#/$defs/node"}}
	}`
	m := scrubToMap(t, in) // must terminate
	if _, ok := m["properties"]; !ok {
		t.Error("cyclic schema should still produce properties")
	}
}
