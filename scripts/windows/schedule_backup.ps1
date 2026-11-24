# schedule_backup.ps1 - registers the nightly backup as a Windows scheduled task.
#
# WHY THIS EXISTS
#
# backup.ps1 was written, reviewed and never run. The installer does not
# schedule it, nothing else calls it, and on this project's own development
# machine the backup root was empty: in the entire life of the installation,
# no database backup had ever been taken. A procedure that depends on an
# operator remembering it is not a procedure, and "we have a backup script" is
# not the same claim as "we have backups".
#
# It is separate from install.ps1 on purpose. Scheduling a job that writes
# encryption keys and live credentials to a folder is a decision an operator
# should make knowingly, at a path they choose, rather than find out about
# afterwards. install.ps1 points at this script; it does not run it.
#
# WHAT IT SCHEDULES
#
# backup.ps1 daily, as SYSTEM, keeping the newest -KeepLast backups. SYSTEM
# because the task must run whether or not anybody is logged in, and because
# backup.ps1 restricts the output folder to the account that runs it - an
# operator-owned folder would become unreadable the day that operator leaves.
#
# ASCII only - PowerShell 5.1 reads a .ps1 with no UTF-8 BOM as ANSI.
#
# Usage:
#   .\scripts\windows\schedule_backup.ps1
#   .\scripts\windows\schedule_backup.ps1 -At 03:15 -OutRoot E:\isp-backups -KeepLast 30
#   .\scripts\windows\schedule_backup.ps1 -Remove
[CmdletBinding()]
param(
    [string] $OutRoot   = 'D:\isp-backups',
    [string] $At        = '02:30',
    [int]    $KeepLast  = 14,
    [string] $TaskName  = 'ISP BSS Nightly Backup',
    [switch] $Remove
)
$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
        ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this from an elevated PowerShell: registering a scheduled task that runs as SYSTEM requires it.'
}

if ($Remove) {
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Write-Host "Removed scheduled task '$TaskName'." -ForegroundColor Yellow
        Write-Host 'Nothing is taking backups now.' -ForegroundColor Yellow
    } else {
        Write-Host "No scheduled task named '$TaskName'." -ForegroundColor Yellow
    }
    return
}

$backupScript = Join-Path $PSScriptRoot 'backup.ps1'
if (-not (Test-Path $backupScript)) { throw "cannot find backup.ps1 beside this script" }

# Parsed here rather than handed to the scheduler as a string, so a bad -At is
# an error now instead of a task that never fires.
try { $when = [datetime]::ParseExact($At, 'HH:mm', $null) }
catch { throw "-At must be a 24-hour time such as 02:30, got '$At'" }

if (-not (Test-Path $OutRoot)) {
    New-Item -ItemType Directory -Path $OutRoot -Force | Out-Null
    Write-Host "Created $OutRoot"
}

$action = New-ScheduledTaskAction `
    -Execute 'powershell.exe' `
    -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{0}" -OutRoot "{1}" -KeepLast {2}' -f `
        $backupScript, $OutRoot, $KeepLast)

$trigger = New-ScheduledTaskTrigger -Daily -At $when

# SYSTEM, highest privileges: pg_dump reads the whole database and the task
# must not depend on anybody being logged in.
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest

# StartWhenAvailable matters on a machine that is not always on: without it a
# missed 02:30 is simply skipped, and a week of "the server was off overnight"
# is a week with no backups and no error anywhere.
$settings = New-ScheduledTaskSettingsSet `
    -StartWhenAvailable `
    -DontStopOnIdleEnd `
    -ExecutionTimeLimit (New-TimeSpan -Hours 2) `
    -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Principal $principal -Settings $settings -Force | Out-Null

Write-Host "Scheduled '$TaskName' daily at $At." -ForegroundColor Green
Write-Host "  writes to    $OutRoot"
Write-Host "  keeps        the newest $KeepLast complete backups"
Write-Host ''
Write-Host 'This is a hypothesis until it has run once and been restored once. Do both now:'
Write-Host "  Start-ScheduledTask -TaskName '$TaskName'"
Write-Host "  Get-ScheduledTaskInfo -TaskName '$TaskName'   # LastTaskResult must be 0"
Write-Host "  .\scripts\windows\verify_restore.ps1 -BackupDir (Get-ChildItem '$OutRoot' -Directory | Sort-Object Name -Descending | Select-Object -First 1).FullName"
Write-Host ''
Write-Host 'The backup folder holds encryption keys and live credentials. It is as' -ForegroundColor Yellow
Write-Host 'sensitive as the database. Keep it off shared drives and out of repositories,' -ForegroundColor Yellow
Write-Host 'and make sure whatever copies it off this machine is encrypted in transit.' -ForegroundColor Yellow
