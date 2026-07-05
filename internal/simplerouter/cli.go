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

// Backend providers Claude Code can be launched against.
const (
	providerOpenRouter = "openrouter"
	providerGemini     = "gemini"
	providerOpenAI     = "openai"
	providerDeepSeek   = "deepseek"
	providerZAI        = "zai"
)

type app struct {
	stdin           io.Reader
	stdout          io.Writer
	stderr          io.Writer
	httpClient      *http.Client
	apiBase         string // OpenRouter API base override (tests)
	geminiAPIBase   string // Gemini API base override (tests)
	openAIAPIBase   string // OpenAI API base override (tests)
	deepSeekAPIBase string // DeepSeek API base override (tests)
	zaiAPIBase      string // Z.AI API base override (tests)
	lineReader      *bufio.Reader
	runCommand      func(spec launchSpec) error
}

// startGeminiProxyFn is a seam so tests can stub the translating proxy.
var startGeminiProxyFn = startGeminiProxy
var startOpenAIProxyFn = startOpenAIProxy
var startZAIProxyFn = startZAIProxy

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
	var providerFlag string
	var selectModel bool
	var resetKey bool
	var disableThinking bool
	fs := flag.NewFlagSet("simplerouter", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.StringVar(&modelFlag, "model", "", "Model id, name, or unique suffix")
	fs.StringVar(&providerFlag, "provider", "", `Model provider: "openrouter", "gemini", "openai", "deepseek", or "zai"`)
	fs.BoolVar(&selectModel, "select-model", false, "Select a provider and model interactively")
	fs.BoolVar(&resetKey, "reset-key", false, "Forget the saved API keys before launching")
	fs.BoolVar(&disableThinking, "disable-thinking", false, "Disable Claude Code thinking/beta request features for provider compatibility")
	fs.Usage = func() {
		fmt.Fprintln(a.stderr, "Usage: simplerouter [--model MODEL] [--provider PROVIDER] [--select-model] [--reset-key] [--disable-thinking] [path-or-prompt] [-- CLAUDE_ARGS...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	positionals, passthrough := splitPassthrough(fs.Args())
	dir, claudePositionals := resolveWorkingDir(positionals)
	bareCommand := modelFlag == "" && providerFlag == "" && !selectModel && len(positionals) == 0 && len(passthrough) == 0

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
	firstRun := modelFlag == "" && cfg.Provider == "" && cfg.LastModel == "" && cfg.LastGeminiModel == "" && cfg.LastOpenAIModel == "" && cfg.LastDeepSeekModel == "" && cfg.LastZAIModel == ""
	if firstRun {
		printSetupBanner(a.stderr, style)
		fmt.Fprintln(a.stderr)
		fmt.Fprintln(a.stderr, style.header("simplerouter setup"))
		fmt.Fprintln(a.stderr, style.paint(clrDim, "Choose a provider, validate key, choose a model, then launch Claude Code."))
	}

	provider, err := a.determineProvider(cfg, modelFlag, providerFlag, selectModel, firstRun, bareCommand)
	if err != nil {
		return err
	}

	// Backing out of the model picker (ESC / "b") always returns here to
	// re-choose the provider — even when the provider came from a flag,
	// saved config, or --model inference.
	var key string
	var res pickResult
	for {
		switch provider {
		case providerGemini:
			key, res, err = a.selectGemini(ctx, cfg, modelFlag, selectModel, firstRun, style)
		case providerOpenAI:
			key, res, err = a.selectOpenAI(ctx, cfg, modelFlag, selectModel, firstRun, style)
		case providerDeepSeek:
			key, res, err = a.selectDeepSeek(ctx, cfg, modelFlag, selectModel, firstRun, style)
		case providerZAI:
			key, res, err = a.selectZAI(ctx, cfg, modelFlag, selectModel, firstRun, style)
		default:
			key, res, err = a.selectOpenRouter(ctx, cfg, modelFlag, selectModel, firstRun, style)
		}
		if errors.Is(err, errPickerBack) {
			opt, perr := a.pickOne("Select a provider", providerOptions(), provider)
			if perr != nil {
				return perr
			}
			provider = opt.ID
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	selected := res.Model
	modelID := selected.ID

	cfg.Provider = provider
	switch provider {
	case providerGemini:
		cfg.GeminiAPIKey = key
		cfg.LastGeminiModel = modelID
	case providerOpenAI:
		cfg.OpenAIAPIKey = key
		cfg.LastOpenAIModel = modelID
	case providerDeepSeek:
		cfg.DeepSeekAPIKey = key
		cfg.LastDeepSeekModel = modelID
	case providerZAI:
		cfg.ZAIAPIKey = key
		cfg.LastZAIModel = modelID
	default:
		cfg.OpenRouterAPIKey = key
		cfg.LastModel = modelID
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}

	claudePath, err := findClaude()
	if err != nil {
		return err
	}

	baseURL := defaultAnthropicBaseURL
	effortLevel := ""
	switch {
	case provider == providerGemini:
		// Gemini has no Anthropic-compatible endpoint, so Claude Code talks to
		// a local proxy that translates Anthropic Messages <-> generateContent.
		// The key rides on ANTHROPIC_AUTH_TOKEN and comes back to the proxy as
		// the Authorization header.
		proxyURL, stop, perr := startGeminiProxyFn(a.geminiBase(), modelID, a.httpClient)
		if perr != nil {
			return fmt.Errorf("start gemini proxy: %w", perr)
		}
		defer stop()
		baseURL = proxyURL
	case provider == providerOpenAI:
		proxyURL, stop, perr := startOpenAIProxyFn(a.openAIBase(), modelID, a.httpClient)
		if perr != nil {
			return fmt.Errorf("start OpenAI proxy: %w", perr)
		}
		defer stop()
		baseURL = proxyURL
	case provider == providerDeepSeek:
		baseURL = a.deepSeekAnthropicBase()
		effortLevel = "max"
	case provider == providerZAI:
		proxyURL, stop, perr := startZAIProxyFn(a.zaiBase(), modelID, a.httpClient, disableThinking)
		if perr != nil {
			return fmt.Errorf("start Z.AI proxy: %w", perr)
		}
		defer stop()
		baseURL = proxyURL
	case res.ProviderTag != "":
		// When an OpenRouter provider is pinned, route Claude Code through a
		// local proxy that injects provider.only into each request body (the
		// only way to pin a provider, since Claude Code controls the body and
		// OpenRouter ignores it in the model string).
		proxyURL, stop, perr := startProviderProxy(defaultAnthropicBaseURL, res.ProviderTag)
		if perr != nil {
			return fmt.Errorf("start provider proxy: %w", perr)
		}
		defer stop()
		baseURL = proxyURL
	}

	claudeModel := claudeCodeModel(modelID, selected.ContextLength)
	a.printLaunchSummary(modelID, claudeModel, selected.ContextLength, disableThinking, dir, launchProviderLabel(provider, res))
	spec := launchSpec{
		Path: claudePath,
		Dir:  dir,
		Args: claudeArgs(claudeModel, claudePositionals, passthrough),
		Env:  buildClaudeEnvWithEffort(os.Environ(), baseURL, key, modelID, selected.ContextLength, disableThinking, effortLevel),
	}
	if a.runCommand != nil {
		return a.runCommand(spec)
	}
	return runClaudeCommand(spec)
}

// determineProvider resolves which backend to use, in precedence order:
// explicit --provider flag, inference from --model, the provider picker (on
// bare launch, --select-model, or first run), then the saved provider (default
// OpenRouter).
func (a *app) determineProvider(cfg Config, modelFlag, providerFlag string, selectModel, firstRun, bareCommand bool) (string, error) {
	if p := canonicalProvider(providerFlag); p != "" {
		if !isKnownProvider(p) {
			return "", fmt.Errorf("unknown provider %q (use %s)", providerFlag, strings.Join(providerNames(), ", "))
		}
		return p, nil
	}
	if p := inferProviderFromModel(modelFlag); p != "" {
		return p, nil
	}
	if bareCommand || selectModel || firstRun {
		opt, err := a.pickOne("Select a provider", providerOptions(), cfg.Provider)
		if err != nil {
			return "", err
		}
		return opt.ID, nil
	}
	if cfg.Provider != "" {
		return cfg.Provider, nil
	}
	return providerOpenRouter, nil
}

// inferProviderFromModel guesses the backend from a --model value. OpenRouter
// ids are always "author/slug"; first-class provider ids are bare families.
func inferProviderFromModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(model, "models/")
	lower := strings.ToLower(trimmed)
	if strings.Contains(trimmed, "/") {
		return providerOpenRouter
	}
	if strings.HasPrefix(lower, "gemini") || trimmed != model {
		return providerGemini
	}
	if strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4") {
		return providerOpenAI
	}
	if strings.HasPrefix(lower, "deepseek-") {
		return providerDeepSeek
	}
	if strings.HasPrefix(lower, "glm-") {
		return providerZAI
	}
	return ""
}

func canonicalProvider(input string) string {
	p := strings.ToLower(strings.TrimSpace(input))
	switch p {
	case "z-ai", "z.ai", "bigmodel", "zhipu":
		return providerZAI
	default:
		return p
	}
}

func isKnownProvider(provider string) bool {
	switch provider {
	case "", providerOpenRouter, providerGemini, providerOpenAI, providerDeepSeek, providerZAI:
		return true
	default:
		return false
	}
}

func providerNames() []string {
	return []string{providerOpenRouter, providerGemini, providerOpenAI, providerDeepSeek, providerZAI}
}

// geminiBase returns the Gemini API base, honoring the test override.
func (a *app) geminiBase() string {
	if strings.TrimSpace(a.geminiAPIBase) != "" {
		return a.geminiAPIBase
	}
	return defaultGeminiAPIBase
}

func (a *app) openAIBase() string {
	if strings.TrimSpace(a.openAIAPIBase) != "" {
		return a.openAIAPIBase
	}
	return defaultOpenAIAPIBase
}

func (a *app) deepSeekBase() string {
	if strings.TrimSpace(a.deepSeekAPIBase) != "" {
		return a.deepSeekAPIBase
	}
	return defaultDeepSeekAPIBase
}

func (a *app) deepSeekAnthropicBase() string {
	return strings.TrimRight(a.deepSeekBase(), "/") + "/anthropic"
}

func (a *app) zaiBase() string {
	if strings.TrimSpace(a.zaiAPIBase) != "" {
		return a.zaiAPIBase
	}
	return defaultZAIAPIBase
}

func launchProviderLabel(provider string, res pickResult) string {
	switch provider {
	case providerGemini:
		return "Google AI Studio"
	case providerOpenAI:
		return "OpenAI"
	case providerDeepSeek:
		return "DeepSeek"
	case providerZAI:
		return "Z.AI"
	}
	return res.ProviderName // pinned OpenRouter endpoint, or ""
}

// selectOpenRouter acquires the OpenRouter key and resolves the model to
// launch (picker or --model resolution).
func (a *app) selectOpenRouter(ctx context.Context, cfg Config, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	key, err := a.openRouterKey(ctx, cfg)
	if err != nil {
		return "", pickResult{}, err
	}
	client := newOpenRouterClient(a.httpClient, a.apiBase)
	modelID := strings.TrimSpace(modelFlag)
	if selectModel || modelID == "" {
		if firstRun {
			fmt.Fprintln(a.stderr, style.paint(clrDim, "Fetching OpenRouter models..."))
		}
		models, err := openRouterModels(ctx, client, key)
		if err != nil {
			return "", pickResult{}, err
		}
		current := cfg.LastModel
		if modelID != "" {
			current = modelID
		}
		endpointsFn := func(id string) ([]Endpoint, error) { return openRouterEndpoints(ctx, client, key, id) }
		res, err := a.pickModel("Select an OpenRouter model", models, current, endpointsFn)
		return key, res, err
	}
	res, err := a.resolveOpenRouterModel(ctx, client, key, modelID)
	return key, res, err
}

// selectGemini mirrors selectOpenRouter for Google AI Studio.
func (a *app) selectGemini(ctx context.Context, cfg Config, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	key, err := a.geminiKey(ctx, cfg)
	if err != nil {
		return "", pickResult{}, err
	}
	client := newGeminiClient(a.httpClient, a.geminiAPIBase)
	modelID := strings.TrimPrefix(strings.TrimSpace(modelFlag), "models/")
	if selectModel || modelID == "" {
		if firstRun {
			fmt.Fprintln(a.stderr, style.paint(clrDim, "Fetching Gemini models..."))
		}
		models, err := geminiModels(ctx, client, key)
		if err != nil {
			return "", pickResult{}, err
		}
		current := cfg.LastGeminiModel
		if modelID != "" {
			current = modelID
		}
		// nil endpoints: the Tab providers view is OpenRouter-only.
		res, err := a.pickModel("Select a Gemini model", models, current, nil)
		return key, res, err
	}
	res, err := a.resolveGeminiModel(ctx, client, key, modelID)
	return key, res, err
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
	key, err := a.promptAPIKey("OpenRouter")
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

// geminiKey mirrors openRouterKey for Google AI Studio: env var wins, then
// the saved key (validated), then a prompt.
func (a *app) geminiKey(ctx context.Context, cfg Config) (string, error) {
	client := newGeminiClient(a.httpClient, a.geminiAPIBase)
	// GEMINI_API_KEY is the documented name; GOOGLE_API_KEY is the Google SDK
	// convention, accepted as a fallback.
	if key := cleanAPIKey(os.Getenv("GEMINI_API_KEY")); key != "" {
		return key, nil
	}
	if key := cleanAPIKey(os.Getenv("GOOGLE_API_KEY")); key != "" {
		return key, nil
	}
	if cfg.GeminiAPIKey != "" {
		if err := validateGeminiKey(ctx, client, cfg.GeminiAPIKey); err == nil {
			return cfg.GeminiAPIKey, nil
		} else if errors.Is(err, errGeminiKeyRejected) {
			fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Saved Google AI Studio API key is no longer valid."))
		} else {
			// Transient failure: proceed optimistically, matching openRouterKey.
			fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Could not reach Google AI Studio to validate the saved key; using it anyway."))
			return cfg.GeminiAPIKey, nil
		}
	}
	key, err := a.promptAPIKey("Google AI Studio")
	if err != nil {
		return "", err
	}
	if err := validateGeminiKey(ctx, client, key); err != nil {
		if errors.Is(err, errGeminiKeyRejected) {
			return "", err
		}
		fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Could not reach Google AI Studio to validate the key; using it anyway."))
		return key, nil
	}
	return key, nil
}

func (a *app) openAIKey(ctx context.Context, cfg Config) (string, error) {
	return a.providerKey(ctx, "OpenAI", []string{"OPENAI_API_KEY"}, cfg.OpenAIAPIKey, func(ctx context.Context, key string) error {
		return validateBearerModelsKey(ctx, a.httpClient, a.openAIBase(), key, "OpenAI")
	})
}

func (a *app) deepSeekKey(ctx context.Context, cfg Config) (string, error) {
	return a.providerKey(ctx, "DeepSeek", []string{"DEEPSEEK_API_KEY"}, cfg.DeepSeekAPIKey, func(ctx context.Context, key string) error {
		return validateBearerModelsKey(ctx, a.httpClient, a.deepSeekBase(), key, "DeepSeek")
	})
}

func (a *app) zaiKey(ctx context.Context, cfg Config) (string, error) {
	return a.providerKey(ctx, "Z.AI", []string{"ZAI_API_KEY", "BIGMODEL_API_KEY"}, cfg.ZAIAPIKey, nil)
}

func (a *app) providerKey(ctx context.Context, label string, envNames []string, saved string, validate func(context.Context, string) error) (string, error) {
	for _, name := range envNames {
		if key := cleanAPIKey(os.Getenv(name)); key != "" {
			return key, nil
		}
	}
	if saved != "" {
		if validate == nil {
			return saved, nil
		}
		if err := validate(ctx, saved); err == nil {
			return saved, nil
		} else if errors.Is(err, errProviderKeyRejected) {
			fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Saved "+label+" API key is no longer valid."))
		} else {
			fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Could not reach "+label+" to validate the saved key; using it anyway."))
			return saved, nil
		}
	}
	key, err := a.promptAPIKey(label)
	if err != nil {
		return "", err
	}
	if validate == nil {
		return key, nil
	}
	if err := validate(ctx, key); err != nil {
		if errors.Is(err, errProviderKeyRejected) {
			return "", err
		}
		fmt.Fprintln(a.stderr, newTerminalStyle(a.stderr).warning("Could not reach "+label+" to validate the key; using it anyway."))
		return key, nil
	}
	return key, nil
}

func (a *app) promptAPIKey(label string) (string, error) {
	style := newTerminalStyle(a.stderr)
	fmt.Fprintf(a.stderr, "%s %s ", style.paint(clrAccentBold, "❯"), style.paint(clrHead, "Paste your "+label+" API key:"))
	required := errors.New(label + " API key is required")
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		if err != nil {
			return "", err
		}
		key := cleanAPIKey(string(data))
		if key == "" {
			return "", required
		}
		return key, nil
	}
	line, err := a.readLine()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	key := cleanAPIKey(line)
	if key == "" {
		return "", required
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
		return a.pickModel("Select an OpenRouter model", res.Ambiguous, input, endpointsFn)
	}
	return pickResult{Model: res.Model}, nil
}

func (a *app) selectOpenAI(ctx context.Context, cfg Config, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	key, err := a.openAIKey(ctx, cfg)
	if err != nil {
		return "", pickResult{}, err
	}
	return a.selectStaticModel(providerOpenAI, "OpenAI", key, cfg.LastOpenAIModel, modelFlag, selectModel, firstRun, style)
}

func (a *app) selectDeepSeek(ctx context.Context, cfg Config, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	key, err := a.deepSeekKey(ctx, cfg)
	if err != nil {
		return "", pickResult{}, err
	}
	return a.selectStaticModel(providerDeepSeek, "DeepSeek", key, cfg.LastDeepSeekModel, modelFlag, selectModel, firstRun, style)
}

func (a *app) selectZAI(ctx context.Context, cfg Config, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	key, err := a.zaiKey(ctx, cfg)
	if err != nil {
		return "", pickResult{}, err
	}
	return a.selectStaticModel(providerZAI, "Z.AI", key, cfg.LastZAIModel, modelFlag, selectModel, firstRun, style)
}

func (a *app) selectStaticModel(provider, label, key, lastModel, modelFlag string, selectModel, firstRun bool, style terminalStyle) (string, pickResult, error) {
	models := curatedProviderModels(provider)
	modelID := strings.TrimSpace(modelFlag)
	if selectModel || modelID == "" {
		if firstRun {
			fmt.Fprintf(a.stderr, "%s\n", style.paint(clrDim, "Loading "+label+" models..."))
		}
		current := lastModel
		if modelID != "" {
			current = modelID
		}
		res, err := a.pickModel("Select a "+label+" model", models, current, nil)
		return key, res, err
	}
	res, err := a.resolveStaticModel(label, models, modelID)
	return key, res, err
}

func (a *app) resolveStaticModel(label string, models []Model, input string) (pickResult, error) {
	res, ok := resolveModel(input, models)
	if !ok {
		if yes, err := a.confirm(fmt.Sprintf("Model %q was not found in %s. Pass it through anyway?", input, label)); err != nil {
			return pickResult{}, err
		} else if !yes {
			return pickResult{}, errors.New("model selection cancelled")
		}
		return pickResult{Model: Model{ID: input}}, nil
	}
	if len(res.Ambiguous) > 0 {
		return a.pickModel("Select a "+label+" model", res.Ambiguous, input, nil)
	}
	return pickResult{Model: res.Model}, nil
}

// resolveGeminiModel mirrors resolveOpenRouterModel for the Gemini catalog.
func (a *app) resolveGeminiModel(ctx context.Context, client *geminiClient, key, input string) (pickResult, error) {
	models, err := geminiModels(ctx, client, key)
	if err != nil {
		return pickResult{}, fmt.Errorf("could not reach Google AI Studio to verify model %q: %w", input, err)
	}
	res, ok := resolveModel(input, models)
	if !ok {
		if yes, err := a.confirm(fmt.Sprintf("Model %q was not found in Google AI Studio. Pass it through anyway?", input)); err != nil {
			return pickResult{}, err
		} else if !yes {
			return pickResult{}, errors.New("model selection cancelled")
		}
		return pickResult{Model: Model{ID: input}}, nil
	}
	if len(res.Ambiguous) > 0 {
		return a.pickModel("Select a Gemini model", res.Ambiguous, input, nil)
	}
	return pickResult{Model: res.Model}, nil
}

const openRouterRequestTimeout = 30 * time.Second
const geminiRequestTimeout = 30 * time.Second
const providerRequestTimeout = 30 * time.Second

func validateOpenRouterKey(ctx context.Context, client *openRouterClient, key string) error {
	ctx, cancel := context.WithTimeout(ctx, openRouterRequestTimeout)
	defer cancel()
	return client.validateKey(ctx, key)
}

func validateGeminiKey(ctx context.Context, client *geminiClient, key string) error {
	ctx, cancel := context.WithTimeout(ctx, geminiRequestTimeout)
	defer cancel()
	return client.validateKey(ctx, key)
}

func geminiModels(ctx context.Context, client *geminiClient, key string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, geminiRequestTimeout)
	defer cancel()
	models, err := client.models(ctx, key)
	if err != nil {
		return nil, err
	}
	return models, nil
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

func validateBearerModelsKey(ctx context.Context, httpClient *http.Client, apiBase, key, label string) error {
	ctx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	return validateBearerModels(ctx, httpClient, apiBase, key, label)
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
