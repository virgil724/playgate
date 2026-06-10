# PlayGate cross-compilation script for Windows (PowerShell equivalent of Makefile)
# ─────────────────────────────────────────────────────────────────────────────
# Usage:
#   .\build.ps1                          # build all four binaries
#   .\build.ps1 -Target host-linux-arm64
#   .\build.ps1 -Target host-linux-amd64
#   .\build.ps1 -Target server-linux-amd64
#   .\build.ps1 -Target server-linux-arm64
#   .\build.ps1 -Target test             # run Go + Vitest tests
#   .\build.ps1 -Target clean
# ─────────────────────────────────────────────────────────────────────────────
[CmdletBinding()]
param(
    [string]$Target = "all",
    [string]$Version = ""
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ── Resolve version ──────────────────────────────────────────────────────────
if (-not $Version) {
    try {
        $Version = (git describe --tags --always --dirty 2>$null).Trim()
    } catch {}
    if (-not $Version) { $Version = "dev" }
}
try {
    $Commit = (git rev-parse --short HEAD 2>$null).Trim()
} catch { $Commit = "unknown" }
$BuildDate = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")

$Ldflags = "-s -w -X main.Version=$Version -X main.Commit=$Commit -X main.BuildDate=$BuildDate"

$Root      = $PSScriptRoot
$DistDir   = Join-Path $Root "dist"
$HostDir   = Join-Path $Root "playgate-host"
$ServerDir = Join-Path $Root "playgate-server"
$WebDir    = Join-Path $Root "playgate-web"

function Ensure-Dist {
    if (-not (Test-Path $DistDir)) {
        New-Item -ItemType Directory -Path $DistDir | Out-Null
    }
}

function Set-CrossEnv([string]$goos, [string]$goarch) {
    $env:GOOS        = $goos
    $env:GOARCH      = $goarch
    $env:CGO_ENABLED = "0"
}

function Clear-CrossEnv {
    Remove-Item Env:\GOOS        -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH      -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
}

function Build-HostLinuxArm64 {
    Ensure-Dist
    Write-Host "==> host linux/arm64" -ForegroundColor Cyan
    Set-CrossEnv "linux" "arm64"
    try {
        $out = Join-Path $DistDir "playgate-host-linux-arm64"
        go -C $HostDir build -trimpath -ldflags $Ldflags -o $out ./cmd/host
        if ($LASTEXITCODE -ne 0) { throw "go build failed" }
        Write-Host "    => $out" -ForegroundColor Green
    } finally { Clear-CrossEnv }
}

function Build-HostLinuxAmd64 {
    Ensure-Dist
    Write-Host "==> host linux/amd64" -ForegroundColor Cyan
    Set-CrossEnv "linux" "amd64"
    try {
        $out = Join-Path $DistDir "playgate-host-linux-amd64"
        go -C $HostDir build -trimpath -ldflags $Ldflags -o $out ./cmd/host
        if ($LASTEXITCODE -ne 0) { throw "go build failed" }
        Write-Host "    => $out" -ForegroundColor Green
    } finally { Clear-CrossEnv }
}

function Build-ServerLinuxAmd64 {
    Ensure-Dist
    Write-Host "==> server linux/amd64" -ForegroundColor Cyan
    Set-CrossEnv "linux" "amd64"
    try {
        $out = Join-Path $DistDir "playgate-server-linux-amd64"
        go -C $ServerDir build -trimpath -ldflags $Ldflags -o $out .
        if ($LASTEXITCODE -ne 0) { throw "go build failed" }
        Write-Host "    => $out" -ForegroundColor Green
    } finally { Clear-CrossEnv }
}

function Build-ServerLinuxArm64 {
    Ensure-Dist
    Write-Host "==> server linux/arm64" -ForegroundColor Cyan
    Set-CrossEnv "linux" "arm64"
    try {
        $out = Join-Path $DistDir "playgate-server-linux-arm64"
        go -C $ServerDir build -trimpath -ldflags $Ldflags -o $out .
        if ($LASTEXITCODE -ne 0) { throw "go build failed" }
        Write-Host "    => $out" -ForegroundColor Green
    } finally { Clear-CrossEnv }
}

function Run-Tests {
    Write-Host "==> go test playgate-host" -ForegroundColor Cyan
    go -C $HostDir test ./...
    if ($LASTEXITCODE -ne 0) { throw "playgate-host tests failed" }

    Write-Host "==> go test playgate-server" -ForegroundColor Cyan
    go -C $ServerDir test ./...
    if ($LASTEXITCODE -ne 0) { throw "playgate-server tests failed" }

    Write-Host "==> vitest playgate-web" -ForegroundColor Cyan
    Push-Location $WebDir
    try {
        npm run test
        if ($LASTEXITCODE -ne 0) { throw "vitest failed" }
    } finally { Pop-Location }
}

function Do-Clean {
    if (Test-Path $DistDir) {
        Remove-Item -Recurse -Force $DistDir
        Write-Host "Removed $DistDir" -ForegroundColor Yellow
    }
}

# ── Dispatch ─────────────────────────────────────────────────────────────────
switch ($Target) {
    "all" {
        Build-HostLinuxArm64
        Build-HostLinuxAmd64
        Build-ServerLinuxAmd64
        Build-ServerLinuxArm64
    }
    "host-linux-arm64"    { Build-HostLinuxArm64 }
    "host-linux-amd64"    { Build-HostLinuxAmd64 }
    "server-linux-amd64"  { Build-ServerLinuxAmd64 }
    "server-linux-arm64"  { Build-ServerLinuxArm64 }
    "test"                { Run-Tests }
    "clean"               { Do-Clean }
    default {
        Write-Error "Unknown target: $Target. Valid: all, host-linux-arm64, host-linux-amd64, server-linux-amd64, server-linux-arm64, test, clean"
    }
}

Write-Host "Done." -ForegroundColor Green
