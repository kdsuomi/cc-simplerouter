package simplerouter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const configDirName = ".simplerouter"

var userHomeDir = os.UserHomeDir

func configPath() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDirName, "config.json"), nil
}

func loadConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	// An empty or whitespace-only file (e.g. a write interrupted mid-save)
	// is treated as first-run rather than a fatal parse error.
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.OpenRouterAPIKey = cleanAPIKey(cfg.OpenRouterAPIKey)
	cfg.GeminiAPIKey = cleanAPIKey(cfg.GeminiAPIKey)
	cfg.OpenAIAPIKey = cleanAPIKey(cfg.OpenAIAPIKey)
	cfg.DeepSeekAPIKey = cleanAPIKey(cfg.DeepSeekAPIKey)
	cfg.ZAIAPIKey = cleanAPIKey(cfg.ZAIAPIKey)
	cfg.MetaAPIKey = cleanAPIKey(cfg.MetaAPIKey)
	cfg.LastModel = strings.TrimSpace(cfg.LastModel)
	cfg.LastGeminiModel = strings.TrimSpace(cfg.LastGeminiModel)
	cfg.LastOpenAIModel = strings.TrimSpace(cfg.LastOpenAIModel)
	cfg.LastDeepSeekModel = strings.TrimSpace(cfg.LastDeepSeekModel)
	cfg.LastZAIModel = strings.TrimSpace(cfg.LastZAIModel)
	cfg.LastMetaModel = strings.TrimSpace(cfg.LastMetaModel)
	// A hand-edited unknown provider must not break launch: fall back to the
	// default (OpenRouter) rather than erroring.
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if !isKnownProvider(cfg.Provider) {
		cfg.Provider = ""
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Write to a temp file and rename so an interrupted save can't leave a
	// truncated/empty config behind. Rename replaces the target on both
	// POSIX and Windows.
	tmp, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// resetSavedKey forgets all saved API keys. It runs before the provider is
// determined, so clearing both is the honest semantic; only the provider used
// next will re-prompt.
func resetSavedKey() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.OpenRouterAPIKey = ""
	cfg.GeminiAPIKey = ""
	cfg.OpenAIAPIKey = ""
	cfg.DeepSeekAPIKey = ""
	cfg.ZAIAPIKey = ""
	cfg.MetaAPIKey = ""
	return saveConfig(cfg)
}

func cleanAPIKey(key string) string {
	key = strings.ReplaceAll(key, "\x00", "")
	key = strings.TrimPrefix(key, "\ufeff")
	key = strings.TrimSpace(key)
	key = strings.Trim(key, `"'`)
	return strings.TrimSpace(key)
}
