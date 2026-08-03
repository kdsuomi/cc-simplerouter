# Debugging SimpleRouter CLI Patches

## Codex companion: know the source of truth

Current `main` launches Codex. Its patched companion is represented by the
ordered mail patches under `codex/patches/0.145.0`, based on the exact upstream
commit named in `scripts/prepare_codex_companion.ps1`. The checkout under
`.build/codex-rust-v0.145.0` is generated and ignored; edits made only there are
not part of the SimpleRouter repository.

The preparation script verifies the patched Git tree, not merely whether every
patch command returned success. This catches a wrong upstream tag, missing or
reordered patches, and unnoticed patch drift. The Cargo target directory is
separate at `.build/codex-target`, so use `-RefreshSource` when source is suspect
without throwing away the expensive incremental build cache.

For a companion change:

1. Make and commit the change in the generated checkout.
2. Regenerate the complete series from `rust-v0.145.0` with
   `git format-patch --no-signature`; do not copy the upstream source tree into
   this repository.
3. Replace `codex/patches/0.145.0`, update the expected tree hash in the prepare
   script, and prepare a fresh checkout.
4. Run the focused Rust tests in that checkout, then build/install through the
   root script so the tested and launched binaries use the same path.

On Windows, an upstream checkout can show a very large `git status` after line
ending conversion even when `git diff` has no content. Before treating that as
real work, inspect `git diff --name-only HEAD`, the committed patch range, and
the final tree hash. Export patches from commits, never from a noisy working
tree.

`findCodex` prefers the installed bundle at
`~/.local/share/simplerouter/simplerouter-codex/bin/codex-simplerouter.exe`.
Testing an executable elsewhere does not prove that `simplerouter` launched it;
inspect that bundle or the resolved launch path.

## Legacy Claude Code binary patching

The notes below describe the Claude-focused history before the Codex variant
became `main`. They remain useful when diagnosing or backporting that branch.

## Know which binary is running

`findClaude` finds the source binary through `PATH`, then `~/.local/bin/claude.exe` or `~/.claude/local/claude.exe`. SimpleRouter never modifies it. `prepareClaudePatch` writes the launch copy to:

```text
~/.simplerouter/claude-patches/claude-live-thinking-<os>-<arch>-<source-sha-prefix>.exe
```

The filename hashes the original Claude binary, not the SimpleRouter patch implementation or enabled feature set. The cache is still refreshed when its bytes differ from the newly generated result. Rebuild and run the local SimpleRouter executable when testing patch changes; a globally installed older SimpleRouter will regenerate its older patch.

The optional launch-card version suffix `p` confirms that a patched copy was selected when the current Claude build supports that cosmetic edit; its absence proves nothing. To learn the exact executable path, inspect the `launchSpec.Path` produced by the CLI or log `claudePath` after `prepareClaudePatch`; do not inspect the installed source and assume it was patched.

## How patching works

Claude's Bun executable contains its minified JavaScript as searchable byte ranges. Each patch:

1. Locates a code shape with a structural regex and captures that build's minified identifiers.
2. Generates replacement JavaScript using those identifiers.
3. Requires `len(replacement) <= target length`.
4. Copies the replacement in place and fills unused bytes with ASCII spaces so every later byte offset remains unchanged.

This makes emitted bytes the source of truth. Always inspect the complete applied range plus the bytes immediately before and after it. Template boundaries are not JavaScript token boundaries: for example, `}else{{ENTRY}}` emitted `}elseU`, which is valid syntax using identifier `elseU`, not the intended `else U`. A loadable executable and marker matches therefore do not prove semantic correctness.

Patches run in this order:

- `live-thinking`: required; rewrites the interactive `thinking_delta` dispatcher.
- `throughput-meter`: optional; rewrites the metrics state function, spinner, and turn-end controller.
- `launch-version-marker`: cosmetic.

Useful isolation controls, set on the SimpleRouter process:

| Environment | Result |
| --- | --- |
| none | All compatible patches |
| `SIMPLEROUTER_DISABLE_TOKEN_RATE=1` | Live thinking without the three throughput edits |
| `SIMPLEROUTER_DISABLE_CLAUDE_PATCH=1` | Original Claude binary |

For a failure inside a multi-edit feature, create diagnostic copies with `live-thinking` plus exactly one `claudePatchEdit`. For throughput, split `findThroughputStateEdit`, `findThroughputSpinnerEdit`, and `findThroughputControllerEdit`; do not treat "throughput patch" as one indivisible change.

## Test the same Claude execution path that failed

`claude_capture_test.go` and `claude_stream_probe_test.go` invoke `claude -p` with JSON/stream-JSON output. They are useful for request and SSE inspection, but they do **not** exercise the interactive Ink terminal renderer. A response can be fully received while an exception in an interactive metrics callback prevents the assistant message from appearing.

For OpenRouter, set `SIMPLEROUTER_OPENROUTER_TRACE` to a file path before launching SimpleRouter to append each raw upstream SSE JSON event before translation. This proves what reached the proxy, not what Claude rendered. Trace files can contain reasoning, text, and tool data; keep them in a private scratch location and remove them after diagnosis.

For an interactive rendering failure on Windows:

1. Launch the exact patched copy under a real ConPTY. Redirected pipes and custom terminal bridges are not equivalent to Claude's TUI path.
2. Point `ANTHROPIC_BASE_URL` at a local server implementing `/v1/messages/count_tokens`, `/v1/models`, and `/v1/messages`.
3. Return a deterministic SSE sequence: `message_start`, thinking block start/delta/signature/stop, text block start/delta/stop, `message_delta` with final `output_tokens`, then `message_stop`.
4. Capture both proof layers: the server received/completed the request, and the ConPTY output contains the expected thinking and final text.
5. Run the same fixture against the original, each isolated edit, the full patch, and the proposed minimal correction.

## What the existing tests prove

```powershell
go test ./internal/simplerouter -run TestFindThroughputEditsRewritesAllInMemoryHooks -count=1
go test ./internal/simplerouter -run TestClaudePatchesMatchInstalledClaude -count=1 -v
$env:SIMPLEROUTER_TEST_PATCHED_CLAUDE = '1'
go test ./internal/simplerouter -run TestPreparedInstalledClaudeRuns -count=1 -v
```

The first test validates generated fake-bundle edits and should assert critical token boundaries, not only feature markers. The installed-binary canary proves structural matches and size limits. The opt-in smoke test proves the patched executable loads. Only a ConPTY test with deterministic SSE proves the interactive response renders.
