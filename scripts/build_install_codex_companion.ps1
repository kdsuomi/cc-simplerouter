param(
    [string]$CodexSource = (Join-Path (Split-Path -Parent $PSScriptRoot) ".build\codex-rust-v0.145.0\codex-rs"),
    [string]$CargoTarget = "",
    [string]$InstallRoot = (Join-Path ([Environment]::GetFolderPath("UserProfile")) ".local\share\simplerouter\simplerouter-codex"),
    [ValidateSet("dev-small", "release")]
    [string]$Profile = "dev-small",
    [switch]$SkipBuild,
    [switch]$RefreshSource
)

$ErrorActionPreference = "Stop"

if (-not ($IsWindows -or $env:OS -eq "Windows_NT")) {
    throw "This script builds the Windows Codex companion bundle."
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$defaultSourceRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot ".build\codex-rust-v0.145.0"))
$defaultCodexSource = Join-Path $defaultSourceRoot "codex-rs"
$codexSourcePath = [System.IO.Path]::GetFullPath($CodexSource)
$cargoManifest = Join-Path $codexSourcePath "Cargo.toml"

if ($RefreshSource -and $codexSourcePath -ne $defaultCodexSource) {
    throw "-RefreshSource can only be used with the default generated Codex source."
}
if ($codexSourcePath -eq $defaultCodexSource) {
    & (Join-Path $PSScriptRoot "prepare_codex_companion.ps1") -SourceRoot $defaultSourceRoot -Refresh:$RefreshSource
}
if (-not (Test-Path -LiteralPath $cargoManifest -PathType Leaf)) {
    throw "Codex Rust workspace not found at $codexSourcePath"
}

# sqlx migration checksums include line endings. Official Windows Codex state
# databases record CRLF checksums, so never install a companion built from LF
# migration files, including when the caller supplies a custom source checkout.
$migrationFiles = @(Get-ChildItem -LiteralPath (Join-Path $codexSourcePath "state") -Filter "*.sql" -File -Recurse)
if ($migrationFiles.Count -eq 0) {
    throw "No Codex state migration files found under $codexSourcePath\state"
}
foreach ($migration in $migrationFiles) {
    $bytes = [System.IO.File]::ReadAllBytes($migration.FullName)
    for ($index = 0; $index -lt $bytes.Length; $index++) {
        if ($bytes[$index] -eq 10 -and ($index -eq 0 -or $bytes[$index - 1] -ne 13)) {
            throw "Codex Windows companion migration uses LF line endings: $($migration.FullName). Use a CRLF checkout; do not repair the user's SQLite migration ledger."
        }
    }
}

$cargo = (Get-Command cargo -ErrorAction SilentlyContinue).Source
if (-not $cargo) {
    $cargo = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".cargo\bin\cargo.exe"
}
if (-not (Test-Path -LiteralPath $cargo -PathType Leaf)) {
    throw "Could not find cargo. Install Rust or open a terminal with cargo on PATH."
}

$targetRoot = if ($CargoTarget) {
    [System.IO.Path]::GetFullPath($CargoTarget)
} elseif ($env:CARGO_TARGET_DIR) {
    if ([System.IO.Path]::IsPathRooted($env:CARGO_TARGET_DIR)) {
        [System.IO.Path]::GetFullPath($env:CARGO_TARGET_DIR)
    } else {
        [System.IO.Path]::GetFullPath((Join-Path $codexSourcePath $env:CARGO_TARGET_DIR))
    }
} else {
    [System.IO.Path]::GetFullPath((Join-Path $repoRoot ".build\codex-target"))
}

if (-not $SkipBuild) {
    New-Item -ItemType Directory -Force -Path $targetRoot | Out-Null
    Push-Location $codexSourcePath
    try {
        & $cargo build --target-dir $targetRoot --profile $Profile `
            --package codex-cli --bin codex `
            --package codex-windows-sandbox --bin codex-command-runner --bin codex-windows-sandbox-setup
        if ($LASTEXITCODE -ne 0) {
            throw "Codex companion build failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
}
$outputDir = Join-Path $targetRoot $Profile
$sources = @(
    @{ Source = (Join-Path $outputDir "codex.exe"); Destination = "bin\codex-simplerouter.exe" },
    @{ Source = (Join-Path $outputDir "codex-command-runner.exe"); Destination = "codex-resources\codex-command-runner.exe" },
    @{ Source = (Join-Path $outputDir "codex-windows-sandbox-setup.exe"); Destination = "codex-resources\codex-windows-sandbox-setup.exe" }
)

foreach ($file in $sources) {
    if (-not (Test-Path -LiteralPath $file.Source -PathType Leaf)) {
        throw "Missing build output: $($file.Source)"
    }
}

foreach ($file in $sources) {
    $destination = Join-Path $InstallRoot $file.Destination
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $destination) | Out-Null
    Copy-Item -LiteralPath $file.Source -Destination $destination -Force
    $sourceHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $file.Source).Hash
    $destinationHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $destination).Hash
    if ($sourceHash -ne $destinationHash) {
        throw "Installed file verification failed: $destination"
    }
}

Write-Host "Installed SimpleRouter Codex companion bundle to $InstallRoot"
Write-Host "  bin\codex-simplerouter.exe"
Write-Host "  codex-resources\codex-command-runner.exe"
Write-Host "  codex-resources\codex-windows-sandbox-setup.exe"
