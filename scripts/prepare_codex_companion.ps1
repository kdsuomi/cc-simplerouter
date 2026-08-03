param(
    [string]$SourceRoot = (Join-Path (Split-Path -Parent $PSScriptRoot) ".build\codex-rust-v0.145.0"),
    [string]$Repository = "https://github.com/openai/codex.git",
    [switch]$Refresh
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$upstreamTag = "rust-v0.145.0"
$upstreamCommit = "25af12f7e61572b0bc18ddb1008be543b91519b0"
$expectedTree = "7af7d58073477d66405b71a76fbb54df3d830a4d"
$patchRoot = Join-Path $repoRoot "codex\patches\0.145.0"
$defaultBuildRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot ".build"))
$sourcePath = [System.IO.Path]::GetFullPath($SourceRoot)

$git = (Get-Command git -ErrorAction SilentlyContinue).Source
if (-not $git) {
    throw "Could not find git on PATH."
}

$patches = @(Get-ChildItem -LiteralPath $patchRoot -Filter "*.patch" -File | Sort-Object Name)
if ($patches.Count -eq 0) {
    throw "No Codex patches found under $patchRoot."
}

function Test-PreparedSource {
    if (-not (Test-Path -LiteralPath (Join-Path $sourcePath ".git") -PathType Container)) {
        return $false
    }

    $tree = (& $git -C $sourcePath rev-parse --verify 'HEAD^{tree}' 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $tree) {
        return $false
    }

    & $git -C $sourcePath diff --quiet
    if ($LASTEXITCODE -ne 0) {
        return $false
    }
    & $git -C $sourcePath diff --cached --quiet
    if ($LASTEXITCODE -ne 0) {
        return $false
    }

    return $tree.Trim() -eq $expectedTree
}

if (-not $Refresh -and (Test-PreparedSource)) {
    Write-Host "Codex companion source is already prepared at $sourcePath"
    return
}

if (Test-Path -LiteralPath $sourcePath) {
    if (-not $Refresh) {
        throw "Codex source at $sourcePath is not the expected clean patched tree. Inspect it, or rerun with -Refresh."
    }

    $buildPrefix = $defaultBuildRoot.TrimEnd([char[]]"\/") + [System.IO.Path]::DirectorySeparatorChar
    if (-not $sourcePath.StartsWith($buildPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "For safety, -Refresh only removes generated checkouts beneath $defaultBuildRoot"
    }
    Remove-Item -LiteralPath $sourcePath -Recurse -Force
}

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $sourcePath) | Out-Null
& $git clone --branch $upstreamTag --depth 1 --single-branch --config core.autocrlf=false $Repository $sourcePath
if ($LASTEXITCODE -ne 0) {
    throw "Could not clone $Repository at $upstreamTag"
}

$actualBase = (& $git -C $sourcePath rev-parse --verify HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $actualBase -ne $upstreamCommit) {
    throw "Upstream tag $upstreamTag resolved to $actualBase; expected $upstreamCommit"
}

foreach ($patch in $patches) {
    & $git -c user.name=SimpleRouter -c user.email=simplerouter@local -C $sourcePath am --whitespace=nowarn $patch.FullName
    if ($LASTEXITCODE -ne 0) {
        & $git -C $sourcePath am --abort 2>$null
        throw "Could not apply $($patch.Name)"
    }
}

$actualTree = (& $git -C $sourcePath rev-parse --verify 'HEAD^{tree}').Trim()
if ($LASTEXITCODE -ne 0 -or $actualTree -ne $expectedTree) {
    throw "Patched Codex tree is $actualTree; expected $expectedTree"
}
if (-not (Test-PreparedSource)) {
    throw "Prepared Codex checkout is not clean."
}

@(
    "tag=$upstreamTag"
    "base=$upstreamCommit"
    "tree=$expectedTree"
) | Set-Content -LiteralPath (Join-Path $sourcePath ".simplerouter-patchset") -Encoding ASCII

Write-Host "Prepared Codex companion source at $sourcePath"
Write-Host "  upstream: $upstreamTag ($upstreamCommit)"
Write-Host "  patched tree: $expectedTree"
