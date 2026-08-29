param(
    [string]$SourceRoot = (Join-Path (Split-Path -Parent $PSScriptRoot) ".build\codex-rust-v0.147.0"),
    [string]$Repository = "https://github.com/openai/codex.git",
    [switch]$Refresh
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$upstreamTag = "rust-v0.147.0"
$upstreamCommit = "be6e8eac029b183056b7e4402879f15d2c85f61b"
$expectedTree = "b8f1c0c2f39dbfca62b23c6327ddbf94c0a6562d"
$patchRoot = Join-Path $repoRoot "codex\patches\0.147.0"
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

    $autoCrlf = (& $git -C $sourcePath config --bool core.autocrlf 2>$null)
    if ($LASTEXITCODE -ne 0 -or $autoCrlf.Trim() -ne "true") {
        return $false
    }

    $migrationFiles = @(Get-ChildItem -LiteralPath (Join-Path $sourcePath "codex-rs\state") -Filter "*.sql" -File -Recurse)
    if ($migrationFiles.Count -eq 0) {
        return $false
    }
    foreach ($migration in $migrationFiles) {
        $bytes = [System.IO.File]::ReadAllBytes($migration.FullName)
        for ($index = 0; $index -lt $bytes.Length; $index++) {
            if ($bytes[$index] -eq 10 -and ($index -eq 0 -or $bytes[$index - 1] -ne 13)) {
                return $false
            }
        }
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
# sqlx hashes the raw migration bytes embedded at compile time. The official
# Windows Codex build uses CRLF migrations, so the Windows companion must do the
# same or it cannot open the user's existing state databases.
& $git clone --branch $upstreamTag --depth 1 --single-branch --config core.autocrlf=true $Repository $sourcePath
if ($LASTEXITCODE -ne 0) {
    throw "Could not clone $Repository at $upstreamTag"
}

# Make the migration line endings explicit instead of relying only on Git's
# text-file heuristic. info/attributes affects the generated checkout without
# changing the pinned upstream tree or the exported SimpleRouter patch series.
$infoAttributes = Join-Path $sourcePath ".git\info\attributes"
@(
    "# SimpleRouter Windows companion build invariant"
    "codex-rs/state/**/*.sql text eol=crlf"
) | Set-Content -LiteralPath $infoAttributes -Encoding ASCII
& $git -C $sourcePath checkout --force HEAD -- "codex-rs/state"
if ($LASTEXITCODE -ne 0) {
    throw "Could not materialize Codex state migrations with CRLF line endings"
}
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$migrationFiles = @(Get-ChildItem -LiteralPath (Join-Path $sourcePath "codex-rs\state") -Filter "*.sql" -File -Recurse)
foreach ($migration in $migrationFiles) {
    $content = [System.IO.File]::ReadAllText($migration.FullName)
    $crlfContent = $content.Replace("`r`n", "`n").Replace("`n", "`r`n")
    [System.IO.File]::WriteAllText($migration.FullName, $crlfContent, $utf8NoBom)
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
