# Patched Codex companion

The companion binary is OpenAI Codex `rust-v0.145.0` plus the ordered patch
series in `patches/0.145.0`. Those patch files—not a generated Codex checkout—
are the source of truth committed to this repository.

Prepare a clean checkout and verify its exact resulting Git tree:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\prepare_codex_companion.ps1
```

The checkout lives under ignored `.build/`. Build and install the companion
bundle with:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_install_codex_companion.ps1
```

Cargo outputs live separately in `.build/codex-target`, so refreshing the
upstream checkout does not discard incremental build artifacts. Pass
`-SkipBuild` to reinstall existing outputs or `-RefreshSource` to recreate and
repatch the generated checkout.

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
the complete series from `rust-v0.145.0` with `git format-patch --no-signature`,
replace the files in `patches/0.145.0`, and update the expected tree in
`prepare_codex_companion.ps1`. A clean preparation must reproduce that tree
before the root repository is committed.
