# repair_app_role_password.ps1 - puts the application database role's password
# back to the one app.env expects.
#
# Run this if the live services can no longer reach PostgreSQL after an
# integration test run.
#
# WHY THIS CAN HAPPEN
#
# PostgreSQL roles are cluster-wide, not per-database. A test that runs
# ALTER ROLE inside the test database changes the password of the role the
# production services log in with, on any machine where both databases share
# one PostgreSQL cluster - which is every native install.
#
# The symptom is delayed and misleading. pgxpool keeps the connections it has
# already authenticated, so /readyz answers 200 and the console works, while
# every new connection is refused. It looks fine until it suddenly does not.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
[CmdletBinding()]
param(
    [string] $ConfigDir  = 'C:\ProgramData\ISP BSS\config',
    [string] $InstallDir = 'C:\Program Files\ISP BSS'
)
$ErrorActionPreference = 'Stop'

$envPath = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envPath)) { throw "cannot read $envPath" }
$dsn = [regex]::Match((Get-Content $envPath -Raw), '(?m)^DB_DSN=(.*)$').Groups[1].Value.Trim()
$m = [regex]::Match($dsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $m.Success) { throw 'could not parse DB_DSN out of app.env' }
$user = $m.Groups[1].Value
$pw   = $m.Groups[2].Value
$dbHost = $m.Groups[3].Value
$dbPort = $m.Groups[4].Value
$dbName = $m.Groups[5].Value

$pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
$suPw = $null
if (Test-Path $pwFile) {
    try { $suPw = (Get-Content $pwFile -Raw -ErrorAction Stop).Trim() } catch { $suPw = $null }
}
if (-not $suPw) {
    $sec = Read-Host -AsSecureString 'PostgreSQL superuser (postgres) password'
    $suPw = [Net.NetworkCredential]::new('', $sec).Password
}

$psql = Join-Path $InstallDir 'pgsql\bin\psql.exe'
Write-Host "Resetting role '$user' to the password in app.env ..."
$env:PGPASSWORD = $suPw
try {
    & $psql -h $dbHost -p $dbPort -U postgres -d postgres -v ON_ERROR_STOP=1 `
        -c "ALTER ROLE $user WITH PASSWORD '$($pw -replace "'","''")'"
    if ($LASTEXITCODE -ne 0) { throw "ALTER ROLE failed (exit $LASTEXITCODE)" }
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}

# Prove it, rather than assume it: connect as the application role itself.
Write-Host 'Verifying the application role can log in ...'
$env:PGPASSWORD = $pw
try {
    & $psql -h $dbHost -p $dbPort -U $user -d $dbName -v ON_ERROR_STOP=1 `
        -tAc 'SELECT current_user' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'the application role still cannot log in' }
} finally {
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}
Write-Host "OK - '$user' can authenticate again." -ForegroundColor Green
Write-Host 'Restart the services so their pools drop any connections opened before the change:'
Write-Host '  Restart-Service ISPBSSApi, ISPBSSAaaCore'
