# install.ps1 - orchestrates the native install's first-run sequence:
# provision PostgreSQL, migrate and generate secrets, register the two
# Windows services.
#
# This exists because MSI custom actions are the wrong place to express
# "run these three already-tested scripts, in this order, and stop with a
# clear reason if any of them fails." Passing data between multiple
# deferred custom actions (the Postgres password, in particular) needs
# either session properties that deferred actions cannot see, or a
# CustomActionData string built up earlier in the sequence - real
# machinery for something a single PowerShell script already does in six
# lines. The MSI's job is reduced to laying files down and invoking this
# script once, elevated, after they exist.
#
# Idempotent for the same reason each of the three steps it calls already
# is: an upgrade runs this exact script again unchanged, and every step
# below already knows how to leave an existing install alone rather than
# needing this script to guess whether it is a first run or not.
#
# ASCII only - see setup_postgres.ps1's note on why.
#
# Usage (as the MSI's install custom action runs it):
#   install.ps1 -InstallDir 'C:\Program Files\ISP BSS' -ConfigDir 'C:\ProgramData\ISP BSS\config'

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [Parameter(Mandatory = $true)][string]$ConfigDir,
    [int]$DBPort = 5432,
    [string]$DatabaseName = 'isp_bss_oss'
)

$ErrorActionPreference = 'Stop'
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null

# The PostgreSQL superuser password. Generated once and kept in a file
# under $ConfigDir rather than only ever held in a variable: an operator
# legitimately needs it later for pg_dump, manual recovery, or connecting a
# database GUI tool, and there is no other record of it anywhere - the
# services themselves never see it, only the least-privilege bss_app
# credentials cmd/bootstrap provisions. ACLed to Administrators and SYSTEM
# immediately after writing it, the same reasoning app.env and
# aes_keys.json are already 0600 on disk for.
$pwFile = Join-Path $ConfigDir 'postgres_superuser.txt'
if (Test-Path $pwFile) {
    $superuserPassword = (Get-Content -Path $pwFile -Raw).Trim()
    Write-Host "install: reusing existing PostgreSQL superuser credentials"
} else {
    # 32 random bytes, base64url-encoded (RFC 4648 sec. 5, no padding) -
    # the same encoding cmd/bootstrap's own randomSecret uses for
    # JWT_SECRET and friends, and for the same reason: standard base64 can
    # contain '+', '/' and '=', any of which would corrupt the
    # postgres://user:password@host DSN built from this value below ('/'
    # in particular reads as an early path separator to a DSN parser).
    # base64url's alphabet is exactly [A-Za-z0-9_-], none of which are
    # reserved in a URI's userinfo segment, so the raw value is safe to
    # interpolate directly with no percent-encoding needed.
    #
    # RandomNumberGenerator.Create() + GetBytes(), not the static .Fill()
    # method: Windows PowerShell 5.1 runs on .NET Framework, not modern
    # .NET, and .Fill() does not exist there - confirmed by running this
    # exact line, which failed with "does not contain a method named
    # 'Fill'" on the first attempt.
    $bytes = [byte[]]::new(32)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($bytes)
    } finally {
        $rng.Dispose()
    }
    $superuserPassword = [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')

    Set-Content -Path $pwFile -Value $superuserPassword -NoNewline -Encoding ascii
    & icacls $pwFile /inheritance:r /grant:r "*S-1-5-32-544:R" "*S-1-5-18:F" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls failed to lock down $pwFile with exit code $LASTEXITCODE" }
    Write-Host "install: generated new PostgreSQL superuser credentials"
}

# Steps 1-3 below are '&'-invoked scripts, not native executables: an
# uncaught `throw` inside any of them is a terminating error that
# propagates straight through the '&' call under this script's own
# $ErrorActionPreference = 'Stop', with no need to separately inspect
# $LASTEXITCODE the way a native .exe call does (bootstrap.exe, further
# down, is the one call here that does need it).

# 1. Provision PostgreSQL: initdb, loopback-only config, service, createdb.
& (Join-Path $scriptDir 'setup_postgres.ps1') `
    -InstallDir $InstallDir -SuperuserPassword $superuserPassword `
    -Port $DBPort -DatabaseName $DatabaseName

# 2. Migrate the schema and generate the application's own secrets.
$dsn = "postgres://postgres:$superuserPassword@127.0.0.1:$DBPort/${DatabaseName}?sslmode=disable"
& (Join-Path $InstallDir 'bootstrap.exe') -superuser-dsn $dsn -config-dir $ConfigDir -db-port $DBPort -db-name $DatabaseName
if ($LASTEXITCODE -ne 0) { throw "bootstrap.exe failed with exit code $LASTEXITCODE" }

# 3. Register and start both Windows services.
& (Join-Path $scriptDir 'register_services.ps1') -InstallDir $InstallDir -ConfigDir $ConfigDir

Write-Host "install: complete"
