# simplerouter for Codex CLI

`simplerouter` launches an installed [OpenAI Codex CLI](https://developers.openai.com/codex)
against OpenRouter, Google AI Studio, OpenAI, DeepSeek, Z.AI, or Meta Model API.
It supplies Codex with one session-scoped Responses provider and a model descriptor
derived from the installed Codex release, preserving Codex features such as freeform
`apply_patch`, namespaced tools, parallel tool calls, reasoning, and multi-agent support.

The launcher does not rewrite `~/.codex/config.toml` or replace the user's Codex
installation. Provider overrides, temporary model metadata, and any localhost proxy
exist only for the child Codex process and are removed when that process exits.
Launched sessions also use Codex's standard service tier, disabling Fast mode without
changing the user's global service-tier preference for direct Codex sessions.
When the companion `codex-simplerouter` binary is installed, launched sessions stream
reasoning in the transcript by default and expose `/thinking` as a session-only toggle.

```powershell
simplerouter                                      # pick provider, enter key, pick model
simplerouter --model z-ai/glm-5.2 .               # OpenRouter model, current directory
simplerouter --provider gemini --select-model     # live Google AI Studio catalog
simplerouter --provider openai --model gpt-5.6-sol
simplerouter --provider deepseek --model deepseek-v4-flash
simplerouter --provider zai --model glm-5.2
simplerouter --provider meta --model muse-spark-1.1
simplerouter . -- --full-auto                     # pass an option through to Codex
```

## Requirements and installation

Install a current Codex CLI first. OpenAI's standalone installers are:

Windows:

```powershell
powershell -ExecutionPolicy Bypass -c "irm https://chatgpt.com/codex/install.ps1 | iex"
```

macOS/Linux:

```sh
curl -fsSL https://chatgpt.com/codex/install.sh | sh
```

Then build and install `simplerouter` from this repository. This requires Go 1.24
or newer.

Windows:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install.ps1
```

macOS/Linux:

```sh
sh ./scripts/build_install.sh
```

Both scripts install the binary under `~/.local/bin` and explain any required
`PATH` change.

For a published `simplerouter` release, the download installers are:

```powershell
irm https://raw.githubusercontent.com/kdsuomi/cc-simplerouter/main/scripts/install.ps1 | iex
```

```sh
curl -fsSL https://raw.githubusercontent.com/kdsuomi/cc-simplerouter/main/scripts/install.sh | sh
```

Use a release whose notes identify Codex CLI support. The older `v0.1.x`
releases target Claude Code.

## Provider routing

Codex custom providers use the Responses wire protocol. `simplerouter` connects
native Responses providers directly and translates only where the upstream API
uses a different protocol.

| Provider | Upstream API | Session route |
| --- | --- | --- |
| OpenRouter | `POST https://openrouter.ai/api/v1/responses` | Direct, unless an inference endpoint is pinned |
| Google AI Studio | `POST https://generativelanguage.googleapis.com/v1/interactions?alt=sse` | Loopback Responses-to-Interactions translator |
| OpenAI | `POST https://api.openai.com/v1/responses` | Direct |
| DeepSeek | `POST https://api.deepseek.com/chat/completions` | Loopback Responses-to-Chat translator |
| Z.AI | `POST https://api.z.ai/api/paas/v4/chat/completions` | Loopback Responses-to-Chat translator |
| Meta Model API | `POST https://api.meta.ai/v1/responses` | Direct |

The loopback servers bind to `127.0.0.1`, forward the selected session key, and
shut down with Codex.

### Live thinking

`simplerouter` prefers the companion `codex-simplerouter` binary over the normal
Codex executable when it is available. That build ports the live reasoning behavior
from [openai/codex#6006](https://github.com/openai/codex/pull/6006) to current Codex.
It is enabled by default only when the active provider is `simplerouter_session`;
running the ordinary `codex` command remains unchanged.

Use `/thinking` to toggle the live reasoning block, or `/thinking on`,
`/thinking off`, and `/thinking status` for an explicit action. The toggle changes
only the current launched process and continues to honor Codex's
`hide_agent_reasoning` setting.

### OpenRouter endpoint pinning

OpenRouter routes providers through request-body fields. From the OpenRouter
model picker, press `Tab` to select a particular inference endpoint.
`simplerouter` then starts a thin Responses passthrough that injects:

```json
{
  "provider": {
    "only": ["selected-provider"],
    "allow_fallbacks": false
  }
}
```

Without endpoint pinning, Codex connects straight to OpenRouter and OpenRouter
chooses the endpoint.

### Protocol translation

The Gemini adapter uses the stable V1 Interactions API with `store: false`. It
translates Codex functions, custom tools, namespaces, reasoning levels, images,
and Google Search, then replays the provider's typed steps, thought signatures,
citations, and search results on later turns.

The DeepSeek and Z.AI adapters translate the complete Responses conversation to
Chat Completions. They flatten namespaced tools, wrap freeform custom tools such
as `apply_patch` as functions, and reconstruct the original Codex tool identity
on the return stream. Provider reasoning is streamed live and preserved
verbatim in encrypted replay metadata so `reasoning_content` can be sent back
unchanged on tool-follow-up turns.

DeepSeek receives its documented `thinking` object, compatible reasoning-effort
mapping, and streamed usage request. Z.AI receives `thinking.clear_thinking:
false`, `tool_stream: true`, and its documented reasoning effort, including
`none`.

## Model selection

Run `simplerouter` or `simplerouter --select-model` to open the provider and
model pickers.

- `↑` / `↓`: move the highlight and cross page boundaries
- `←` / `→`: change pages
- Type: filter by model ID or display name
- `Enter`: select the highlighted item
- `Tab`: choose an OpenRouter inference endpoint
- `Esc`: return to provider selection

OpenRouter and Gemini catalogs are fetched from their live model endpoints.
The OpenRouter list keeps the provider's popularity order with recommended
coding models pinned at the top. The Gemini list is restricted to usable text
generation models. OpenAI, DeepSeek, Z.AI, and Meta use small, documented
curated lists.

The current OpenAI picker begins with GPT-5.6 Sol, Terra, and Luna. The official
`gpt-5.6` alias for Sol is accepted with `--model` but omitted from the picker
to avoid a duplicate row.

## Command line

```text
simplerouter [--model MODEL] [--provider PROVIDER] [--select-model] [--reset-key] [--disable-thinking] [path-or-prompt] [-- CODEX_ARGS...]
```

- `--model MODEL`: model ID, display name, or unique suffix
- `--provider PROVIDER`: `openrouter`, `gemini`, `openai`, `deepseek`, `zai`, or `meta`
- `--select-model`: open the provider/model picker even when a choice is saved
- `--reset-key`: clear every saved provider key before selection
- `--disable-thinking`: disable Codex reasoning and the provider's thinking mode
- `-- CODEX_ARGS...`: forward the remaining arguments to Codex unchanged

If the first positional argument names a directory, Codex starts there. Other
positional text becomes the initial prompt. A provider can usually be inferred
from an explicit model ID: slash-qualified IDs select OpenRouter, while
`gemini-*`, `gpt-*`, `deepseek-*`, `glm-*`, and `muse-*` select their first-class
providers.

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

If no environment value is present, the launcher validates a saved key where
the provider exposes a suitable endpoint, or prompts without echoing in an
interactive terminal. Selected keys and models are stored in
`~/.simplerouter/config.json`; `--reset-key` removes the stored keys without
discarding model choices.

The temporary Codex provider reads the selected key from the private
`SIMPLEROUTER_CODEX_API_KEY` environment variable. Existing environment entries
and normal Codex configuration remain available to Codex unchanged.

## Compatibility notes

- Use a current Codex CLI. `simplerouter` derives its temporary one-model
  catalog from `codex debug models --bundled` so its protocol descriptor stays
  aligned with the installed release.
- All internal proxies accept the streaming Responses requests generated by
  Codex; they are not intended as general-purpose public API gateways.
- Native Responses routes retain the provider's server tools. Gemini maps
  Codex web search to Google Search. DeepSeek and Z.AI currently document only
  function tools on these Chat endpoints, so Codex server-side web search is
  unavailable on those two routes. Shell, `apply_patch`, MCP/function tools,
  and multi-agent namespace tools remain available.
- A model or endpoint can still reject a capability that its provider does not
  implement. `--disable-thinking` is the fallback for models that do not accept
  reasoning.
- Pinned OpenRouter endpoints deliberately disable provider fallback. Launch
  without pinning when automatic failover is preferred.

## Development

```powershell
$env:GOCACHE = Join-Path (Get-Location) ".gocache"
go test ./... -count=1
go vet ./...
go build ./...
```

Build release artifacts for Windows amd64/arm64, macOS arm64, and Linux
amd64/arm64 with:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\package.ps1 -Version vX.Y.Z
```

Protocol references used by this implementation:

- [Codex configuration](https://developers.openai.com/codex/config-reference/)
- [OpenAI Responses and current models](https://developers.openai.com/api/docs/models)
- [OpenRouter Responses API](https://openrouter.ai/docs/api/reference/responses/overview)
- [OpenRouter provider routing](https://openrouter.ai/docs/guides/routing/provider-selection)
- [Gemini Interactions API V1](https://ai.google.dev/api/interactions-api-v1)
- [DeepSeek Chat Completions](https://api-docs.deepseek.com/api/create-chat-completion)
- [Z.AI Chat Completions](https://docs.z.ai/api-reference/llm/chat-completion)
- [Meta Muse Spark 1.1 and Model API](https://ai.meta.com/blog/introducing-muse-spark-meta-model-api/)
