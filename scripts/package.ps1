param(
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot

if (-not $Version) {
    $Version = "dev"
    $tag = git -C $repo describe --tags --exact-match 2>$null
    if ($LASTEXITCODE -eq 0 -and $tag) {
        $Version = $tag.Trim()
    }
}

if ($Version -match '[\\/]' -or $Version -eq "." -or $Version -eq "..") {
    throw "Version must be a simple name like v0.1.0; path separators are not allowed."
}

$distRoot = Join-Path $repo "dist"
$dist = Join-Path $distRoot $Version
if (Test-Path $dist) {
    Remove-Item -Recurse -Force $dist
}
New-Item -ItemType Directory -Force -Path $dist | Out-Null

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go) {
    throw "Could not find 'go' on PATH. Install Go from https://go.dev/dl/ and rerun this script."
}

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "simplerouter_windows_amd64.exe" },
    @{ GOOS = "windows"; GOARCH = "arm64"; Name = "simplerouter_windows_arm64.exe" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; Name = "simplerouter_darwin_arm64" },
    @{ GOOS = "linux";   GOARCH = "amd64"; Name = "simplerouter_linux_amd64" },
    @{ GOOS = "linux";   GOARCH = "arm64"; Name = "simplerouter_linux_arm64" }
)

$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED

Push-Location $repo
try {
    foreach ($target in $targets) {
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $env:CGO_ENABLED = "0"

        $out = Join-Path $dist $target.Name
        Write-Host "Building $($target.Name)"
        & $go build -trimpath -buildvcs=false -ldflags="-s -w" -o $out ./cmd/simplerouter
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed for $($target.GOOS)/$($target.GOARCH) with exit code $LASTEXITCODE"
        }
    }
} finally {
    Pop-Location
    if ($null -eq $oldGoos) { Remove-Item Env:\GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGoos }
    if ($null -eq $oldGoarch) { Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGoarch }
    if ($null -eq $oldCgo) { Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCgo }
}

$checksumPath = Join-Path $dist "checksums.txt"
Get-ChildItem -File $dist |
    Where-Object { $_.Name -ne "checksums.txt" } |
    Sort-Object Name |
    ForEach-Object {
        $hash = Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName
        "$($hash.Hash.ToLowerInvariant())  $($_.Name)"
    } |
    Set-Content -Encoding ascii $checksumPath

Write-Host ""
Write-Host "Release artifacts written to $dist"
Write-Host "Upload these files to the matching GitHub Release."
