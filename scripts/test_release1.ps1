# scripts/test_release1.ps1
# Comprehensive Verification Suite for Release 1:
# - Secure Registration & Subscription Approval Workflow
# - Private API Catalog Isolation
# - OpenAPI Draft Staging & Controlled Publishing
# - Signature Feature 1: Change Impact Preview
# - Signature Feature 2: Fleet Configuration Proof
# - Signature Feature 3: Request Decision Explorer (plain-English audit of every 401/403/429)
# - Strict JWT validation (required exp claim)
# - Enterprise Audit Trail

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$A = 'http://localhost:9090'; $G = 'http://localhost:8080'
$H = @{ Authorization = 'Bearer relayops-admin' }

function Adm($m, $p, $b) {
  if ($b -is [string]) {
    Invoke-RestMethod -Method $m -Uri "$A$p" -Headers $H -ContentType 'text/plain' -Body $b
  } elseif ($b) {
    Invoke-RestMethod -Method $m -Uri "$A$p" -Headers $H -ContentType 'application/json' -Body ($b | ConvertTo-Json -Depth 5)
  } else {
    Invoke-RestMethod -Method $m -Uri "$A$p" -Headers $H
  }
}

function Gw($p, $hdr = @{}, $b = $null) {
  try {
    $params = @{ Uri = "$G$p"; Headers = $hdr; UseBasicParsing = $true }
    if ($b) { $params['Method'] = 'POST'; $params['ContentType'] = 'application/json'; $params['Body'] = ($b | ConvertTo-Json -Depth 5) }
    $r = Invoke-WebRequest @params
    return @{ s = [int]$r.StatusCode; b = $r.Content; h = $r.Headers }
  } catch {
    $resp = $_.Exception.Response
    $body = ""
    if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
      $body = $_.ErrorDetails.Message
    } elseif ($resp) {
      try {
        $sr = New-Object IO.StreamReader($resp.GetResponseStream())
        $body = $sr.ReadToEnd()
      } catch {}
    }
    $code = 0
    if ($resp) { $code = [int]$resp.StatusCode }
    return @{ s = $code; b = $body; h = if ($resp) { $resp.Headers } else { @{} } }
  }
}

function Check($name, $cond) {
  if ($cond) { Write-Host "PASS  $name" -ForegroundColor Green }
  else { Write-Host "FAIL  $name" -ForegroundColor Red; $script:fail++ }
}
$fail = 0

Write-Host "=== TEST 1: Secure Portal Governance & Private APIs ===" -ForegroundColor Cyan

# 1. Create a private API and verify it does NOT show in public catalog
$privApi = Adm POST '/api/apis' @{
  name = "internal-billing-$([guid]::NewGuid().ToString().Substring(0,6))"
  base_path = "/internal-billing"
  upstream_url = "http://localhost:7070/billing"
  visibility = "private"
  auth_type = "api_key"
}
$cat = Invoke-RestMethod "$A/portal/api/catalog"
$inCat = $cat.apis | Where-Object id -eq $privApi.id
Check "Private API hidden from public portal catalog" (-not $inCat)

# 2. Create an API requiring manual subscription approval
$approvalApi = Adm POST '/api/apis' @{
  name = "payroll-$([guid]::NewGuid().ToString().Substring(0,6))"
  base_path = "/payroll"
  upstream_url = "http://localhost:7070/payroll"
  visibility = "public"
  require_approval = $true
  auth_type = "api_key"
}
$cat2 = Invoke-RestMethod "$A/portal/api/catalog"
$payrollPub = $cat2.apis | Where-Object id -eq $approvalApi.id
Check "Approval-required API visible in portal with require_approval=true" ($payrollPub.require_approval -eq $true)

# 3. Developer registers for the approval-required API
$devName = "corp-dev-" + ([guid]::NewGuid().ToString().Substring(0,8))
$reg = Invoke-RestMethod -Method POST -Uri "$A/portal/api/register" -ContentType 'application/json' `
  -Body (@{ name = $devName; email = "$devName@corp.io"; api_ids = @($approvalApi.id) } | ConvertTo-Json)
$clientKey = $reg.api_key
Start-Sleep -Milliseconds 400

# 4. Attempt calling gateway BEFORE admin approval: must receive 403 subscription_pending_approval
$preApprove = Gw '/payroll/salaries' @{ 'X-API-Key' = $clientKey }
Write-Host "DEBUG preApprove status: $($preApprove.s), body: $($preApprove.b)" -ForegroundColor Gray
Check "Gated API blocks unapproved subscription (403)" ($preApprove.s -eq 403)
Check "Rejection specifies subscription_pending_approval" ($preApprove.b -match 'subscription_pending_approval')

# 5. Admin lists pending approvals and approves the subscription
$pending = Adm GET '/api/subscriptions/pending'
$mySub = $pending | Where-Object consumer_id -eq $reg.consumer.id
Check "Pending subscription visible in admin queue" ([bool]$mySub)

$approved = Adm POST "/api/subscriptions/$($mySub.id)/approve" @{}
Check "Admin approval flushes subscription to active" ($approved.status -eq 'approved' -and $approved.active -eq $true)
Start-Sleep -Milliseconds 400

# 6. Call gateway AFTER admin approval: must now succeed (200)
$postApprove = Gw '/payroll/salaries' @{ 'X-API-Key' = $clientKey }
Check "Gated API allows call after admin approval (200)" ($postApprove.s -eq 200)

Write-Host "`n=== TEST 2: OpenAPI Draft Staging & Safe Publishing ===" -ForegroundColor Cyan
$sampleDraftSpec = "openapi: 3.0.0`ninfo:`n  title: Warehouse Inventory API`n  version: 1.0.0`nservers:`n  - url: http://localhost:7070`npaths:`n  /stock:`n    get:`n      summary: Get warehouse stock levels"
# Import with ?draft=true -> must be saved as DRAFT
$draftImport = Adm POST '/api/apis/import-openapi?draft=true' $sampleDraftSpec
Check "OpenAPI import staged as DRAFT" ($draftImport.is_draft -eq $true)
Check "OpenAPI draft is disabled initially" ($draftImport.api.enabled -eq $false)

Start-Sleep -Milliseconds 300
$testDraftCall = Gw '/warehouse-inventory-api/stock'
Check "Draft API route is isolated from data plane (404)" ($testDraftCall.s -eq 404)

# Publish the draft
$published = Adm POST "/api/apis/$($draftImport.api.id)/publish" @{}
Check "API published successfully" ($published.is_draft -eq $false -and $published.enabled -eq $true)
Start-Sleep -Milliseconds 400

$testLiveCall = Gw '/warehouse-inventory-api/stock'
Check "Published API is immediately live on data plane (200)" ($testLiveCall.s -eq 200)

Write-Host "`n=== TEST 3: Signature Feature 1 - Change Impact Preview ===" -ForegroundColor Cyan
# Propose changing the published API from auth_type 'none' to 'api_key' and reducing rate limit
$previewProposal = @{
  auth_type = "api_key"
  rate_limit_per_minute = 10
}
$preview = Adm POST "/api/apis/$($draftImport.api.id)/preview-change" $previewProposal
Check "Preview generated impact report" ($preview.impacts.Count -ge 2)
$authWarning = $preview.impacts | Where-Object { $_.message -match "Auth type changing from 'none' to 'api_key'" }
Check "Preview detected breaking auth change with severity critical" ($authWarning.severity -eq "critical")

Write-Host "`n=== TEST 4: Signature Feature 3 - Request Decision Explorer ===" -ForegroundColor Cyan
Start-Sleep -Seconds 1
# Query recent logs to verify decision trail columns are populated
$logs = Adm GET '/api/logs?limit=10'
$pendingLog = $logs | Where-Object { $_.decision_reason -eq 'subscription_pending_approval' }
Check "Request decision explorer recorded subscription_pending_approval" ([bool]$pendingLog)
Check "Auth status recorded as 'ok'" ($pendingLog.auth_status -eq 'ok')
Check "Subscription status recorded as 'pending_approval'" ($pendingLog.subscription_status -eq 'pending_approval')

$successLog = $logs | Where-Object { $_.status -eq 200 -and $_.decision_reason -eq 'proxied_successfully' }
Check "Successful request recorded decision_reason='proxied_successfully'" ([bool]$successLog)

Write-Host "`n=== TEST 5: Signature Feature 2 - Fleet Configuration Proof ===" -ForegroundColor Cyan
$fleet = Adm GET '/api/fleet/status'
Check "Fleet status reports target revision" ($fleet.target_revision -ge 1)
Check "Fleet status reports node acknowledgements" ($fleet.nodes.Count -ge 1)
Check "Cluster fleet converged" ($fleet.converged -eq $true)
Write-Host "Fleet Node: $($fleet.nodes[0].node_id), Revision: $($fleet.nodes[0].revision), Applied in: $($fleet.nodes[0].took_ms)ms" -ForegroundColor Green

Write-Host "`n=== TEST 6: Strict Token Validation and Enterprise Audit Trail ===" -ForegroundColor Cyan
# 1. Strict JWT: missing exp must be rejected
$headerB64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes('{"alg":"HS256"}')).TrimEnd('=')
$payloadNoExp = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes('{"sub":"hacker"}')).TrimEnd('=')
$hmac = New-Object System.Security.Cryptography.HMACSHA256
$hmac.Key = [System.Text.Encoding]::UTF8.GetBytes("super-secret-12345678")
$sigB64 = [Convert]::ToBase64String($hmac.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$headerB64.$payloadNoExp"))).TrimEnd('=').Replace('+','-').Replace('/','_')
$noExpToken = "$headerB64.$payloadNoExp.$sigB64"

# Create JWT API
$jwtApi = Adm POST '/api/apis' @{
  name = "jwt-strict-$([guid]::NewGuid().ToString().Substring(0,6))"
  base_path = "/jwt-strict"
  upstream_url = "http://localhost:7070"
  auth_type = "jwt"
  jwt_secret = "super-secret-12345678"
}
Start-Sleep -Milliseconds 500
$jwtResp = Gw '/jwt-strict/test' @{ Authorization = "Bearer $noExpToken" }
Write-Host "DEBUG jwtResp status: $($jwtResp.s), body: $($jwtResp.b)" -ForegroundColor Gray
Check "Strict JWT rejects token missing required exp claim (401)" ($jwtResp.s -eq 401 -and $jwtResp.b -match 'exp claim')

# 2. Audit Trail
$audits = Adm GET '/api/audit-logs?limit=20'
Check "Audit trail recorded administrative actions" ($audits.Count -ge 4)
$hasPublishAudit = $audits | Where-Object { $_.action -eq 'PUBLISH' }
Check "Audit log recorded draft PUBLISH action" ([bool]$hasPublishAudit)

# Cleanup test resources
Adm DELETE "/api/apis/$($privApi.id)" | Out-Null
Adm DELETE "/api/apis/$($approvalApi.id)" | Out-Null
Adm DELETE "/api/apis/$($draftImport.api.id)" | Out-Null
Adm DELETE "/api/apis/$($jwtApi.id)" | Out-Null
Adm DELETE "/api/consumers/$($reg.consumer.id)" | Out-Null

Write-Host ""
if ($fail) { Write-Host "$fail check(s) FAILED" -ForegroundColor Red; exit 1 }
else { Write-Host "ALL RELEASE 1 FOUNDATION CHECKS PASSED!" -ForegroundColor Green }
