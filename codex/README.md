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

When changing the companion, commit the work in the generated checkout, export
the complete series from `rust-v0.145.0` with `git format-patch --no-signature`,
replace the files in `patches/0.145.0`, and update the expected tree in
`prepare_codex_companion.ps1`. A clean preparation must reproduce that tree
before the root repository is committed.
