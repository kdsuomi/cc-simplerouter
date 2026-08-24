# simplerouter for Codex CLI

`simplerouter` launches an installed [OpenAI Codex CLI](https://developers.openai.com/codex)
with an existing Codex subscription or against OpenRouter, Google AI Studio, OpenAI,
DeepSeek, Z.AI, or Meta Model API.
It supplies Codex with one session-scoped Responses provider and a model descriptor
derived from the installed Codex release, preserving Codex features such as freeform
`apply_patch`, namespaced tools, parallel tool calls, reasoning, and multi-agent support.

The launcher does not rewrite `~/.codex/config.toml` or replace the user's Codex
installation. Provider overrides, temporary model metadata, and any localhost proxy
exist only for the child Codex process and are removed when that process exits.
Launched sessions also use Codex's standard service tier, disabling Fast mode without
changing the user's global service-tier preference for direct Codex sessions.
When the patched Codex companion bundle is installed, launched sessions stream
reasoning in the transcript by default and expose `/thinking` as a session-only toggle.

```powershell
simplerouter                                      # pick provider, enter key, pick model
simplerouter --model z-ai/glm-5.2 .               # OpenRouter model, current directory
simplerouter --provider gemini --select-model     # live Google AI Studio catalog
simplerouter --provider openai --model gpt-5.6-sol
simplerouter --provider xai --model grok-4.5       # Grok CLI login or XAI_API_KEY
simplerouter --provider deepseek --model deepseek-v4-flash
simplerouter --provider zai --model glm-5.2
simplerouter --provider meta --model muse-spark-1.2
simplerouter --provider codex .                    # existing ChatGPT sign-in, standard routing
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

To prepare, build, and install the patched Codex companion on Windows, run:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install_codex_companion.ps1
```

The build script clones the pinned upstream Codex release into ignored
`.build/`, applies the patch series committed under `codex/patches`, and
verifies the exact resulting Git tree before compiling. Cargo outputs are kept
separately in `.build/codex-target`, so source refreshes retain incremental
build artifacts. It installs a version-isolated bundle under
`~/.local/share/simplerouter/simplerouter-codex`, including the two helper
executables required by Codex's Windows sandbox. Keeping the helpers with the
patched binary avoids collisions with a separately installed official Codex
release. Pass `-SkipBuild` to reinstall existing Cargo outputs without
recompiling, or `-RefreshSource` to recreate the generated checkout.

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
| xAI (Grok) | `POST https://api.x.ai/v1/responses` | Loopback Responses compatibility proxy |
| DeepSeek | `POST https://api.deepseek.com/chat/completions` | Loopback Responses-to-Chat translator |
| Z.AI | `POST https://api.z.ai/api/paas/v4/chat/completions` | Loopback Responses-to-Chat translator |
| Meta Model API | `POST https://api.meta.ai/v1/responses` | Loopback Responses compatibility proxy |
| Codex subscription | Standard Codex backend | Direct; no provider or endpoint overrides |

The loopback servers bind to `127.0.0.1`, forward the selected session key, and
shut down with Codex.

### Live thinking

`simplerouter` prefers the canonical patched companion bundle under
`~/.local/share/simplerouter/simplerouter-codex` over the normal Codex executable.
That build ports the live reasoning behavior
from [openai/codex#6006](https://github.com/openai/codex/pull/6006) to current Codex.
It is enabled by default only when the active provider is `simplerouter_session`;
running the ordinary `codex` command remains unchanged.

Use `/thinking` to toggle the live reasoning block, or `/thinking on`,
`/thinking off`, and `/thinking status` for an explicit action. The toggle changes
only the current launched process and continues to honor Codex's
`hide_agent_reasoning` setting.

The live block shows the last 20 rows of the current reasoning stream. When a
reasoning block finishes, its last 20 rows stay in the scrollback; the full
text is always available in the `Ctrl+T` transcript overlay.

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

Without endpoint pinning, the passthrough leaves provider routing untouched so
OpenRouter chooses the endpoint. It also handles structured
output compatibility: if an endpoint rejects `json_schema` but supports
`json_object`, the request is retried once and that capability is remembered
for later automatic reviews in the same session.

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

The Meta adapter preserves the native Responses protocol and server-managed
reasoning replay. It caps Codex-only `max` and `ultra` reasoning levels at
Meta's documented `xhigh` maximum. Meta's current Codex endpoint accepts
function tools but rejects Codex 0.145's freeform `custom` tools, so the proxy
presents them as equivalent function tools upstream and translates calls and
results back to Codex's custom-tool format. It also disables strict validation
on function declarations whose optional parameters Meta would otherwise reject,
and omits Codex's optional `tool_search.limit` field so Meta can validate that
built-in tool's required-only schema. Codex's unsupported
`web_search.search_content_types` hint is removed while web search remains enabled.
Recursive app-tool schemas are made finite by relaxing only cycle-closing local
`$ref` edges, allowing tools such as Gmail's nested MIME-part inputs to remain
available on Meta.

The xAI (Grok) adapter also preserves the native Responses protocol against
`https://api.x.ai/v1`. It rewrites Codex freeform `custom` tools (for example
`apply_patch`) as function tools, flattens multi-agent `namespace` groups into
`namespace__tool` functions (restoring the original name + `namespace` on the
return stream), and drops unsupported Codex tool types such as `tool_search`.
xAI accepts only `function` plus built-ins such as `web_search` and `x_search`.
Reasoning effort is normalized per model: Grok 4.5 clamps Codex-only levels to
its documented `low`/`medium`/`high` range, Grok 4.3 preserves true `none`, and
Grok 4.20 Multi-Agent preserves `xhigh`. Grok Build 0.1 and Grok 4.20 Reasoning
use their fixed provider effort because xAI rejects the effort control for those
models; their supported summary control is preserved. Default Grok 4.5 effort is
`high` with summarized reasoning streamed live. On continuation requests, the
adapter also removes Codex's serialization-only `content: null` while preserving
xAI's opaque encrypted reasoning state verbatim.

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
generation models. OpenAI, xAI, DeepSeek, Z.AI, and Meta use small, documented
curated lists.

xAI's curated list starts with `grok-4.5` (500k context, default high reasoning),
then `grok-4.3`, `grok-build-0.1`, and the Grok 4.20 reasoning / non-reasoning /
multi-agent variants. Documented aliases such as `grok-4.5-latest` and
`grok-latest` are accepted with `--model` but omitted from the picker.

The current OpenAI picker begins with GPT-5.6 Sol, Terra, and Luna. The official
`gpt-5.6` alias for Sol is accepted with `--model` but omitted from the picker
to avoid a duplicate row.

Meta's curated list follows the Model API catalog: `muse-spark-1.2` (standard
tier default), `muse-spark-1.2-contributor` (discounted contributor tier), and
`muse-spark-1.1`. All three share a 1M-token context window and the same
reasoning defaults (`high` effort, `auto` summary).

## Command line

```text
simplerouter [--model MODEL] [--provider PROVIDER] [--select-model] [--reset-key] [--disable-thinking] [path-or-prompt] [-- CODEX_ARGS...]
```

- `--model MODEL`: model ID, display name, or unique suffix
- `--provider PROVIDER`: `openrouter`, `gemini`, `openai`, `xai` (alias `grok`), `deepseek`, `zai`, `meta`, or `codex`
- `--select-model`: open the provider/model picker even when a choice is saved
- `--reset-key`: clear every saved provider key before selection
- `--disable-thinking`: disable Codex reasoning and the provider's thinking mode
- `-- CODEX_ARGS...`: forward the remaining arguments to Codex unchanged

If the first positional argument names a directory, Codex starts there. Other
positional text becomes the initial prompt. A provider can usually be inferred
from an explicit model ID: slash-qualified IDs select OpenRouter, while
`gemini-*`, `gpt-*`, `grok-*`, `deepseek-*`, `glm-*`, and `muse-*` select their first-class
providers.

## Keys and local configuration

Environment variables take precedence over saved values:

| Provider | Environment variables, in order |
| --- | --- |
| OpenRouter | `OPENROUTER_API_KEY` |
| Google AI Studio | `GEMINI_API_KEY`, `GOOGLE_API_KEY` |
| OpenAI | `OPENAI_API_KEY` |
| xAI (Grok) | `XAI_API_KEY`, `GROK_API_KEY`, then Grok CLI session in `~/.grok/auth.json` |
| DeepSeek | `DEEPSEEK_API_KEY` |
| Z.AI | `ZAI_API_KEY`, `BIGMODEL_API_KEY` |
| Meta | `META_API_KEY`, `MODEL_API_KEY` |

If no environment value is present, the launcher validates a saved key where
the provider exposes a suitable endpoint, or prompts without echoing in an
interactive terminal. For xAI, a valid Grok CLI login (`grok login`) is reused
automatically and refreshed via OIDC when expired; that session token is not
copied into SimpleRouter's config. Selected keys and models are stored in
`~/.simplerouter/config.json`; `--reset-key` removes the stored keys without
discarding model choices.

The temporary Codex provider reads the selected key from the private
`SIMPLEROUTER_CODEX_API_KEY` environment variable. Existing environment entries
and normal Codex configuration remain available to Codex unchanged.

The `codex` provider launches the patched companion with the user's existing Codex
configuration and stored ChatGPT sign-in. It does not create a temporary model
provider, set a base URL, select an API key, or start a proxy. A child-process-only
marker enables the companion's generation-rate display without changing routing.

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
- [Meta Model API models](https://dev.meta.ai/docs/models) (`muse-spark-1.2`, `muse-spark-1.2-contributor`, `muse-spark-1.1`)
- [Meta Model API coding agents (Codex)](https://dev.meta.ai/docs/coding-agents)
- [Meta Muse Spark 1.1 and Model API](https://ai.meta.com/blog/introducing-muse-spark-meta-model-api/)
- [xAI Models](https://docs.x.ai/developers/models) (`grok-4.5`, `grok-4.3`, `grok-build-0.1`)
- [xAI Responses API](https://docs.x.ai/developers/model-capabilities/text/generate-text)
- [xAI Reasoning](https://docs.x.ai/developers/model-capabilities/text/reasoning)
- [Grok Build authentication](https://docs.x.ai/build/enterprise#authentication)
