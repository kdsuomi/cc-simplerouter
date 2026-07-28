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
	userHomeDir = func() (string, error) { return dir, nil }
	findCodexFn = func() (string, error) { return filepath.Join(dir, "codex-test"), nil }
	prepareCodexModelCatalogFn = func(_ string, _ Model, _ bool) (string, func(), error) {
		return filepath.Join(dir, "models.json"), func() {}, nil
	}
	t.Cleanup(func() {
		userHomeDir = old
		findCodexFn = oldFindCodex
		prepareCodexModelCatalogFn = oldPrepareCatalog
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

func TestOpenRouterLaunchUsesCodexDirectly(t *testing.T) {
	home := withTestHome(t)
	if err := saveConfig(Config{OpenRouterAPIKey: "sk-or-test"}); err != nil {
		t.Fatal(err)
	}

	srv := openRouterTestServer(t, http.StatusOK, []Model{{ID: "z-ai/glm-5.2", Name: "GLM 5.2", ContextLength: 202752}})
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
	if spec.Path != filepath.Join(home, "codex-test") {
		t.Fatalf("Codex path = %q", spec.Path)
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
	if got := launchThinkingMode(providerOpenRouter, false); got != "provider default" {
		t.Fatalf("OpenRouter reasoning mode = %q", got)
	}
	if got := launchThinkingMode(providerZAI, false); got != "provider default" {
		t.Fatalf("Z.AI reasoning mode = %q", got)
	}
	if got := launchThinkingMode(providerOpenRouter, true); got != "disabled" {
		t.Fatalf("disabled reasoning mode = %q", got)
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
		LastModel:         "z-ai/glm-5.2",
		LastGeminiModel:   "gemini-2.5-flash",
		LastOpenAIModel:   "gpt-5.5",
		LastDeepSeekModel: "deepseek-v4-flash",
		LastZAIModel:      "glm-5.2",
		LastMetaModel:     "muse-spark-1.1",
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
	if got.OpenRouterAPIKey != "" || got.GeminiAPIKey != "" || got.OpenAIAPIKey != "" || got.DeepSeekAPIKey != "" || got.ZAIAPIKey != "" || got.MetaAPIKey != "" {
		t.Fatalf("keys not cleared: %+v", got)
	}
	if got.Provider != providerGemini || got.LastModel != cfg.LastModel || got.LastGeminiModel != cfg.LastGeminiModel || got.LastOpenAIModel != cfg.LastOpenAIModel || got.LastDeepSeekModel != cfg.LastDeepSeekModel || got.LastZAIModel != cfg.LastZAIModel || got.LastMetaModel != cfg.LastMetaModel {
		t.Fatalf("non-key fields changed: %+v", got)
	}
}

func TestInferProviderFromModel(t *testing.T) {
	cases := map[string]string{
		"z-ai/glm-5.2":          providerOpenRouter,
		"gemini-2.5-flash":      providerGemini,
		"models/gemini-2.5-pro": providerGemini,
		"models/other-model":    providerGemini,
		"glm-5.2":               providerZAI,
		"muse-spark-1.1":        providerMeta,
		"":                      "",
	}
	for input, want := range cases {
		if got := inferProviderFromModel(input); got != want {
			t.Errorf("inferProviderFromModel(%q) = %q, want %q", input, got, want)
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
		stdin:      strings.NewReader("5\n\n"), // Z.AI, first model
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
