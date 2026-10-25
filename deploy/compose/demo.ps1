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
    Ben     = "dev-approver-ben"
    Sara    = "dev-approver-sara"
    Dana    = "dev-ops-dana"
    Mekong  = "dev-borrower-mekong"
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

# Uploads a small mock PDF through a single-use upload link, then attaches it.
function Send-Document([string]$Token, [string]$AppId, [string]$PartyId, [string]$DocType, [string[]]$Fields) {
    $text = "%PDF-1.4`n% TAMARIND-DOC: $DocType`n" + (($Fields | ForEach-Object { "$_`n" }) -join "") + "%%EOF`n"
    $link = Invoke-Tool $Token "document.upload_url" @{
        application_id = $AppId; party_id = $PartyId; file_name = "$DocType.pdf"; mime_type = "application/pdf"
    }
    $bytes = [System.Text.Encoding]::ASCII.GetBytes($text)
    $null = Invoke-WebRequest -Uri "$BaseUrl$($link.result.upload_url)" -Method Put -Body $bytes `
        -ContentType "application/octet-stream" -UseBasicParsing
    return Invoke-Tool $Token "document.attach" @{
        application_id = $AppId; upload_token = $link.result.upload_token; declared_type = $DocType
    }
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

Write-Host ""
Write-Host "6. One loan from intake to booking" -ForegroundColor Cyan
$r = Invoke-Tool $Tokens.Ana "application.create" @{
    product_code = "SME-WC"; requested_amount_minor = 5000000; currency = "USD"; requested_term_months = 12
    purpose = "working capital for peak season"; borrower = "borrower:mekong"
}
Check "Ana opens a 50,000 application for the Mekong portal user" $r "ok"
$loanId = $r.result.application_id
$r = Invoke-Tool $Tokens.Ana "party.add" @{
    application_id = $loanId; kind = "business"; role = "applicant"; legal_name = "Mekong Trading Co."
    registration_number = "0105561234567"; country = "TH"
}
$applicant = $r.result.party_id
$r = Invoke-Tool $Tokens.Ana "party.add" @{
    application_id = $loanId; kind = "individual"; role = "director"; legal_name = "Somchai P."
    date_of_birth = "1980-04-02"; ownership_pct = 60
}
$director = $r.result.party_id
$r = Invoke-Tool $Tokens.Ana "application.submit" @{ application_id = $loanId }
Check "parties added and submitted" $r "ok"
Write-Host ("         state '{0}': waiting for the borrower's consent" -f $r.result.state) -ForegroundColor DarkGray

$uploads = @(
    @($applicant, "business_registration", @("registration_number: 0105561234567", "legal_name: Mekong Trading Co.")),
    @($applicant, "bank_statements", @("account_holder: Mekong Trading Co.", "months_covered: 12")),
    @($applicant, "proof_of_address", @("address: 88 Riverside Road, Bangkok")),
    @($applicant, "financial_statements", @("years_covered: 2")),
    @($director, "identity_document", @("legal_name: Somchai P.", "date_of_birth: 1980-04-02"))
)
foreach ($u in $uploads) { $r = Send-Document $Tokens.Mekong $loanId $u[0] $u[1] $u[2] }
Check "the borrower uploads five documents in the portal" $r "ok"

$click = @{ method = "portal_click"; ip = "203.0.113.10" }
foreach ($c in @(@($applicant, "kyc"), @($applicant, "credit_bureau"), @($applicant, "bank_data"), @($director, "kyc"))) {
    $r = Invoke-Tool $Tokens.Mekong "consent.record" @{ application_id = $loanId; party_id = $c[0]; scope = $c[1]; evidence = $click }
}
Check "the borrower gives consent in the portal" $r "ok"

# From here the built-in agents (the "agents" service) take over: the document agent
# classifies and reads the uploads (completing intake), the verification agent runs the
# checks and the underwriting agent proposes a decision, with no further calls from us.
$proposal = $null
for ($i = 0; $i -lt 45 -and -not $proposal; $i++) {
    Start-Sleep -Seconds 1
    $q = Invoke-Tool $Tokens.Chai "approval.list" @{ application_id = $loanId }
    $proposal = @($q.result.approval_requests | Where-Object { $_.tool -eq "decision.recommend" }) | Select-Object -First 1
}
if (-not $proposal) {
    $script:Failed++
    Write-Host "  [FAIL] no proposal from the agents after 45 seconds. Is the agents service running? (docker compose ps)" -ForegroundColor Red
    Write-Host ("{0} passed, {1} failed." -f $script:Passed, $script:Failed) -ForegroundColor Red
    exit 1
}
$app = (Invoke-Tool $Tokens.Ana "application.get" @{ application_id = $loanId }).result.application
$script:Passed++
$verified = @($app.evidence | Where-Object { $_.status -eq "verified" }).Count
Write-Host ("  [ok]   {0,-58} -> {1} documents" -f "the document agent classified and read the uploads", @($app.documents).Count) -ForegroundColor Green
Write-Host ("         {0} of {1} extracted fields verified against the application" -f $verified, @($app.evidence).Count) -ForegroundColor DarkGray
$script:Passed++
Write-Host ("  [ok]   {0,-58} -> {1} checks" -f "the verification agent ran the checks on its own", @($app.checks).Count) -ForegroundColor Green
foreach ($c in $app.checks) { Write-Host ("         {0,-14} {1}" -f $c.check_type, $c.status) -ForegroundColor DarkGray }
$script:Passed++
Write-Host ("  [ok]   {0,-58} -> {1}" -f "the underwriting agent proposed a decision (L2)", $proposal.proposal.outcome) -ForegroundColor Green
Write-Host ("         DSCR {0}; reason codes {1}" -f $app.metrics.dscr, ($proposal.proposal.reason_codes -join ", ")) -ForegroundColor DarkGray
Write-Host ("         risk grade {0} ({1} of {2} points) -> priced at {3:N2}% APR" -f $app.risk_grade.grade, $app.risk_grade.points, $app.risk_grade.max_points, ($app.risk_grade.apr_bps / 100)) -ForegroundColor DarkGray
($proposal.proposal.narrative -split "`n") | Select-Object -First 3 | ForEach-Object { Write-Host ("         | {0}" -f $_) -ForegroundColor DarkGray }

# Each of those agent actions is a recorded run, with every tool call it made.
$runs = @((Invoke-Tool $Tokens.Ana "agent.run_list" @{ application_id = $loanId }).result.runs)
if ($runs.Count -gt 0 -and -not ($runs | Where-Object { $_.status -eq "failed" })) {
    $script:Passed++
    Write-Host ("  [ok]   {0,-58} -> {1} runs" -f "every agent action is a recorded run", $runs.Count) -ForegroundColor Green
} else {
    $script:Failed++
    Write-Host ("  [FAIL] agent runs: {0}" -f (($runs | ForEach-Object { "$($_.agent) $($_.status)" }) -join ", ")) -ForegroundColor Red
}
[array]::Reverse($runs)
foreach ($run in $runs) {
    Write-Host ("         {0,-20} {1,-10} {2,2} steps  {3}" -f $run.agent, $run.status, $run.steps, $run.summary) -ForegroundColor DarkGray
}

$r = Invoke-Tool $Tokens.Chai "approval.respond" @{ approval_request_id = $proposal.approval_request_id; decision = "approve"; note = "memo reviewed" }
Check "Chai reviews the memo and records the recommendation" $r "ok"
$recId = $r.result.result.decision_record_id

$r = Invoke-Tool $Tokens.Ben "decision.approve" @{ application_id = $loanId; recommendation_id = $recId }
Check "Ben cannot approve: 50,000 is above his limit" $r "error" "POLICY_DENIED"
$r = Invoke-Tool $Tokens.Sara "decision.approve" @{ application_id = $loanId; recommendation_id = $recId }
Check "Sara (senior approver) approves" $r "ok"
$decisionId = $r.result.decision_record_id

$r = Invoke-Tool $Tokens.Ana "offer.generate" @{ application_id = $loanId; decision_record_id = $decisionId }
$offerId = $r.result.offer_id
Write-Host ("         offer: 12 payments of {0:N2}" -f ($r.result.schedule.monthly_payment_minor / 100)) -ForegroundColor DarkGray
$r = Invoke-Tool $Tokens.Ana "offer.send" @{ offer_id = $offerId; channel = "portal" }
Check "Ana generates and sends the offer" $r "ok"
$r = Invoke-Tool $Tokens.Mekong "offer.accept" @{ offer_id = $offerId; signature = $click }
Check "the borrower accepts in the portal" $r "ok"
$r = Invoke-Tool $Tokens.Dana "loan.handoff" @{ application_id = $loanId; target = "mock-core" }
Check "Dana (operations) books the loan" $r "ok"
Write-Host ("         state '{0}', core banking loan id {1}" -f $r.result.state, $r.result.external_loan_id) -ForegroundColor DarkGray

if (-not $SkipAuditCheck -and (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Host ""
    Write-Host "7. Tamper-evident audit log" -ForegroundColor Cyan
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
