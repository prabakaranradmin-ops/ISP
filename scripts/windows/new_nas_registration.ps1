# new_nas_registration.ps1 - generates the details for registering one NAS
# (router) on the operations console's Routers screen, and the matching
# RouterOS commands for the router's own side, printed together so both
# sides get the exact same secret and ports. See
# specification_docs_v2/23_MTK_MikroTik_Integration_Guide.md for the full
# background this script is a shortcut for.
#
# This does not talk to the running BSS or to the router itself - it only
# generates and prints text for you to copy into the console's web form and
# into a Winbox/SSH terminal on the router. Nothing it does touches machine
# state, so it needs no elevation.
#
# The RADIUS secret is the one field worth generating rather than typing:
# it has to be at least 16 characters, and - since it must match
# character-for-character on both the console and the router - a
# copy-pasted machine-generated value is far less error-prone than typing
# the same long random string twice by hand.
#
# ASCII only - see setup_postgres.ps1's note on why (PowerShell 5.1 default
# console codepage garbles non-ASCII on this project's target machines).
#
# Usage:
#   .\new_nas_registration.ps1
#       Prompts for whatever it needs.
#   .\new_nas_registration.ps1 -RouterIP 192.168.88.1 -Vendor mikrotik -Description 'Main office'
#       Skips the prompts it has answers for.
#   .\new_nas_registration.ps1 -AllowMAB
#       Also prints the extra hotspot-MAB RouterOS lines.
#   .\new_nas_registration.ps1 -SaveTo 'D:\nas-secrets\main-office.txt'
#       Also writes the output to a file - useful since, like the console
#       itself, this script only shows you the secret once.

[CmdletBinding()]
param(
    [string]$RouterIP = '',
    [string]$Vendor = 'mikrotik',
    [string]$Description = '',
    [string]$ServerAddress = '',
    [int]$CoAPort = 1700,
    [int]$PoDPort = 1700,
    [switch]$AllowMAB,
    [int]$SecretLength = 24,
    [string]$SaveTo = ''
)

$ErrorActionPreference = 'Stop'

# RFC 5080 Section 2.3's own minimum for a RADIUS shared secret - matches
# the console's own validation (internal/api/nas.go), so a secret this
# script generates is never rejected by the form.
$minSecretLength = 16
if ($SecretLength -lt $minSecretLength) {
    Write-Warning "SecretLength $SecretLength is below the $minSecretLength-character minimum RADIUS secrets require; using $minSecretLength instead."
    $SecretLength = $minSecretLength
}

function Read-IfBlank {
    param([string]$Value, [string]$Prompt, [string]$Default = '')
    if ($Value -ne '') { return $Value }
    $entered = Read-Host $Prompt
    if ($entered -eq '' -and $Default -ne '') { return $Default }
    return $entered
}

# New-RandomSecret avoids [System.Security.Cryptography.RandomNumberGenerator]::Fill,
# a .NET 6+ static method not present under PowerShell 5.1's .NET Framework
# runtime - the same runtime gap this project's own build/install scripts
# have already hit once. RNGCryptoServiceProvider is the .NET Framework
# equivalent, available since .NET 2.0.
function New-RandomSecret {
    param([int]$Length)
    $alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    $bytes = New-Object byte[] $Length
    $rng = New-Object System.Security.Cryptography.RNGCryptoServiceProvider
    try {
        $rng.GetBytes($bytes)
    } finally {
        $rng.Dispose()
    }
    -join ($bytes | ForEach-Object { $alphabet[$_ % $alphabet.Length] })
}

# Best-effort detection of this machine's own LAN address, offered as a
# suggestion only - which address the router can actually reach this
# server on depends on the network layout, so this never guesses silently.
function Get-CandidateServerAddresses {
    Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Where-Object {
            $_.IPAddress -ne '127.0.0.1' -and
            $_.PrefixOrigin -ne 'WellKnown' -and
            -not $_.IPAddress.StartsWith('169.254.')
        } |
        Select-Object -ExpandProperty IPAddress -Unique
}

# ── Gather details ──────────────────────────────────────────────────────────

$RouterIP = Read-IfBlank -Value $RouterIP -Prompt 'Router IP address (the one this server will see its RADIUS packets come from)'
if ($RouterIP -eq '') { throw 'A router IP address is required.' }

if ($Description -eq '') {
    $Description = Read-Host 'Description (optional, e.g. "Main office MikroTik" - press Enter to skip)'
}

if ($ServerAddress -eq '') {
    $candidates = @(Get-CandidateServerAddresses)
    if ($candidates.Count -gt 0) {
        Write-Host "This machine's own network address(es): $($candidates -join ', ')"
    }
    $ServerAddress = Read-Host 'This server''s address, as the router will reach it (pick one from above, or type another)'
    if ($ServerAddress -eq '' -and $candidates.Count -eq 1) { $ServerAddress = $candidates[0] }
}

$secret = New-RandomSecret -Length $SecretLength

# ── Print ────────────────────────────────────────────────────────────────────

$lines = New-Object System.Collections.Generic.List[string]
$lines.Add('=' * 70)
$lines.Add('PASTE INTO THE CONSOLE (Routers screen -> Register a device)')
$lines.Add('=' * 70)
$lines.Add("Vendor:        $Vendor")
$lines.Add("IP address:    $RouterIP")
$lines.Add("Description:   $Description")
$lines.Add("RADIUS secret: $secret")
$lines.Add("CoA port:      $CoAPort")
$lines.Add("PoD port:      $PoDPort")
$lines.Add("Allow MAC-only auth (hotspot): $(if ($AllowMAB) { 'checked' } else { 'unchecked' })")
$lines.Add('')
$lines.Add('=' * 70)
$lines.Add('PASTE INTO A ROUTEROS TERMINAL ON THE ROUTER (Winbox -> New Terminal)')
$lines.Add('=' * 70)
$lines.Add('/radius')
$lines.Add("add service=ppp,hotspot address=$ServerAddress secret=$secret authentication-port=1812 accounting-port=1813 timeout=3s")
$lines.Add('')
$lines.Add('/ppp aaa')
$lines.Add('set use-radius=yes')
$lines.Add('')
$lines.Add('/radius incoming')
$lines.Add("set accept=yes port=$CoAPort")
if ($AllowMAB) {
    $lines.Add('')
    $lines.Add('/ip hotspot profile')
    $lines.Add('set [find] use-radius=yes')
    $lines.Add('set [find] login-by=mac,http-chap')
    $lines.Add('')
    $lines.Add('/ip hotspot walled-garden')
    $lines.Add("add dst-host=$ServerAddress")
} else {
    $lines.Add('')
    $lines.Add('# If this router also does hotspot/Wi-Fi (not just PPPoE), also run:')
    $lines.Add('# /ip hotspot profile')
    $lines.Add('# set [find] use-radius=yes')
    $lines.Add('# /ip hotspot walled-garden')
    $lines.Add("# add dst-host=$ServerAddress")
    $lines.Add('# (re-run this script with -AllowMAB if you also want MAC-only reconnect)')
}
$lines.Add('')
$lines.Add('=' * 70)
$lines.Add('The secret above will not be shown again once you save it in the')
$lines.Add('console - copy it somewhere safe now if you are not saving this')
$lines.Add('output to a file (-SaveTo).')
$lines.Add('Wait about 60 seconds after registering before testing a real login -')
$lines.Add('the RADIUS daemon only refreshes its router list once a minute.')
$lines.Add('=' * 70)

$output = $lines -join "`r`n"
Write-Host ''
Write-Host $output

if ($SaveTo -ne '') {
    $dir = Split-Path -Parent $SaveTo
    if ($dir -ne '' -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
    Set-Content -Path $SaveTo -Value $output -Encoding ASCII
    Write-Host ''
    Write-Host "Also saved to $SaveTo - this file contains the plaintext secret; keep it out of source control and delete it once you no longer need it."
}
