# Cross-compile rmgk-lobby for Windows and Linux from one host.
#
# Usage:
#   .\build.ps1                  # builds both platforms (default)
#   .\build.ps1 -Os linux        # Linux only
#   .\build.ps1 -Os windows      # Windows only
#   .\build.ps1 -Arch arm64      # build for ARM64 instead of amd64
#   .\build.ps1 -NoStrip         # keep debug symbols (debugging)
#
# Output: bin/<os>/<binary>[.exe]
# CGO is force-disabled — produces static binaries, no glibc dependency.

param(
    [ValidateSet("all", "windows", "linux")]
    [string]$Os = "all",

    [ValidateSet("amd64", "arm64")]
    [string]$Arch = "amd64",

    [switch]$NoStrip
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$binaries = @("lobby-server", "smoke-client")
$ldflags  = if ($NoStrip) { "" } else { "-s -w" }

$targets = @()
if ($Os -in @("windows", "all")) { $targets += @{ GOOS = "windows"; Suffix = ".exe" } }
if ($Os -in @("linux",   "all")) { $targets += @{ GOOS = "linux";   Suffix = ""     } }

# Snapshot any pre-existing env so this script doesn't taint the caller's shell.
$savedGoos       = $env:GOOS
$savedGoarch     = $env:GOARCH
$savedCgoEnabled = $env:CGO_ENABLED

try {
    foreach ($t in $targets) {
        $outDir = Join-Path "bin" $t.GOOS
        New-Item -ItemType Directory -Force -Path $outDir | Out-Null

        $env:GOOS        = $t.GOOS
        $env:GOARCH      = $Arch
        $env:CGO_ENABLED = "0"

        foreach ($bin in $binaries) {
            $outPath = Join-Path $outDir ($bin + $t.Suffix)
            Write-Host "→ $($t.GOOS)/$Arch  $outPath"

            $buildArgs = @("build", "-trimpath")
            if ($ldflags) { $buildArgs += "-ldflags=$ldflags" }
            $buildArgs += @("-o", $outPath, "./cmd/$bin")

            & go @buildArgs
            if ($LASTEXITCODE -ne 0) { throw "go build failed: $bin on $($t.GOOS)/$Arch" }
        }
    }
}
finally {
    if ($null -ne $savedGoos)       { $env:GOOS = $savedGoos }       else { Remove-Item Env:GOOS       -ErrorAction SilentlyContinue }
    if ($null -ne $savedGoarch)     { $env:GOARCH = $savedGoarch }   else { Remove-Item Env:GOARCH     -ErrorAction SilentlyContinue }
    if ($null -ne $savedCgoEnabled) { $env:CGO_ENABLED = $savedCgoEnabled } else { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue }
}

Write-Host ""
Write-Host "Done. Artifacts:"
Get-ChildItem -Recurse bin -File | ForEach-Object {
    $sizeMb = "{0:N2}" -f ($_.Length / 1MB)
    Write-Host "  $($_.FullName)  ($sizeMb MB)"
}
