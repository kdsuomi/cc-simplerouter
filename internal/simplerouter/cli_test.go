package simplerouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func withTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := userHomeDir
	oldFindCodex := findCodexFn
	oldPrepareCatalog := prepareCodexModelCatalogFn
	oldStartResponsesPassthroughProxy := startResponsesPassthroughProxyFn
	oldStartMetaResponsesProxy := startMetaResponsesProxyFn
	oldStartXAIResponsesProxy := startXAIResponsesProxyFn
	oldStartLMStudioResponsesProxy := startLMStudioResponsesProxyFn
	oldLoadGrokCLISession := loadGrokCLISessionTokenFn
	userHomeDir = func() (string, error) { return dir, nil }
	findCodexFn = func() (string, error) { return filepath.Join(dir, "codex-test"), nil }
	prepareCodexModelCatalogFn = func(_ string, _ Model, _ bool) (string, func(), error) {
		return filepath.Join(dir, "models.json"), func() {}, nil
	}
	startResponsesPassthroughProxyFn = func(upstreamBase, _ string, _ *http.Client, _ responsesPassthroughOptions) (string, func(), error) {
		return upstreamBase, func() {}, nil
	}
	startMetaResponsesProxyFn = func(upstreamBase, _ string, _ *http.Client) (string, func(), error) {
		return upstreamBase, func() {}, nil
	}
	startXAIResponsesProxyFn = func(upstreamBase, _ string, _ *http.Client) (string, func(), error) {
		return upstreamBase, func() {}, nil
	}
	startLMStudioResponsesProxyFn = func(upstreamBase, _ string, _ *http.Client) (string, func(), error) {
		return upstreamBase, func() {}, nil
	}
	// Default: no Grok CLI session so unit tests do not touch the real machine login.
	loadGrokCLISessionTokenFn = func(context.Context, *http.Client) (string, error) {
		return "", nil
	}
	t.Cleanup(func() {
		userHomeDir = old
		findCodexFn = oldFindCodex
		prepareCodexModelCatalogFn = oldPrepareCatalog
		startResponsesPassthroughProxyFn = oldStartResponsesPassthroughProxy
		startMetaResponsesProxyFn = oldStartMetaResponsesProxy
		startXAIResponsesProxyFn = oldStartXAIResponsesProxy
		startLMStudioResponsesProxyFn = oldStartLMStudioResponsesProxy
		loadGrokCLISessionTokenFn = oldLoadGrokCLISession
	})
	return dir
}

func TestConfigRoundTripAndReset(t *testing.T) {
	withTestHome(t)
	cfg := Config{OpenRouterAPIKey: "sk-or-test", LastModel: "z-ai/glm-5.2"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg {
		t.Fatalf("config = %+v, want %+v", got, cfg)
	}
	if err := resetSavedKey(); err != nil {
		t.Fatal(err)
	}
	got, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenRouterAPIKey != "" || got.LastModel != cfg.LastModel {
		t.Fatalf("after reset = %+v", got)
	}
}

func TestLoadConfigAcceptsUTF8BOM(t *testing.T) {
	home := withTestHome(t)
	path := filepath.Join(home, configDirName, "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"openrouter_api_key":"sk-or-test","last_model":"z-ai/glm-5.2"}`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenRouterAPIKey != "sk-or-test" || cfg.LastModel != "z-ai/glm-5.2" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigTreatsEmptyFileAsFirstRun(t *testing.T) {
	home := withTestHome(t)
	path := filepath.Join(home, configDirName, "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"", "   \r\n", "\ufeff", "\ufeff  \n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig(%q) errored: %v", content, err)
		}
		if cfg != (Config{}) {
			t.Fatalf("loadConfig(%q) = %+v, want zero Config", content, cfg)
		}
	}
}

func TestCleanAPIKey(t *testing.T) {
	tests := map[string]string{
		" sk-or-v1-test \r\n":       "sk-or-v1-test",
		"\ufeffsk-or-v1-test":       "sk-or-v1-test",
		"\"sk-or-v1-test\"":         "sk-or-v1-test",
		"s\x00k\x00-\x00o\x00r\x00": "sk-or",
	}
	for input, want := range tests {
		if got := cleanAPIKey(input); got != want {
			t.Fatalf("cleanAPIKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveModel(t *testing.T) {
	models := []Model{
		{ID: "z-ai/glm-5.2", Name: "GLM 5.2"},
		{ID: "anthropic/claude-sonnet-4.5", Name: "Claude Sonnet 4.5"},
		{ID: "other/glm-5.2", Name: "Other GLM 5.2"},
	}
	if got, ok := resolveModel("anthropic/claude-sonnet-4.5", models); !ok || got.Model.ID != "anthropic/claude-sonnet-4.5" || !got.Exact {
		t.Fatalf("exact = %+v ok=%v", got, ok)
	}
	if got, ok := resolveModel("claude-sonnet-4.5", models); !ok || got.Model.ID != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("suffix = %+v ok=%v", got, ok)
	}
	if got, ok := resolveModel("glm-5.2", models); !ok || len(got.Ambiguous) != 2 {
		t.Fatalf("ambiguous = %+v ok=%v", got, ok)
	}
	if _, ok := resolveModel("missing", models); ok {
		t.Fatal("missing model unexpectedly matched")
	}
}

func TestCurrentOpenAIModelsAndAlias(t *testing.T) {
	models := curatedProviderModels(providerOpenAI)
	if len(models) < 3 {
		t.Fatalf("OpenAI models = %#v", models)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
	for i, id := range want {
		if models[i].ID != id || models[i].ContextLength != 1_050_000 || !modelSupportsReasoning(models[i]) {
			t.Fatalf("OpenAI model %d = %#v", i, models[i])
		}
	}
	for _, model := range models {
		if model.ID == "gpt-5.6" {
			t.Fatal("GPT-5.6 alias should not duplicate Sol in the picker")
		}
	}
	resolveModels := append(append([]Model(nil), models...), curatedProviderModelAliases(providerOpenAI)...)
	res, ok := resolveModel("gpt-5.6", resolveModels)
	if !ok || !res.Exact || res.Model.ID != "gpt-5.6" || res.Model.ContextLength != 1_050_000 {
		t.Fatalf("GPT-5.6 alias resolution = %#v, %v", res, ok)
	}
}

func TestCurrentXAIReasoningMetadata(t *testing.T) {
	models := append(curatedProviderModels(providerXAI), curatedProviderModelAliases(providerXAI)...)
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}

	for _, id := range []string{"grok-4.5", "grok-4.5-latest", "grok-build-latest"} {
		if got := byID[id].SupportedReasoningEfforts; !slices.Equal(got, []string{"low", "medium", "high"}) {
			t.Fatalf("%s reasoning efforts = %#v", id, got)
		}
	}
	for _, id := range []string{"grok-build-0.1", "grok-4.20-0309-reasoning"} {
		if got := byID[id].SupportedReasoningEfforts; len(got) != 0 || byID[id].DefaultReasoningEffort != "" || !modelSupportsReasoning(byID[id]) {
			t.Fatalf("%s fixed reasoning metadata = %#v", id, byID[id])
		}
	}
	for _, id := range []string{"grok-4.3", "grok-4.3-latest", "grok-latest"} {
		if got := byID[id].SupportedReasoningEfforts; !slices.Equal(got, []string{"none", "low", "medium", "high"}) {
			t.Fatalf("%s reasoning efforts = %#v", id, got)
		}
	}
	if got := byID["grok-4.20-multi-agent-0309"].SupportedReasoningEfforts; !slices.Equal(got, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("Grok Multi-Agent reasoning efforts = %#v", got)
	}
	if modelSupportsReasoning(byID["grok-4.20-0309-non-reasoning"]) {
		t.Fatal("Grok non-reasoning model unexpectedly advertises reasoning")
	}
}

func TestArgParsingAndLaunchSpec(t *testing.T) {
	home := withTestHome(t)

	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(Config{OpenRouterAPIKey: "sk-or-test"}); err != nil {
		t.Fatal(err)
	}

	srv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 202752}})
	defer srv.Close()

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:      strings.NewReader(""),
		stdout:     &strings.Builder{},
		stderr:     stderr,
		httpClient: srv.Client(),
		apiBase:    srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"--model", "z-ai/glm-5.2", work, "--", "--debug"}); err != nil {
		t.Fatal(err)
	}
	if spec.Dir != work {
		t.Fatalf("Dir = %q, want %q", spec.Dir, work)
	}
	wantArgs := codexArgs(
		"z-ai/glm-5.2",
		srv.URL,
		filepath.Join(home, "models.json"),
		false,
		nil,
		[]string{"--debug"},
	)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
	env := envMap(spec.Env)
	if env[codexAPIKeyEnv] != "sk-or-test" {
		t.Fatalf("%s not set from config", codexAPIKeyEnv)
	}
	if !strings.Contains(stderr.String(), "Launching Codex CLI: model z-ai/glm-5.2 | context 202,752 | reasoning provider default | dir "+work) {
		t.Fatalf("launch summary missing or wrong: %q", stderr.String())
	}
}

func TestOpenRouterLaunchUsesResponsesPassthrough(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{OpenRouterAPIKey: "sk-or-test"}); err != nil {
		t.Fatal(err)
	}

	srv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 202752}})
	defer srv.Close()

	var spec launchSpec
	var gotUpstreamBase, gotModel string
	var gotOptions responsesPassthroughOptions
	startResponsesPassthroughProxyFn = func(upstreamBase, model string, _ *http.Client, options responsesPassthroughOptions) (string, func(), error) {
		gotUpstreamBase = upstreamBase
		gotModel = model
		gotOptions = options
		return "http://127.0.0.1:43210/v1", func() {}, nil
	}
	a := &app{
		stdin:      strings.NewReader(""),
		stdout:     &strings.Builder{},
		stderr:     &strings.Builder{},
		httpClient: srv.Client(),
		apiBase:    srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"--model", "z-ai/glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	if spec.Path != filepath.Join(home, "codex-test") {
		t.Fatalf("Codex path = %q", spec.Path)
	}
	if gotUpstreamBase != srv.URL || gotModel != "z-ai/glm-5.2" {
		t.Fatalf("passthrough route = %q model %q", gotUpstreamBase, gotModel)
	}
	if gotOptions.Label != "OpenRouter" || gotOptions.ProviderTag != "" {
		t.Fatalf("passthrough options = %#v", gotOptions)
	}
	wantArgs := codexArgs("z-ai/glm-5.2", "http://127.0.0.1:43210/v1", filepath.Join(home, "models.json"), false, nil, nil)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
}

func TestOneMillionContextStaysInCodexCatalog(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{OpenRouterAPIKey: "sk-or-test"}); err != nil {
		t.Fatal(err)
	}

	srv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 1_048_576}})
	defer srv.Close()

	var spec launchSpec
	a := &app{
		stdin:      strings.NewReader(""),
		stdout:     &strings.Builder{},
		stderr:     &strings.Builder{},
		httpClient: srv.Client(),
		apiBase:    srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"--model", "z-ai/glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	wantArgs := codexArgs("z-ai/glm-5.2", srv.URL, filepath.Join(home, "models.json"), false, nil, nil)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
}

func TestPromptedKeyIsValidatedBeforeSave(t *testing.T) {
	withTestHome(t)
	srv := openRouterTestServer(t, http.StatusUnauthorized, nil)
	defer srv.Close()

	a := &app{
		stdin:      strings.NewReader("bad-key\n"),
		stdout:     &strings.Builder{},
		stderr:     &strings.Builder{},
		httpClient: srv.Client(),
		apiBase:    srv.URL,
	}
	err := a.run(context.Background(), []string{"--model", "some/model"})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejected key error, got %v", err)
	}
	cfg, loadErr := loadConfig()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if cfg.OpenRouterAPIKey != "" {
		t.Fatalf("invalid key was saved: %+v", cfg)
	}
}

func TestInvalidSavedKeyPromptsForReplacement(t *testing.T) {
	withTestHome(t)
	if err := saveConfig(Config{OpenRouterAPIKey: "stale-key", LastModel: "z-ai/glm-5.2"}); err != nil {
		t.Fatal(err)
	}

	var keyChecks int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key":
			count := atomic.AddInt32(&keyChecks, 1)
			if count == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":"bad key"}`)
				return
			}
			fmt.Fprint(w, `{"data":{"label":"replacement"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &app{
		stdin:      strings.NewReader("replacement-key\n"),
		stdout:     &strings.Builder{},
		stderr:     &strings.Builder{},
		httpClient: srv.Client(),
		apiBase:    srv.URL,
	}
	key, err := a.openRouterKey(context.Background(), Config{OpenRouterAPIKey: "stale-key"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "replacement-key" {
		t.Fatalf("key = %q, want replacement-key", key)
	}
	if !strings.Contains(a.stderr.(*strings.Builder).String(), "no longer valid") {
		t.Fatalf("expected stale-key warning, got %q", a.stderr.(*strings.Builder).String())
	}
}

func TestLaunchThinkingMode(t *testing.T) {
	if got := launchThinkingMode(providerOpenRouter, Model{}, false); got != "provider default" {
		t.Fatalf("OpenRouter reasoning mode = %q", got)
	}
	if got := launchThinkingMode(providerZAI, Model{}, false); got != "provider default" {
		t.Fatalf("Z.AI reasoning mode = %q", got)
	}
	if got := launchThinkingMode(providerOpenRouter, Model{}, true); got != "disabled" {
		t.Fatalf("disabled reasoning mode = %q", got)
	}
	grok45 := Model{SupportedParameters: []string{"reasoning"}, SupportedReasoningEfforts: []string{"low", "medium", "high"}, DefaultReasoningEffort: "high"}
	if got := launchThinkingMode(providerXAI, grok45, true); got != "low" {
		t.Fatalf("Grok 4.5 disabled reasoning mode = %q", got)
	}
	grok43 := Model{SupportedParameters: []string{"reasoning"}, SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}, DefaultReasoningEffort: "low"}
	if got := launchThinkingMode(providerXAI, grok43, true); got != "disabled" {
		t.Fatalf("Grok 4.3 disabled reasoning mode = %q", got)
	}
	nonReasoning := Model{SupportedParameters: []string{"tools"}}
	if got := launchThinkingMode(providerXAI, nonReasoning, false); got != "disabled" {
		t.Fatalf("Grok non-reasoning mode = %q", got)
	}
	fixedReasoning := Model{SupportedParameters: []string{"tools", "reasoning"}}
	if got := launchThinkingMode(providerXAI, fixedReasoning, true); got != "fixed" {
		t.Fatalf("Grok fixed reasoning mode = %q", got)
	}
}

func TestModelsEndpointFiltersToUsableModels(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query().Encode()
		// Two models in deliberate (popularity) order; client must preserve it.
		fmt.Fprint(w, `{"data":[{"id":"second/model","name":"Second","context_length":2222},{"id":"first/model","name":"First","context_length":1111}]}`)
	}))
	defer srv.Close()

	client := newOpenRouterClient(srv.Client(), srv.URL)
	models, err := client.models(context.Background(), "sk-or-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"output_modalities=text", "supported_parameters=tools", "sort=most-popular"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("models request query %q missing %q", gotQuery, want)
		}
	}
	// The API's order (popularity) must be preserved, not re-sorted alphabetically.
	if len(models) != 2 || models[0].ID != "second/model" || models[1].ID != "first/model" {
		t.Fatalf("models order not preserved: %+v", models)
	}
}

func TestFirstRunWizardRecommendsAndSavesModel(t *testing.T) {
	home := withTestHome(t)

	srv := openRouterTestServer(t, http.StatusOK, []Model{
		{ID: "vendor/other", Name: "Other", ContextLength: 8192},
		{ID: "z-ai/glm-5.2", Name: "Z.ai: GLM 5.2", ContextLength: 1_048_576},
	})
	defer srv.Close()

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		// First-run flow: provider choice (Enter keeps OpenRouter), key, model.
		stdin:      strings.NewReader("\nsk-or-test\n\n"),
		stdout:     &strings.Builder{},
		stderr:     stderr,
		httpClient: srv.Client(),
		apiBase:    srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Args, codexArgs("z-ai/glm-5.2", srv.URL, filepath.Join(home, "models.json"), false, nil, nil)) {
		t.Fatalf("Args = %v", spec.Args)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenRouterAPIKey != "sk-or-test" || cfg.LastModel != "z-ai/glm-5.2" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Provider != providerOpenRouter {
		t.Fatalf("provider = %q", cfg.Provider)
	}
	out := stderr.String()
	for _, want := range []string{"simplerouter setup", "Select a provider", "Fetching OpenRouter models", "Launching Codex CLI"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q: %q", want, out)
		}
	}
}

func TestPickerRecommendedColumnsAndEnterDefault(t *testing.T) {
	stderr := &strings.Builder{}
	a := &app{
		stdin:  strings.NewReader("\n"),
		stderr: stderr,
	}
	res, err := a.pickModel("Select an OpenRouter model", []Model{
		{ID: "vendor/other", Name: "Other Model", ContextLength: 8192},
		{
			ID:                  "z-ai/glm-5.2",
			Name:                "Z.ai: GLM 5.2",
			ContextLength:       1_048_576,
			PromptPrice:         "0.00000095",
			OutputPrice:         "0.000003",
			SupportedParameters: []string{"tools", "reasoning"},
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model.ID != "z-ai/glm-5.2" {
		t.Fatalf("selected = %s", res.Model.ID)
	}
	out := stderr.String()
	for _, want := range []string{"MODEL", "NAME", "CTX", "PRICE/M", "1,048,576", "$0.95/$3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("non-terminal output should not include ANSI color: %q", out)
	}
}

func TestPickerDetailsAndPagination(t *testing.T) {
	models := make([]Model, 14)
	for i := range models {
		models[i] = Model{
			ID:                  fmt.Sprintf("vendor/model-%02d", i),
			Name:                fmt.Sprintf("Model %02d", i),
			ContextLength:       1000 + i,
			SupportedParameters: []string{"tools"},
		}
	}
	stderr := &strings.Builder{}
	a := &app{
		stdin:  strings.NewReader("? 1\nn\n1\n"),
		stderr: stderr,
	}
	res, err := a.pickModel("Select an OpenRouter model", models, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model.ID != "vendor/model-12" {
		t.Fatalf("selected = %s", res.Model.ID)
	}
	out := stderr.String()
	for _, want := range []string{"Model details", "Params", "page 2/2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker output missing %q: %q", want, out)
		}
	}
}

func TestConfigBackCompatDefaultsToOpenRouter(t *testing.T) {
	home := withTestHome(t)
	path := filepath.Join(home, configDirName, "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Old-format config from before provider support.
	if err := os.WriteFile(path, []byte(`{"openrouter_api_key":"sk-or-test","last_model":"z-ai/glm-5.2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "" || cfg.GeminiAPIKey != "" || cfg.LastGeminiModel != "" {
		t.Fatalf("config = %+v", cfg)
	}
	// Unknown provider values are normalized away rather than breaking launch.
	if err := os.WriteFile(path, []byte(`{"provider":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "" {
		t.Fatalf("unknown provider not normalized: %q", cfg.Provider)
	}
}

func TestResetSavedKeyClearsBothKeys(t *testing.T) {
	withTestHome(t)
	cfg := Config{
		Provider:          providerGemini,
		OpenRouterAPIKey:  "sk-or-test",
		GeminiAPIKey:      "gm-test",
		OpenAIAPIKey:      "sk-openai",
		DeepSeekAPIKey:    "sk-deepseek",
		ZAIAPIKey:         "sk-zai",
		MetaAPIKey:        "sk-meta",
		XAIAPIKey:         "xai-test",
		LastModel:         "z-ai/glm-5.2",
		LastGeminiModel:   "gemini-2.5-flash",
		LastOpenAIModel:   "gpt-5.5",
		LastDeepSeekModel: "deepseek-v4-flash",
		LastZAIModel:      "glm-5.2",
		LastMetaModel:     "muse-spark-1.1",
		LastGrokModel:     "grok-4.5",
		LastLMStudioModel: "qwen/qwen3.8-27b",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := resetSavedKey(); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenRouterAPIKey != "" || got.GeminiAPIKey != "" || got.OpenAIAPIKey != "" || got.DeepSeekAPIKey != "" || got.ZAIAPIKey != "" || got.MetaAPIKey != "" || got.XAIAPIKey != "" {
		t.Fatalf("keys not cleared: %+v", got)
	}
	if got.Provider != providerGemini || got.LastModel != cfg.LastModel || got.LastGeminiModel != cfg.LastGeminiModel || got.LastOpenAIModel != cfg.LastOpenAIModel || got.LastDeepSeekModel != cfg.LastDeepSeekModel || got.LastZAIModel != cfg.LastZAIModel || got.LastMetaModel != cfg.LastMetaModel || got.LastGrokModel != cfg.LastGrokModel || got.LastLMStudioModel != cfg.LastLMStudioModel {
		t.Fatalf("non-key fields changed: %+v", got)
	}
}

func TestInferProviderFromModel(t *testing.T) {
	cases := map[string]string{
		"z-ai/glm-5.2":               providerOpenRouter,
		"gemini-2.5-flash":           providerGemini,
		"models/gemini-2.5-pro":      providerGemini,
		"models/other-model":         providerGemini,
		"glm-5.2":                    providerZAI,
		"muse-spark-1.1":             providerMeta,
		"muse-spark-1.2":             providerMeta,
		"muse-spark-1.2-contributor": providerMeta,
		"grok-4.5":                   providerXAI,
		"grok-build-0.1":             providerXAI,
		"":                           "",
	}
	for input, want := range cases {
		if got := inferProviderFromModel(input); got != want {
			t.Errorf("inferProviderFromModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCanonicalProviderGrokAliases(t *testing.T) {
	for _, input := range []string{"xai", "grok", "XAI", "x-ai", "x.ai"} {
		if got := canonicalProvider(input); got != providerXAI {
			t.Errorf("canonicalProvider(%q) = %q, want %q", input, got, providerXAI)
		}
	}
}

func TestCanonicalProviderLMStudioAliases(t *testing.T) {
	for _, input := range []string{"lmstudio", "LM Studio", "lm-studio", "lm_studio", "local"} {
		if got := canonicalProvider(input); got != providerLMStudio {
			t.Errorf("canonicalProvider(%q) = %q, want %q", input, got, providerLMStudio)
		}
	}
}

func geminiTestServer(t *testing.T, keyStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if keyStatus != http.StatusOK {
			w.WriteHeader(keyStatus)
			fmt.Fprint(w, `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`)
			return
		}
		fmt.Fprint(w, `{"models":[
			{"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash","inputTokenLimit":1048576,"supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-2.5-flash-lite","displayName":"Gemini 2.5 Flash-Lite","inputTokenLimit":1048576,"supportedGenerationMethods":["generateContent"]}
		]}`)
	}))
}

func TestGeminiModelFlagLaunchesThroughProxy(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{GeminiAPIKey: "gm-test", LastModel: "z-ai/glm-5.2"}); err != nil {
		t.Fatal(err)
	}

	srv := geminiTestServer(t, http.StatusOK)
	defer srv.Close()

	var proxyKeyBase, proxyModel string
	var proxyDisableReasoning bool
	oldProxy := startGeminiResponsesProxyFn
	startGeminiResponsesProxyFn = func(upstreamBase, model string, _ *http.Client, disableReasoning bool) (string, func(), error) {
		proxyKeyBase, proxyModel = upstreamBase, model
		proxyDisableReasoning = disableReasoning
		return "http://127.0.0.1:9999/v1", func() {}, nil
	}
	t.Cleanup(func() { startGeminiResponsesProxyFn = oldProxy })

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:         strings.NewReader(""),
		stdout:        &strings.Builder{},
		stderr:        stderr,
		httpClient:    srv.Client(),
		geminiAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"--model", "gemini-2.5-flash"}); err != nil {
		t.Fatal(err)
	}

	if proxyModel != "gemini-2.5-flash" || proxyKeyBase != srv.URL {
		t.Fatalf("proxy stub got (%q, %q)", proxyKeyBase, proxyModel)
	}
	if proxyDisableReasoning {
		t.Fatal("Gemini reasoning unexpectedly disabled")
	}
	if !slices.Equal(spec.Args, codexArgs(
		"gemini-2.5-flash",
		"http://127.0.0.1:9999/v1",
		filepath.Join(home, "models.json"),
		false,
		nil,
		nil,
	)) {
		t.Fatalf("Args = %v", spec.Args)
	}
	env := envMap(spec.Env)
	if env[codexAPIKeyEnv] != "gm-test" {
		t.Fatalf("session key = %q, want the real Gemini key", env[codexAPIKeyEnv])
	}
	if !strings.Contains(stderr.String(), "provider Google AI Studio") {
		t.Fatalf("launch summary missing provider label: %q", stderr.String())
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerGemini || cfg.LastGeminiModel != "gemini-2.5-flash" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.LastModel != "z-ai/glm-5.2" {
		t.Fatalf("OpenRouter last model clobbered: %+v", cfg)
	}
}

func TestDeepSeekLaunchesThroughResponsesProxy(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerDeepSeek, DeepSeekAPIKey: "ds-test", LastDeepSeekModel: "deepseek-v4-flash"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ds-test" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var proxyBase, proxyModel string
	var proxyDisableReasoning bool
	oldProxy := startDeepSeekResponsesProxyFn
	startDeepSeekResponsesProxyFn = func(upstreamBase, model string, _ *http.Client, disableReasoning bool) (string, func(), error) {
		proxyBase, proxyModel, proxyDisableReasoning = upstreamBase, model, disableReasoning
		return "http://127.0.0.1:9090/v1", func() {}, nil
	}
	t.Cleanup(func() { startDeepSeekResponsesProxyFn = oldProxy })

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:           strings.NewReader("\n\n"),
		stdout:          &strings.Builder{},
		stderr:          stderr,
		httpClient:      srv.Client(),
		deepSeekAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if proxyBase != srv.URL || proxyModel != "deepseek-v4-flash" || proxyDisableReasoning {
		t.Fatalf("proxy got (%q, %q, %v)", proxyBase, proxyModel, proxyDisableReasoning)
	}
	env := envMap(spec.Env)
	if env[codexAPIKeyEnv] != "ds-test" {
		t.Fatalf("session key = %q", env[codexAPIKeyEnv])
	}
	if !slices.Equal(spec.Args, codexArgs(
		"deepseek-v4-flash",
		"http://127.0.0.1:9090/v1",
		filepath.Join(home, "models.json"),
		false,
		nil,
		nil,
	)) {
		t.Fatalf("Args = %v", spec.Args)
	}
	if !strings.Contains(stderr.String(), "provider DeepSeek") {
		t.Fatalf("launch summary missing provider label: %q", stderr.String())
	}
}

func TestOpenAILaunchesAgainstNativeResponses(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerOpenAI, OpenAIAPIKey: "oa-test", LastOpenAIModel: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var spec launchSpec
	a := &app{
		stdin:         strings.NewReader("\n\n"),
		stdout:        &strings.Builder{},
		stderr:        &strings.Builder{},
		httpClient:    srv.Client(),
		openAIAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	env := envMap(spec.Env)
	if env[codexAPIKeyEnv] != "oa-test" {
		t.Fatalf("session key = %q", env[codexAPIKeyEnv])
	}
	if !slices.Equal(spec.Args, codexArgs("gpt-5.5", srv.URL, filepath.Join(home, "models.json"), false, nil, nil)) {
		t.Fatalf("Args = %v", spec.Args)
	}
}

func TestCodexSubscriptionLaunchPreservesExistingCodexRoutingAndAuth(t *testing.T) {
	home := withTestHome(t)
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(Config{
		Provider:         providerOpenRouter,
		OpenRouterAPIKey: "sk-or-test",
		LastModel:        "z-ai/glm-5.2",
	}); err != nil {
		t.Fatal(err)
	}

	prepareCodexModelCatalogFn = func(string, Model, bool) (string, func(), error) {
		t.Fatal("subscription launch must not prepare a temporary model catalog")
		return "", func() {}, nil
	}
	t.Setenv(codexAPIKeyEnv, "stale-session-key")
	t.Setenv(codexTokenRateEnv, "stale-marker")
	t.Setenv("SIMPLEROUTER_TEST_INHERITED", "present")

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:  strings.NewReader(""),
		stdout: &strings.Builder{},
		stderr: stderr,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{
		"--provider", "codex",
		work,
		"fix", "the tests",
		"--", "--full-auto",
	}); err != nil {
		t.Fatal(err)
	}

	wantArgs := []string{"--full-auto", "fix the tests"}
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if spec.Path != filepath.Join(home, "codex-test") || spec.Dir != work {
		t.Fatalf("launch target = (%q, %q)", spec.Path, spec.Dir)
	}
	env := envMap(spec.Env)
	if env[codexTokenRateEnv] != "1" || env["SIMPLEROUTER_TEST_INHERITED"] != "present" {
		t.Fatalf("subscription environment = %v", env)
	}
	if _, ok := env[codexAPIKeyEnv]; ok {
		t.Fatalf("subscription launch retained %s", codexAPIKeyEnv)
	}
	if !strings.Contains(stderr.String(), "existing ChatGPT sign-in") {
		t.Fatalf("subscription launch summary missing: %q", stderr.String())
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerCodex || cfg.OpenRouterAPIKey != "sk-or-test" || cfg.LastModel != "z-ai/glm-5.2" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestMetaLaunchesThroughResponsesCompatibilityProxy(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerMeta, MetaAPIKey: "meta-test", LastMetaModel: "muse-spark-1.2"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var proxyBase, proxyModel string
	startMetaResponsesProxyFn = func(upstreamBase, model string, _ *http.Client) (string, func(), error) {
		proxyBase, proxyModel = upstreamBase, model
		return "http://127.0.0.1:9393/v1", func() {}, nil
	}

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:       strings.NewReader("\n\n"),
		stdout:      &strings.Builder{},
		stderr:      stderr,
		httpClient:  srv.Client(),
		metaAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if proxyBase != srv.URL || proxyModel != "muse-spark-1.2" {
		t.Fatalf("Meta proxy got (%q, %q)", proxyBase, proxyModel)
	}
	model := curatedProviderModels(providerMeta)[0]
	if model.ID != "muse-spark-1.2" {
		t.Fatalf("default Meta model = %q, want muse-spark-1.2", model.ID)
	}
	wantArgs := metaCodexArgs(model, "http://127.0.0.1:9393/v1", filepath.Join(home, "models.json"), false, nil, nil)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if envMap(spec.Env)[codexAPIKeyEnv] != "meta-test" {
		t.Fatalf("session key was not forwarded")
	}
	if !strings.Contains(stderr.String(), "reasoning high") {
		t.Fatalf("launch summary missing Meta reasoning level: %q", stderr.String())
	}
}

func TestXAILaunchesThroughResponsesCompatibilityProxy(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerXAI, XAIAPIKey: "xai-test", LastGrokModel: "grok-4.5"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var proxyBase, proxyModel string
	startXAIResponsesProxyFn = func(upstreamBase, model string, _ *http.Client) (string, func(), error) {
		proxyBase, proxyModel = upstreamBase, model
		return "http://127.0.0.1:9494/v1", func() {}, nil
	}
	// Avoid reading the real machine's Grok CLI credentials in unit tests.
	loadGrokCLISessionTokenFn = func(context.Context, *http.Client) (string, error) {
		return "", nil
	}

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:      strings.NewReader("\n\n"),
		stdout:     &strings.Builder{},
		stderr:     stderr,
		httpClient: srv.Client(),
		xaiAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if proxyBase != srv.URL || proxyModel != "grok-4.5" {
		t.Fatalf("xAI proxy got (%q, %q)", proxyBase, proxyModel)
	}
	model := curatedProviderModels(providerXAI)[0]
	if model.ID != "grok-4.5" {
		t.Fatalf("default Grok model = %q, want grok-4.5", model.ID)
	}
	wantArgs := reasoningAwareCodexArgs(model, "http://127.0.0.1:9494/v1", filepath.Join(home, "models.json"), false, nil, nil)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if envMap(spec.Env)[codexAPIKeyEnv] != "xai-test" {
		t.Fatalf("session key was not forwarded")
	}
	if !strings.Contains(stderr.String(), "reasoning high") {
		t.Fatalf("launch summary missing xAI reasoning level: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "provider xAI") {
		t.Fatalf("launch summary missing xAI provider label: %q", stderr.String())
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerXAI || cfg.XAIAPIKey != "xai-test" || cfg.LastGrokModel != "grok-4.5" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLMStudioLaunchesThroughResponsesCompatibilityProxy(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerLMStudio, LastLMStudioModel: "qwen/qwen3.8-27b"}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"models":[{"type":"llm","key":"qwen/qwen3.8-27b","display_name":"Qwen3.8 27B","max_context_length":262144,"capabilities":{"trained_for_tool_use":true,"reasoning":{"allowed_options":["off","low","medium","xhigh","on"],"default":"xhigh"}},"loaded_instances":[{"config":{"context_length":162048}}]}]}`)
	}))
	defer server.Close()

	var proxyBase, proxyModel string
	startLMStudioResponsesProxyFn = func(upstreamBase, model string, _ *http.Client) (string, func(), error) {
		proxyBase, proxyModel = upstreamBase, model
		return "http://127.0.0.1:9595/v1", func() {}, nil
	}

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:           strings.NewReader("\n\n"),
		stdout:          &strings.Builder{},
		stderr:          stderr,
		httpClient:      server.Client(),
		lmStudioAPIBase: server.URL + "/v1",
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if proxyBase != server.URL+"/v1" || proxyModel != "qwen/qwen3.8-27b" {
		t.Fatalf("LM Studio proxy got (%q, %q)", proxyBase, proxyModel)
	}
	model := Model{
		ID:                        "qwen/qwen3.8-27b",
		Name:                      "Qwen3.8 27B",
		ContextLength:             162048,
		SupportedParameters:       []string{"tools", "reasoning"},
		SupportedReasoningEfforts: []string{"none", "low", "medium", "xhigh"},
		DefaultReasoningEffort:    "xhigh",
		DefaultReasoningSummary:   "auto",
		AutoCompactTokenLimit:     129638,
	}
	wantArgs := reasoningAwareCodexArgs(model, "http://127.0.0.1:9595/v1", filepath.Join(home, "models.json"), false, nil, nil)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
	if envMap(spec.Env)[codexAPIKeyEnv] != lmStudioSessionKey {
		t.Fatalf("local session key = %q", envMap(spec.Env)[codexAPIKeyEnv])
	}
	if !strings.Contains(stderr.String(), "reasoning xhigh") || !strings.Contains(stderr.String(), "provider LM Studio") {
		t.Fatalf("launch summary missing LM Studio metadata: %q", stderr.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerLMStudio || cfg.LastLMStudioModel != "qwen/qwen3.8-27b" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestXAIUsesGrokCLISessionWithoutPersistingToken(t *testing.T) {
	withTestHome(t)
	if err := saveConfig(Config{Provider: providerXAI, LastGrokModel: "grok-4.5"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer session-jwt" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	startXAIResponsesProxyFn = func(string, string, *http.Client) (string, func(), error) {
		return "http://127.0.0.1:9495/v1", func() {}, nil
	}
	loadGrokCLISessionTokenFn = func(context.Context, *http.Client) (string, error) {
		return "session-jwt", nil
	}

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		stdin:      strings.NewReader("\n\n"),
		stdout:     &strings.Builder{},
		stderr:     stderr,
		httpClient: srv.Client(),
		xaiAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if envMap(spec.Env)[codexAPIKeyEnv] != "session-jwt" {
		t.Fatalf("session JWT was not forwarded to Codex")
	}
	if !strings.Contains(stderr.String(), "Using Grok CLI login") {
		t.Fatalf("expected Grok CLI login notice: %q", stderr.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.XAIAPIKey != "" {
		t.Fatalf("session JWT should not be persisted: %+v", cfg)
	}
	if cfg.LastGrokModel != "grok-4.5" {
		t.Fatalf("last model not saved: %+v", cfg)
	}
}

func TestMetaContributorModelLaunches(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerMeta, MetaAPIKey: "meta-test", LastMetaModel: "muse-spark-1.2-contributor"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var proxyModel string
	startMetaResponsesProxyFn = func(_, model string, _ *http.Client) (string, func(), error) {
		proxyModel = model
		return "http://127.0.0.1:9394/v1", func() {}, nil
	}

	var spec launchSpec
	a := &app{
		stdin:       strings.NewReader("\n\n"),
		stdout:      &strings.Builder{},
		stderr:      &strings.Builder{},
		httpClient:  srv.Client(),
		metaAPIBase: srv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if proxyModel != "muse-spark-1.2-contributor" {
		t.Fatalf("Meta proxy model = %q, want muse-spark-1.2-contributor", proxyModel)
	}
	var model Model
	for _, m := range curatedProviderModels(providerMeta) {
		if m.ID == "muse-spark-1.2-contributor" {
			model = m
			break
		}
	}
	if model.ID == "" {
		t.Fatal("muse-spark-1.2-contributor missing from curated Meta models")
	}
	wantArgs := metaCodexArgs(model, "http://127.0.0.1:9394/v1", filepath.Join(home, "models.json"), false, nil, nil)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("Args = %v, want %v", spec.Args, wantArgs)
	}
}

func TestZAILaunchesThroughProxy(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{Provider: providerZAI, ZAIAPIKey: "zai-test", LastZAIModel: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}

	var proxyBase, proxyModel string
	var proxyDisableThinking bool
	oldProxy := startZAIResponsesProxyFn
	startZAIResponsesProxyFn = func(upstreamBase, model string, _ *http.Client, disableThinking bool) (string, func(), error) {
		proxyBase, proxyModel, proxyDisableThinking = upstreamBase, model, disableThinking
		return "http://127.0.0.1:9292/v1", func() {}, nil
	}
	t.Cleanup(func() { startZAIResponsesProxyFn = oldProxy })

	var spec launchSpec
	a := &app{
		stdin:  strings.NewReader("\n\n"),
		stdout: &strings.Builder{},
		stderr: &strings.Builder{},
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"--disable-thinking"}); err != nil {
		t.Fatal(err)
	}
	if proxyBase != defaultZAIAPIBase || proxyModel != "glm-5.2" || !proxyDisableThinking {
		t.Fatalf("proxy got (%q, %q, %v)", proxyBase, proxyModel, proxyDisableThinking)
	}
	env := envMap(spec.Env)
	if env[codexAPIKeyEnv] != "zai-test" {
		t.Fatalf("session key = %q", env[codexAPIKeyEnv])
	}
	if !slices.Equal(spec.Args, codexArgs(
		"glm-5.2",
		"http://127.0.0.1:9292/v1",
		filepath.Join(home, "models.json"),
		true,
		nil,
		nil,
	)) {
		t.Fatalf("Args = %v", spec.Args)
	}
}

func TestBareRelaunchShowsProviderPicker(t *testing.T) {
	withTestHome(t)
	if err := saveConfig(Config{Provider: providerGemini, GeminiAPIKey: "gm-test", LastGeminiModel: "gemini-2.5-flash"}); err != nil {
		t.Fatal(err)
	}

	srv := geminiTestServer(t, http.StatusOK)
	defer srv.Close()
	oldProxy := startGeminiResponsesProxyFn
	startGeminiResponsesProxyFn = func(string, string, *http.Client, bool) (string, func(), error) {
		return "http://127.0.0.1:9999/v1", func() {}, nil
	}
	t.Cleanup(func() { startGeminiResponsesProxyFn = oldProxy })

	stderr := &strings.Builder{}
	a := &app{
		stdin:         strings.NewReader("\n\n"), // provider Enter, model Enter
		stdout:        &strings.Builder{},
		stderr:        stderr,
		httpClient:    srv.Client(),
		geminiAPIBase: srv.URL,
		runCommand:    func(launchSpec) error { return nil },
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Select a provider") {
		t.Fatalf("bare relaunch must show the provider picker: %q", out)
	}
	if !strings.Contains(out, "Select a Gemini model") {
		t.Fatalf("model picker missing: %q", out)
	}
}

func TestBareCommandWithLegacyLastModelShowsProviderPicker(t *testing.T) {
	withTestHome(t)
	if err := saveConfig(Config{LastModel: "z-ai/glm-5.2", ZAIAPIKey: "zai-test"}); err != nil {
		t.Fatal(err)
	}

	oldProxy := startZAIResponsesProxyFn
	startZAIResponsesProxyFn = func(string, string, *http.Client, bool) (string, func(), error) {
		return "http://127.0.0.1:9292/v1", func() {}, nil
	}
	t.Cleanup(func() { startZAIResponsesProxyFn = oldProxy })

	stderr := &strings.Builder{}
	a := &app{
		stdin:      strings.NewReader("6\n\n"), // Z.AI, first model
		stdout:     &strings.Builder{},
		stderr:     stderr,
		runCommand: func(launchSpec) error { return nil },
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	out := stderr.String()
	if !strings.Contains(out, "Select a provider") || !strings.Contains(out, "Select a Z.AI model") {
		t.Fatalf("bare legacy config should start at provider picker: %q", out)
	}
	if strings.Contains(out, "Paste your OpenRouter API key") {
		t.Fatalf("must not prompt for OpenRouter key before provider choice: %q", out)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerZAI || cfg.LastZAIModel != "glm-5.2" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestGeminiInvalidSavedKeyPromptsForReplacement(t *testing.T) {
	withTestHome(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First validation (saved key) rejects with Google's 400; later calls succeed.
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`)
			return
		}
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer srv.Close()

	a := &app{
		stdin:         strings.NewReader("replacement-key\n"),
		stdout:        &strings.Builder{},
		stderr:        &strings.Builder{},
		httpClient:    srv.Client(),
		geminiAPIBase: srv.URL,
	}
	key, err := a.geminiKey(context.Background(), Config{GeminiAPIKey: "stale-key"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "replacement-key" {
		t.Fatalf("key = %q", key)
	}
	if !strings.Contains(a.stderr.(*strings.Builder).String(), "no longer valid") {
		t.Fatalf("missing stale-key warning: %q", a.stderr.(*strings.Builder).String())
	}
}

func TestModelPickerBackReturnsToProviderPicker(t *testing.T) {
	withTestHome(t)
	// Both keys saved so no key prompts interrupt the picker flow.
	if err := saveConfig(Config{OpenRouterAPIKey: "sk-or-test", GeminiAPIKey: "gm-test"}); err != nil {
		t.Fatal(err)
	}

	orSrv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 202752}})
	defer orSrv.Close()
	gmSrv := geminiTestServer(t, http.StatusOK)
	defer gmSrv.Close()

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		// Provider: 2 (Gemini) -> model picker: b (back) -> provider: 1
		// (OpenRouter) -> model picker: Enter (first model).
		stdin:         strings.NewReader("2\nb\n1\n\n"),
		stdout:        &strings.Builder{},
		stderr:        stderr,
		httpClient:    orSrv.Client(),
		apiBase:       orSrv.URL,
		geminiAPIBase: gmSrv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"--select-model"}); err != nil {
		t.Fatal(err)
	}

	out := stderr.String()
	for _, want := range []string{"Select a Gemini model", "Select an OpenRouter model", "b back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q: %q", want, out)
		}
	}
	// The provider picker must have been shown twice (initial + after back).
	if strings.Count(out, "Select a provider") < 2 {
		t.Fatalf("provider picker not re-shown after back: %q", out)
	}
	if len(spec.Args) < 2 || spec.Args[0] != "--model" || spec.Args[1] != "z-ai/glm-5.2" {
		t.Fatalf("Args = %v", spec.Args)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerOpenRouter || cfg.LastModel != "z-ai/glm-5.2" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestRelaunchWithSavedProviderCanStillGoBack(t *testing.T) {
	// Bare relaunch opens the provider picker first. The model picker must
	// still offer "back" after accepting the saved provider.
	withTestHome(t)
	if err := saveConfig(Config{
		Provider:         providerGemini,
		OpenRouterAPIKey: "sk-or-test",
		GeminiAPIKey:     "gm-test",
		LastGeminiModel:  "gemini-2.5-flash",
	}); err != nil {
		t.Fatal(err)
	}

	orSrv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 202752}})
	defer orSrv.Close()
	gmSrv := geminiTestServer(t, http.StatusOK)
	defer gmSrv.Close()

	var spec launchSpec
	stderr := &strings.Builder{}
	a := &app{
		// No flags: provider picker (Enter keeps Gemini) -> Gemini model
		// picker: b (back) -> provider picker -> 1 (OpenRouter) -> Enter.
		stdin:         strings.NewReader("\nb\n1\n\n"),
		stdout:        &strings.Builder{},
		stderr:        stderr,
		httpClient:    orSrv.Client(),
		apiBase:       orSrv.URL,
		geminiAPIBase: gmSrv.URL,
		runCommand: func(s launchSpec) error {
			spec = s
			return nil
		},
	}
	if err := a.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	out := stderr.String()
	for _, want := range []string{"Select a Gemini model", "b back", "Select a provider", "Select an OpenRouter model"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q: %q", want, out)
		}
	}
	if len(spec.Args) < 2 || spec.Args[0] != "--model" || spec.Args[1] != "z-ai/glm-5.2" {
		t.Fatalf("Args = %v", spec.Args)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerOpenRouter || cfg.LastModel != "z-ai/glm-5.2" || cfg.LastGeminiModel != "gemini-2.5-flash" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestModelPickerBackFromExplicitProviderFlag(t *testing.T) {
	// Back is available even when the provider was pinned by --provider: the
	// user is at an interactive picker and may change their mind.
	withTestHome(t)
	if err := saveConfig(Config{OpenRouterAPIKey: "sk-or-test", GeminiAPIKey: "gm-test"}); err != nil {
		t.Fatal(err)
	}
	orSrv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 202752}})
	defer orSrv.Close()
	gmSrv := geminiTestServer(t, http.StatusOK)
	defer gmSrv.Close()

	stderr := &strings.Builder{}
	a := &app{
		// --provider gemini -> model picker: b (back) -> provider picker: 1
		// (OpenRouter) -> Enter (first model).
		stdin:         strings.NewReader("b\n1\n\n"),
		stdout:        &strings.Builder{},
		stderr:        stderr,
		httpClient:    orSrv.Client(),
		apiBase:       orSrv.URL,
		geminiAPIBase: gmSrv.URL,
		runCommand:    func(launchSpec) error { return nil },
	}
	if err := a.run(context.Background(), []string{"--provider", "gemini"}); err != nil {
		t.Fatal(err)
	}
	out := stderr.String()
	for _, want := range []string{"Select a Gemini model", "Select a provider", "Select an OpenRouter model"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q: %q", want, out)
		}
	}
}

func openRouterTestServer(t *testing.T, keyStatus int, models []Model) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key":
			w.WriteHeader(keyStatus)
			fmt.Fprint(w, `{"data":{"label":"test"}}`)
		case "/models":
			fmt.Fprint(w, `{"data":[`)
			for i, m := range models {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"id":%q,"name":%q,"context_length":%d,"pricing":{"prompt":%q,"completion":%q}}`, m.ID, m.Name, m.ContextLength, m.PromptPrice, m.OutputPrice)
			}
			fmt.Fprint(w, `]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func envMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		out[k] = v
	}
	return out
}
