package simplerouter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func findClaude() (string, error) {
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	for _, fallback := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".claude", "local", name),
	} {
		if _, err := os.Stat(fallback); err == nil {
			return fallback, nil
		}
	}
	return "", fmt.Errorf("claude binary not found; install Claude Code first")
}

func buildClaudeEnv(base []string, baseURL, key, model string, contextLength int, disableThinking bool) []string {
	return buildClaudeEnvWithEffort(base, baseURL, key, model, contextLength, disableThinking, "")
}

func buildClaudeEnvWithEffort(base []string, baseURL, key, model string, contextLength int, disableThinking bool, effortLevel string) []string {
	claudeModel := claudeCodeModel(model, contextLength)
	env := envWithout(base,
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
		"CLAUDE_CODE_ATTRIBUTION_HEADER",
		"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION",
		"CLAUDE_CODE_DISABLE_THINKING",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"MAX_THINKING_TOKENS",
		"ENABLE_CLAUDEAI_MCP_SERVERS",
	)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAnthropicBaseURL
	}
	env = append(env,
		"ANTHROPIC_BASE_URL="+baseURL,
		"ANTHROPIC_AUTH_TOKEN="+key,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_DEFAULT_OPUS_MODEL="+claudeModel,
		"ANTHROPIC_DEFAULT_SONNET_MODEL="+claudeModel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL="+claudeModel,
		"CLAUDE_CODE_SUBAGENT_MODEL="+claudeModel,
		"CLAUDE_CODE_ATTRIBUTION_HEADER=0",
		// Disable Claude Code's "suggest what to type next" feature: with a
		// pinned model it re-sends the whole conversation just to predict the
		// next prompt, adding a full-context request per turn.
		"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false",
		// Claude Code cannot use claude.ai org connectors while API-key style
		// OpenRouter auth is taking precedence. Disable that connector fetch
		// for this child process so Claude does not show the connector warning.
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
	)
	if contextLength > 0 {
		env = append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.Itoa(contextLength))
	}
	if effort := strings.TrimSpace(effortLevel); effort != "" && !disableThinking {
		env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+effort)
	}
	if disableThinking {
		env = append(env,
			"CLAUDE_CODE_DISABLE_THINKING=1",
			"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1",
			"MAX_THINKING_TOKENS=0",
		)
	}
	return env
}

func claudeCodeModel(model string, contextLength int) string {
	if contextLength < 1_000_000 || strings.Contains(model, "[1m]") {
		return model
	}
	return model + "[1m]"
}

func envWithout(env []string, keys ...string) []string {
	blocked := make(map[string]bool, len(keys))
	for _, key := range keys {
		blocked[strings.ToUpper(key)] = true
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && blocked[strings.ToUpper(key)] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func claudeArgs(model string, positionals, passthrough []string) []string {
	args := []string{"--model", model}
	args = append(args, passthrough...)
	args = append(args, positionals...)
	return args
}
