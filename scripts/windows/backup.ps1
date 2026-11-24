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
#   .\scripts\windows\backup.ps1 -KeepLast 30     # keep 30 instead of 14
#   .\scripts\windows\backup.ps1 -KeepLast 0      # keep everything
#
# RETENTION
#
# Each run writes a new timestamped folder and the old ones are not touched by
# anything else, so a daily schedule fills the disk and takes the database down
# with it - a backup job that causes the outage. -KeepLast deletes all but the
# newest N *complete* backups (a folder with no manifest.json is a run that
# died partway and is never counted as one of the N, nor deleted, so a failed
# run cannot push a good backup out).
#
# Fourteen is a default, not a policy. It is the shortest window that survives
# a problem introduced on a Friday and noticed the following Friday. If the
# retention that matters here is a regulatory one, set it deliberately.
[CmdletBinding()]
param(
    [string] $OutRoot    = 'D:\isp-backups',
    [string] $ConfigDir  = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir = 'C:\Program Files\ISP BSS',
    [int]    $KeepLast   = 14
)
$ErrorActionPreference = 'Stop'
# WHY THIS SCRIPT USED TO DIE ON ITS FIRST STATEMENT
#
# Windows PowerShell 5.1 turns a native program's stderr output into an
# ErrorRecord. With $ErrorActionPreference = 'Stop' that error is
# *terminating* - so one benign NOTICE from psql aborts the script, even
# though psql exited 0.
#
# DROP DATABASE IF EXISTS emits exactly such a NOTICE ("database ... does not
# exist, skipping") on any run where the scratch database is not already
# there, which is every first run. This script therefore failed before it
# restored anything, every time, with a NativeCommandError that reads like a
# psql problem and is not one. Two mitigations, because either alone is thin:
#
#   1. PGOPTIONS silences NOTICE at the source, so there is nothing on stderr
#      to promote.
#   2. Native-command stderr is not treated as terminating below. Exit codes
#      are checked explicitly instead, which is the only thing that was ever
#      a reliable signal from a native program.
$env:PGOPTIONS = '--client-min-messages=warning'


$envPath = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envPath)) { throw "cannot read $envPath" }
$dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
$m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
$dbUser = $m.Groups[1].Value; $dbPw = $m.Groups[2].Value
$dbHost = $m.Groups[3].Value; $dbPort = $m.Groups[4].Value; $dbName = $m.Groups[5].Value

# WHY THIS DOES NOT DUMP AS THE APPLICATION'S OWN ROLE
#
# DB_DSN names bss_app, the least-privilege role the services run as. Migration
# 019 revokes SELECT on lea_audit_log from it (the LEA trail is append-only and
# the application never reads it back) and revokes ALL on goose_db_version.
#
# pg_dump reads every table it dumps. Missing SELECT on one is not a warning
# and not a partial dump - pg_dump stops with "permission denied for table
# lea_audit_log" and exits non-zero. This script dumped as $dbUser, so on any
# standard installation it could not produce a backup at all. The failure was
# loud, which is the only good thing about it: nobody got a backup that was
# silently missing the audit trail.
#
# verify_restore.ps1 already connected as postgres. The two halves disagreed
# about who takes a backup, and only the half that restores was ever right.
$pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
$suPw = $null
if (Test-Path $pwFile) {
    try { $suPw = (Get-Content $pwFile -Raw -ErrorAction Stop).Trim() } catch { $suPw = $null }
}
if (-not $suPw) {
    $sec  = Read-Host -AsSecureString 'PostgreSQL superuser (postgres) password'
    $suPw = [Net.NetworkCredential]::new('', $sec).Password
}

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
# pg_dump's own warnings are not failures; its exit code is, and it is
# checked below.
$ErrorActionPreference = 'Continue'
$env:PGPASSWORD = $suPw
try {
    & (Join-Path $InstallDir 'pgsql\bin\pg_dump.exe') `
        -h $dbHost -p $dbPort -U postgres -d $dbName -Fc -f $dumpPath
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
$env:PGPASSWORD = $suPw
try {
    $psql = Join-Path $InstallDir 'pgsql\bin\psql.exe'
    $counts = & $psql -h $dbHost -p $dbPort -U postgres -d $dbName -tAc @"
SELECT 'subscribers=' || (SELECT count(*) FROM subscribers)
    || ',kyc=' || (SELECT count(*) FROM kyc_verifications WHERE aadhaar_encrypted IS NOT NULL OR pan_encrypted IS NOT NULL)
    || ',invoices=' || (SELECT count(*) FROM invoices)
    || ',gl_lines=' || (SELECT count(*) FROM gl_journal_lines)
    || ',lea_audit=' || (SELECT count(*) FROM lea_audit_log)
    || ',schema_version=' || (SELECT max(version_id) FROM goose_db_version)
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

# ---- 5. Retention --------------------------------------------------------
# Only now, after this backup is written and verified complete. Pruning first
# would mean a failed run leaves one fewer backup than before it started.
if ($KeepLast -gt 0) {
    $complete = @(Get-ChildItem -Path $OutRoot -Directory -Filter 'isp-bss-*' -ErrorAction SilentlyContinue |
        Where-Object { Test-Path (Join-Path $_.FullName 'manifest.json') } |
        Sort-Object Name -Descending)
    if ($complete.Count -gt $KeepLast) {
        foreach ($old in $complete[$KeepLast..($complete.Count - 1)]) {
            Remove-Item -Path $old.FullName -Recurse -Force
            Write-Host ("  pruned {0}" -f $old.Name) -ForegroundColor DarkGray
        }
    }
    $partial = @(Get-ChildItem -Path $OutRoot -Directory -Filter 'isp-bss-*' -ErrorAction SilentlyContinue |
        Where-Object { -not (Test-Path (Join-Path $_.FullName 'manifest.json')) })
    if ($partial.Count -gt 0) {
        Write-Host ("  {0} incomplete backup folder(s) left in place; a run died partway. Check and remove by hand." -f $partial.Count) -ForegroundColor Yellow
    }
}

Write-Host "`nBackup complete: $outDir" -ForegroundColor Green
Write-Host 'This folder contains encryption keys and live credentials. Treat it as' -ForegroundColor Yellow
Write-Host 'you would the database itself: encrypted at rest, access-controlled,' -ForegroundColor Yellow
Write-Host 'and never in a shared drive or a repository.' -ForegroundColor Yellow
Write-Host ''
Write-Host 'A backup nobody has restored is a hypothesis. Verify this one:'
Write-Host "  .\scripts\windows\verify_restore.ps1 -BackupDir '$outDir'"
