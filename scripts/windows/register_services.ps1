# register_services.ps1 - register api_service.exe and aaa_core_daemon.exe
# as Windows services.
#
# Run after setup_postgres.ps1 and bootstrap.exe (see README.md for the full
# install order) - this script refuses to run without an app.env already in
# place, the same way bootstrap.exe refuses to run without a key store: an
# app.env is what a service points at, not what this script generates.
#
# Neither service is told its configuration through the environment the way
# it would be under Docker Compose or a developer's shell. The Service
# Control Manager execs a binary directly with a fixed environment and no
# shell in between, so there is no moment where anything would "source
# app.env" for it. Both binaries instead take an -env-file flag
# (internal/envfile) and read the file themselves; this script's whole job
# is pointing that flag, plus a couple of Windows-specific registrations,
# at the install already sitting on disk.
#
# ASCII only - see setup_postgres.ps1's note on why.
#
# Usage (from an elevated prompt, as the MSI runs it):
#   .\register_services.ps1 -InstallDir 'C:\Program Files\ISP BSS' -ConfigDir 'C:\ProgramData\ISP BSS\config'
#
# Preview without touching machine state:
#   .\register_services.ps1 -InstallDir ... -ConfigDir ... -DryRun
#
# Remove both services (the MSI's uninstall action):
#   .\register_services.ps1 -InstallDir ... -ConfigDir ... -Uninstall

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InstallDir,
    [Parameter(Mandatory = $true)][string]$ConfigDir,

    # Matches setup_postgres.ps1's own default -ServiceName. Both services
    # depend on it: the SCM will not start api_service or aaa_core_daemon
    # until PostgreSQL has reported Running, which orders startup correctly
    # even though nothing here waits for it directly.
    [string]$PostgresServiceName = 'ISPBSSPostgres',

    # Names must match the serviceName constants in cmd/api/main.go and
    # cmd/radiusd/main.go - winservice.IsWindowsService/Run only engage the
    # Windows-service code path when the SCM starts the process under
    # exactly this name.
    [string]$ApiServiceName = 'ISPBSSApi',
    [string]$AaaServiceName = 'ISPBSSAaaCore',

    # Print what would happen without registering, starting, or stopping
    # anything.
    [switch]$DryRun,

    # Stop and remove both services instead of registering them.
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

$services = @(
    @{
        Name        = $ApiServiceName
        DisplayName = 'ISP BSS API'
        Description = 'ISP BSS/OSS HTTPS API, subscriber portal, staff console and TR-069 ACS.'
        Exe         = Join-Path $InstallDir 'api_service.exe'
        # TLS_CERT_DIR's own default (internal/config: "./config/tls") is
        # relative, which resolves fine for a developer running the binary
        # from its own directory but not for a service: the SCM always
        # starts a service with %SystemRoot%\System32 as the working
        # directory, never the install directory, so an unset TLS_CERT_DIR
        # here would have pkg/tlscert try to read and write a cert under
        # System32 instead. Found the same way the AES_KEY_STORE_URL fix in
        # cmd/bootstrap was: running api_service.exe from a directory that
        # is not its install directory and watching where it looked.
        #
        # API_ADDR: :8080 (internal/config's default) was the right answer
        # only while Caddy fronted this service and terminated TLS on 443
        # for it. That reverse proxy is gone - api_service terminates TLS
        # itself now - so on a native install nothing sits in front of it
        # and it should own the standard HTTPS port directly. Without this
        # the subscriber portal, staff console and captive portal are all
        # reachable only on an explicit :8080, which is not what an
        # operator (or this repo's own documented
        # `curl -k https://127.0.0.1/readyz` check) expects.
        #
        # METRICS_ADDR: both binaries default to :9101, which was fine
        # under Docker Compose - each container had its own network
        # namespace, so both could bind :9101 internally and the host-side
        # mapping kept them apart. Natively they share one namespace and
        # cannot both have it: whichever service the SCM started second
        # died on startup. Split explicitly here, API keeping the
        # historical :9101 so existing Prometheus scrape configs pointed at
        # it still work.
        ExtraEnv    = @{
            TLS_CERT_DIR = Join-Path $InstallDir 'tls'
            API_ADDR     = ':443'
            METRICS_ADDR = ':9101'
        }
        # Windows Services never trigger the interactive "Windows Security
        # Alert - allow this app?" prompt that a normal foreground program
        # gets on its first inbound connection - that prompt only fires for
        # a program running in a logged-in user's own session. A service
        # gets no such moment, and Windows Firewall's default inbound
        # action is Block, so without an explicit rule here the staff
        # console, subscriber portal and captive portal are all completely
        # unreachable from any other machine on a real customer's network -
        # only loopback (https://localhost on the box itself) still works,
        # which is exactly what made this gap invisible during development.
        FirewallRule = @{ DisplayName = 'ISP BSS API (HTTPS)'; Protocol = 'TCP'; Ports = @(443) }
    },
    @{
        Name        = $AaaServiceName
        DisplayName = 'ISP BSS AAA Core'
        Description = 'ISP BSS RADIUS AAA daemon, FUP scanner and background task workers.'
        Exe         = Join-Path $InstallDir 'aaa_core_daemon.exe'
        # No TLS_CERT_DIR: aaa_core_daemon never reads it (radiusd does not
        # terminate TLS). METRICS_ADDR is the other half of the :9101
        # collision described above - :9102 rather than the shared default,
        # so both services can expose metrics at once.
        ExtraEnv    = @{ METRICS_ADDR = ':9102' }
        # Same reasoning as the API rule above, for RADIUS auth/accounting
        # instead of HTTPS. Found by an actual MikroTik CHR VM test: real
        # PPPoE dial-in negotiated fine but every Access-Request timed out
        # from the NAS's point of view, because Windows Firewall was
        # silently dropping it before it ever reached this process - the
        # only existing rule on that test machine was scoped to a leftover
        # dev-build exe path, not the installed one, which is precisely the
        # class of bug an installer-provisioned rule (scoped to the actual
        # installed path) prevents from ever happening on a customer site.
        FirewallRule = @{ DisplayName = 'ISP BSS AAA Core (RADIUS)'; Protocol = 'UDP'; Ports = @(1812, 1813) }
    }
)

if ($Uninstall) {
    foreach ($svc in $services) {
        $existing = Get-Service -Name $svc.Name -ErrorAction SilentlyContinue
        if ($null -eq $existing) {
            Write-Host "register_services: $($svc.Name) not registered, nothing to remove"
        } elseif ($DryRun) {
            Write-Host "register_services: [dry run] would stop and remove $($svc.Name)"
        } else {
            if ($existing.Status -eq 'Running') {
                Stop-Service -Name $svc.Name -Force
                $existing.WaitForStatus('Stopped', (New-TimeSpan -Seconds 30))
            }
            & sc.exe delete $svc.Name | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "sc.exe delete failed for $($svc.Name) with exit code $LASTEXITCODE" }
            Write-Host "register_services: removed $($svc.Name)"
        }

        $fwRule = Get-NetFirewallRule -DisplayName $svc.FirewallRule.DisplayName -ErrorAction SilentlyContinue
        if ($null -eq $fwRule) {
            Write-Host "register_services: firewall rule '$($svc.FirewallRule.DisplayName)' not present, nothing to remove"
        } elseif ($DryRun) {
            Write-Host "register_services: [dry run] would remove firewall rule '$($svc.FirewallRule.DisplayName)'"
        } else {
            Remove-NetFirewallRule -DisplayName $svc.FirewallRule.DisplayName
            Write-Host "register_services: removed firewall rule '$($svc.FirewallRule.DisplayName)'"
        }
    }
    return
}

# Refused rather than guessed at, the same reasoning bootstrap.exe applies
# to its own credential files: a service pointed at an app.env that does
# not exist yet would just fail to start with a config error, which is a
# worse first thing for an operator to see than a clear refusal here.
$envFile = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envFile)) {
    throw "missing $envFile - run bootstrap.exe (with -config-dir $ConfigDir) before registering services"
}

$logDir = Join-Path $InstallDir 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

foreach ($svc in $services) {
    if (-not (Test-Path $svc.Exe)) {
        throw "missing $($svc.Exe)"
    }

    # winservice.Fatal (internal/winservice) writes a service's own
    # startup-failure reason to the Event Log under this same service name
    # - the one place an operator can look before LOG_FILE below even
    # exists to be opened. eventlog.Open in that package silently does
    # nothing if the source was never registered, so this step is what
    # makes that log line actually appear rather than vanish.
    #
    # Checked via the registry rather than
    # [System.Diagnostics.EventLog]::SourceExists(): that .NET method
    # enumerates every event log on the machine to confirm the name is not
    # already claimed elsewhere, Security included - and Security is
    # unreadable even to an elevated Administrator by default (it needs
    # SeSecurityPrivilege specifically, not just admin rights), so the call
    # throws here even when this script is running exactly as intended.
    # Found by running this in -DryRun, not by reading it: it fails on the
    # very first service, every time, elevated or not.
    $sourceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\$($svc.Name)"
    if (-not (Test-Path $sourceKey)) {
        if ($DryRun) {
            Write-Host "register_services: [dry run] would create Event Log source $($svc.Name)"
        } else {
            New-EventLog -LogName Application -Source $svc.Name
            Write-Host "register_services: created Event Log source $($svc.Name)"
        }
    }

    # Everything after startup goes to LOG_FILE instead: there is no
    # console for stdout to reach under the SCM, and svclog.Configure
    # (internal/svclog) opens this path append-only precisely so a restart
    # does not erase the entries explaining why the previous run stopped.
    $logFile = Join-Path $logDir ("{0}.log" -f $svc.Name)

    # This exact string becomes the service's registry ImagePath value, so
    # its quoting matters more than it looks like it should: the executable
    # path and the -env-file value both need their own quotes because
    # either can contain spaces ("C:\Program Files\..."), and the embedded
    # quotes must be literal characters in this one PowerShell string, not
    # something PowerShell's own parser sees. Do not "simplify" this by
    # removing the inner quotes - the SCM splits ImagePath on whitespace
    # exactly like any other command line, so without them the executable
    # path alone would be read as three separate arguments.
    #
    # Both consumers below are cmdlets (New-Service, Set-ItemProperty)
    # rather than native executables, which is what keeps these quotes
    # intact: PowerShell 5.1 cannot pass an argument containing double
    # quotes to a native .exe without adding a layer of its own, and doing
    # exactly that is what broke the reconfigure path - see the note there.
    $binPath = '"{0}" -env-file "{1}"' -f $svc.Exe, $envFile

    $existing = Get-Service -Name $svc.Name -ErrorAction SilentlyContinue
    if ($null -eq $existing) {
        if ($DryRun) {
            Write-Host "register_services: [dry run] would create service $($svc.Name) -> $binPath"
        } else {
            New-Service -Name $svc.Name -BinaryPathName $binPath -DisplayName $svc.DisplayName `
                -Description $svc.Description -StartupType Automatic -DependsOn $PostgresServiceName | Out-Null
            Write-Host "register_services: created service $($svc.Name)"
        }
    } else {
        Write-Host "register_services: $($svc.Name) already registered, reconfiguring for this install"
        if (-not $DryRun) {
            if ($existing.Status -eq 'Running') {
                Stop-Service -Name $svc.Name -Force
                $existing.WaitForStatus('Stopped', (New-TimeSpan -Seconds 30))
            }
            # Written straight to the registry rather than through
            # `sc.exe config`. Set-Service in Windows PowerShell 5.1 has no
            # -BinaryPathName (that arrived in PowerShell 7+), which leaves
            # sc.exe as the only *command-line* route - and $binPath
            # contains embedded double quotes, which PowerShell 5.1 cannot
            # hand to a native executable intact. It wraps the whole value
            # in one more quote layer, so sc.exe receives
            # `binPath= ""C:\...\api_service.exe" -env-file "..."" ` and
            # rejects it outright with 1639 (ERROR_INVALID_COMMAND_LINE).
            # Confirmed by reproducing that exact mangled command line
            # standalone, and by the real install: creation via New-Service
            # (a cmdlet - a direct API call with no command-line parsing in
            # between) succeeded, and only this reconfigure path failed.
            #
            # These three values are exactly what New-Service itself sets
            # above, and what `sc.exe config binPath=/start=/depend=` would
            # have set: the SCM reads the service's configuration from this
            # key, so writing it here is the same operation without a
            # command line in the middle. Set-ItemProperty is a cmdlet too,
            # so the quotes in $binPath survive untouched. The value kinds
            # match what the SCM expects and what New-Service produced on
            # the first install: ImagePath REG_EXPAND_SZ, DependOnService
            # REG_MULTI_SZ, Start REG_DWORD 2 (= Automatic).
            $svcKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$($svc.Name)"
            Set-ItemProperty -Path $svcKey -Name ImagePath -Value $binPath -Type ExpandString
            Set-ItemProperty -Path $svcKey -Name DependOnService -Value @($PostgresServiceName) -Type MultiString
            Set-ItemProperty -Path $svcKey -Name Start -Value 2 -Type DWord
        }
    }

    # LOG_FILE, plus each service's own ExtraEnv (TLS_CERT_DIR for the API),
    # set through the service's registry Environment value (REG_MULTI_SZ) -
    # the one way the SCM actually injects environment variables into a
    # service process. Per-service, not shared: each service must write its
    # own log file, not race the other one for the same handle, and only
    # the API needs TLS_CERT_DIR at all.
    if (-not $DryRun) {
        $envLines = @("LOG_FILE=$logFile")
        foreach ($key in $svc.ExtraEnv.Keys) {
            $envLines += "$key=$($svc.ExtraEnv[$key])"
        }
        $envKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$($svc.Name)"
        Set-ItemProperty -Path $envKey -Name Environment -Value $envLines -Type MultiString
    }

    if (-not $DryRun) {
        # Restart on crash, three times a minute apart, then give up and
        # leave it stopped for an operator to look at. A minute covers the
        # ordinary "PostgreSQL's own service reported Running before it was
        # actually done with startup" race - depend= above only orders the
        # two services, it does not wait for PostgreSQL to be ready to
        # accept connections - without retrying forever against a cause
        # that will not fix itself, like a wrong password after a botched
        # manual edit of app.env.
        & sc.exe failure $svc.Name reset= 86400 actions= restart/60000/restart/60000/restart/60000 | Out-Null
    }

    if (-not $DryRun) {
        Start-Service -Name $svc.Name
        Write-Host "register_services: $($svc.Name) started"
    }

    # Scoped to the exe path (-Program), not just the port: an app-scoped
    # rule is what keeps this correct across a reinstall to a different
    # drive or folder, and is also why a rule left over from an old install
    # path (or a dev build elsewhere on disk) does not silently cover a
    # differently-located production binary - each install path needs, and
    # gets, its own rule.
    $fw = $svc.FirewallRule
    $existingFw = Get-NetFirewallRule -DisplayName $fw.DisplayName -ErrorAction SilentlyContinue
    if ($null -ne $existingFw) {
        if ($DryRun) {
            Write-Host "register_services: [dry run] would leave existing firewall rule '$($fw.DisplayName)' as-is"
        } else {
            Remove-NetFirewallRule -DisplayName $fw.DisplayName
            New-NetFirewallRule -DisplayName $fw.DisplayName -Direction Inbound -Action Allow `
                -Program $svc.Exe -Protocol $fw.Protocol -LocalPort $fw.Ports -Profile Any | Out-Null
            Write-Host "register_services: refreshed firewall rule '$($fw.DisplayName)' -> $($svc.Exe)"
        }
    } elseif ($DryRun) {
        Write-Host "register_services: [dry run] would create firewall rule '$($fw.DisplayName)' -> $($svc.Exe)"
    } else {
        New-NetFirewallRule -DisplayName $fw.DisplayName -Direction Inbound -Action Allow `
            -Program $svc.Exe -Protocol $fw.Protocol -LocalPort $fw.Ports -Profile Any | Out-Null
        Write-Host "register_services: created firewall rule '$($fw.DisplayName)' -> $($svc.Exe)"
    }
}

Write-Host "register_services: done"
