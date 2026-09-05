# Measured token-rate starting defaults

Measured on 2026-09-04 through OpenRouter. The companion uses these values before valid live calibration is available. They are fallback values only: they contribute no synthetic sample weight, and learning continues with its existing 0.5 history discount.

For each model, the starting ratio is the sum of answer characters divided by the sum of answer tokens across completed samples. Answer tokens are reported completion tokens minus reported reasoning tokens. Characters include whitespace and use Unicode scalar counts, matching the TUI.

| Model | Completed samples | Answer characters | Answer tokens | Starting chars/token |
|---|---:|---:|---:|---:|
| `anthropic/claude-fable-5` | 1 | 5,525 | 2,173 | 2.542568 |
| `anthropic/claude-opus-5` | 2 | 22,515 | 8,746 | 2.574320 |
| `deepseek/deepseek-v4-flash-0731` | 1 | 11,131 | 2,969 | 3.749074 |
| `google/gemini-3.8-flash` | 2 | 26,560 | 6,963 | 3.814448 |
| `meta/muse-spark-1.3` | 2 | 13,313 | 3,714 | 3.584545 |
| `moonshotai/kimi-k3` | 3 | 32,924 | 8,428 | 3.906502 |
| `openai/gpt-5.6-luna` | 1 | 10,093 | 2,553 | 3.953388 |
| `openai/gpt-5.6-sol` | 2 | 21,672 | 4,967 | 4.363197 |
| `qwen/qwen3.8-27b` | 1 | 6,806 | 1,907 | 3.568956 |
| `tencent/hy3` | 1 | 5,835 | 1,639 | 3.560098 |
| `x-ai/grok-4.6` | 2 | 13,097 | 3,407 | 3.844144 |
| `z-ai/glm-5.3` | 3 | 27,173 | 7,087 | 3.834203 |
| `z-ai/glm-5.3-flash` | 1 | 11,077 | 2,971 | 3.728374 |

The Rust table keeps the integer totals as divisions, preserving the measured ratio without rounding. It recognizes the exact OpenRouter ID, its bare model ID, and an optional `:nitro` suffix. It does not guess defaults for unmeasured versions, Pro variants, or other provider namespaces.

## How the fallback is used

- A learned value for the output category wins, followed by its existing learned fallback category. The measured model value is used only when neither has a learned value.
- These are answer-text measurements. Tool arguments and raw reasoning can initially use this value as a proxy under the existing category fallback policy; it is not a separately measured ratio for those categories. Reasoning summaries use the text category.
- Unknown models retain 4 characters/token. Hy4 preview has no default because its only sample exhausted the budget without producing an answer.
- The measurements do not seed calibration counts. The first eligible live sample establishes the learned ratio; subsequent samples continue to update it. Learning remains in memory, keyed by the configured model ID.

## Sample basis

The samples combine English coding tutorials (TypeScript, SQL, JSON, tests, diffs and shell commands), versions with a basic algebra explanation, and stories mixing source code and prose. Fable uses its successful simpler story prompt. The other five recent models each pool one coding/algebra tutorial and one code story. Qwen and the earlier GLM/Kimi batches include low-reasoning samples; later batches use high reasoning. Completed tests use Nitro routing.

Filtered responses, incomplete DeepSeek output, and reasoning-only capped responses are excluded. Hidden reasoning and summaries are not counted as answer characters. These are workload estimates from a small number of samples, not exact tokenizer constants.

Detailed responses and metrics are retained locally under ignored `.private/token-ratio-*` experiment directories. The aggregate counts above are the reproducible basis committed with the patch; credentials and generated responses are not included.
