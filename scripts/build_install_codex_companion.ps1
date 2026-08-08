param(
    [string]$CodexSource = (Join-Path (Split-Path -Parent $PSScriptRoot) ".build\codex-rust-v0.147.0\codex-rs"),
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
$defaultSourceRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot ".build\codex-rust-v0.147.0"))
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

$rustc = Join-Path (Split-Path -Parent $cargo) "rustc.exe"
$target = "unknown"
if (Test-Path -LiteralPath $rustc -PathType Leaf) {
    $targetMatch = & $rustc -vV | Select-String -Pattern '^host: (.+)$'
    if ($targetMatch) {
        $target = $targetMatch.Matches.Groups[1].Value
    }
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

$officialPackage = if ($env:SIMPLEROUTER_CODEX_OFFICIAL_PACKAGE) {
    $env:SIMPLEROUTER_CODEX_OFFICIAL_PACKAGE
} else {
    $standaloneRoot = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".codex\packages\standalone"
    $pinnedOfficialPackage = Join-Path $standaloneRoot "releases\0.147.0-$target"
    if (Test-Path -LiteralPath $pinnedOfficialPackage -PathType Container) {
        $pinnedOfficialPackage
    } else {
        Join-Path $standaloneRoot "current"
    }
}
$officialCodeModeHost = Join-Path $officialPackage "bin\codex-code-mode-host.exe"
$buildCodeModeHost = -not (Test-Path -LiteralPath $officialCodeModeHost -PathType Leaf)

if (-not $SkipBuild) {
    New-Item -ItemType Directory -Force -Path $targetRoot | Out-Null
    Push-Location $codexSourcePath
    try {
        $buildArguments = @(
            "build", "--locked", "--target-dir", $targetRoot, "--profile", $Profile,
            "--package", "codex-cli", "--bin", "codex",
            "--package", "codex-windows-sandbox", "--bin", "codex-command-runner", "--bin", "codex-windows-sandbox-setup"
        )
        if ($buildCodeModeHost) {
            $buildArguments += @("--package", "codex-code-mode-host", "--bin", "codex-code-mode-host")
        }
        & $cargo @buildArguments
        if ($LASTEXITCODE -ne 0) {
            throw "Codex companion build failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
}
$outputDir = Join-Path $targetRoot $Profile
$codeModeHostSource = if ($buildCodeModeHost) {
    Join-Path $outputDir "codex-code-mode-host.exe"
} else {
    $officialCodeModeHost
}
$sources = @(
    @{ Source = (Join-Path $outputDir "codex.exe"); Destination = "bin\codex.exe" },
    @{ Source = (Join-Path $outputDir "codex.exe"); Destination = "bin\codex-simplerouter.exe" },
    @{ Source = $codeModeHostSource; Destination = "bin\codex-code-mode-host.exe" },
    @{ Source = (Join-Path $outputDir "codex-command-runner.exe"); Destination = "codex-resources\codex-command-runner.exe" },
    @{ Source = (Join-Path $outputDir "codex-windows-sandbox-setup.exe"); Destination = "codex-resources\codex-windows-sandbox-setup.exe" }
)

$rgSource = Join-Path $officialPackage "codex-path\rg.exe"
if (-not (Test-Path -LiteralPath $rgSource -PathType Leaf)) {
    $rgCommand = Get-Command rg.exe -ErrorAction SilentlyContinue
    $rgSource = if ($rgCommand) { $rgCommand.Source } else { "" }
}
if (-not $rgSource -or -not (Test-Path -LiteralPath $rgSource -PathType Leaf)) {
    throw "Could not find rg.exe for the canonical Codex package."
}
$sources += @{ Source = $rgSource; Destination = "codex-path\rg.exe" }

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

$metadata = [ordered]@{
    layoutVersion = 1
    version = "0.147.0"
    target = $target
    variant = "codex"
    entrypoint = "bin/codex.exe"
    resourcesDir = "codex-resources"
    pathDir = "codex-path"
}
$metadataPath = Join-Path $InstallRoot "codex-package.json"
New-Item -ItemType Directory -Force -Path $InstallRoot | Out-Null
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($metadataPath, ($metadata | ConvertTo-Json), $utf8NoBom)

Write-Host "Installed canonical SimpleRouter Codex companion bundle to $InstallRoot"
Write-Host "  bin\codex.exe"
Write-Host "  bin\codex-simplerouter.exe"
Write-Host "  bin\codex-code-mode-host.exe"
Write-Host "  codex-path\rg.exe"
Write-Host "  codex-resources\codex-command-runner.exe"
Write-Host "  codex-resources\codex-windows-sandbox-setup.exe"
