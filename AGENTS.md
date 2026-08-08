# Agent build instructions

## SimpleRouter Codex companion

- For ordinary requests to build, rebuild, or install the Codex companion on
  the current machine, use Cargo's `dev-small` profile.
- Use the `release` profile only when the user explicitly asks for a release,
  production, or fully optimized build. Ambiguous requests use `dev-small`.
- Reuse the existing Cargo home, patched source checkout, and target directory.
  Do not run `cargo clean` or disable incremental compilation during iteration.
- Keep build outputs outside the generated Codex source checkout so refreshing
  or repatching the source does not discard compiled dependencies.
- Run focused `codex-tui` tests while iterating. See `codex/README.md` for the
  exact profile commands and final release workflow.
- On macOS/Linux, `sh ./scripts/build_install_codex_companion.sh` implements
  the default fast build and install. Pass `release` only under the explicit
  release-build rule above.
