# backup.ps1 - takes a restorable backup of the ISP BSS installation.
#
# WHAT A DATABASE DUMP DOES NOT CONTAIN
#
# KYC (Aadhaar, PAN) is stored encrypted in kyc_verifications. The keys that open
# it are not in the database - they are a small JSON file on disk, written by
# cmd/bootstrap into <config>\keys. pg_dump cannot see it.
#
# So a database-only backup restores cleanly, reports success, and leaves every
# KYC value permanently unreadable. Nothing errors at restore time. Nothing
# errors afterwards either, until somebody opens a subscriber record - possibly
# during the audit or the lawful-interception request that is the reason KYC is
# retained at all. pkg/crypto/restore_test.go demonstrates that failure.
#
# This captures the database AND the key store AND app.env together, because
# any one of the three without the others is not a backup.
#
# app.env and the key store hold live secrets. The backup is therefore as
# sensitive as the database itself and must be stored accordingly; this script
# restricts the folder to the current user and says so rather than pretending
# the output is safe to copy around.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
#
# Usage:
#   .\scripts\windows\backup.ps1
#   .\scripts\windows\backup.ps1 -OutRoot D:\isp-backups
[CmdletBinding()]
param(
    [string] $OutRoot    = 'D:\isp-backups',
    [string] $ConfigDir  = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir = 'C:\Program Files\ISP BSS'
)
$ErrorActionPreference = 'Stop'

$envPath = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envPath)) { throw "cannot read $envPath" }
$dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
$m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
$dbUser = $m.Groups[1].Value; $dbPw = $m.Groups[2].Value
$dbHost = $m.Groups[3].Value; $dbPort = $m.Groups[4].Value; $dbName = $m.Groups[5].Value

$stamp  = Get-Date -Format 'yyyyMMdd-HHmmss'
$outDir = Join-Path $OutRoot "isp-bss-$stamp"
New-Item -ItemType Directory -Path $outDir -Force | Out-Null

# Owner-only, before anything sensitive is written into it.
$acl = Get-Acl $outDir
$acl.SetAccessRuleProtection($true, $false)
$acl.SetAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
    [Security.Principal.WindowsIdentity]::GetCurrent().Name,
    'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
Set-Acl -Path $outDir -AclObject $acl

Write-Host "Backing up to $outDir" -ForegroundColor Cyan

# ---- 1. Database ----------------------------------------------------------
# Custom format: compressed, and restorable selectively with pg_restore.
$dumpPath = Join-Path $outDir 'database.dump'
$env:PGPASSWORD = $dbPw
try {
    & (Join-Path $InstallDir 'pgsql\bin\pg_dump.exe') `
        -h $dbHost -p $dbPort -U $dbUser -d $dbName -Fc -f $dumpPath
    if ($LASTEXITCODE -ne 0) { throw "pg_dump failed (exit $LASTEXITCODE)" }
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}
Write-Host ("  database.dump  {0:N1} MB" -f ((Get-Item $dumpPath).Length / 1MB))

# ---- 2. Key store ---------------------------------------------------------
# The half that makes the dump worth restoring.
$keysSrc = Join-Path $ConfigDir 'keys'
if (-not (Test-Path $keysSrc)) {
    throw "key store not found at $keysSrc - refusing to write a backup that cannot decrypt KYC"
}
Copy-Item $keysSrc (Join-Path $outDir 'keys') -Recurse -Force
$keyFiles = @(Get-ChildItem (Join-Path $outDir 'keys') -File -Recurse)
if ($keyFiles.Count -eq 0) {
    throw 'the key store directory is empty - refusing to write a backup that cannot decrypt KYC'
}
Write-Host ("  keys\          {0} file(s)" -f $keyFiles.Count)

# ---- 3. Configuration -----------------------------------------------------
Copy-Item $envPath (Join-Path $outDir 'app.env') -Force
Write-Host '  app.env'

# ---- 4. Manifest ----------------------------------------------------------
# Records what this backup is and what it should restore to, so a restore can
# be checked rather than hoped about.
$env:PGPASSWORD = $dbPw
try {
    $psql = Join-Path $InstallDir 'pgsql\bin\psql.exe'
    $counts = & $psql -h $dbHost -p $dbPort -U $dbUser -d $dbName -tAc @"
SELECT 'subscribers=' || (SELECT count(*) FROM subscribers)
    || ',kyc=' || (SELECT count(*) FROM kyc_verifications WHERE aadhaar_encrypted IS NOT NULL OR pan_encrypted IS NOT NULL)
    || ',invoices=' || (SELECT count(*) FROM invoices)
    || ',gl_lines=' || (SELECT count(*) FROM gl_journal_lines)
"@
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}

@{
    taken_at      = (Get-Date).ToString('o')
    database      = $dbName
    row_counts    = ($counts | Where-Object { $_ }) -join ''
    key_files     = $keyFiles.Name
    dump_sha256   = (Get-FileHash $dumpPath -Algorithm SHA256).Hash
    git_commit    = (& git -C (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) rev-parse --short HEAD 2>$null)
} | ConvertTo-Json -Depth 4 | Set-Content (Join-Path $outDir 'manifest.json') -Encoding utf8
Write-Host '  manifest.json'

Write-Host "`nBackup complete: $outDir" -ForegroundColor Green
Write-Host 'This folder contains encryption keys and live credentials. Treat it as' -ForegroundColor Yellow
Write-Host 'you would the database itself: encrypted at rest, access-controlled,' -ForegroundColor Yellow
Write-Host 'and never in a shared drive or a repository.' -ForegroundColor Yellow
Write-Host ''
Write-Host 'A backup nobody has restored is a hypothesis. Verify this one:'
Write-Host "  .\scripts\windows\verify_restore.ps1 -BackupDir '$outDir'"
