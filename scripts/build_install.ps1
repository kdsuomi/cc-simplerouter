$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot
$outDir = Join-Path $repo "bin"
$exe = Join-Path $outDir "simplerouter.exe"

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go) {
    foreach ($candidate in @(
        "$env:ProgramFiles\Go\bin\go.exe",
        "$env:LOCALAPPDATA\Programs\Go\bin\go.exe",
        "$HOME\go\bin\go.exe",
        "C:\Go\bin\go.exe"
    )) {
        if (Test-Path $candidate) { $go = $candidate; break }
    }
}
if (-not $go) {
    throw "Could not find 'go'. Install Go from https://go.dev/dl/ or open a new terminal so PATH refreshes, then rerun this script."
}

New-Item -ItemType Directory -Force -Path $outDir | Out-Null
Push-Location $repo
try {
    & $go build -buildvcs=false -o $exe ./cmd/simplerouter
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

$installDir = Join-Path $HOME ".local\bin"
New-Item -ItemType Directory -Force -Path $installDir | Out-Null
$installedExe = Join-Path $installDir "simplerouter.exe"
try {
    Copy-Item -Force $exe $installedExe
} catch [System.IO.IOException] {
    throw "Could not replace $installedExe because it is in use. Close any running simplerouter-launched Codex session, then rerun this script."
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }
$entries = $userPath.Split(";") | Where-Object { $_ -ne "" }
if ($entries -notcontains $installDir) {
    $newPath = if ($userPath) { "$userPath;$installDir" } else { $installDir }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "Added $installDir to the user PATH."
    Write-Host "Open a new terminal for the change to take effect."
}

Write-Host "Built and installed simplerouter to $installDir"
