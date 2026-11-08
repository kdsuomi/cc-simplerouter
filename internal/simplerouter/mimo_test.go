package simplerouter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMiMoConfigRoundTripAndReset(t *testing.T) {
	withTestHome(t)
	cfg := Config{
		Provider: providerMiMo, MiMoAPIKey: " \"mimo-test\" ",
		LastMiMoModel: " mimo-v2.6-pro-ultraspeed ", OpenRouterAPIKey: "other-key",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != providerMiMo || got.MiMoAPIKey != "mimo-test" || got.LastMiMoModel != "mimo-v2.6-pro-ultraspeed" || got.OpenRouterAPIKey != "other-key" {
		t.Fatalf("config = %+v", got)
	}
	if err := resetSavedKey(); err != nil {
		t.Fatal(err)
	}
	reset, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got.MiMoAPIKey, got.OpenRouterAPIKey = "", ""
	if reset != got {
		t.Fatalf("reset config = %+v, want %+v", reset, got)
	}
}

func TestMiMoCatalogAndModelResolution(t *testing.T) {
	models := curatedProviderModels(providerMiMo)
	want := []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed"}
	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
		if model.ContextLength != 1_000_000 || !modelSupportsReasoning(model) || !slices.Contains(model.SupportedParameters, "tools") {
			t.Fatalf("MiMo model metadata = %+v", model)
		}
		if inferProviderFromModel(model.ID) != providerMiMo {
			t.Fatalf("provider inference failed for %s", model.ID)
		}
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("MiMo catalog = %v, want %v", ids, want)
	}
	if inferProviderFromModel("xiaomi/mimo-v2.6-pro") != providerOpenRouter {
		t.Fatal("slash-qualified MiMo ID should still use OpenRouter")
	}
	if !slices.Contains(providerNames(), providerMiMo) || !isKnownProvider(canonicalProvider(" MiMo ")) {
		t.Fatal("MiMo missing from provider registry")
	}
	for _, opt := range providerOptions() {
		if opt.ID == providerMiMo {
			return
		}
	}
	t.Fatal("MiMo missing from provider picker")
}

func TestMiMoRejectsUnsupportedModels(t *testing.T) {
	a := &app{stdin: strings.NewReader("yes\n"), stderr: &strings.Builder{}}
	for _, id := range []string{"mimo-v2-flash", "mimo-v2.5", "mimo-v2.5-pro", "mimo-v2.5-tts", "unknown"} {
		_, _, err := a.selectMiMo(context.Background(), Config{}, id, false, false, terminalStyle{})
		if err == nil || !strings.Contains(err.Error(), "unsupported MiMo model") {
			t.Fatalf("selectMiMo(%q) error = %v", id, err)
		}
	}
}

func TestMiMoLaunchesThroughResponsesProxy(t *testing.T) {
	for _, tc := range []struct {
		name, model, input string
		args               []string
		disabled           bool
	}{
		{"inferred", "mimo-v2.6-flash", "", []string{"--model", "mimo-v2.6-flash"}, false},
		{"explicit", "mimo-v2.6-pro", "", []string{"--provider", "mimo", "--model", "mimo-v2.6-pro"}, false},
		{"ultraspeed", "mimo-v2.6-pro-ultraspeed", "", []string{"--model", "mimo-v2.6-pro-ultraspeed", "--disable-thinking"}, true},
		{"saved picker", "mimo-v2.6-pro-ultraspeed", "\n\n", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := withTestHome(t)
			t.Setenv("MIMO_API_KEY", "")
			if err := saveConfig(Config{Provider: providerMiMo, MiMoAPIKey: "mimo-test", LastMiMoModel: tc.model, LastModel: "other/model"}); err != nil {
				t.Fatal(err)
			}
			validated := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer mimo-test" {
					http.Error(w, "bad key validation request", http.StatusBadRequest)
					return
				}
				validated = true
				fmt.Fprint(w, `{"data":[]}`)
			}))
			defer srv.Close()
			stopped, started := false, false
			oldProxy := startMiMoResponsesProxyFn
			startMiMoResponsesProxyFn = func(base, model string, _ *http.Client, disabled bool) (string, func(), error) {
				if base != srv.URL+"/v1" || model != tc.model || disabled != tc.disabled {
					t.Fatalf("proxy got (%q, %q, %v)", base, model, disabled)
				}
				started = true
				return "http://127.0.0.1:9898/v1", func() { stopped = true }, nil
			}
			t.Cleanup(func() { startMiMoResponsesProxyFn = oldProxy })
			var spec launchSpec
			stderr := &strings.Builder{}
			a := &app{
				stdin: strings.NewReader(tc.input), stdout: &strings.Builder{}, stderr: stderr,
				httpClient: srv.Client(), mimoAPIBase: srv.URL + "/v1",
				runCommand: func(s launchSpec) error { spec = s; return nil },
			}
			if err := a.run(context.Background(), tc.args); err != nil {
				t.Fatal(err)
			}
			if !validated || !started || !stopped {
				t.Fatalf("validation/proxy lifecycle = %v/%v/%v", validated, started, stopped)
			}
			wantArgs := codexArgs(tc.model, "http://127.0.0.1:9898/v1", filepath.Join(home, "models.json"), tc.disabled, nil, nil)
			if !slices.Equal(spec.Args, wantArgs) || envMap(spec.Env)[codexAPIKeyEnv] != "mimo-test" {
				t.Fatal("MiMo model, proxy URL, thinking mode, or session key not passed to Codex")
			}
			if !strings.Contains(stderr.String(), "provider MiMo") || strings.Contains(stderr.String(), "mimo-test") {
				t.Fatal("launch summary must identify MiMo without exposing the key")
			}
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Provider != providerMiMo || cfg.LastMiMoModel != tc.model || cfg.MiMoAPIKey != "mimo-test" || cfg.LastModel != "other/model" {
				t.Fatalf("saved config = %+v", cfg)
			}
		})
	}
}

func TestMiMoEnvironmentKeyTakesPrecedence(t *testing.T) {
	t.Setenv("MIMO_API_KEY", " \"environment-key\" ")
	a := &app{}
	key, err := a.mimoKey(context.Background(), Config{MiMoAPIKey: "saved-key"})
	if err != nil || key != "environment-key" {
		t.Fatalf("MiMo key = %q, error = %v", key, err)
	}
}

func TestMiMoResponsesRequestAndReplay(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			body := captureProviderChatRequest(t, "mimo-v2.6-pro-ultraspeed", mimoResponsesOptions(disabled), "ultra")
			wantThinking := "enabled"
			if disabled {
				wantThinking = "disabled"
			}
			if body["model"] != "mimo-v2.6-pro-ultraspeed" || body["thinking"].(map[string]any)["type"] != wantThinking || body["max_completion_tokens"] != float64(2048) {
				t.Fatalf("MiMo request = %#v", body)
			}
			for _, field := range []string{"reasoning_effort", "tool_stream", "stream_options"} {
				if _, ok := body[field]; ok {
					t.Fatalf("MiMo request includes unsupported field %q", field)
				}
			}
			assertProviderReasoningReplay(t, body)
		})
	}
}
