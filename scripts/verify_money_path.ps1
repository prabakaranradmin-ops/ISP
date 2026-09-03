<#
.SYNOPSIS
    End-to-end verification of the money path: signed webhook -> wallet ->
    general ledger -> subscriber notification.

.DESCRIPTION
    Drives a simulated Razorpay payment through the running install and
    asserts what actually happened in the database and at the notification
    gateway, rather than trusting an HTTP 200.

    WHY IT CHECKS PRECONDITIONS FIRST

    Most of what this script asserts can fail *silently* on a system that is
    not fully deployed: the GL posts nothing if migration 045 has not run,
    notifications reach nobody if MOCK_GATEWAY_URL was set after the service
    started, and the webhook returns 503 if RAZORPAY_WEBHOOK_SECRET is
    absent. A script that reported PASS in any of those states would be
    worse than no script — so it refuses to run instead, naming what is
    missing.

    NOTHING HERE IS MOCKED EXCEPT THE FACT THAT MONEY MOVED

    Razorpay signs its webhooks with a secret this deployment chooses, so a
    correctly-signed payload is indistinguishable from a real one to the
    receiving code. Signature verification, the double-entry wallet credit,
    franchise commission settlement, GL posting and the receipt notification
    all run for real.

.PARAMETER SubscriberId
    Subscriber to credit. Default 14 (chrtest).

.PARAMETER Amount
    Rupees. Default 599.00.

.EXAMPLE
    pwsh scripts/verify_money_path.ps1
    pwsh scripts/verify_money_path.ps1 -SubscriberId 14 -Amount 250.00 -SkipNotifications
#>
[CmdletBinding()]
param(
    [int]    $SubscriberId      = 14,
    [string] $Amount            = '599.00',
    [int]    $GatewayPort       = 9999,
    [string] $WebhookUrl        = 'https://localhost/webhooks/razorpay',
    [string] $ConfigDir         = 'C:\ProgramData\ISP BSS\config',
    [string] $PgBin             = 'C:\Program Files\ISP BSS\pgsql\bin\psql.exe',
    [switch] $SkipNotifications,
    [switch] $SkipGL
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent $PSScriptRoot

# ── Result tracking ─────────────────────────────────────────────────────────
$script:Checks = @()
function Add-Result {
    param([string]$Name, [bool]$Ok, [string]$Detail)
    $script:Checks += [PSCustomObject]@{ Check = $Name; Ok = $Ok; Detail = $Detail }
    $mark = if ($Ok) { 'PASS' } else { 'FAIL' }
    $colour = if ($Ok) { 'Green' } else { 'Red' }
    Write-Host ("  [{0}] {1}" -f $mark, $Name) -ForegroundColor $colour
    if ($Detail) { Write-Host ("         {0}" -f $Detail) -ForegroundColor DarkGray }
}
function Section { param([string]$T) Write-Host "`n$T" -ForegroundColor Cyan }
function Abort {
    param([string]$Why, [string]$Fix)
    Write-Host "`nPRECONDITION NOT MET" -ForegroundColor Yellow
    Write-Host "  $Why"
    if ($Fix) { Write-Host "`n  Fix:`n$Fix" -ForegroundColor Gray }
    Write-Host "`nRefusing to run: a PASS from here would not mean what it says.`n" -ForegroundColor Yellow
    exit 2
}

# ── Database helper ─────────────────────────────────────────────────────────
$envPath = Join-Path $ConfigDir 'app.env'
if (-not (Test-Path $envPath)) { Abort "Cannot read $envPath" "Run this on the machine where the stack is installed." }

$envText = Get-Content $envPath -Raw
function Get-EnvValue {
    param([string]$Key)
    $m = [regex]::Match($envText, "(?m)^$Key=(.*)$")
    if ($m.Success) { return $m.Groups[1].Value.Trim() }
    return ''
}

$dbDsn = Get-EnvValue 'DB_DSN'
if (-not $dbDsn) { Abort "DB_DSN missing from app.env" }
# postgres://user:pass@host:port/db?params
$dsnMatch = [regex]::Match($dbDsn, 'postgres://([^:]+):([^@]+)@([^:]+):(\d+)/([^?]+)')
if (-not $dsnMatch.Success) { Abort "Could not parse DB_DSN" }
$dbUser = $dsnMatch.Groups[1].Value
$dbPass = $dsnMatch.Groups[2].Value
$dbHost = $dsnMatch.Groups[3].Value
$dbPort = $dsnMatch.Groups[4].Value
$dbName = $dsnMatch.Groups[5].Value

function Invoke-Sql {
    param([string]$Sql)
    $env:PGPASSWORD = $dbPass
    try {
        $out = & $PgBin -h $dbHost -p $dbPort -U $dbUser -d $dbName -tAc $Sql 2>&1
        if ($LASTEXITCODE -ne 0) { throw "psql failed: $out" }
        return ($out | Where-Object { $_ -ne '' })
    } finally { Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue }
}

# ── mockpay wrapper ─────────────────────────────────────────────────────────
#
# mockpay exits non-zero on any non-200, and `go run` reports that on stderr.
# With $ErrorActionPreference = 'Stop' PowerShell turns that into a
# terminating NativeCommandError, which aborts the run mid-way and hides the
# result the script was about to assert on. A non-200 is data here, not a
# script failure, so the preference is relaxed for the call and the exit code
# is read deliberately.
function Invoke-Mockpay {
    # NOT named $Args: that is a PowerShell automatic variable, and a
    # parameter of that name silently binds to nothing — every extra flag
    # was dropped, so -bad-signature and -payment-id never reached mockpay.
    # The script then reported that a forged payload was accepted and that a
    # replay double-credited, when in truth it had sent a valid, distinct
    # payment each time and the application behaved correctly.
    param([string[]]$ExtraArgs = @())
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $all = @('run', './scripts/mockpay',
                 '-subscriber', "$SubscriberId", '-amount', $Amount,
                 '-secret', $webhookSecret, '-url', $WebhookUrl) + $ExtraArgs
        $out = & go @all 2>&1
        return [PSCustomObject]@{ Text = ($out -join "`n"); ExitCode = $LASTEXITCODE }
    } finally { $ErrorActionPreference = $prev }
}

# Reads the delivery count as a scalar the gateway computes.
#
# Counting a JSON array from PowerShell is genuinely ambiguous:
# Invoke-RestMethod unrolls one onto the pipeline, so
# (Invoke-RestMethod ...).Count and @(Invoke-RestMethod ...).Count disagree —
# 2 versus 1 for the same two records. This script used one form for its
# baseline and the other in the poll loop, compared 1 against 2, and reported
# that no notification had been delivered when two had. The gateway now
# returns {count, deliveries}, so there is a number to read rather than a
# collection to interpret.
function Get-DeliveryCount {
    try { return [int](Invoke-RestMethod -Uri "$gatewayUrl/_deliveries" -TimeoutSec 5).count }
    catch { return 0 }
}

# Reads the HTTP status mockpay echoed. Asserting on the code rather than on
# mockpay's own wording matters: it prints "the invalid signature was
# rejected" for ANY non-200, including a 503 that means the endpoint never
# examined a signature.
function Get-StatusCode {
    param([string]$Text)
    $m = [regex]::Match($Text, '(?m)^\s*(\d{3})\s')
    if ($m.Success) { return [int]$m.Groups[1].Value }
    return 0
}

Write-Host "`nMoney path verification" -ForegroundColor White
Write-Host "  subscriber $SubscriberId, amount $Amount`n" -ForegroundColor DarkGray

# ── Preconditions ───────────────────────────────────────────────────────────
Section "Preconditions"

$webhookSecret = Get-EnvValue 'RAZORPAY_WEBHOOK_SECRET'
if (-not $webhookSecret) {
    Abort "RAZORPAY_WEBHOOK_SECRET is not set in app.env, so the webhook answers 503 and refuses every payload." @"
    (elevated PowerShell)
    Add-Content "$envPath" "``nRAZORPAY_WEBHOOK_SECRET=dev-webhook-secret-change-before-production"
    Restart-Service ISPBSSApi
"@
}
Add-Result "RAZORPAY_WEBHOOK_SECRET present in app.env" $true ""

# app.env is what the service will read on its NEXT start. What matters is
# what the RUNNING one loaded, and the two diverge exactly when someone has
# just added the secret — which is the common case for a first run.
#
# Probed by sending a deliberately unsigned request: a configured endpoint
# answers 400 (signature rejected), an unconfigured one 503 (refuses before
# looking). Without this, the run continues and the signature check passes
# against a 503, certifying verification on an endpoint doing none.
$probe = Invoke-Mockpay -ExtraArgs @('-bad-signature')
$probeStatus = Get-StatusCode $probe.Text
if ($probeStatus -eq 503) {
    Abort "The RUNNING api_service has no webhook secret loaded — it answers 503, refusing before it examines any signature. The value in app.env has not reached the process." @"
    (elevated PowerShell)
    Restart-Service ISPBSSApi

    app.env is read at startup, so a secret added since the service last
    started is not in effect yet.
"@
}
if ($probeStatus -eq 0) {
    Abort "Could not reach $WebhookUrl — no HTTP status came back." "Check that ISPBSSApi is running, and that -url is right."
}
Add-Result "Running service has the secret loaded (probe: HTTP $probeStatus)" ($probeStatus -eq 400) "503 would mean unconfigured; 400 means it checked and refused"

if (-not $SkipGL) {
    $glAccounts = [int](Invoke-Sql "SELECT count(*) FROM chart_of_accounts WHERE code IN ('5200','2100');")
    if ($glAccounts -lt 2) {
        Abort "Migration 045 is not applied: chart-of-accounts codes 5200/2100 are missing, so GL Phase 2 cannot post." @"
    (elevated PowerShell)
    Stop-Service ISPBSSApi, ISPBSSAaaCore -Force
    `$pw  = (Get-Content "$ConfigDir\postgres_superuser.txt" -Raw).Trim()
    & "C:\Program Files\ISP BSS\bootstrap.exe" -superuser-dsn "postgres://postgres:`$pw@127.0.0.1:5432/${dbName}?sslmode=disable" -config-dir "$ConfigDir"
    Start-Service ISPBSSApi, ISPBSSAaaCore

    Or re-run with -SkipGL to verify only the wallet and notification halves.
"@
    }
    Add-Result "Migration 045 applied (GL accounts present)" $true ""
}

$gstRates = [int](Invoke-Sql "SELECT count(*) FROM gst_rates;")
Add-Result "GST rate configured" ($gstRates -gt 0) "$gstRates rate row(s) — invoices cannot be raised with none"

$subExists = [int](Invoke-Sql "SELECT count(*) FROM subscribers WHERE id = $SubscriberId;")
if ($subExists -ne 1) { Abort "Subscriber $SubscriberId does not exist." "Pass -SubscriberId for one that does." }

# ── Mock gateway ────────────────────────────────────────────────────────────
$gatewayProc = $null
$gatewayUrl  = "http://127.0.0.1:$GatewayPort"

if (-not $SkipNotifications) {
    Section "Mock gateway"

    # The services read MOCK_GATEWAY_URL at startup, so setting it here would
    # not reach an already-running process. Checked rather than assumed:
    # otherwise the notification assertion below fails for a reason that has
    # nothing to do with the code under test.
    #
    # Note the limit of this check: it reads app.env, which is what the
    # service will use on its NEXT start, not necessarily what the running
    # one loaded. Adding the variable and running this script without
    # restarting ISPBSSAaaCore in between passes this check and then fails
    # the delivery assertion — so the failure text below says so explicitly
    # rather than leaving it looking like a code fault.
    $mockConfigured = Get-EnvValue 'MOCK_GATEWAY_URL'
    if (-not $mockConfigured) {
        Write-Host "  MOCK_GATEWAY_URL is not in app.env." -ForegroundColor Yellow
        Write-Host "  Notification delivery cannot be observed; skipping that half." -ForegroundColor Yellow
        Write-Host "  To enable it (elevated), then restart ISPBSSAaaCore:" -ForegroundColor DarkGray
        Write-Host "    Add-Content `"$envPath`" `"``nMOCK_GATEWAY_URL=$gatewayUrl`"" -ForegroundColor DarkGray
        $SkipNotifications = $true
    } else {
        $gatewayProc = Start-Process -FilePath 'go' `
            -ArgumentList @('run', './cmd/mockgateway', '-addr', "127.0.0.1:$GatewayPort") `
            -WorkingDirectory $RepoRoot -PassThru -WindowStyle Hidden
        Start-Sleep -Seconds 4
        try {
            $null = Get-DeliveryCount
            Add-Result "Mock gateway listening on $GatewayPort" $true ""
        } catch {
            Add-Result "Mock gateway listening on $GatewayPort" $false $_.Exception.Message
            $SkipNotifications = $true
        }
    }
}

try {
    # ── Baseline ────────────────────────────────────────────────────────────
    Section "Baseline"
    $balanceBefore = [decimal](Invoke-Sql "SELECT wallet_balance FROM subscribers WHERE id = $SubscriberId;")
    $ledgerBefore  = [int](Invoke-Sql "SELECT count(*) FROM wallet_ledgers WHERE subscriber_id = $SubscriberId;")
    $glBefore      = if ($SkipGL) { 0 } else { [int](Invoke-Sql "SELECT count(*) FROM gl_journal_entries;") }
    $notifBefore   = if ($SkipNotifications) { 0 } else { Get-DeliveryCount }
    Write-Host "  wallet=$balanceBefore  ledger_rows=$ledgerBefore  gl_entries=$glBefore" -ForegroundColor DarkGray

    # ── 1. An invalid signature must be rejected ────────────────────────────
    Section "1. Invalid signature"

    # This runs FIRST on purpose. If a forged payload is accepted, anyone on
    # the network can credit any wallet, and every later assertion is moot.
    $bad = Invoke-Mockpay -ExtraArgs @('-bad-signature')
    $badStatus = Get-StatusCode $bad.Text

    # 400 specifically, not merely "not 200".
    #
    # An earlier version of this check accepted any rejection and reported a
    # PASS against a 503 — which means the endpoint refused before looking at
    # the signature at all, because RAZORPAY_WEBHOOK_SECRET was unset in the
    # RUNNING process. That is the precise failure this script exists to
    # catch, so accepting it here made the script worse than useless: it
    # certified signature verification on an endpoint doing none.
    Add-Result "Forged signature rejected with 400" ($badStatus -eq 400) "HTTP $badStatus"

    $balanceAfterBad = [decimal](Invoke-Sql "SELECT wallet_balance FROM subscribers WHERE id = $SubscriberId;")
    Add-Result "Forged payload moved no money" ($balanceAfterBad -eq $balanceBefore) "before=$balanceBefore after=$balanceAfterBad"

    # ── 2. A valid signature must be accepted ───────────────────────────────
    Section "2. Valid payment"
    $paymentId = "pay_verify$([DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds())"
    $good = Invoke-Mockpay -ExtraArgs @('-payment-id', $paymentId)
    $goodStatus = Get-StatusCode $good.Text
    Add-Result "Signed payload accepted" ($goodStatus -eq 200) "HTTP $goodStatus"

    Start-Sleep -Seconds 2  # commission settlement and the receipt enqueue are post-commit

    # ── 3. Wallet ───────────────────────────────────────────────────────────
    Section "3. Wallet"
    $balanceAfter = [decimal](Invoke-Sql "SELECT wallet_balance FROM subscribers WHERE id = $SubscriberId;")
    $expected     = $balanceBefore + [decimal]$Amount
    Add-Result "Balance credited by $Amount" ($balanceAfter -eq $expected) "before=$balanceBefore after=$balanceAfter expected=$expected"

    # Double entry: one recharge writes two rows, and they must offset.
    $ledgerAfter = [int](Invoke-Sql "SELECT count(*) FROM wallet_ledgers WHERE subscriber_id = $SubscriberId;")
    Add-Result "Two ledger legs written" (($ledgerAfter - $ledgerBefore) -eq 2) "added $($ledgerAfter - $ledgerBefore) row(s)"

    $legSum = Invoke-Sql @"
SELECT COALESCE(SUM(CASE WHEN entry_type='credit' THEN amount ELSE -amount END),0)
  FROM wallet_ledgers
 WHERE subscriber_id = $SubscriberId AND transaction_token = '$paymentId';
"@
    # The token is on the credit leg only (idx_wallet_token is unique over
    # non-null tokens), so this is the credit itself rather than a net of two.
    Add-Result "Credit leg carries the payment id" ([decimal]$legSum -eq [decimal]$Amount) "token leg = $legSum"

    # ── 4. Idempotency ──────────────────────────────────────────────────────
    Section "4. Idempotency"

    # Razorpay retries webhooks. A replay must not credit twice — this is the
    # single most expensive bug this path can have.
    $null = Invoke-Mockpay -ExtraArgs @('-payment-id', $paymentId)
    Start-Sleep -Seconds 1
    $balanceReplay = [decimal](Invoke-Sql "SELECT wallet_balance FROM subscribers WHERE id = $SubscriberId;")
    Add-Result "Replayed payment did not double-credit" ($balanceReplay -eq $balanceAfter) "after replay=$balanceReplay (want $balanceAfter)"

    # ── 5. General ledger ───────────────────────────────────────────────────
    if (-not $SkipGL) {
        Section "5. General ledger"
        $glAfter = [int](Invoke-Sql "SELECT count(*) FROM gl_journal_entries;")
        Add-Result "A journal entry was posted" ($glAfter -gt $glBefore) "entries $glBefore -> $glAfter"

        # The invariant that matters. trg_gl_journal_balanced enforces it at
        # the database, so a failure here means the trigger is missing rather
        # than that the posting is merely wrong.
        $unbalanced = [int](Invoke-Sql @"
SELECT count(*) FROM (
  SELECT journal_entry_id FROM gl_journal_lines
  GROUP BY journal_entry_id HAVING SUM(debit) <> SUM(credit)
) t;
"@)
        Add-Result "Every journal entry balances" ($unbalanced -eq 0) "$unbalanced unbalanced entr(y/ies)"

        $walletEntry = [int](Invoke-Sql "SELECT count(*) FROM gl_journal_entries WHERE source_type = 'wallet_ledger';")
        Add-Result "Posted against the wallet source type" ($walletEntry -gt 0) "$walletEntry wallet_ledger entr(y/ies)"
    }

    # ── 6. Notifications ────────────────────────────────────────────────────
    if (-not $SkipNotifications) {
        Section "6. Notifications"

        # The receipt is enqueued, then delivered by radiusd's worker pool —
        # so this is genuinely asynchronous and needs a moment.
        $deliveredNow = $notifBefore
        for ($i = 0; $i -lt 15; $i++) {
            Start-Sleep -Seconds 2
            $deliveredNow = Get-DeliveryCount
            if ($deliveredNow -gt $notifBefore) { break }
        }
        $new = $deliveredNow - $notifBefore
        $detail = if ($new -gt 0) {
            "$new new delivery/ies at the gateway"
        } else {
            "nothing reached the gateway in 30s. If MOCK_GATEWAY_URL was added to " +
            "app.env without restarting ISPBSSAaaCore since, the daemon is still " +
            "sending to the real provider (or nowhere) and this is configuration, not code."
        }
        Add-Result "A receipt notification was dispatched" ($new -gt 0) $detail

        if ($new -gt 0) {
            $last = (Invoke-RestMethod -Uri "$gatewayUrl/_deliveries" -TimeoutSec 5).deliveries |
                    Select-Object -Last $new
            $providers = ($last | ForEach-Object { $_.provider } | Sort-Object -Unique) -join ', '
            Write-Host "         providers: $providers" -ForegroundColor DarkGray
        }

    }

} finally {
    if ($gatewayProc) {
        Section "Teardown"
        # `go run` spawns the built binary as a child, so killing the wrapper
        # alone leaves the listener holding the port.
        Get-CimInstance Win32_Process -Filter "ParentProcessId = $($gatewayProc.Id)" -ErrorAction SilentlyContinue |
            ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
        Stop-Process -Id $gatewayProc.Id -Force -ErrorAction SilentlyContinue
        Get-NetTCPConnection -LocalPort $GatewayPort -State Listen -ErrorAction SilentlyContinue |
            ForEach-Object { Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue }
        Write-Host "  mock gateway stopped" -ForegroundColor DarkGray
    }
}

# ── Summary ─────────────────────────────────────────────────────────────────
$passed = @($script:Checks | Where-Object Ok).Count
$failed = @($script:Checks | Where-Object { -not $_.Ok }).Count

Write-Host "`n────────────────────────────────────────────────" -ForegroundColor White
if ($failed -eq 0) {
    Write-Host " MONEY PATH VERIFIED — $passed checks passed" -ForegroundColor Green
    Write-Host " Signed webhook -> wallet -> ledger" -NoNewline -ForegroundColor DarkGray
    if (-not $SkipGL)            { Write-Host " -> GL" -NoNewline -ForegroundColor DarkGray }
    if (-not $SkipNotifications) { Write-Host " -> notification" -NoNewline -ForegroundColor DarkGray }
    Write-Host ""
} else {
    Write-Host " FAILED — $failed of $($passed + $failed) checks" -ForegroundColor Red
    $script:Checks | Where-Object { -not $_.Ok } | ForEach-Object {
        Write-Host "   - $($_.Check): $($_.Detail)" -ForegroundColor Red
    }
}
Write-Host "────────────────────────────────────────────────`n" -ForegroundColor White

# Assigned first rather than inlined. `exit ($(if ...))` returned -1 on a
# fully passing run: the subexpression yielded something exit could not
# interpret, so a green run reported failure to any CI step reading the code.
if ($failed -eq 0) { exit 0 } else { exit 1 }
