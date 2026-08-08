param(
    [string]$Version = "latest",
    [string]$Repo = "kdsuomi/cc-simplerouter",
    [string]$InstallDir = (Join-Path $HOME ".local\bin")
)

$ErrorActionPreference = "Stop"

function Get-MachineArch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        "ARM64" { return "arm64" }
        "AMD64" { return "amd64" }
        default {
            if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq [System.Runtime.InteropServices.Architecture]::Arm64) {
                return "arm64"
            }
            return "amd64"
        }
    }
}

if (-not $IsWindows -and $PSVersionTable.PSEdition -eq "Core") {
    throw "scripts/install.ps1 installs the Windows binary. Use scripts/install.sh on macOS or Linux."
}

$arch = Get-MachineArch
$asset = "simplerouter_windows_$arch.exe"

if ($Version -eq "latest") {
    $url = "https://github.com/$Repo/releases/latest/download/$asset"
} else {
    $url = "https://github.com/$Repo/releases/download/$Version/$asset"
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$dest = Join-Path $InstallDir "simplerouter.exe"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "$asset.tmp"

Write-Host "Downloading $asset from $Repo ($Version)"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $tmp
Move-Item -Force $tmp $dest

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }

$entries = $userPath.Split(";") | Where-Object { $_ -ne "" }
if ($entries -notcontains $InstallDir) {
    $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "Added $InstallDir to the user PATH."
    Write-Host "Open a new terminal for the PATH change to take effect."
}

Write-Host "Installed simplerouter to $dest"
