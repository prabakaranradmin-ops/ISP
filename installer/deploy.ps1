# deploy.ps1 - installs or upgrades ISP BSS/OSS from a built MSI in one
# step: stop the running services, run msiexec, verify the result.
#
# Default mode is a single-transaction UPGRADE (`msiexec /i` over the
# existing install), not a separate uninstall-then-install. That is
# deliberate, not an oversight: Product.wxs's MajorUpgrade element plus its
# fixed UpgradeCode already make Windows Installer remove the old version
# and lay down the new one atomically inside that one /i call. Running a
# separate `/x` first (against a locked, running install) is exactly what
# produced a stuck transaction during this project's own installer
# debugging - PostgreSQL files got renamed to .tmp mid-uninstall, their
# real deletion got queued in PendingFileRenameOperations for next reboot,
# and the leftover transaction then made every following msiexec call fail
# with "Another program is being installed" (error 1500) until a reboot
# cleared it. Stopping the services ourselves first, before msiexec ever
# runs, is what actually avoids that class of problem - the MSI's own
# ServiceControl entries would do the same thing during the upgrade, but
# stopping them here too means the Files-In-Use / Restart Manager dialog
# never has anything left to complain about.
#
# -FullReinstall is an explicit opt-in for when a real uninstall-then-
# install is actually wanted (troubleshooting a corrupted install, say) -
# it looks up the currently-installed ProductCode from the registry (the
# one Product.wxs generates fresh per build, so it cannot be hardcoded) and
# runs `/x` before the `/i`. Nothing under pgdata\ or config\ is touched by
# either path - see uninstall.ps1's own reasoning for why data always
# outlives the software that used it.
#
# Usage:
#   .\deploy.ps1
#   .\deploy.ps1 -MsiPath 'D:\path\to\some.msi'
#   .\deploy.ps1 -FullReinstall

[CmdletBinding()]
param(
    [string]$MsiPath = '',
    [switch]$FullReinstall,
    [string]$LogDir = ''
)

$ErrorActionPreference = 'Stop'

# Not defaulted directly in the param block above: $PSScriptRoot is only
# reliably populated once the script body starts running, not yet during
# parameter default-value evaluation under `-File` invocation (the same
# issue build.ps1's own $OutDir hit and documents in more detail).
if ($MsiPath -eq '') { $MsiPath = Join-Path $PSScriptRoot 'dist\ISP-BSS-Setup-1.0.0.msi' }
if ($LogDir -eq '') { $LogDir = Join-Path $PSScriptRoot 'dist' }

$services = @('ISPBSSApi', 'ISPBSSAaaCore', 'ISPBSSPostgres')
$productName = 'ISP BSS/OSS'

function Assert-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($id)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "deploy.ps1 must run elevated (Run as Administrator). Use deploy.bat, which elevates itself, rather than calling this script directly."
    }
}

# Dependents before the service they depend on: ISPBSSApi and
# ISPBSSAaaCore both DependOnService ISPBSSPostgres (register_services.ps1),
# so Windows refuses to stop Postgres first while either is still running.
function Stop-AppServices {
    Write-Host "Stopping services (if running)..."
    foreach ($name in $services) {
        $svc = Get-Service -Name $name -ErrorAction SilentlyContinue
        if (-not $svc) {
            Write-Host "  $name - not registered, skipping"
            continue
        }
        if ($svc.Status -eq 'Stopped') {
            Write-Host "  $name - already stopped"
            continue
        }
        Write-Host "  $name - stopping..."
        Stop-Service -Name $name -Force -ErrorAction SilentlyContinue
        try { $svc.WaitForStatus('Stopped', (New-TimeSpan -Seconds 30)) } catch {
            Write-Warning "  $name did not report Stopped within 30s - continuing anyway."
        }
    }
}

# ProductCode is auto-generated per build (Product.wxs has no fixed one -
# that is what lets MajorUpgrade tell "same product, newer version" from
# "different product"), so it has to be looked up fresh from the registry
# rather than assumed from an earlier install.
function Get-InstalledProductCode {
    $roots = @(
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*',
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*'
    )
    foreach ($root in $roots) {
        $hit = Get-ItemProperty -Path $root -ErrorAction SilentlyContinue |
            Where-Object { $_.DisplayName -eq $productName } |
            Select-Object -First 1
        if ($hit) { return $hit.PSChildName }
    }
    return $null
}

function Invoke-Msiexec {
    param([string[]]$MsiArgs, [string]$LogFile)
    $allArgs = $MsiArgs + @('/qn', '/l*v', $LogFile)
    Write-Host "  msiexec $($allArgs -join ' ')"
    $p = Start-Process -FilePath 'msiexec.exe' -ArgumentList $allArgs -Wait -PassThru
    return $p.ExitCode
}

function Show-LogTail {
    param([string]$LogFile, [int]$Lines = 40)
    if (Test-Path $LogFile) {
        Write-Host "`n--- last $Lines lines of $LogFile ---"
        Get-Content $LogFile -Tail $Lines -ErrorAction SilentlyContinue
    }
}

# ── Main ──────────────────────────────────────────────────────────────────

Assert-Admin

if (-not (Test-Path $MsiPath)) {
    throw "MSI not found: $MsiPath (build it first with build.bat / build.ps1)"
}
if (-not (Test-Path $LogDir)) {
    New-Item -ItemType Directory -Path $LogDir -Force | Out-Null
}

if ($FullReinstall) {
    $productCode = Get-InstalledProductCode
    if ($productCode) {
        Write-Host "Full reinstall requested - found installed product $productCode, uninstalling first..."
        Stop-AppServices
        $uninstallLog = Join-Path $LogDir 'uninstall.log'
        $code = Invoke-Msiexec -MsiArgs @('/x', $productCode) -LogFile $uninstallLog
        if ($code -ne 0) {
            Write-Warning "Uninstall exited with code $code."
            Show-LogTail -LogFile $uninstallLog
            Write-Warning "Continuing to install anyway - if this fails too, check the log above before retrying."
        } else {
            Write-Host "Uninstall completed."
        }
    } else {
        Write-Host "Full reinstall requested, but no existing install was found in the registry - nothing to uninstall."
    }
}

Stop-AppServices

Write-Host "`nInstalling $MsiPath ..."
$installLog = Join-Path $LogDir 'install.log'
$code = Invoke-Msiexec -MsiArgs @('/i', $MsiPath) -LogFile $installLog

switch ($code) {
    0 {
        Write-Host "Install succeeded." -ForegroundColor Green
    }
    3010 {
        Write-Host "Install succeeded; Windows is asking for a reboot (pending file operations). Reboot when convenient." -ForegroundColor Yellow
    }
    1618 {
        Write-Warning "Another installation is already in progress (error 1618). Wait for it to finish, or reboot if it looks stuck, then re-run deploy.bat."
        exit $code
    }
    default {
        Write-Warning "msiexec failed with exit code $code."
        Show-LogTail -LogFile $installLog
        exit $code
    }
}

Write-Host "`nWaiting a moment for services to come up..."
Start-Sleep -Seconds 5

Write-Host "`nService status:"
Get-Service -Name $services -ErrorAction SilentlyContinue | Format-Table Name, Status -AutoSize

Write-Host "Health check:"
& curl.exe -sk -o NUL -w "  https://localhost/readyz -> %{http_code}`n" https://localhost/readyz
