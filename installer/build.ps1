# build.ps1 - builds the native Windows MSI end to end: cross-compiles the
# three Go binaries, stages them alongside the PowerShell scripts and a
# bundled PostgreSQL, and invokes wix build to produce the final .msi.
#
# Requires on PATH (or pointed at explicitly via parameters below):
#   - go            (any recent Go toolchain; this repo currently needs 1.23+)
#   - wix           (WiX Toolset CLI, v5.x - see the licensing note below)
#
# PostgreSQL is not fetched by this script. It has to be pointed at an
# already-extracted copy of the official Windows binaries zip
# (https://www.postgresql.org/download/windows/ - the "binaries" zip, not
# the interactive installer) via -PgsqlSource, the same external dependency
# scripts/windows/setup_postgres.ps1 has always had: this repo bundles the
# scripts that provision PostgreSQL, not a redistributable copy of
# PostgreSQL itself.
#
# WiX version: pin to the 5.x line (5.0.2 as of writing), not 6 or 7.
# FireGiant, which now stewards WiX, introduced an Open Source Maintenance
# Fee EULA starting with v6 (see https://wixtoolset.org/osmf/) - accepting
# that EULA on this project's behalf is not this script's call to make, so
# it deliberately does not pass -acceptEula and expects a pre-6 wix on
# PATH. `dotnet tool install --global --version 5.0.2 wix` gets one.
#
# ASCII only - see scripts/windows/setup_postgres.ps1's note on why (this
# script runs under the same Windows PowerShell 5.1 as everything else
# here, on a build machine, not just at install time).
#
# Usage:
#   .\build.ps1 -PgsqlSource 'D:\pgsql' -Version 1.0.0

[CmdletBinding()]
param(
    # An already-extracted PostgreSQL Windows binaries zip - the directory
    # containing bin\, lib\, share\ directly (not the zip itself, and not
    # a parent directory one level up from those).
    [Parameter(Mandatory = $true)][string]$PgsqlSource,

    [string]$Version = '1.0.0',
    [string]$OutDir = '',

    # Overridable for a wix.exe not on PATH - the same reasoning
    # -PgsqlSource takes a path rather than assuming a fixed location.
    [string]$WixExe = 'wix'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$stageDir = Join-Path $PSScriptRoot 'stage'

# Not defaulted directly in the param block above: $PSScriptRoot is only
# reliably populated once the script body starts running, not yet during
# parameter default-value evaluation - a real, invocation-method-dependent
# difference, not a hypothetical one. `& build.ps1 ...` (the call operator,
# how this script was run for every earlier build in this project) happens
# to populate it early enough; `powershell.exe -File build.ps1 ...` (what
# build.bat uses, since it starts a fresh process rather than reusing an
# existing session) does not, and Join-Path against an empty $PSScriptRoot
# in the param block failed outright. Resolved here instead, after
# $PSScriptRoot is guaranteed set.
if ($OutDir -eq '') { $OutDir = Join-Path $PSScriptRoot 'dist' }

# MSI package versions are strictly major.minor.build, each numeric,
# nothing else - no semver-style pre-release/build-metadata suffixes.
# wix build only warns on a version that breaks this ("might be treated as
# an error" in a future release), which is exactly the kind of thing to
# catch here rather than ship a warning nobody reads. Found by trying
# '1.0.0-test' as a version while developing this script - it warned
# twice, then failed for an unrelated reason (disk space), which would
# have been easy to misdiagnose as the version warning's fault instead.
if ($Version -notmatch '^\d{1,3}\.\d{1,3}\.\d{1,5}$') {
    throw "-Version '$Version' is not a valid MSI package version - use exactly major.minor.build (e.g. 1.0.0), no suffixes"
}

foreach ($exe in @('bin\initdb.exe', 'bin\pg_ctl.exe', 'bin\psql.exe')) {
    if (-not (Test-Path (Join-Path $PgsqlSource $exe))) {
        throw "missing $exe under -PgsqlSource $PgsqlSource - point this at the extracted PostgreSQL binaries zip's root (the directory containing bin\, lib\, share\), not the zip itself"
    }
}

if (Test-Path $stageDir) { Remove-Item -Recurse -Force $stageDir }
New-Item -ItemType Directory -Force -Path $stageDir | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $stageDir 'scripts') | Out-Null
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

#  1. Cross-compile the three Go binaries 
#
# windows/amd64 explicitly (GOOS/GOEXE below), regardless of what platform
# this script itself is running on - a build machine building this
# installer is not necessarily the same OS the installer targets.
Write-Host "build: compiling Go binaries"
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
foreach ($target in @(
    @{ Pkg = './cmd/api'; Out = 'api_service.exe' },
    @{ Pkg = './cmd/radiusd'; Out = 'aaa_core_daemon.exe' },
    @{ Pkg = './cmd/bootstrap'; Out = 'bootstrap.exe' }
)) {
    $outPath = Join-Path $stageDir $target.Out
    & go build -C $repoRoot -o $outPath $target.Pkg
    if ($LASTEXITCODE -ne 0) { throw "go build $($target.Pkg) failed with exit code $LASTEXITCODE" }
}
Remove-Item Env:\GOOS, Env:\GOARCH

#  2. Stage the scripts and PostgreSQL 

Write-Host "build: staging scripts and PostgreSQL binaries"
Copy-Item (Join-Path $repoRoot 'scripts\windows\install.ps1') (Join-Path $stageDir 'scripts')
Copy-Item (Join-Path $repoRoot 'scripts\windows\uninstall.ps1') (Join-Path $stageDir 'scripts')
Copy-Item (Join-Path $repoRoot 'scripts\windows\setup_postgres.ps1') (Join-Path $stageDir 'scripts')
Copy-Item (Join-Path $repoRoot 'scripts\windows\register_services.ps1') (Join-Path $stageDir 'scripts')
Copy-Item -Recurse $PgsqlSource (Join-Path $stageDir 'pgsql')

#  3. Build the MSI 

$msiPath = Join-Path $OutDir "ISP-BSS-Setup-$Version.msi"
# wix build stages every harvested file (the whole bundled PostgreSQL tree
# included) into a working folder before cabinet-ing it, and defaults that
# folder to a directory under %TMP% - which is very likely on C:, entirely
# independent of where -StageDir/-OutDir were pointed. Given how large that
# working set is here, an explicit -intermediatefolder next to StageDir
# keeps every byte of this build on the same drive as everything else it
# touches, rather than leaving a few hundred MB to land whatever the
# system TEMP happens to be pointed at.
$intermediateDir = Join-Path $PSScriptRoot 'obj'
if (Test-Path $intermediateDir) { Remove-Item -Recurse -Force $intermediateDir }

Write-Host "build: running wix build -> $msiPath"
& $WixExe build `
    -d "StageDir=$stageDir" `
    -d "ProductVersion=$Version" `
    -arch x64 `
    -intermediatefolder $intermediateDir `
    -o $msiPath `
    (Join-Path $PSScriptRoot 'Product.wxs')
if ($LASTEXITCODE -ne 0) { throw "wix build failed with exit code $LASTEXITCODE" }

Write-Host "build: done -> $msiPath"
