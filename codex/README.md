# Patched Codex companion

The companion binary is OpenAI Codex `rust-v0.153.4` plus the ordered patch
series in `patches/0.153.4`. Those patch files—not a generated Codex checkout—
are the source of truth committed to this repository.

Live token estimates start from [measured model ratios](token-rate-defaults.md)
and continue adapting to valid usage reports during the session.

Prepare a clean checkout and verify its exact resulting Git tree:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\prepare_codex_companion.ps1
```

The checkout lives under ignored `.build/`. Build and install the companion
bundle on Windows with:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install_codex_companion.ps1
```

On macOS/Linux, after preparing the same verified source tree, run:

```sh
sh ./scripts/build_install_codex_companion.sh
```

The installer stages the canonical Codex package layout: `bin/codex`,
`bin/codex-code-mode-host`, `codex-path/rg`, the platform resources,
`codex-package.json`, and the root `codex` entrypoint. During fast iteration it
reuses the signed code-mode host from the matching installed official package
when available. Otherwise, it downloads the version-pinned OpenAI release
binary into `.build/`, verifies its SHA-256 digest and Authenticode signer, and
reuses that cache. Only the patched main CLI needs rebuilding.

On macOS, Chrome's native host rejects browser-protocol clients whose
`node_repl` process does not have an OpenAI-signed Codex parent. The package
therefore also installs `bin/node-repl-signed-parent`, and SimpleRouter injects
that launcher for Codex sessions. Its outer `:danger-full-access` selection
means "do not add a second sandbox"; `node_repl` still creates its normal
per-call sandbox for the JavaScript kernel.

Cargo outputs live separately in `.build/codex-target`, so refreshing the
upstream checkout does not discard incremental build artifacts. Pass
`-SkipBuild` to reinstall existing outputs or `-RefreshSource` to recreate and
repatch the generated checkout.

## Fast iteration builds

Agents must use the existing `dev-small` profile for ordinary requests such as
"build," "rebuild," "install on this machine," or similar. Only use the
`release` profile when the user explicitly requests a release, production, or
fully optimized build. If the request is ambiguous, default to `dev-small`.

Codex is a large workspace, and even with release LTO disabled, optimizing and
linking the final CLI can take substantially longer than compiling the changed
code. The companion PowerShell script therefore defaults to `dev-small`:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install_codex_companion.ps1 -Profile dev-small
```

For direct Cargo builds on macOS/Linux, run this from the generated
`codex-rs` checkout and keep the target directory outside that checkout:

```sh
cargo build --locked --profile dev-small \
  --target-dir ../../codex-target \
  --package codex-cli --bin codex
```

Keep reusing the same source checkout, Cargo home, and target directory. Do not
run `cargo clean` or disable incremental compilation during normal iteration.
Refreshing the generated source checkout is safe because `.build/codex-target`
is separate and remains reusable. Run focused `codex-tui` tests against that
same target directory. Make a `release` build only after the user explicitly
requests it:

```sh
just test --locked --cargo-profile dev-small -p codex-tui --lib \
  --target-dir ../../codex-target \
  -E 'test(generation_rate) | test(live_reasoning) | test(simplerouter)'
cargo build --locked --profile release \
  --target-dir ../../codex-target \
  --package codex-cli --bin codex
```

The pinned Codex workspace already tunes `release` for build throughput with
LTO disabled, incremental compilation enabled, and 16 codegen units. Preserve
those settings unless runtime benchmarks justify a different final profile.
An explicitly requested final release companion can be built and installed
with `sh ./scripts/build_install_codex_companion.sh release` or PowerShell's
`-Profile release` option.

## Windows migration line endings

The Windows companion must be built with CRLF line endings in every
`codex-rs/state/**/*.sql` migration. SQLx hashes the raw embedded migration
bytes, including line endings, and the official Windows Codex CLI records CRLF
checksums in the user's SQLite state databases. A companion built from LF
migrations treats the same SQL as a modified historical migration and refuses
to start.

`prepare_codex_companion.ps1` therefore clones with `core.autocrlf=true` and
adds a generated-checkout-only `.git/info/attributes` rule that explicitly
materializes state SQL as CRLF. `build_install_codex_companion.ps1` checks every
state migration before Cargo runs or any binary is installed. Do not change the
generated Windows checkout to `core.autocrlf=false`. If the guard reports an LF
migration, recreate the generated checkout with `-RefreshSource`; do not edit,
delete, or rewrite the user's `state_*.sqlite` migration ledger.

When changing the companion, commit the work in the generated checkout, export
the complete series from `rust-v0.153.4` with `git format-patch --no-signature`,
replace the files in `patches/0.153.4`, and update the expected tree in
`prepare_codex_companion.ps1`. A clean preparation must reproduce that tree
before the root repository is committed.
