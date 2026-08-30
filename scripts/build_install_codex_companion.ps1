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
$codexVersion = "0.147.0"
$officialSignerName = "OpenAI OpCo, LLC"
$officialCodeModeHostAssets = @{
    "x86_64-pc-windows-msvc" = @{
        Sha256 = "37c23a542037e1bcfd0fa7eb4a150c697229d7ff31bf675c519d5bff7226b191"
        Size = 57450288
    }
    "aarch64-pc-windows-msvc" = @{
        Sha256 = "d322d6d721cf7f7ae523bfe31a504875611ec21bbf9b2bffca4b9fd30bdb1675"
        Size = 54304560
    }
}
$defaultSourceRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot ".build\codex-rust-v$codexVersion"))
$defaultCodexSource = Join-Path $defaultSourceRoot "codex-rs"
$codexSourcePath = [System.IO.Path]::GetFullPath($CodexSource)
$cargoManifest = Join-Path $codexSourcePath "Cargo.toml"

function Assert-OfficialCodeModeHost {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,
        [string]$ExpectedSha256 = "",
        [long]$ExpectedSize = 0
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Official Codex code-mode host not found at $Path"
    }

    if ($ExpectedSize -gt 0) {
        $actualSize = (Get-Item -LiteralPath $Path).Length
        if ($actualSize -ne $ExpectedSize) {
            throw "Official Codex code-mode host at $Path has size $actualSize; expected $ExpectedSize"
        }
    }

    if ($ExpectedSha256) {
        $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash
        if (-not $actualHash.Equals($ExpectedSha256, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Official Codex code-mode host at $Path has SHA-256 $actualHash; expected $ExpectedSha256"
        }
    }

    $signature = Get-AuthenticodeSignature -LiteralPath $Path
    if ($signature.Status -ne "Valid" -or -not $signature.SignerCertificate) {
        throw "Official Codex code-mode host at $Path has Authenticode status $($signature.Status)"
    }
    $signerName = $signature.SignerCertificate.GetNameInfo(
        [System.Security.Cryptography.X509Certificates.X509NameType]::SimpleName,
        $false
    )
    if ($signerName -ne $officialSignerName) {
        throw "Official Codex code-mode host at $Path is signed by '$signerName'; expected '$officialSignerName'"
    }
}

function Get-OfficialCodeModeHost {
    param(
        [string]$PackageRoot,
        [string]$Target
    )

    if ($PackageRoot) {
        $packageHost = Join-Path $PackageRoot "bin\codex-code-mode-host.exe"
        if (Test-Path -LiteralPath $packageHost -PathType Leaf) {
            try {
                Assert-OfficialCodeModeHost -Path $packageHost
                Write-Host "Reusing signed official Codex code-mode host from $packageHost"
                return $packageHost
            } catch {
                Write-Warning "Ignoring unusable official Codex code-mode host: $($_.Exception.Message)"
            }
        }
    }

    $asset = $officialCodeModeHostAssets[$Target]
    if (-not $asset) {
        $supportedTargets = ($officialCodeModeHostAssets.Keys | Sort-Object) -join ", "
        throw "No pinned official Codex code-mode host for target $Target. Supported targets: $supportedTargets"
    }

    $assetName = "codex-code-mode-host-$Target.exe"
    $cacheRoot = Join-Path $repoRoot ".build\official-codex-$codexVersion-$Target"
    $cachedHost = Join-Path $cacheRoot "bin\codex-code-mode-host.exe"
    if (Test-Path -LiteralPath $cachedHost -PathType Leaf) {
        try {
            Assert-OfficialCodeModeHost -Path $cachedHost -ExpectedSha256 $asset.Sha256 -ExpectedSize $asset.Size
            Write-Host "Reusing verified official Codex code-mode host from $cachedHost"
            return $cachedHost
        } catch {
            Write-Warning "Replacing invalid cached Codex code-mode host: $($_.Exception.Message)"
            Remove-Item -LiteralPath $cachedHost -Force
        }
    }

    $downloadUrl = "https://github.com/openai/codex/releases/download/rust-v$codexVersion/$assetName"
    $downloadPath = "$cachedHost.download.$PID"
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $cachedHost) | Out-Null
    try {
        if (Test-Path -LiteralPath $downloadPath) {
            Remove-Item -LiteralPath $downloadPath -Force
        }
        Write-Host "Downloading signed official Codex code-mode host from $downloadUrl"
        $savedProgressPreference = $ProgressPreference
        try {
            $ProgressPreference = "SilentlyContinue"
            [Net.ServicePointManager]::SecurityProtocol =
                [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
            Invoke-WebRequest -UseBasicParsing -Uri $downloadUrl -OutFile $downloadPath
        } finally {
            $ProgressPreference = $savedProgressPreference
        }
        Assert-OfficialCodeModeHost -Path $downloadPath -ExpectedSha256 $asset.Sha256 -ExpectedSize $asset.Size
        Move-Item -LiteralPath $downloadPath -Destination $cachedHost -Force
    } finally {
        if (Test-Path -LiteralPath $downloadPath) {
            Remove-Item -LiteralPath $downloadPath -Force
        }
    }

    Write-Host "Cached verified official Codex code-mode host at $cachedHost"
    return $cachedHost
}

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

$officialPackage = ""
$officialCodeModeHostPackage = ""
if ($env:SIMPLEROUTER_CODEX_OFFICIAL_PACKAGE) {
    $officialPackage = $env:SIMPLEROUTER_CODEX_OFFICIAL_PACKAGE
    $officialCodeModeHostPackage = $officialPackage
} else {
    $standaloneRoot = Join-Path ([Environment]::GetFolderPath("UserProfile")) ".codex\packages\standalone"
    $pinnedOfficialPackage = Join-Path $standaloneRoot "releases\$codexVersion-$target"
    if (Test-Path -LiteralPath $pinnedOfficialPackage -PathType Container) {
        $officialPackage = $pinnedOfficialPackage
        $officialCodeModeHostPackage = $pinnedOfficialPackage
    } else {
        $currentOfficialPackage = Join-Path $standaloneRoot "current"
        if (Test-Path -LiteralPath $currentOfficialPackage -PathType Container) {
            # Current may be a different Codex version, so only reuse its
            # version-independent resources such as rg.exe.
            $officialPackage = $currentOfficialPackage
        }
    }
}
$codeModeHostSource = Get-OfficialCodeModeHost -PackageRoot $officialCodeModeHostPackage -Target $target

if (-not $SkipBuild) {
    New-Item -ItemType Directory -Force -Path $targetRoot | Out-Null
    Push-Location $codexSourcePath
    try {
        $buildArguments = @(
            "build", "--locked", "--target-dir", $targetRoot, "--profile", $Profile,
            "--package", "codex-cli", "--bin", "codex",
            "--package", "codex-windows-sandbox", "--bin", "codex-command-runner", "--bin", "codex-windows-sandbox-setup"
        )
        & $cargo @buildArguments
        if ($LASTEXITCODE -ne 0) {
            throw "Codex companion build failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
}
$outputDir = Join-Path $targetRoot $Profile
$sources = @(
    @{ Source = (Join-Path $outputDir "codex.exe"); Destination = "bin\codex.exe" },
    @{ Source = (Join-Path $outputDir "codex.exe"); Destination = "bin\codex-simplerouter.exe" },
    @{ Source = $codeModeHostSource; Destination = "bin\codex-code-mode-host.exe" },
    @{ Source = (Join-Path $outputDir "codex-command-runner.exe"); Destination = "codex-resources\codex-command-runner.exe" },
    @{ Source = (Join-Path $outputDir "codex-windows-sandbox-setup.exe"); Destination = "codex-resources\codex-windows-sandbox-setup.exe" }
)

$rgSource = if ($officialPackage) { Join-Path $officialPackage "codex-path\rg.exe" } else { "" }
if (-not $rgSource -or -not (Test-Path -LiteralPath $rgSource -PathType Leaf)) {
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
    version = $codexVersion
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
