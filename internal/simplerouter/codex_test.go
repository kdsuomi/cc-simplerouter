package simplerouter

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestBuildCodexModelCatalogPreservesFullToolDescriptor(t *testing.T) {
	source := []byte(`{
	  "models": [
	    {
	      "slug": "gpt-template",
	      "display_name": "Template",
	      "base_instructions": "You are Codex.",
	      "apply_patch_tool_type": "freeform",
	      "supports_parallel_tool_calls": true,
	      "context_window": 272000,
	      "max_context_window": 272000,
	      "default_reasoning_level": "medium",
	      "supported_reasoning_levels": [{"effort":"low","description":"Low"}],
	      "default_reasoning_summary": "auto",
	      "input_modalities": ["text","image"]
	    }
	  ]
	}`)
	raw, err := buildCodexModelCatalog(source, Model{
		ID:            "vendor/model",
		Name:          "Vendor Model",
		ContextLength: 1_000_000,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	var got codexModelCatalog
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(got.Models))
	}
	model := got.Models[0]
	if model["slug"] != "vendor/model" || model["display_name"] != "Vendor Model" {
		t.Fatalf("routed identity = %#v", model)
	}
	if intValue(model["context_window"]) != 1_000_000 || intValue(model["max_context_window"]) != 1_000_000 {
		t.Fatalf("context windows = %v/%v", model["context_window"], model["max_context_window"])
	}
	if model["base_instructions"] != "You are Codex." {
		t.Fatalf("base instructions were not preserved")
	}
	if model["apply_patch_tool_type"] != "freeform" || !boolValue(model["supports_parallel_tool_calls"]) {
		t.Fatalf("full Codex tools were not preserved: %#v", model)
	}
	if model["default_reasoning_level"] != "medium" {
		t.Fatalf("reasoning descriptor was not preserved: %#v", model)
	}
}

func TestBuildCodexModelCatalogDisablesUnsupportedReasoning(t *testing.T) {
	source := []byte(`{"models":[{
	  "slug":"template",
	  "apply_patch_tool_type":"freeform",
	  "supports_parallel_tool_calls":true,
	  "context_window":272000,
	  "default_reasoning_level":"high",
	  "supported_reasoning_levels":[{"effort":"high","description":"High"}],
	  "default_reasoning_summary":"auto"
	}]}`)
	raw, err := buildCodexModelCatalog(source, Model{ID: "plain-model"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var got codexModelCatalog
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	model := got.Models[0]
	if model["default_reasoning_level"] != nil {
		t.Fatalf("default reasoning = %#v, want nil", model["default_reasoning_level"])
	}
	if levels, ok := model["supported_reasoning_levels"].([]any); !ok || len(levels) != 0 {
		t.Fatalf("supported reasoning levels = %#v, want empty", model["supported_reasoning_levels"])
	}
	if model["default_reasoning_summary"] != "none" {
		t.Fatalf("default reasoning summary = %#v", model["default_reasoning_summary"])
	}
}

func TestCodexArgsUseSessionProviderAndPreservePrompt(t *testing.T) {
	args := codexArgs(
		"vendor/model",
		"http://127.0.0.1:8080/v1/",
		`C:\Temp Folder\models.json`,
		true,
		[]string{"fix", "the tests"},
		[]string{"--sandbox", "workspace-write"},
	)
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"--model\nvendor/model",
		`model_provider="simplerouter_session"`,
		`base_url = "http://127.0.0.1:8080/v1"`,
		`env_key = "SIMPLEROUTER_CODEX_API_KEY"`,
		`wire_api = "responses"`,
		`model_catalog_json="C:\\Temp Folder\\models.json"`,
		`model_reasoning_effort="none"`,
		"--sandbox\nworkspace-write",
		"fix the tests",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q:\n%s", want, joined)
		}
	}
	if args[len(args)-1] != "fix the tests" {
		t.Fatalf("prompt = %q, want one final positional", args[len(args)-1])
	}
}

func TestBuildCodexEnvOnlyReplacesPrivateSessionKey(t *testing.T) {
	got := buildCodexEnv([]string{
		"OPENAI_API_KEY=user-openai-key",
		"CODEX_HOME=/user/codex",
		codexAPIKeyEnv + "=stale",
	}, "routed-key")
	if !slices.Contains(got, "OPENAI_API_KEY=user-openai-key") || !slices.Contains(got, "CODEX_HOME=/user/codex") {
		t.Fatalf("user Codex environment was not preserved: %#v", got)
	}
	if slices.Contains(got, codexAPIKeyEnv+"=stale") || !slices.Contains(got, codexAPIKeyEnv+"=routed-key") {
		t.Fatalf("private session key was not replaced: %#v", got)
	}
}

func TestPrepareCodexModelCatalogWithInstalledCodex(t *testing.T) {
	codexPath, err := findCodex()
	if err != nil {
		t.Skip(err)
	}
	path, cleanup, err := prepareCodexModelCatalog(codexPath, Model{
		ID:            "test/provider-model",
		ContextLength: 512_000,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got codexModelCatalog
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0]["slug"] != "test/provider-model" {
		t.Fatalf("generated catalog = %#v", got)
	}
}
