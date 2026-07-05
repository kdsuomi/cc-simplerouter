# cc-simplerouter

`simplerouter` instantly launches [Claude Code](https://claude.com/claude-code) against
[OpenRouter](https://openrouter.ai) models or Google AI Studio Gemini models, with a launch ui for
selecting your provider, model, and OpenRouter inference provider if desired.

The only configuration required is pasting your OpenRouter or Gemini API key on first launch.
Unlike other "claude code routers", simplerouter configures everything automatically on launch, so
your normal Claude Code setup is untouched and you can stop messing with environment variables,
local webservers, or manually editing your .claude files.


```powershell
simplerouter                              # first run: pick provider + key + model
simplerouter --model z-ai/glm-5.2 .       # launch with a specific model in the current dir
simplerouter --provider gemini --select-model  # pick a Gemini model from Google AI Studio
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

Requires [Go](https://go.dev/dl/).

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install.ps1
```

```sh
sh ./scripts/build_install.sh
```

These scripts build from the cloned repo and install the result to `~/.local/bin`.


## The model picker

Run `simplerouter --select-model` to open the provider and model pickers.

<img width="675" height="462" alt="image" src="https://github.com/user-attachments/assets/1f15087a-ef63-4cf4-b875-54b1bb2052ce" />


- **↑ / ↓** — move the highlight (auto-pages at the top/bottom of a page)
- **← / →** — flip pages
- **type** — filter live by id or name
- **↵** — launch the highlighted model
- **Tab** — open OpenRouter endpoint selection for the highlighted model (see below)
- **esc** — go back to provider selection

The list is pre-filtered to models usable by Claude Code. OpenRouter models are ordered by
OpenRouter popularity, with recommended models pinned at the top; Gemini models are fetched from
Google AI Studio and filtered to text/function-calling models.

## Provider / endpoint selection

simplerouter first asks whether to use OpenRouter or Google AI Studio. From the model picker, press
**`Esc`** to go back and switch providers.

OpenRouter defaults to its choice of inference provider. If you want to select a specific
OpenRouter endpoint, press **`Tab`** on a highlighted OpenRouter model:

<img width="674" height="461" alt="image" src="https://github.com/user-attachments/assets/d2093cc0-270a-43ef-a980-b972e93439dc" />

OpenRouter only honors provider routing in the request **body**, and Claude Code doesn't let you add body fields. So when you pin a provider, `simplerouter` starts a tiny localhost proxy for the session and points `ANTHROPIC_BASE_URL` at it; the proxy injects `provider.only` into each request before forwarding to OpenRouter. It binds to `127.0.0.1`, makes no changes to
your OpenRouter account, and shuts down when `claude` exits.

Gemini also uses a session-only localhost proxy, but as a translator: Claude Code sends Anthropic
Messages, and the proxy forwards Gemini `generateContent` requests to Google AI Studio.

> **Note:** pinning sets `allow_fallbacks: false`, so a transient error from the
> chosen provider isn't absorbed by OpenRouter's fallback and Claude Code will
> retry. If a provider is flaky, just pick another (or skip provider selection
> and let OpenRouter route).

## Flags

```
simplerouter [--model MODEL] [--provider PROVIDER] [--select-model] [--reset-key] [--disable-thinking] [path-or-prompt] [-- CLAUDE_ARGS...]
```

- `--model MODEL` — OpenRouter or Gemini model id, name, or unique suffix (skips the picker)
- `--provider PROVIDER` — `openrouter` or `gemini`
- `--select-model` — show the provider/model picker even when a model is saved
- `--reset-key` — forget saved OpenRouter and Gemini API keys, then prompt again
- `--disable-thinking` — drop Claude Code's Anthropic-specific thinking/beta
  request fields (see below)

## What it sets in Claude Code's environment

Only for the launched process. Notably:

- `ANTHROPIC_BASE_URL` → OpenRouter, the local OpenRouter endpoint proxy, or the local Gemini proxy
- `ANTHROPIC_AUTH_TOKEN` → your selected provider key; all model tiers (opus/sonnet/
  haiku/subagent) point at your chosen model
- `CLAUDE_CODE_AUTO_COMPACT_WINDOW` → the model's context length
- `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false` → disables the "suggest what to
  type next" feature, which otherwise re-sends the whole conversation each turn
  just to predict your next prompt and wastes money.

## Model compatibility

`simplerouter` targets OpenRouter models that work through Claude Code's Anthropic-compatible API
path and Gemini models that work through Google AI Studio's `generateContent` API. The picker
filters both lists to text models that support tool calling.

By default it preserves Claude Code's normal thinking behavior. If a provider
chokes on Claude Code's thinking/beta request fields, retry with
`--disable-thinking`:

```powershell
simplerouter --disable-thinking --model XXX.
```

