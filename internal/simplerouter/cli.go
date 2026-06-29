package simplerouter

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

type app struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	httpClient *http.Client
	apiBase    string
	lineReader *bufio.Reader
	runCommand func(spec launchSpec) error
}

func Main(args []string) int {
	a := &app{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		httpClient: http.DefaultClient,
	}
	if err := a.run(context.Background(), args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "simplerouter:", err)
		return 1
	}
	return 0
}

func (a *app) run(ctx context.Context, args []string) error {
	var modelFlag string
	var selectModel bool
	var resetKey bool
	var disableThinking bool
	fs := flag.NewFlagSet("simplerouter", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.StringVar(&modelFlag, "model", "", "OpenRouter model id, name, or unique suffix")
	fs.BoolVar(&selectModel, "select-model", false, "Select a model from OpenRouter")
	fs.BoolVar(&resetKey, "reset-key", false, "Forget the saved OpenRouter API key before launching")
	fs.BoolVar(&disableThinking, "disable-thinking", false, "Disable Claude Code thinking/beta request features for provider compatibility")
	fs.Usage = func() {
		fmt.Fprintln(a.stderr, "Usage: simplerouter [--model MODEL] [--select-model] [--reset-key] [--disable-thinking] [path-or-prompt] [-- CLAUDE_ARGS...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	positionals, passthrough := splitPassthrough(fs.Args())
	dir, claudePositionals := resolveWorkingDir(positionals)

	if resetKey {
		if err := resetSavedKey(); err != nil {
			return err
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	style := newTerminalStyle(a.stderr)
	firstRun := modelFlag == "" && cfg.LastModel == ""
	if firstRun {
		printSetupBanner(a.stderr, style)
		fmt.Fprintln(a.stderr)
		fmt.Fprintln(a.stderr, style.header("simplerouter setup"))
		fmt.Fprintln(a.stderr, style.paint(clrDim, "Validate key, choose a model, then launch Claude Code."))
	}
	key, err := a.openRouterKey(ctx, cfg)
	if err != nil {
		return err
	}

	client := newOpenRouterClient(a.httpClient, a.apiBase)
	endpointsFn := func(id string) ([]Endpoint, error) { return openRouterEndpoints(ctx, client, key, id) }
	modelID := strings.TrimSpace(modelFlag)
	var res pickResult
	if selectModel || modelID == "" {
		if firstRun {
			fmt.Fprintln(a.stderr, style.paint(clrDim, "Fetching OpenRouter models..."))
		}
		models, err := openRouterModels(ctx, client, key)
		if err != nil {
			return err
		}
		current := cfg.LastModel
		if modelID != "" {
			current = modelID
		}
		res, err = a.pickModel(models, current, endpointsFn)
		if err != nil {
			return err
		}
	} else {
		res, err = a.resolveOpenRouterModel(ctx, client, key, modelID)
		if err != nil {
			return err
		}
	}
	selected := res.Model
	modelID = selected.ID

	cfg.OpenRouterAPIKey = key
	cfg.LastModel = modelID
	if err := saveConfig(cfg); err != nil {
		return err
	}

	claudePath, err := findClaude()
	if err != nil {
		return err
	}

	// When a provider is pinned, route Claude Code through a local proxy that
	// injects provider.only into each request body (the only way to pin a
	// provider, since Claude Code controls the body and OpenRouter ignores it
	// in the model string).
	baseURL := defaultAnthropicBaseURL
	if res.ProviderTag != "" {
		proxyURL, stop, perr := startProviderProxy(defaultAnthropicBaseURL, res.ProviderTag)
		if perr != nil {
			return fmt.Errorf("start provider proxy: %w", perr)
		}
		defer stop()
		baseURL = proxyURL
	}

	claudeModel := claudeCodeModel(modelID, selected.ContextLength)
	a.printLaunchSummary(modelID, claudeModel, selected.ContextLength, disableThinking, dir, res.ProviderName)
	spec := launchSpec{
		Path: claudePath,
		Dir:  dir,
		Args: claudeArgs(claudeModel, claudePositionals, passthrough),
		Env:  buildClaudeEnv(os.Environ(), baseURL, key, modelID, selected.ContextLength, disableThinking),
	}
	if a.runCommand != nil {
		return a.runCommand(spec)
	}
	return runClaudeCommand(spec)
}

func splitPassthrough(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return append([]string(nil), args[:i]...), append([]string(nil), args[i+1:]...)
		}
	}
	return append([]string(nil), args...), nil
}

func resolveWorkingDir(args []string) (string, []string) {
	if len(args) == 0 {
		return "", nil
	}
	first := args[0]
	if info, err := os.Stat(first); err == nil && info.IsDir() {
		if abs, err := filepath.Abs(first); err == nil {
			return abs, append([]string(nil), args[1:]...)
		}
		return first, append([]string(nil), args[1:]...)
	}
	return "", append([]string(nil), args...)
}

func (a *app) openRouterKey(ctx context.Context, cfg Config) (string, error) {
	client := newOpenRouterClient(a.httpClient, a.apiBase)
	if key := cleanAPIKey(os.Getenv("OPENROUTER_API_KEY")); key != "" {
		return key, nil
	}
	if cfg.OpenRouterAPIKey != "" {
		if err := validateOpenRouterKey(ctx, client, cfg.OpenRouterAPIKey); err == nil {
			return cfg.OpenRouterAPIKey, nil
		} else if errors.Is(err, errOpenRouterKeyRejected) {
			fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Saved OpenRouter API key is no longer valid."))
		} else {
			// Transient failure (network, timeout, 429, 5xx): the saved key is
			// probably fine — OpenRouter will reject it at request time if it
			// isn't. Don't claim the key is invalid; proceed optimistically.
			fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Could not reach OpenRouter to validate the saved key; using it anyway."))
			return cfg.OpenRouterAPIKey, nil
		}
	}
	key, err := a.promptAPIKey()
	if err != nil {
		return "", err
	}
	if err := validateOpenRouterKey(ctx, client, key); err != nil {
		if errors.Is(err, errOpenRouterKeyRejected) {
			return "", err
		}
		// Transient failure: proceed with the pasted key optimistically.
		fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Could not reach OpenRouter to validate the key; using it anyway."))
		return key, nil
	}
	return key, nil
}

func (a *app) promptAPIKey() (string, error) {
	style := newTerminalStyle(a.stderr)
	fmt.Fprintf(a.stderr, "%s %s ", style.paint(clrAccentBold, "❯"), style.paint(clrHead, "Paste your OpenRouter API key:"))
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", err
		}
		key := cleanAPIKey(string(data))
		if key == "" {
			return "", errors.New("OpenRouter API key is required")
		}
		return key, nil
	}
	line, err := a.readLine()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if line == "" {
		return "", errors.New("OpenRouter API key is required")
	}
	key := cleanAPIKey(line)
	if key == "" {
		return "", errors.New("OpenRouter API key is required")
	}
	return key, nil
}

func (a *app) resolveOpenRouterModel(ctx context.Context, client *openRouterClient, key, input string) (pickResult, error) {
	models, err := openRouterModels(ctx, client, key)
	if err != nil {
		// Don't silently pass the raw input through on a transient fetch
		// failure: the unverified string would be launched and persisted as
		// LastModel with no signal to the user. Surface the failure so the
		// caller aborts (or retries) rather than degrading to a passthrough.
		return pickResult{}, fmt.Errorf("could not reach OpenRouter to verify model %q: %w", input, err)
	}
	res, ok := resolveModel(input, models)
	if !ok {
		if yes, err := a.confirm(fmt.Sprintf("Model %q was not found in OpenRouter. Pass it through anyway?", input)); err != nil {
			return pickResult{}, err
		} else if !yes {
			return pickResult{}, errors.New("model selection cancelled")
		}
		return pickResult{Model: Model{ID: input}}, nil
	}
	if len(res.Ambiguous) > 0 {
		endpointsFn := func(id string) ([]Endpoint, error) { return openRouterEndpoints(ctx, client, key, id) }
		return a.pickModel(res.Ambiguous, input, endpointsFn)
	}
	return pickResult{Model: res.Model}, nil
}

const openRouterRequestTimeout = 30 * time.Second

func validateOpenRouterKey(ctx context.Context, client *openRouterClient, key string) error {
	ctx, cancel := context.WithTimeout(ctx, openRouterRequestTimeout)
	defer cancel()
	return client.validateKey(ctx, key)
}

func openRouterModels(ctx context.Context, client *openRouterClient, key string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, openRouterRequestTimeout)
	defer cancel()
	return client.models(ctx, key)
}

func openRouterEndpoints(ctx context.Context, client *openRouterClient, key, modelID string) ([]Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, openRouterRequestTimeout)
	defer cancel()
	return client.endpoints(ctx, key, modelID)
}

func filterModels(models []Model, filter string) []Model {
	filter = normalizeModelText(filter)
	if filter == "" {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if strings.Contains(normalizeModelText(m.ID), filter) || strings.Contains(normalizeModelText(m.Name), filter) {
			out = append(out, m)
		}
	}
	return out
}

func (a *app) confirm(prompt string) (bool, error) {
	fmt.Fprintf(a.stderr, "%s [y/N] ", prompt)
	line, err := a.readLine()
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func (a *app) readLine() (string, error) {
	if a.lineReader == nil {
		a.lineReader = bufio.NewReader(a.stdin)
	}
	line, err := a.lineReader.ReadString('\n')
	return strings.TrimSpace(line), err
}

func (a *app) printLaunchSummary(modelID, claudeModel string, contextLength int, disableThinking bool, dir, providerName string) {
	launchDir := dir
	if launchDir == "" {
		if wd, err := os.Getwd(); err == nil {
			launchDir = wd
		} else {
			launchDir = "."
		}
	}
	thinking := "default"
	if disableThinking {
		thinking = "disabled"
	}
	style := newTerminalStyle(a.stderr)
	sep := style.paint(clrFaint, "|")
	fmt.Fprintf(a.stderr, "%s model %s %s claude %s %s context %s %s thinking %s %s dir %s",
		style.paint(clrAccentBold, "Launching Claude Code:"),
		style.paint(clrModelHi, modelID),
		sep,
		style.paint(clrModel, claudeModel),
		sep,
		style.paint(ctxColor(contextLength), formatContextLength(contextLength)),
		sep,
		style.paint(clrDim, thinking),
		sep,
		style.paint(clrDim, launchDir),
	)
	if providerName != "" {
		fmt.Fprintf(a.stderr, " %s provider %s", sep, style.paint(clrModelHi, providerName))
	}
	fmt.Fprintln(a.stderr)
}
