# run_integration_tests.ps1 - runs the integration suite against a throwaway
# database on the natively-installed PostgreSQL.
#
# WHY THIS EXISTS
#
# scripts/run_db_tests.sh is the documented way in, and it needs Docker. On a
# native Windows install there is no Docker, so TEST_DB_DSN never got set - and
# every integration test calls t.Skip when it is unset. The suite therefore
# reported "ok" for every package while running none of it. Thirty-nine files
# have never executed.
#
# THE GUARD IS THE POINT OF THIS SCRIPT
#
# The harness calls TRUNCATE TABLE ... RESTART IDENTITY CASCADE on every table
# before each test (internal/db/harness_test.go). Pointed at the live database
# that is not a test run, it is a wipe: subscribers, invoices, credit notes,
# the general ledger, all of it, with no confirmation prompt anywhere in the
# path. So this refuses to run against the database app.env names, and it does
# that check before it does anything else.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
#
# Usage:
#   .\scripts\windows\run_integration_tests.ps1
#   .\scripts\windows\run_integration_tests.ps1 -TestFlags '-run TestWallet'
#   .\scripts\windows\run_integration_tests.ps1 -KeepDatabase
[CmdletBinding()]
param(
    [string] $TestDbName  = 'isp_bss_test',
    [string] $ConfigDir   = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir  = 'C:\Program Files\ISP BSS',
    [string] $TestFlags   = '',
    [switch] $KeepDatabase
)
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Push-Location $repoRoot
try {
    # ---- Guard: never the live database -------------------------------------
    $envPath = Join-Path $ConfigDir 'app.env'
    if (-not (Test-Path $envPath)) { throw "cannot read $envPath" }
    $dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
    $m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
    if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
    $liveDb   = $m.Groups[5].Value
    $dbHost   = $m.Groups[3].Value
    $dbPort   = $m.Groups[4].Value

    if ($TestDbName -eq $liveDb) {
        throw ("REFUSING TO RUN. -TestDbName is '$TestDbName', which is the live database. " +
               "The harness truncates every table before each test; this would destroy it.")
    }
    Write-Host "live database  : $liveDb (will not be touched)" -ForegroundColor Green
    Write-Host "test database  : $TestDbName" -ForegroundColor Green

    # ---- Superuser credentials ----------------------------------------------
    # Creating a database needs the postgres superuser. The password file is
    # ACLed, so this reads it when elevated and prompts otherwise.
    $pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
    $suPw = $null
    if (Test-Path $pwFile) {
        try { $suPw = (Get-Content $pwFile -Raw -ErrorAction Stop).Trim() } catch { $suPw = $null }
    }
    if (-not $suPw) {
        $sec = Read-Host -AsSecureString 'PostgreSQL superuser (postgres) password'
        $suPw = [Net.NetworkCredential]::new('', $sec).Password
    }
    if (-not $suPw) { throw 'no superuser password supplied' }

    # ---- Build the test database --------------------------------------------
    # bootstrap.exe creates the database, applies every migration and writes an
    # app.env for it. -config-dir points at a throwaway directory so the live
    # configuration is never rewritten.
    $tmpConfig = Join-Path ([IO.Path]::GetTempPath()) ("isp-itest-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmpConfig | Out-Null
    Write-Host "`nApplying migrations to $TestDbName ..." -ForegroundColor Cyan
    & (Join-Path $InstallDir 'bootstrap.exe') `
        -superuser-dsn "postgres://postgres:$suPw@${dbHost}:${dbPort}/postgres?sslmode=disable" `
        -config-dir $tmpConfig `
        -db-name $TestDbName `
        -db-host $dbHost -db-port $dbPort
    if ($LASTEXITCODE -ne 0) { throw "bootstrap failed (exit $LASTEXITCODE)" }

    # TEST_DB_DSN must be a SUPERUSER connection, not the bss_app one bootstrap
    # writes into app.env.
    #
    # The harness truncates lea_audit_log, live_sessions and jobqueue_tasks,
    # and migration 019 deliberately denies bss_app exactly those rights —
    # least privilege on the audit log is a security control, not an oversight,
    # and internal/db/lea_audit_rls_integration_test.go exists to prove it
    # holds. That test also runs ALTER ROLE and seeds rows "as the superuser"
    # before deriving its own restricted connection, which only works from a
    # superuser DSN.
    #
    # Pointed at the app user instead, the suite fails ~200 tests with
    # SQLSTATE 42501 and every one of them looks like a product defect.
    $testDsn = "postgres://postgres:$suPw@${dbHost}:${dbPort}/${TestDbName}?sslmode=disable"
    if ($TestDbName -eq $liveDb) {
        throw 'refusing to continue: the test DSN resolves to the live database'
    }

    # ---- Run the suite -------------------------------------------------------
    $env:TEST_DB_DSN = $testDsn
    # The API integration tests mint their own tokens and only need a value
    # that is stable within the run.
    $env:JWT_SECRET = 'integration-test-secret-not-used-outside-tests'

    Write-Host "`nRunning integration suite ..." -ForegroundColor Cyan
    $args = @('test', '-tags=integration', '-count=1', './...')
    if ($TestFlags) { $args += $TestFlags.Split(' ') }
    & go @args
    $testExit = $LASTEXITCODE

    # A green run that skipped everything is the failure this script exists to
    # prevent, so say plainly whether anything actually executed.
    Write-Host "`nRe-running with -v to count skips ..." -ForegroundColor DarkGray
    $verbose = & go test -tags=integration -count=1 -v ./... 2>&1
    $ran     = ([regex]::Matches(($verbose -join "`n"), '(?m)^--- (PASS|FAIL)')).Count
    $skipped = ([regex]::Matches(($verbose -join "`n"), '(?m)^--- SKIP')).Count
    Write-Host ("tests executed : {0}" -f $ran)
    Write-Host ("tests skipped  : {0}" -f $skipped)
    if ($ran -eq 0) {
        Write-Host 'NOTHING RAN. TEST_DB_DSN did not reach the tests.' -ForegroundColor Red
    }

    exit $testExit
} finally {
    Remove-Item Env:TEST_DB_DSN -ErrorAction SilentlyContinue
    Remove-Item Env:JWT_SECRET  -ErrorAction SilentlyContinue
    if (-not $KeepDatabase -and $tmpConfig -and (Test-Path $tmpConfig)) {
        Remove-Item $tmpConfig -Recurse -Force -ErrorAction SilentlyContinue
    }
    Pop-Location
}
