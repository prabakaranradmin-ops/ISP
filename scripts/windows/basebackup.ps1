# basebackup.ps1 - takes a base backup and prunes the WAL that is no longer
# needed by any backup it keeps.
#
# WHY THIS IS NOT OPTIONAL ONCE ARCHIVING IS ON
#
# WAL replays on top of a base backup; it restores nothing on its own. The
# archive must therefore keep every segment written since the OLDEST base
# backup still being relied on. With no new base backups, that is every
# segment ever written: the archive grows without bound and a restore has to
# replay months of it, so the recovery that was supposed to take minutes takes
# hours - exactly when nobody has hours.
#
# Pruning in the other direction is just as important and more dangerous. A
# segment removed while any kept base backup still needs it makes that backup
# unrestorable, and nothing says so until the restore. This script therefore
# prunes strictly against the OLDEST backup it is keeping, using
# pg_archivecleanup and the .backup label PostgreSQL itself wrote, rather than
# by age - "older than 30 days" is a rule that deletes the segment a 31-day-old
# base backup needs.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
#
# Usage:
#   .\scripts\windows\basebackup.ps1 -ArchiveRoot E:\isp-wal
#   .\scripts\windows\basebackup.ps1 -ArchiveRoot E:\isp-wal -KeepLast 4
#   .\scripts\windows\basebackup.ps1 -ArchiveRoot E:\isp-wal -KeepLast 0   # prune nothing
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $ArchiveRoot,
    # Two is the working minimum: while a new one is being taken, the previous
    # one is still the restorable one.
    [int]    $KeepLast   = 2,
    [string] $ConfigDir  = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir = 'C:\Program Files\ISP BSS'
)
$ErrorActionPreference = 'Stop'

if ($KeepLast -eq 1) {
    throw 'KeepLast 1 leaves no restorable backup while a new one is in progress. Use 2 or more, or 0 to prune nothing.'
}

$walDir  = Join-Path $ArchiveRoot 'wal'
$baseDir = Join-Path $ArchiveRoot 'base'
foreach ($d in $walDir, $baseDir) {
    if (-not (Test-Path $d)) { throw "$d does not exist - run enable_wal_archiving.ps1 first" }
}

$envPath = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envPath)) { throw "cannot read $envPath" }
$dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
$m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
$dbHost = $m.Groups[3].Value; $dbPort = $m.Groups[4].Value

$pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
$suPw = $null
if (Test-Path $pwFile) {
    try { $suPw = (Get-Content $pwFile -Raw -ErrorAction Stop).Trim() } catch { $suPw = $null }
}
if (-not $suPw) {
    $sec  = Read-Host -AsSecureString 'PostgreSQL superuser (postgres) password'
    $suPw = [Net.NetworkCredential]::new('', $sec).Password
}

$pgBaseBackup   = Join-Path $InstallDir 'pgsql\bin\pg_basebackup.exe'
$archiveCleanup = Join-Path $InstallDir 'pgsql\bin\pg_archivecleanup.exe'
foreach ($exe in $pgBaseBackup, $archiveCleanup) {
    if (-not (Test-Path $exe)) { throw "cannot find $exe" }
}

$env:PGOPTIONS = '--client-min-messages=warning'
$env:PGPASSWORD = $suPw
# Native stderr must not be terminating; exit codes are checked instead.
$ErrorActionPreference = 'Continue'

try {
    $stamp  = Get-Date -Format 'yyyyMMdd-HHmmss'
    $target = Join-Path $baseDir "base-$stamp"
    Write-Host "Base backup -> $target" -ForegroundColor Cyan

    # -Xstream so the backup carries the WAL it needs to reach a consistent
    # state on its own: it is restorable even if the archive is unreachable.
    # -c fast so the checkpoint does not wait for the next scheduled one.
    & $pgBaseBackup -h $dbHost -p $dbPort -U postgres -D $target -Fp -Xstream -c fast -P
    if ($LASTEXITCODE -ne 0) {
        # A half-written directory is worse than none: it looks like a backup.
        if (Test-Path $target) { Remove-Item $target -Recurse -Force }
        throw "pg_basebackup failed (exit $LASTEXITCODE); the partial directory was removed"
    }

    $sizeGB = '{0:N2}' -f ((Get-ChildItem $target -Recurse -File |
        Measure-Object Length -Sum).Sum / 1GB)
    Write-Host "  $sizeGB GB" -ForegroundColor Green

    # A backup with no backup_label is not a base backup, whatever else is in
    # the directory. Checked before it is counted as one of the N to keep.
    if (-not (Test-Path (Join-Path $target 'backup_label'))) {
        Write-Host '  WARNING: no backup_label in the new backup; not counting it as complete' -ForegroundColor Yellow
    }

    if ($KeepLast -le 0) {
        Write-Host 'KeepLast 0: nothing pruned, and the WAL archive will keep growing.' -ForegroundColor Yellow
        return
    }

    # Complete backups only, newest first. An incomplete directory is never
    # counted and never deleted - a failed run must not push a good backup out
    # of the window, and must not be mistaken for one.
    $complete = @(Get-ChildItem $baseDir -Directory -Filter 'base-*' -ErrorAction SilentlyContinue |
        Where-Object { Test-Path (Join-Path $_.FullName 'backup_label') } |
        Sort-Object Name -Descending)
    Write-Host "  complete base backups: $($complete.Count) (keeping $KeepLast)"

    if ($complete.Count -gt $KeepLast) {
        foreach ($old in $complete[$KeepLast..($complete.Count - 1)]) {
            Remove-Item $old.FullName -Recurse -Force
            Write-Host "  pruned base backup $($old.Name)" -ForegroundColor DarkGray
        }
        $complete = @(Get-ChildItem $baseDir -Directory -Filter 'base-*' -ErrorAction SilentlyContinue |
            Where-Object { Test-Path (Join-Path $_.FullName 'backup_label') } |
            Sort-Object Name -Descending)
    }

    # Prune WAL against the OLDEST backup still kept, never by age.
    #
    # The oldest kept backup's START segment is the earliest one any kept
    # backup can need. pg_archivecleanup removes everything before it and
    # nothing from it onward, which is the only safe cut.
    $oldest = $complete | Select-Object -Last 1
    if (-not $oldest) {
        Write-Host '  no complete base backup to prune against; leaving the WAL archive alone' -ForegroundColor Yellow
        return
    }
    $label = Get-Content (Join-Path $oldest.FullName 'backup_label') -Raw
    $startSeg = [regex]::Match($label, '(?m)^START WAL LOCATION:.*\(file ([0-9A-F]{24})\)').Groups[1].Value
    if (-not $startSeg) {
        Write-Host "  could not read the start segment from $($oldest.Name)'s backup_label; leaving the WAL archive alone" -ForegroundColor Yellow
        return
    }

    $before = @(Get-ChildItem $walDir -File).Count
    Write-Host "  pruning WAL before $startSeg (needed by $($oldest.Name))"
    & $archiveCleanup $walDir $startSeg
    if ($LASTEXITCODE -ne 0) {
        Write-Host "  pg_archivecleanup exited $LASTEXITCODE; the archive was left as it was" -ForegroundColor Yellow
        return
    }
    $after = @(Get-ChildItem $walDir -File).Count
    Write-Host "  WAL segments: $before -> $after" -ForegroundColor Green

    Write-Host ''
    Write-Host 'Recovery point is now the newest committed transaction, replaying from' -ForegroundColor Green
    Write-Host "$($complete[0].Name) onward. This is a claim until it has been rehearsed:" -ForegroundColor Green
    Write-Host '  restore to a chosen instant on a scratch cluster (runbook 12.4) and confirm'
    Write-Host '  a row written just before it comes back, and one written just after does not.'
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
    Remove-Item Env:PGOPTIONS -ErrorAction SilentlyContinue
}
