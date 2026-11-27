# enable_wal_archiving.ps1 - turns on continuous WAL archiving, so the recovery
# point is seconds rather than the last nightly dump.
#
# WHAT THIS CHANGES
#
# Without it, the only thing standing between an incident and lost data is
# backup.ps1's nightly pg_dump: worst-case loss is 24 hours of recharges,
# payments, tickets and session records. With it, every committed transaction
# is copied to an archive within archive_timeout, and a restore can stop at any
# chosen instant - "the state at 14:05, just before the bad import".
#
# A base backup is still required. WAL on its own restores nothing: recovery
# replays the archive ON TOP OF a base backup, so this script takes one and
# basebackup.ps1 keeps taking them.
#
# THE TWO WINDOWS TRAPS, BOTH OF WHICH LOOK LIKE A MISSING FILE
#
#   1. postgresql.conf processes backslash escapes inside a single-quoted
#      value. A path containing a backslash followed by digits is read as an
#      OCTAL escape and silently becomes a different character. On the machine
#      this was first rehearsed on, "...\claude\475a77f..." became
#      "...claude=a77f..." and the archiver failed twelve times against a path
#      that does not exist. Every backslash below is doubled for that reason.
#   2. `copy` is a cmd builtin and will not READ a forward-slash path. A
#      forward-slash archive destination happens to work, which makes archiving
#      look correct while restoring fails - the asymmetry that made the first
#      rehearsal look like a database problem.
#
# Both are why this writes the config rather than leaving an operator to.
#
# Verified end to end before being written down: a row committed after the
# base backup and before a chosen instant came back, and a row committed after
# that instant did not.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
#
# Usage (elevated):
#   .\scripts\windows\enable_wal_archiving.ps1 -ArchiveRoot E:\isp-wal
#   .\scripts\windows\enable_wal_archiving.ps1 -ArchiveRoot E:\isp-wal -WhatIf
[CmdletBinding(SupportsShouldProcess)]
param(
    # Put this on a different disk from the database. WAL on the same spindle
    # as the data it protects is lost in the same failure.
    [Parameter(Mandatory)][string] $ArchiveRoot,
    [string] $ConfigDir  = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir = 'C:\Program Files\ISP BSS',
    [string] $ServiceName = 'ISPBSSPostgres',
    # How long a quiet database may go before its WAL is archived anyway. The
    # recovery point is at worst this far behind.
    [int] $ArchiveTimeoutSeconds = 60
)
$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
        ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell: it edits postgresql.conf and restarts PostgreSQL.'
}

$envPath = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envPath)) { throw "cannot read $envPath" }
$dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
$m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
$dbHost = $m.Groups[3].Value; $dbPort = $m.Groups[4].Value; $dbName = $m.Groups[5].Value

# As postgres: reading pg_settings and the data directory needs more than the
# application role has.
$pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
$suPw = $null
if (Test-Path $pwFile) {
    try { $suPw = (Get-Content $pwFile -Raw -ErrorAction Stop).Trim() } catch { $suPw = $null }
}
if (-not $suPw) {
    $sec  = Read-Host -AsSecureString 'PostgreSQL superuser (postgres) password'
    $suPw = [Net.NetworkCredential]::new('', $sec).Password
}

$psql = Join-Path $InstallDir 'pgsql\bin\psql.exe'
$pgBaseBackup = Join-Path $InstallDir 'pgsql\bin\pg_basebackup.exe'
foreach ($exe in $psql, $pgBaseBackup) {
    if (-not (Test-Path $exe)) { throw "cannot find $exe" }
}

# NOTICE on stderr becomes a terminating error under 'Stop'; see the same note
# in verify_restore.ps1.
$env:PGOPTIONS = '--client-min-messages=warning'
$env:PGPASSWORD = $suPw
$ErrorActionPreference = 'Continue'

try {
    $dataDir = (& $psql -h $dbHost -p $dbPort -U postgres -d $dbName -tAc 'SHOW data_directory').Trim()
    if (-not $dataDir) { throw 'could not read data_directory' }
    $conf = Join-Path ($dataDir -replace '/', '\') 'postgresql.conf'
    if (-not (Test-Path $conf)) { throw "cannot find $conf" }

    $walDir = Join-Path $ArchiveRoot 'wal'
    $baseDir = Join-Path $ArchiveRoot 'base'
    foreach ($d in $ArchiveRoot, $walDir, $baseDir) {
        if (-not (Test-Path $d)) {
            if ($PSCmdlet.ShouldProcess($d, 'create')) {
                New-Item -ItemType Directory -Path $d -Force | Out-Null
            }
        }
    }

    # Owner-only: WAL contains every row written, so the archive is as
    # sensitive as the database.
    if ($PSCmdlet.ShouldProcess($ArchiveRoot, 'restrict to administrators and SYSTEM')) {
        $acl = Get-Acl $ArchiveRoot
        $acl.SetAccessRuleProtection($true, $false)
        foreach ($who in 'BUILTIN\Administrators', 'NT AUTHORITY\SYSTEM') {
            $acl.SetAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
                $who, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
        }
        Set-Acl -Path $ArchiveRoot -AclObject $acl
    }

    # Doubled backslashes: see the header. This is the whole reason this file
    # writes the config instead of documenting it.
    $walEscaped = ($walDir -replace '/', '\') -replace '\\', '\\'
    $archiveCommand = 'if not exist "{0}\\%f" copy "%p" "{0}\\%f"' -f $walEscaped
    $restoreCommand = 'copy "{0}\\%f" "%p"' -f $walEscaped

    $block = @"

# ---- continuous WAL archiving (enable_wal_archiving.ps1) -------------------
# Backslashes are DOUBLED on purpose: postgresql.conf processes escapes inside
# a single-quoted value, and a backslash followed by digits is read as an
# octal escape. Undoubling them corrupts this path silently.
# `copy` cannot read a forward-slash path, so forward slashes are not an
# option here either.
wal_level = replica
archive_mode = on
archive_command = '$archiveCommand'
archive_timeout = ${ArchiveTimeoutSeconds}s
# restore_command is not read during normal running; it is here so a restore
# can be performed by copying this line into the recovery config rather than
# reconstructing the escaping under pressure. See runbook 12.4.
#restore_command = '$restoreCommand'
# ---------------------------------------------------------------------------
"@

    if (Select-String -Path $conf -Pattern 'enable_wal_archiving.ps1' -Quiet) {
        Write-Host 'postgresql.conf already carries this block; leaving it alone.' -ForegroundColor Yellow
        Write-Host 'Remove it by hand first if you want to change the archive location.' -ForegroundColor Yellow
    } elseif ($PSCmdlet.ShouldProcess($conf, 'append the archiving block')) {
        Copy-Item $conf "$conf.before-wal-archiving" -Force
        Add-Content -Path $conf -Value $block -Encoding ascii
        Write-Host "Appended to $conf (previous kept as postgresql.conf.before-wal-archiving)"
    }

    # archive_mode needs a restart; the rest would reload.
    if ($PSCmdlet.ShouldProcess($ServiceName, 'restart')) {
        Write-Host "Restarting $ServiceName ..." -ForegroundColor Cyan
        Restart-Service $ServiceName -Force
        Start-Sleep -Seconds 3
    }

    $mode = (& $psql -h $dbHost -p $dbPort -U postgres -d $dbName -tAc 'SHOW archive_mode').Trim()
    if ($mode -ne 'on') {
        throw "archive_mode is '$mode' after the restart; the config was not picked up"
    }

    # Prove it archives before saying it works. A forced switch produces a
    # segment the archiver must copy within a few seconds.
    Write-Host 'Forcing a WAL switch and checking it lands ...' -ForegroundColor Cyan
    & $psql -h $dbHost -p $dbPort -U postgres -d $dbName -tAc 'SELECT pg_switch_wal()' | Out-Null
    $archived = $false
    for ($i = 0; $i -lt 20; $i++) {
        Start-Sleep -Seconds 1
        $stat = (& $psql -h $dbHost -p $dbPort -U postgres -d $dbName -tAc `
            'SELECT archived_count || ''/'' || failed_count FROM pg_stat_archiver').Trim()
        $parts = $stat -split '/'
        if ([int]$parts[0] -gt 0) { $archived = $true; break }
        if ([int]$parts[1] -gt 0) { break }
    }
    if (-not $archived) {
        $why = (& $psql -h $dbHost -p $dbPort -U postgres -d $dbName -tAc `
            'SELECT COALESCE(last_failed_wal, ''(none)'') FROM pg_stat_archiver').Trim()
        Write-Host ''
        Write-Host "ARCHIVING IS NOT WORKING. Last failed segment: $why" -ForegroundColor Red
        Write-Host 'Check the PostgreSQL log for "archive command failed"; the DETAIL line' -ForegroundColor Red
        Write-Host 'shows the command as the server ran it. A path with the backslashes' -ForegroundColor Red
        Write-Host 'missing is the escape problem described at the top of this script.' -ForegroundColor Red
        exit 1
    }
    Write-Host "  archived: $(Get-ChildItem $walDir -File | Measure-Object | Select-Object -ExpandProperty Count) segment(s)" -ForegroundColor Green

    # A base backup now, because WAL without one restores nothing.
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $target = Join-Path $baseDir "base-$stamp"
    if ($PSCmdlet.ShouldProcess($target, 'take a base backup')) {
        Write-Host "Taking a base backup into $target ..." -ForegroundColor Cyan
        & $pgBaseBackup -h $dbHost -p $dbPort -U postgres -D $target -Fp -Xstream -c fast -P
        if ($LASTEXITCODE -ne 0) { throw "pg_basebackup failed (exit $LASTEXITCODE)" }
        Write-Host "  base backup: $('{0:N1}' -f ((Get-ChildItem $target -Recurse -File | Measure-Object Length -Sum).Sum / 1GB)) GB"
    }

    Write-Host ''
    Write-Host 'Continuous archiving is on and verified.' -ForegroundColor Green
    Write-Host "  WAL archive:  $walDir"
    Write-Host "  base backups: $baseDir"
    Write-Host "  recovery point: at worst $ArchiveTimeoutSeconds seconds, from the newest base backup onward"
    Write-Host ''
    Write-Host 'Two things are now required of you, and neither is optional:'
    Write-Host '  1. A fresh base backup on a schedule. WAL since the oldest base backup must'
    Write-Host '     be kept, so without new ones the archive grows forever and recovery gets'
    Write-Host '     slower every day. basebackup.ps1 -KeepLast does both.'
    Write-Host '  2. A rehearsal. Restore to a chosen instant on a scratch cluster and confirm'
    Write-Host '     a row written just before it comes back. Runbook 12.4 has the procedure.'
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
    Remove-Item Env:PGOPTIONS -ErrorAction SilentlyContinue
}
