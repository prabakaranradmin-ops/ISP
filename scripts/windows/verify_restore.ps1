# verify_restore.ps1 - restores a backup into a scratch database and checks it.
#
# A backup nobody has restored is a hypothesis. This turns it into a fact, or
# tells you it is not one, without touching the live installation.
#
# WHAT IT CHECKS, AND WHY EACH ONE
#
#   1. The dump restores at all.
#   2. Row counts match the manifest - a restore that silently dropped a table
#      still "succeeds" as far as pg_restore is concerned.
#   3. The schema is at the migration version the application expects. A dump
#      from an older install restores happily and then fails at runtime on a
#      column that does not exist yet.
#   4. KYC decrypts using the backed-up key store. This is the one that
#      matters, and the one a database-only backup fails: the keys live in a
#      file on disk, not in the database, so a dump restored beside a freshly
#      generated key store leaves every Aadhaar and PAN permanently
#      unreadable, with no error at any point.
#
# Check 4 needs the application's own crypto, so it runs through `go test` in
# the repository rather than being reimplemented in PowerShell. A second
# implementation of the decryption path would be the thing most likely to be
# wrong, and it would be wrong in the direction of reporting success.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
#
# Usage:
#   .\scripts\windows\verify_restore.ps1 -BackupDir D:\isp-backups\isp-bss-20260908-101500
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $BackupDir,
    [string] $ScratchDbName = 'isp_bss_restore_check',
    [string] $ConfigDir     = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir    = 'C:\Program Files\ISP BSS',
    [switch] $KeepDatabase
)
$ErrorActionPreference = 'Stop'

function Fail($msg) { Write-Host "  [FAIL] $msg" -ForegroundColor Red; $script:failures++ }
function Pass($msg) { Write-Host "  [ OK ] $msg" -ForegroundColor Green }
$script:failures = 0

foreach ($f in 'database.dump', 'manifest.json') {
    if (-not (Test-Path (Join-Path $BackupDir $f))) { throw "backup is missing $f" }
}
$manifest = Get-Content (Join-Path $BackupDir 'manifest.json') -Raw | ConvertFrom-Json

# ---- Guard: never the live database ---------------------------------------
$envPath = Join-Path $ConfigDir 'app.env'
$dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
$m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
$dbHost = $m.Groups[3].Value; $dbPort = $m.Groups[4].Value; $liveDb = $m.Groups[5].Value
if ($ScratchDbName -eq $liveDb) {
    throw "REFUSING TO RUN: -ScratchDbName is the live database. This drops and recreates it."
}

$pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
$suPw = $null
if (Test-Path $pwFile) { try { $suPw = (Get-Content $pwFile -Raw -ErrorAction Stop).Trim() } catch { $suPw = $null } }
if (-not $suPw) {
    $sec  = Read-Host -AsSecureString 'PostgreSQL superuser (postgres) password'
    $suPw = [Net.NetworkCredential]::new('', $sec).Password
}

$psql       = Join-Path $InstallDir 'pgsql\bin\psql.exe'
$pgRestore  = Join-Path $InstallDir 'pgsql\bin\pg_restore.exe'
$env:PGPASSWORD = $suPw

try {
    Write-Host "`nRestoring into scratch database '$ScratchDbName' ..." -ForegroundColor Cyan
    & $psql -h $dbHost -p $dbPort -U postgres -d postgres -v ON_ERROR_STOP=1 `
        -c "DROP DATABASE IF EXISTS $ScratchDbName" | Out-Null
    & $psql -h $dbHost -p $dbPort -U postgres -d postgres -v ON_ERROR_STOP=1 `
        -c "CREATE DATABASE $ScratchDbName" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'could not create the scratch database' }

    # pg_restore reports non-zero for benign ownership notices on a database
    # whose roles differ, so its exit code is not the check - the row counts
    # below are.
    & $pgRestore -h $dbHost -p $dbPort -U postgres -d $ScratchDbName --no-owner --no-acl `
        (Join-Path $BackupDir 'database.dump') 2>&1 | Out-Null

    Write-Host "`nChecks" -ForegroundColor Cyan

    # 1 + 2. The data came back, in the quantity the manifest recorded.
    $counts = & $psql -h $dbHost -p $dbPort -U postgres -d $ScratchDbName -tAc @"
SELECT 'subscribers=' || (SELECT count(*) FROM subscribers)
    || ',kyc=' || (SELECT count(*) FROM kyc_verifications WHERE aadhaar_encrypted IS NOT NULL OR pan_encrypted IS NOT NULL)
    || ',invoices=' || (SELECT count(*) FROM invoices)
    || ',gl_lines=' || (SELECT count(*) FROM gl_journal_lines)
"@
    $counts = ($counts | Where-Object { $_ }) -join ''
    if ($counts -eq $manifest.row_counts) {
        Pass "row counts match the manifest ($counts)"
    } else {
        Fail "row counts differ. backup: $($manifest.row_counts)  restored: $counts"
    }

    # 3. Schema version. A dump from an older install restores cleanly and then
    #    fails at runtime on a column that does not exist.
    $ver = (& $psql -h $dbHost -p $dbPort -U postgres -d $ScratchDbName -tAc `
        'SELECT max(version_id) FROM goose_db_version WHERE is_applied') -join ''
    $expected = (Get-ChildItem (Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) 'migrations') `
        -Filter '*.sql' | ForEach-Object { [int]($_.Name -split '_')[0] } | Measure-Object -Maximum).Maximum
    if ([int]$ver -eq $expected) {
        Pass "schema at migration $ver, matching this checkout"
    } else {
        Fail "restored schema is at migration $ver, this checkout expects $expected"
    }

    # 4. The check a database-only backup fails.
    $keysDir = Join-Path $BackupDir 'keys'
    if (-not (Test-Path $keysDir) -or @(Get-ChildItem $keysDir -File -Recurse).Count -eq 0) {
        Fail 'no key store in this backup - KYC in it is permanently unreadable'
    } else {
        $keyFile = (Get-ChildItem $keysDir -File -Recurse | Select-Object -First 1).FullName
        $kycRows = [int](((& $psql -h $dbHost -p $dbPort -U postgres -d $ScratchDbName -tAc `
            'SELECT count(*) FROM kyc_verifications') -join '').Trim())
        if ($kycRows -eq 0) {
            Write-Host '  [SKIP] no KYC rows in this backup to decrypt' -ForegroundColor DarkGray
        } else {
            $repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
            $env:RESTORE_CHECK_DSN = "postgres://postgres:$suPw@${dbHost}:${dbPort}/${ScratchDbName}?sslmode=disable"
            $env:RESTORE_CHECK_KEYSTORE = "local:$keyFile"
            try {
                Push-Location $repoRoot
                $out = & go test -tags=integration -count=1 -run TestRestoredKYCDecrypts ./internal/db/ 2>&1
                if ($LASTEXITCODE -eq 0) {
                    Pass "KYC decrypts from the backed-up key store ($kycRows row(s))"
                } else {
                    Fail "KYC did not decrypt from the backed-up key store"
                    $out | Select-Object -First 12 | ForEach-Object { Write-Host "         $_" -ForegroundColor DarkGray }
                }
            } finally {
                Pop-Location
                Remove-Item Env:RESTORE_CHECK_DSN, Env:RESTORE_CHECK_KEYSTORE -ErrorAction SilentlyContinue
            }
        }
    }

    Write-Host ''
    if ($script:failures -eq 0) {
        Write-Host 'RESTORE VERIFIED. This backup is recoverable.' -ForegroundColor Green
    } else {
        Write-Host "RESTORE NOT VERIFIED - $($script:failures) check(s) failed." -ForegroundColor Red
        Write-Host 'Do not rely on this backup.' -ForegroundColor Red
    }
    exit $script:failures
} finally {
    if (-not $KeepDatabase) {
        & $psql -h $dbHost -p $dbPort -U postgres -d postgres `
            -c "DROP DATABASE IF EXISTS $ScratchDbName" 2>&1 | Out-Null
    }
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}
