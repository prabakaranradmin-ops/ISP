# uninstall.ps1 - reverses register_services.ps1 and setup_postgres.ps1's
# service registrations.
#
# Deliberately leaves data behind: pgdata\, config\ (app.env, the AES key
# store, the TLS certificate) and logs\ all survive an uninstall. Deleting
# a customer's database and encryption keys because the software that used
# them was removed would be a second, much worse problem on top of
# whatever prompted the uninstall - the same reasoning cmd/bootstrap
# refuses to regenerate a key store rather than silently replacing one a
# real database still depends on. A reinstall (or a restore onto a new
# machine, with the same data files copied into place) finds everything it
# needs still there.
#
# ASCII only - see setup_postgres.ps1's note on why.
#
# Usage (from an elevated prompt, as the MSI's uninstall action runs it):
#   uninstall.ps1 -InstallDir 'C:\Program Files\ISP BSS' -ConfigDir 'C:\ProgramData\ISP BSS\config'

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [Parameter(Mandatory = $true)][string]$ConfigDir
)

$ErrorActionPreference = 'Stop'
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

# Normalised for the same reason install.ps1 does it - see the fuller
# explanation there. Nothing below currently passes either path to a
# native executable (the trap only springs there, not on PowerShell-to-
# PowerShell calls), but these arrive from the same MSI Directory
# properties with the same trailing backslash, and the next thing added
# here should not have to rediscover that.
$InstallDir = $InstallDir.TrimEnd('\')
$ConfigDir = $ConfigDir.TrimEnd('\')

# Both app services first, then PostgreSQL - the reverse of install.ps1's
# order, so nothing is ever left trying to depend on a service that already
# stopped.
& (Join-Path $scriptDir 'register_services.ps1') -InstallDir $InstallDir -ConfigDir $ConfigDir -Uninstall
& (Join-Path $scriptDir 'setup_postgres.ps1') -InstallDir $InstallDir -Uninstall

Write-Host "uninstall: complete (data under $InstallDir and $ConfigDir left in place)"
