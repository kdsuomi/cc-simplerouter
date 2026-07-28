package simplerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	codexAPIKeyEnv    = "SIMPLEROUTER_CODEX_API_KEY"
	codexProviderName = "simplerouter_session"
)

type codexModelCatalog struct {
	Models []map[string]any `json:"models"`
}

func findCodex() (string, error) {
	for _, name := range []string{"codex", "codex.cmd"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}

	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	var fallbacks []string
	if runtime.GOOS == "windows" {
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			fallbacks = append(fallbacks, filepath.Join(appData, "npm", "codex.cmd"))
		}
		fallbacks = append(fallbacks,
			filepath.Join(home, "AppData", "Roaming", "npm", "codex.cmd"),
			filepath.Join(home, ".local", "bin", "codex.exe"),
		)
	} else {
		fallbacks = append(fallbacks,
			filepath.Join(home, ".local", "bin", "codex"),
			"/usr/local/bin/codex",
		)
	}
	for _, path := range fallbacks {
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("codex binary not found; install the OpenAI Codex CLI first")
}

// prepareCodexModelCatalog derives a one-model catalog from the installed
// Codex CLI. Reusing Codex's bundled descriptor preserves its current system
// prompt, apply_patch freeform tool, and parallel-tool support for model ids
// that Codex does not know natively.
func prepareCodexModelCatalog(codexPath string, model Model, supportsReasoning bool) (path string, cleanup func(), err error) {
	cmd := exec.Command(codexPath, "debug", "models", "--bundled")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", nil, fmt.Errorf("read Codex bundled model catalog: %w: %s", err, detail)
		}
		return "", nil, fmt.Errorf("read Codex bundled model catalog: %w", err)
	}

	catalog, err := buildCodexModelCatalog(stdout.Bytes(), model, supportsReasoning)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "simplerouter-codex-")
	if err != nil {
		return "", nil, fmt.Errorf("create Codex session directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, catalog, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write Codex model catalog: %w", err)
	}
	return path, cleanup, nil
}

func buildCodexModelCatalog(raw []byte, model Model, supportsReasoning bool) ([]byte, error) {
	var source codexModelCatalog
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("parse Codex bundled model catalog: %w", err)
	}
	if len(source.Models) == 0 {
		return nil, fmt.Errorf("Codex bundled model catalog is empty")
	}

	var template map[string]any
	for _, candidate := range source.Models {
		if candidate["apply_patch_tool_type"] == "freeform" &&
			boolValue(candidate["supports_parallel_tool_calls"]) &&
			candidate["tool_mode"] == nil {
			template = candidate
			break
		}
	}
	if template == nil {
		for _, candidate := range source.Models {
			if candidate["apply_patch_tool_type"] == "freeform" && boolValue(candidate["supports_parallel_tool_calls"]) {
				template = candidate
				break
			}
		}
	}
	if template == nil {
		return nil, fmt.Errorf("Codex bundled model catalog has no full-featured tool model")
	}
	encoded, err := json.Marshal(template)
	if err != nil {
		return nil, fmt.Errorf("copy Codex model descriptor: %w", err)
	}
	var routed map[string]any
	if err := json.Unmarshal(encoded, &routed); err != nil {
		return nil, fmt.Errorf("copy Codex model descriptor: %w", err)
	}

	modelID := strings.TrimSpace(model.ID)
	if modelID == "" {
		return nil, fmt.Errorf("selected model id is empty")
	}
	displayName := strings.TrimSpace(model.Name)
	if displayName == "" {
		displayName = modelID
	}
	contextWindow := model.ContextLength
	if contextWindow <= 0 {
		contextWindow = intValue(routed["context_window"])
	}
	if contextWindow <= 0 {
		contextWindow = 272_000
	}

	routed["slug"] = modelID
	routed["display_name"] = displayName
	routed["description"] = "Routed by simplerouter"
	routed["priority"] = 1
	routed["visibility"] = "list"
	routed["supported_in_api"] = true
	routed["context_window"] = contextWindow
	routed["max_context_window"] = contextWindow
	routed["apply_patch_tool_type"] = "freeform"
	routed["supports_parallel_tool_calls"] = true
	routed["tool_mode"] = nil
	routed["multi_agent_version"] = "v2"
	routed["use_responses_lite"] = false
	routed["upgrade"] = nil
	routed["service_tiers"] = []any{}
	routed["additional_speed_tiers"] = []any{}
	if !supportsReasoning {
		routed["default_reasoning_level"] = nil
		routed["supported_reasoning_levels"] = []any{}
		routed["default_reasoning_summary"] = "none"
	}

	out, err := json.MarshalIndent(codexModelCatalog{Models: []map[string]any{routed}}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Codex model catalog: %w", err)
	}
	return append(out, '\n'), nil
}

func buildCodexEnv(base []string, key string) []string {
	env := envWithout(base, codexAPIKeyEnv)
	return append(env, codexAPIKeyEnv+"="+key)
}

func codexArgs(model, baseURL, catalogPath string, disableThinking bool, positionals, passthrough []string) []string {
	provider := fmt.Sprintf(
		`{ name = %s, base_url = %s, env_key = %s, wire_api = "responses", requires_openai_auth = false }`,
		tomlString("simplerouter"),
		tomlString(strings.TrimRight(baseURL, "/")),
		tomlString(codexAPIKeyEnv),
	)
	args := []string{
		"--model", model,
		"-c", "model_provider=" + tomlString(codexProviderName),
		"-c", "model_providers." + codexProviderName + "=" + provider,
		"-c", "model_catalog_json=" + tomlString(catalogPath),
	}
	if disableThinking {
		args = append(args,
			"-c", `model_reasoning_effort="none"`,
			"-c", `model_reasoning_summary="none"`,
		)
	}
	args = append(args, passthrough...)
	if prompt := strings.TrimSpace(strings.Join(positionals, " ")); prompt != "" {
		args = append(args, prompt)
	}
	return args
}

func tomlString(value string) string {
	return strconv.Quote(value)
}

func boolValue(value any) bool {
	got, _ := value.(bool)
	return got
}

func intValue(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		got, _ := strconv.Atoi(n.String())
		return got
	default:
		return 0
	}
}
