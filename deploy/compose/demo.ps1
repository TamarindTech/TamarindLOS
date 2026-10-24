<#
  Tamarind LOS — local demo
  Runs the agent-native walkthrough against a local deployment and checks each result.

  Usage (from deploy/compose, after `docker compose up -d --build`):
      .\demo.ps1
  Works in Windows PowerShell 5.1 and PowerShell 7+.
#>
param(
    [string]$BaseUrl = "http://localhost:8080",
    [switch]$SkipAuditCheck
)
$ErrorActionPreference = "Stop"
$script:Passed = 0
$script:Failed = 0

$Tokens = @{
    Agent   = "dev-agent-intake"
    Ana     = "dev-officer-ana"
    Chai    = "dev-officer-chai"
    Zoe     = "dev-other-zoe"
}

function Invoke-Api([string]$Path, [string]$Token, [hashtable]$Body) {
    $json = $Body | ConvertTo-Json -Depth 20 -Compress
    $headers = @{ Authorization = "Bearer $Token" }
    try {
        $r = Invoke-WebRequest -Uri "$BaseUrl$Path" -Method Post -Body $json -ContentType "application/json" `
            -Headers $headers -UseBasicParsing
        $status = [int]$r.StatusCode
        $content = $r.Content
    } catch {
        $resp = $_.Exception.Response
        if ($null -eq $resp) { throw }
        $status = [int]$resp.StatusCode
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            $content = $_.ErrorDetails.Message
        } else {
            $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
            $content = $reader.ReadToEnd()
        }
    }
    $obj = $content | ConvertFrom-Json
    $obj | Add-Member -NotePropertyName http -NotePropertyValue $status -Force
    return $obj
}

function Invoke-Tool([string]$Token, [string]$Tool, [hashtable]$ToolInput, [hashtable]$Envelope = @{}) {
    $body = @{ input = $ToolInput; idempotency_key = [guid]::NewGuid().ToString() }
    foreach ($k in $Envelope.Keys) { $body[$k] = $Envelope[$k] }
    return Invoke-Api "/v1/tools/$Tool" $Token $body
}

function Check([string]$Step, $Response, [string]$WantStatus, [string]$WantCode = "") {
    $code = ""
    if ($Response.error) { $code = $Response.error.code }
    if ($Response.status -eq $WantStatus -and $code -eq $WantCode) {
        $script:Passed++
        $detail = if ($code) { "$WantStatus ($code)" } else { $WantStatus }
        Write-Host ("  [ok]   {0,-58} -> {1}" -f $Step, $detail) -ForegroundColor Green
    } else {
        $script:Failed++
        Write-Host ("  [FAIL] {0,-58} -> got {1} {2}, want {3} {4}" -f $Step, $Response.status, $code, $WantStatus, $WantCode) -ForegroundColor Red
        if ($Response.error) { Write-Host ("         {0}" -f $Response.error.message) -ForegroundColor DarkGray }
    }
}

Write-Host "Tamarind LOS demo against $BaseUrl" -ForegroundColor Cyan

# 0. Wait for the service.
$ready = $false
for ($i = 0; $i -lt 60; $i++) {
    try {
        $h = Invoke-RestMethod -Uri "$BaseUrl/healthz" -UseBasicParsing
        if ($h.status -eq "ok") { $ready = $true; break }
    } catch { }
    Start-Sleep -Seconds 2
}
if (-not $ready) {
    Write-Host "The service is not responding at $BaseUrl/healthz. Is 'docker compose up' running?" -ForegroundColor Red
    exit 1
}
Write-Host ("  Service ready: version {0}, policy pack {1}" -f $h.version, $h.policy_pack)
Write-Host ""

$why = @{ reason = "demo: processing a new SME application" }

Write-Host "1. An AI agent takes in an application" -ForegroundColor Cyan
$r = Invoke-Tool $Tokens.Agent "application.create" @{
    product_code = "SME-WC"; requested_amount_minor = 3000000; currency = "USD"; requested_term_months = 12
    purpose = "inventory for peak season"
}
Check "agent call without a reason is rejected" $r "error" "VALIDATION_FAILED"

$r = Invoke-Tool $Tokens.Agent "application.create" @{
    product_code = "SME-WC"; requested_amount_minor = 3000000; currency = "USD"; requested_term_months = 12
    purpose = "inventory for peak season"
} $why
Check "agent creates the application" $r "ok"
$appId = $r.result.application_id
Write-Host ("         reference {0}, id {1}" -f $r.result.reference, $appId) -ForegroundColor DarkGray

$r = Invoke-Tool $Tokens.Agent "application.submit" @{ application_id = $appId } $why
Check "submitting with no parties fails a precondition" $r "error" "PRECONDITION_FAILED"

$r = Invoke-Tool $Tokens.Agent "party.add" @{
    application_id = $appId; kind = "business"; role = "applicant"; legal_name = "Mekong Trading Co."
    registration_number = "0105561234567"; country = "TH"
} $why
Check "agent adds the business applicant" $r "ok"

$r = Invoke-Tool $Tokens.Agent "party.add" @{
    application_id = $appId; kind = "individual"; role = "director"; legal_name = "Somchai P."
    date_of_birth = "1980-04-02"; ownership_pct = 60
} $why
Check "agent adds a director" $r "ok"

Write-Host ""
Write-Host "2. The agent asks what it may do next" -ForegroundColor Cyan
$r = Invoke-Tool $Tokens.Agent "application.next_actions" @{ application_id = $appId } $why
Check "next_actions" $r "ok"
foreach ($a in $r.result.allowed) {
    Write-Host ("         {0,-22} autonomy {1,-4} -> {2}" -f $a.tool, $a.autonomy, $a.outcome) -ForegroundColor DarkGray
}

$r = Invoke-Tool $Tokens.Agent "application.submit" @{ application_id = $appId } $why
Check "agent submits (autonomy L3: acts on its own)" $r "ok"
Write-Host ("         state is now '{0}' (draft -> submitted -> intake automatically)" -f $r.result.state) -ForegroundColor DarkGray

Write-Host ""
Write-Host "3. A request the agent may not carry out alone" -ForegroundColor Cyan
$r = Invoke-Tool $Tokens.Agent "application.withdraw" @{ application_id = $appId; reason = "customer cancelled by phone" } @{
    reason = "customer asked to cancel"; on_behalf_of = "user:officer.ana"
}
Check "agent asks to withdraw (autonomy L2: needs a human)" $r "approval_required"
$approvalId = $r.approval_request_id

$r = Invoke-Tool $Tokens.Ana "approval.respond" @{ approval_request_id = $approvalId; decision = "approve" }
Check "Ana cannot approve a request made for her (maker-checker)" $r "error" "MAKER_CHECKER_VIOLATION"

$r = Invoke-Tool $Tokens.Chai "approval.respond" @{ approval_request_id = $approvalId; decision = "approve"; note = "confirmed with customer" }
Check "Chai approves; the withdrawal runs as Chai" $r "ok"
Write-Host ("         state is now '{0}'" -f $r.result.result.state) -ForegroundColor DarkGray

Write-Host ""
Write-Host "4. Isolation between lenders" -ForegroundColor Cyan
$r = Invoke-Tool $Tokens.Zoe "application.get" @{ application_id = $appId }
Check "another lender cannot see the application" $r "error" "NOT_FOUND"

Write-Host ""
Write-Host "5. MCP: what an AI agent sees" -ForegroundColor Cyan
$list = Invoke-Api "/mcp" $Tokens.Agent @{ jsonrpc = "2.0"; id = 1; method = "tools/list"; params = @{} }
$names = @($list.result.tools | ForEach-Object { $_.name })
if ($names -contains "application_create" -and -not ($names -contains "approval_respond")) {
    $script:Passed++
    Write-Host ("  [ok]   {0,-58} -> {1} tools" -f "tools/list for the intake agent", $names.Count) -ForegroundColor Green
} else {
    $script:Failed++
    Write-Host "  [FAIL] tools/list for the intake agent -> $($names -join ', ')" -ForegroundColor Red
}
Write-Host ("         {0}" -f ($names -join ", ")) -ForegroundColor DarkGray

if (-not $SkipAuditCheck -and (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Host ""
    Write-Host "6. Tamper-evident audit log" -ForegroundColor Cyan
    # Docker writes progress messages to stderr, which Windows PowerShell 5.1 turns into
    # errors. Run it through cmd so stderr is discarded before PowerShell sees it.
    $out = cmd /c "docker compose exec -T core tamarind verify-audit --tenant demo 2>nul"
    $exitCode = $LASTEXITCODE
    if ($exitCode -eq 0) {
        $script:Passed++
        Write-Host ("  [ok]   {0,-58} -> valid" -f "hash chain verifies") -ForegroundColor Green
    } else {
        $script:Failed++
        Write-Host "  [FAIL] hash chain verification" -ForegroundColor Red
    }
    $out | Where-Object { $_ -match '"(events|valid|head)"' } | ForEach-Object { Write-Host ("        {0}" -f $_) -ForegroundColor DarkGray }
}

Write-Host ""
if ($script:Failed -eq 0) {
    Write-Host ("All {0} checks passed." -f $script:Passed) -ForegroundColor Green
} else {
    Write-Host ("{0} passed, {1} failed." -f $script:Passed, $script:Failed) -ForegroundColor Red
    exit 1
}
