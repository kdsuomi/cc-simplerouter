package simplerouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAbliterationConfigRoundTripAndReset(t *testing.T) {
	withTestHome(t)
	cfg := Config{Provider: providerAbliteration, AbliterationAPIKey: " \"ak-test\" ", LastAbliterationModel: " abliterated-model-large-flash ", MiMoAPIKey: "other-key", LastModel: "other/model"}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.AbliterationAPIKey != "ak-test" || got.LastAbliterationModel != "abliterated-model-large-flash" || got.Provider != providerAbliteration || got.MiMoAPIKey != "other-key" {
		t.Fatal("Abliteration config did not round trip")
	}
	if err := resetSavedKey(); err != nil {
		t.Fatal(err)
	}
	reset, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got.AbliterationAPIKey, got.MiMoAPIKey = "", ""
	if reset != got {
		t.Fatal("key reset must retain model selections")
	}
}

func TestAbliterationEnvironmentKeyPrecedence(t *testing.T) {
	t.Setenv("ABLITERATION_API_KEY", " \"primary\" ")
	t.Setenv("ABLIT_KEY", "fallback")
	a := &app{}
	key, err := a.abliterationKey(context.Background(), Config{AbliterationAPIKey: "saved"})
	if err != nil || key != "primary" {
		t.Fatal("ABLITERATION_API_KEY must take precedence")
	}
	t.Setenv("ABLITERATION_API_KEY", "")
	key, err = a.abliterationKey(context.Background(), Config{AbliterationAPIKey: "saved"})
	if err != nil || key != "fallback" {
		t.Fatal("ABLIT_KEY must be accepted")
	}
}

func TestAbliterationModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer ak-test" {
			http.Error(w, "bad discovery request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"data":[
			{"id":"abliterated-model-large-flash","display_name":"Live Flash","context_length":1048576,"pricing":{"prompt":"0.000001","completion":"0.000002"},"access":{"available":true},"is_ready":true},
			{"id":"abliterated-model-large-v2","name":"Large v2"},
			{"id":"locked","access":{"locked":true}},
			{"id":"unavailable","access":{"available":false}},
			{"id":"not-ready","is_ready":false},
			{"id":""},
			{"id":"abliterated-model-large-flash"}
		]}`)
	}))
	defer srv.Close()
	models, err := abliterationModels(context.Background(), srv.Client(), srv.URL+"/v1/", "ak-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	flash := models[0]
	if flash.Name != "Live Flash" || flash.ContextLength != 1_048_576 || flash.AutoCompactTokenLimit != 838_860 || flash.PromptPrice != "0.000001" || flash.OutputPrice != "0.000002" {
		t.Fatalf("model metadata = %+v", flash)
	}
	if models[1].ContextLength != 1_000_000 || models[1].DefaultReasoningEffort != "xhigh" || slices.Contains(models[1].SupportedReasoningEfforts, "none") {
		t.Fatalf("Large v2 metadata = %+v", models[1])
	}
	for _, model := range curatedProviderModels(providerAbliteration) {
		if inferProviderFromModel(model.ID) != providerAbliteration || !modelSupportsReasoning(model) {
			t.Fatalf("provider inference or reasoning missing for %s", model.ID)
		}
	}
	if inferProviderFromModel("abliteration/abliterated-model") != providerOpenRouter || !isKnownProvider(canonicalProvider(" Abliteration.AI ")) || !slices.Contains(providerNames(), providerAbliteration) {
		t.Fatal("provider registry is incomplete")
	}
	for _, opt := range providerOptions() {
		if opt.ID == providerAbliteration {
			return
		}
	}
	t.Fatal("Abliteration missing from provider picker")
}

func TestAbliterationModelsErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{401, "", "rejected"}, {403, "", "rejected"}, {429, "", "HTTP 429"},
		{500, "", "HTTP 500"}, {200, "invalid", "decode"}, {200, `{"data":[]}`, "no available models"},
	} {
		t.Run(fmt.Sprint(tc.status, tc.want), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			_, err := abliterationModels(context.Background(), srv.Client(), srv.URL, "ak-test")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			if (tc.status == 401 || tc.status == 403) && !errors.Is(err, errProviderKeyRejected) {
				t.Fatal("missing key rejection sentinel")
			}
		})
	}
}

func TestAbliterationLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, model, input string
		args               []string
		disabled           bool
	}{
		{"inferred", "abliterated-model", "", []string{"--model", "abliterated-model"}, false},
		{"explicit", "abliterated-model-large-flash", "", []string{"--provider", "abliteration", "--model", "abliterated-model-large-flash"}, false},
		{"disabled", "abliterated-model-large-v2", "", []string{"--model", "abliterated-model-large-v2", "--disable-thinking"}, true},
		{"saved picker", "abliterated-model-large-flash", "\n\n", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := withTestHome(t)
			t.Setenv("ABLITERATION_API_KEY", "")
			t.Setenv("ABLIT_KEY", "")
			if err := saveConfig(Config{Provider: providerAbliteration, AbliterationAPIKey: "ak-test", LastAbliterationModel: tc.model, MiMoAPIKey: "other-key"}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer ak-test" {
					http.Error(w, "bad validation request", http.StatusBadRequest)
					return
				}
				fmt.Fprintf(w, `{"data":[{"id":%q}]}`, tc.model)
			}))
			defer srv.Close()
			started, stopped := false, false
			startResponsesPassthroughProxyFn = func(base, model string, _ *http.Client, opts responsesPassthroughOptions) (string, func(), error) {
				if base != srv.URL+"/v1" || model != tc.model || opts.Label != "Abliteration" || !opts.TranslateCustomTools || !opts.FlattenNamespaces {
					t.Fatal("incorrect proxy configuration")
				}
				started = true
				return "http://127.0.0.1:9898/v1", func() { stopped = true }, nil
			}
			var spec launchSpec
			stderr := &strings.Builder{}
			a := &app{stdin: strings.NewReader(tc.input), stdout: &strings.Builder{}, stderr: stderr, httpClient: srv.Client(), abliterationAPIBase: srv.URL + "/v1", runCommand: func(s launchSpec) error { spec = s; return nil }}
			if err := a.run(context.Background(), tc.args); err != nil {
				t.Fatal(err)
			}
			model := abliterationModelDefault(tc.model, tc.model, 262_144)
			for _, known := range abliterationModelDefaults() {
				if known.ID == tc.model {
					model = known
				}
			}
			want := reasoningAwareCodexArgs(model, "http://127.0.0.1:9898/v1", filepath.Join(home, "models.json"), tc.disabled, nil, nil)
			if !started || !stopped || !slices.Equal(spec.Args, want) || envMap(spec.Env)[codexAPIKeyEnv] != "ak-test" {
				t.Fatal("proxy lifecycle or Codex launch configuration is incorrect")
			}
			if !strings.Contains(stderr.String(), "provider Abliteration") || strings.Contains(stderr.String(), "ak-test") {
				t.Fatal("launch summary must identify Abliteration without exposing the key")
			}
			if tc.disabled && !strings.Contains(stderr.String(), "low") {
				t.Fatal("Large v2 disabled-thinking summary must report low reasoning")
			}
			cfg, err := loadConfig()
			if err != nil || cfg.Provider != providerAbliteration || cfg.LastAbliterationModel != tc.model || cfg.AbliterationAPIKey != "ak-test" || cfg.MiMoAPIKey != "other-key" {
				t.Fatal("provider selection was not saved correctly")
			}
		})
	}
}

func TestAbliterationResponsesCompatibility(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{
		{"abliterated-model", "max", "xhigh"},
		{"abliterated-model", "ultra", "xhigh"},
		{"abliterated-model-large-v2", "xhigh", "max"},
		{"abliterated-model-large-flash", "xhigh", "max"},
		{"abliterated-model-large", "ultra", "max"},
		{"abliterated-model-large-v2", "none", "none"},
	} {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			captured := make(chan map[string]any, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer ak-test" {
					http.Error(w, "bad upstream request", http.StatusBadRequest)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				captured <- body
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"name\":\"test__word\",\"call_id\":\"c1\",\"arguments\":\"{}\"}}\n\n")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer upstream.Close()
			proxy := httptest.NewServer(newResponsesPassthroughProxy(upstream.URL, tc.model, upstream.Client(), abliterationResponsesOptions(tc.model)))
			defer proxy.Close()
			body := fmt.Sprintf(`{"model":"ignored","input":[],"stream":true,"prompt_cache_key":"stable","reasoning":{"effort":%q,"summary":"none"},"tools":[{"type":"custom","name":"apply_patch"},{"type":"namespace","name":"test","tools":[{"type":"function","name":"word","parameters":{"type":"object","properties":{}}}]},{"type":"web_search"},{"type":"tool_search"}]}`, tc.effort)
			req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer ak-test")
			resp, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			stream, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, stream)
			}
			got := <-captured
			if got["model"] != tc.model || got["reasoning"].(map[string]any)["effort"] != tc.want || got["prompt_cache_key"] != "stable" {
				t.Fatalf("upstream controls = %#v", got)
			}
			if _, found := got["reasoning"].(map[string]any)["summary"]; found {
				t.Fatal("Codex summary=none must be omitted for Abliteration")
			}
			tools := got["tools"].([]any)
			if len(tools) != 3 || tools[0].(map[string]any)["type"] != "function" || tools[1].(map[string]any)["name"] != "test__word" || tools[2].(map[string]any)["type"] != "web_search" {
				t.Fatalf("translated tools = %#v", tools)
			}
			events := decodeTestSSE(t, string(stream))
			item := events[0]["item"].(map[string]any)
			if item["name"] != "word" || item["namespace"] != "test" {
				t.Fatalf("tool identity not restored: %#v", item)
			}
		})
	}
}
