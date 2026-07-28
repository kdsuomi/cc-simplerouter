package simplerouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildCodexModelCatalogPreservesFullToolDescriptor(t *testing.T) {
	source := []byte(`{
	  "models": [
	    {
	      "slug": "code-mode-template",
	      "base_instructions": "Code mode only.",
	      "apply_patch_tool_type": "freeform",
	      "supports_parallel_tool_calls": true,
	      "tool_mode": "code_mode_only",
	      "context_window": 272000
	    },
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
		t.Fatalf("direct-tool base instructions were not preserved")
	}
	if model["apply_patch_tool_type"] != "freeform" || !boolValue(model["supports_parallel_tool_calls"]) {
		t.Fatalf("full Codex tools were not preserved: %#v", model)
	}
	if model["tool_mode"] != nil || model["multi_agent_version"] != "v2" {
		t.Fatalf("expected direct tools plus multi-agent v2, got %#v", model)
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
		`show_raw_agent_reasoning=true`,
		`service_tier="default"`,
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

func TestFindCodexPrefersSimpleRouterBinary(t *testing.T) {
	home := t.TempDir()
	oldUserHomeDir := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = oldUserHomeDir })

	name := codexSimpleRouterBinaryName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	patchedPath := filepath.Join(home, ".local", "bin", name)
	if err := os.MkdirAll(filepath.Dir(patchedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patchedPath, []byte("patched codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findCodex()
	if err != nil {
		t.Fatal(err)
	}
	if got != patchedPath {
		t.Fatalf("findCodex() = %q, want patched binary %q", got, patchedPath)
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

func TestInstalledCodexExecUsesGeneratedResponsesProvider(t *testing.T) {
	codexPath, err := findCodex()
	if err != nil {
		t.Skip(err)
	}
	requests := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer routed-test-key" {
			http.Error(w, "missing routed authorization", http.StatusUnauthorized)
			return
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_test"}}`,
			``,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","item":{"type":"message","id":"msg_test","role":"assistant","content":[{"type":"output_text","text":""}]}}`,
			``,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"OK"}`,
			``,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_test","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"OK"}]}}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_test","end_turn":true,"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}}`,
			``,
			``,
		}, "\n"))
	}))
	defer server.Close()

	catalogPath, cleanup, err := prepareCodexModelCatalog(codexPath, Model{
		ID:            "test/provider-model",
		ContextLength: 512_000,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	args := []string{
		"exec",
		"--ignore-user-config",
		"--ignore-rules",
		"--ephemeral",
		"--skip-git-repo-check",
		"--color", "never",
		"-c", `service_tier="fast"`,
	}
	args = append(args, codexArgs(
		"test/provider-model",
		server.URL+"/v1",
		catalogPath,
		false,
		[]string{"Reply with OK and do not call tools."},
		nil,
	)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Env = buildCodexEnv(os.Environ(), "routed-test-key")
	cmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("codex exec: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "OK") {
		t.Fatalf("stdout did not contain model response:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "Configured service tier") {
		t.Fatalf("Codex emitted a service-tier warning:\n%s", stderr.String())
	}

	select {
	case raw := <-requests:
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "test/provider-model" || body["stream"] != true {
			t.Fatalf("unexpected Responses request: %#v", body)
		}
		if serviceTier, ok := body["service_tier"]; ok {
			t.Fatalf("Responses request included service_tier = %#v", serviceTier)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) == 0 {
			t.Fatal("Codex request did not include its tools")
		}
		var toolKinds []string
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			toolKinds = append(toolKinds, fmt.Sprint(tool["type"])+":"+fmt.Sprint(tool["name"]))
			if tool["type"] == "namespace" || tool["type"] == "web_search" || tool["type"] == "custom" {
				encoded, _ := json.Marshal(tool)
				t.Logf("captured special tool: %s", encoded)
			}
		}
		t.Logf("captured Codex tools: %s", strings.Join(toolKinds, ", "))
	default:
		t.Fatal("mock server did not receive a Responses request")
	}
}

func TestInstalledCodexExecThroughChatCompatibilityProxy(t *testing.T) {
	codexPath, err := findCodex()
	if err != nil {
		t.Skip(err)
	}

	chatRequests := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer chat-provider-key" {
			http.Error(w, "missing routed authorization", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chatRequests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`data: {"id":"chatcmpl_codex","choices":[{"index":0,"delta":{"reasoning_content":"brief thought"}}]}`,
			``,
			`data: {"id":"chatcmpl_codex","choices":[{"index":0,"delta":{"content":"CHAT_OK"}}]}`,
			``,
			`data: {"id":"chatcmpl_codex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: {"id":"chatcmpl_codex","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"completion_tokens_details":{"reasoning_tokens":1}}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newResponsesChatProxy(
		upstream.URL,
		"test/chat-model",
		upstream.Client(),
		responsesChatProxyOptions{
			Label:                "Test Chat Provider",
			SendReasoningEffort:  true,
			ReasoningReplayField: "reasoning_content",
			IncludeStreamUsage:   true,
		},
	))
	defer proxy.Close()

	catalogPath, cleanup, err := prepareCodexModelCatalog(codexPath, Model{
		ID:            "test/chat-model",
		ContextLength: 512_000,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	args := []string{
		"exec",
		"--ignore-user-config",
		"--ignore-rules",
		"--ephemeral",
		"--skip-git-repo-check",
		"--color", "never",
	}
	args = append(args, codexArgs(
		"test/chat-model",
		proxy.URL+"/v1",
		catalogPath,
		false,
		[]string{"Reply with CHAT_OK and do not call tools."},
		nil,
	)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Env = buildCodexEnv(os.Environ(), "chat-provider-key")
	cmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("codex exec through chat proxy: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "CHAT_OK") {
		t.Fatalf("stdout did not contain translated response:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	select {
	case body := <-chatRequests:
		if body["model"] != "test/chat-model" || body["stream"] != true {
			t.Fatalf("unexpected Chat request: %#v", body)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) == 0 {
			t.Fatal("translated Chat request did not include Codex tools")
		}
		messages, _ := body["messages"].([]any)
		if len(messages) < 2 {
			t.Fatalf("translated Chat messages = %#v", messages)
		}
	default:
		t.Fatal("mock Chat provider did not receive a request")
	}
}

func TestInstalledCodexExecThroughGeminiInteractionsProxy(t *testing.T) {
	codexPath, err := findCodex()
	if err != nil {
		t.Skip(err)
	}

	interactionRequests := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/interactions" || r.URL.Query().Get("alt") != "sse" {
			http.Error(w, "unexpected URL "+r.URL.String(), http.StatusNotFound)
			return
		}
		if r.Header.Get("x-goog-api-key") != "gemini-provider-key" {
			http.Error(w, "missing Gemini key", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		interactionRequests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: interaction.created`,
			`data: {"event_type":"interaction.created","interaction":{"id":"v1_codex","status":"in_progress","model":"gemini-3.5-flash"}}`,
			``,
			`event: step.start`,
			`data: {"event_type":"step.start","index":0,"step":{"type":"thought","summary":[{"type":"text","text":"brief thought"}]}}`,
			``,
			`event: step.delta`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"signed-thought"}}`,
			``,
			`event: step.stop`,
			`data: {"event_type":"step.stop","index":0}`,
			``,
			`event: step.start`,
			`data: {"event_type":"step.start","index":1,"step":{"type":"model_output","content":[{"type":"text","text":"GEMINI_OK"}]}}`,
			``,
			`event: step.stop`,
			`data: {"event_type":"step.stop","index":1}`,
			``,
			`event: interaction.completed`,
			`data: {"event_type":"interaction.completed","interaction":{"id":"v1_codex","status":"completed","model":"gemini-3.5-flash","usage":{"total_input_tokens":3,"total_output_tokens":1,"total_thought_tokens":1,"total_tokens":5}}}`,
			``,
			`event: done`,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newGeminiInteractionsProxy(
		upstream.URL,
		"gemini-3.5-flash",
		upstream.Client(),
		false,
	))
	defer proxy.Close()

	catalogPath, cleanup, err := prepareCodexModelCatalog(codexPath, Model{
		ID:            "gemini-3.5-flash",
		ContextLength: 1_000_000,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	args := []string{
		"exec",
		"--ignore-user-config",
		"--ignore-rules",
		"--ephemeral",
		"--skip-git-repo-check",
		"--color", "never",
	}
	args = append(args, codexArgs(
		"gemini-3.5-flash",
		proxy.URL+"/v1",
		catalogPath,
		false,
		[]string{"Reply with GEMINI_OK and do not call tools."},
		nil,
	)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Env = buildCodexEnv(os.Environ(), "gemini-provider-key")
	cmd.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("codex exec through Gemini proxy: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "GEMINI_OK") {
		t.Fatalf("stdout did not contain translated Gemini response:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	select {
	case body := <-interactionRequests:
		if body["model"] != "gemini-3.5-flash" || body["store"] != false || body["stream"] != true {
			t.Fatalf("unexpected Interactions request: %#v", body)
		}
		tools, _ := body["tools"].([]any)
		var sawFunction, sawSearch bool
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			switch tool["type"] {
			case "function":
				sawFunction = true
			case "google_search":
				sawSearch = true
			}
		}
		if !sawFunction || !sawSearch {
			t.Fatalf("Interactions tools lost Codex functions/search: %#v", tools)
		}
	default:
		t.Fatal("mock Gemini provider did not receive a request")
	}
}
