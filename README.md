# cc-simplerouter

`simplerouter` instantly launches [Claude Code](https://claude.com/claude-code) against
[OpenRouter](https://openrouter.ai), Google AI Studio Gemini, OpenAI, DeepSeek, Z.AI, or Meta models, with
a launch UI for selecting your provider, model, and OpenRouter inference provider if desired.

The only configuration required is pasting your provider API key on first launch.
Unlike other "claude code routers", simplerouter configures everything automatically on launch, so
your normal Claude Code setup is untouched and you can stop messing with environment variables,
local webservers, or manually editing your .claude files.


```powershell
simplerouter                              # first run: pick provider + key + model
simplerouter --model z-ai/glm-5.2 .       # launch with a specific model in the current dir
simplerouter --provider gemini --select-model  # pick a Gemini model from Google AI Studio
simplerouter --provider openai --model gpt-5.6-sol
simplerouter --provider deepseek --model deepseek-v4-flash
simplerouter --provider zai --model glm-5.2
simplerouter --provider meta --model muse-spark-1.1
```


## Install

Requires an installed `claude` CLI.

Windows:

```powershell
irm https://raw.githubusercontent.com/kdsuomi/cc-simplerouter/main/scripts/install.ps1 | iex
```

macOS/Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/kdsuomi/cc-simplerouter/main/scripts/install.sh | sh
```

The install scripts download the latest GitHub Release binary and install it to
`~/.local/bin`. macOS release binaries are Apple Silicon only.

## Build from source

Requires [Go 1.24 or newer](https://go.dev/dl/).

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install.ps1
```

```sh
sh ./scripts/build_install.sh
```

These scripts build from the cloned repo and install the result to `~/.local/bin`.


## The model picker

Run `simplerouter` or `simplerouter --select-model` to open the provider and model pickers.

<img width="675" height="462" alt="image" src="https://github.com/user-attachments/assets/1f15087a-ef63-4cf4-b875-54b1bb2052ce" />


- **↑ / ↓** — move the highlight (auto-pages at the top/bottom of a page)
- **← / →** — flip pages
- **type** — filter live by id or name
- **↵** — launch the highlighted model
- **Tab** — open OpenRouter endpoint selection for the highlighted model (see below)
- **esc** — go back to provider selection

The list is pre-filtered to models usable by Claude Code. OpenRouter models are ordered by
OpenRouter popularity, with recommended models pinned at the top; Gemini models are fetched from
Google AI Studio and filtered to text/function-calling models. OpenAI, DeepSeek, Z.AI, and Meta use
small curated model lists.

The current OpenAI picker starts with GPT-5.6 Sol, Terra, and Luna. The official
`gpt-5.6` Sol alias is accepted with `--model` but omitted from the picker so it
does not appear as a duplicate. Gemini 3.6 Flash is pinned at the top of the
recommended Gemini models when it is returned by Google AI Studio.

## Provider / endpoint selection

simplerouter first asks which provider to use. From the model picker, press
**`Esc`** to go back and switch providers.

OpenRouter defaults to its choice of inference provider. If you want to select a specific
OpenRouter endpoint, press **`Tab`** on a highlighted OpenRouter model:

<img width="674" height="461" alt="image" src="https://github.com/user-attachments/assets/d2093cc0-270a-43ef-a980-b972e93439dc" />

OpenRouter only honors provider routing in the request **body**. Current Claude
Code exposes `CLAUDE_CODE_EXTRA_BODY` for static additions, but simplerouter
still needs a session-only localhost proxy to translate the Anthropic Messages
protocol, stream reasoning live, replay provider reasoning state across tool
turns, and inject the endpoint chosen at launch. When an endpoint is pinned,
the proxy adds `provider.only` and `allow_fallbacks: false` to every request.
It binds to `127.0.0.1`, makes no changes to your OpenRouter account, and shuts
down when `claude` exits.

Gemini also uses a session-only localhost proxy, but as a translator: Claude Code sends Anthropic
Messages, and the proxy forwards Gemini `generateContent` requests to Google AI Studio.

OpenAI and Z.AI also use session-only localhost translators. DeepSeek is launched directly through
DeepSeek's Anthropic-compatible API. Meta's Messages API is also Anthropic-compatible; its localhost
proxy is a thin passthrough that only strips the request fields Meta rejects (`stop_sequences`,
`top_k`).

> **Note:** pinning sets `allow_fallbacks: false`, so a transient error from the
> chosen provider isn't absorbed by OpenRouter's fallback and Claude Code will
> retry. If a provider is flaky, just pick another (or skip provider selection
> and let OpenRouter route).

## Flags

```
simplerouter [--model MODEL] [--provider PROVIDER] [--select-model] [--reset-key] [--disable-thinking] [path-or-prompt] [-- CLAUDE_ARGS...]
```

- `--model MODEL` — model id, name, or unique suffix (skips the picker)
- `--provider PROVIDER` — `openrouter`, `gemini`, `openai`, `deepseek`, `zai`, or `meta`
- `--select-model` — show the provider/model picker even when a model is saved
- `--reset-key` — forget saved API keys, then prompt again
- `--disable-thinking` — disable Claude Code thinking and experimental beta
  request features for models that do not accept them (see below)

## Keys and local configuration

Environment variables take precedence over saved values:

| Provider | Environment variables, in order |
| --- | --- |
| OpenRouter | `OPENROUTER_API_KEY` |
| Google AI Studio | `GEMINI_API_KEY`, `GOOGLE_API_KEY` |
| OpenAI | `OPENAI_API_KEY` |
| DeepSeek | `DEEPSEEK_API_KEY` |
| Z.AI | `ZAI_API_KEY`, `BIGMODEL_API_KEY` |
| Meta | `META_API_KEY`, `MODEL_API_KEY` |

If no environment value is present, simplerouter validates a saved key where
the provider exposes a suitable endpoint or prompts without echoing in an
interactive terminal. Keys and selected models are stored in
`~/.simplerouter/config.json`. `--reset-key` removes the stored keys without
discarding model choices.

## What it sets in Claude Code's environment

Only for the launched process. Notably:

- `ANTHROPIC_BASE_URL` → the selected provider endpoint or session-only local proxy
- `ANTHROPIC_AUTH_TOKEN` → your selected provider key for every route;
  `ANTHROPIC_API_KEY` is cleared so it cannot take precedence
- Opus, Sonnet, Haiku, the custom model entry, and subagents → your chosen model
- Model names, descriptions, and supported capabilities → the selected model;
  this enables current effort and adaptive-thinking controls for gateway IDs
- `CLAUDE_CODE_DISABLE_FAST_MODE=1` → prevents a saved Anthropic Fast Mode
  preference from leaking into a non-Anthropic provider session
- `CLAUDE_CODE_AUTO_COMPACT_WINDOW` → the model's context length
- `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false` → disables the "suggest what to
  type next" feature, which otherwise re-sends the whole conversation each turn
  just to predict your next prompt and wastes money.

## Model compatibility

`simplerouter` targets OpenRouter models through OpenRouter's Chat Completions API (via a
session-only local proxy that streams reasoning live and round-trips `reasoning_details` across
turns), Gemini models through Google AI Studio's `generateContent` API, OpenAI models through the
Responses API, DeepSeek through its Anthropic-compatible API, Z.AI through its Chat Completions
API, and Meta (Muse Spark) through its Anthropic-compatible Messages API. The picker filters or
curates these lists to text models that support tool calling.

Current Claude Code sends adaptive thinking as `thinking.type=adaptive` with
`output_config.effort`. simplerouter maps that effort to OpenAI and OpenRouter
directly, to the supported Z.AI effort levels, and to Gemini thinking levels or
safe 2.5-generation budgets. Anthropic-compatible routes preserve
`thinking`, `output_config`, and `context_management` in the original request.

For OpenRouter, model reasoning streams into Claude Code as it is generated. A
session-only patched copy of Claude Code renders it token by token without
modifying the installed binary. The functional live-thinking patch is verified
against the installed bundle; if a future Claude update changes that code,
simplerouter prints a compatibility warning and uses periodic thinking blocks
instead. The cosmetic patched-version marker is best-effort and cannot disable
the functional patch.

By default it preserves Claude Code's normal thinking behavior. If a provider
chokes on Claude Code's thinking/beta request fields, retry with
`--disable-thinking`:

```powershell
simplerouter --disable-thinking --model MODEL_ID
```

## Development

```powershell
$env:GOCACHE = Join-Path (Get-Location) ".gocache"
go test ./... -count=1
go vet ./...
go build ./...
```

Build release artifacts with:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\package.ps1 -Version vX.Y.Z
```

Protocol and Claude Code references used by the implementation:

- [Claude Code environment variables](https://code.claude.com/docs/en/env-vars)
- [Claude Code model configuration](https://code.claude.com/docs/en/model-config)
- [Claude Code LLM gateway protocol](https://code.claude.com/docs/en/llm-gateway-protocol)
- [OpenAI Responses and current models](https://developers.openai.com/api/docs/models)
- [OpenRouter provider routing](https://openrouter.ai/docs/guides/routing/provider-selection)
- [Gemini models](https://ai.google.dev/gemini-api/docs/models)
- [DeepSeek Anthropic API compatibility](https://api-docs.deepseek.com/guides/anthropic_api)

