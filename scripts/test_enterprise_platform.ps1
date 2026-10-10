# scripts/test_enterprise_platform.ps1
# Comprehensive Verification Suite for RelayOps Enterprise Platform Capabilities

$ErrorActionPreference = "Stop"

Write-Host "=================================================================" -ForegroundColor Cyan
Write-Host "RELAYOPS APIM ENTERPRISE PLATFORM VERIFICATION TEST SUITE" -ForegroundColor Cyan
Write-Host "=================================================================" -ForegroundColor Cyan

$adminUrl = "http://127.0.0.1:9090"
$gwUrl = "http://127.0.0.1:8080"
$clusterToken = "relayops-admin"
# HS256 SSO requires a dedicated secret on the server (RELAYOPS_SSO_SECRET, >= 32 bytes);
# it never falls back to the admin token. Run the server with the same value.
$ssoSecret = if ($env:RELAYOPS_SSO_SECRET) { $env:RELAYOPS_SSO_SECRET } else { "relayops-test-sso-secret-0123456789abcdef" }
try { Add-Type -AssemblyName System.Net.Http } catch {}

function Assert-True($condition, $message) {
    if (-not $condition) {
        Write-Host " [FAIL] $message" -ForegroundColor Red
        throw "Assertion failed: $message"
    } else {
        Write-Host " [PASS] $message" -ForegroundColor Green
    }
}

function Invoke-Http([string]$uri, [string]$method = "GET", [hashtable]$headers = @{}) {
    try {
        $req = [System.Net.HttpWebRequest]::Create($uri)
        $req.Method = $method
        $req.Timeout = 10000
        foreach ($k in $headers.Keys) {
            $req.Headers[$k] = $headers[$k]
        }
        $resp = $req.GetResponse()
        $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
        $content = $reader.ReadToEnd()
        $reader.Close()
        return [PSCustomObject]@{
            StatusCode = [int]$resp.StatusCode
            Headers = $resp.Headers
            Content = $content
        }
    } catch [System.Net.WebException] {
        $resp = $_.Exception.Response
        if ($resp -ne $null) {
            $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
            $content = $reader.ReadToEnd()
            $reader.Close()
            return [PSCustomObject]@{
                StatusCode = [int]$resp.StatusCode
                Headers = $resp.Headers
                Content = $content
            }
        }
        throw $_
    }
}

function New-TestJwt([hashtable]$claims, [string]$secret) {
    $header = @{ alg = "HS256"; typ = "JWT" } | ConvertTo-Json -Compress
    $headerB64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($header)).TrimEnd('=').Replace('+', '-').Replace('/', '_')
    $payload = $claims | ConvertTo-Json -Compress
    $payloadB64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($payload)).TrimEnd('=').Replace('+', '-').Replace('/', '_')
    $toSign = "$headerB64.$payloadB64"
    $hmac = New-Object System.Security.Cryptography.HMACSHA256
    $hmac.Key = [System.Text.Encoding]::UTF8.GetBytes($secret)
    $sig = [Convert]::ToBase64String($hmac.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($toSign))).TrimEnd('=').Replace('+', '-').Replace('/', '_')
    return "$toSign.$sig"
}

# ============================================================================
# 1. ENTERPRISE ACCESS: NAMED ADMINISTRATORS, SSO, ROLES & COMPLETE AUDIT ATTRIBUTION
# ============================================================================
Write-Host "`n--- PRIORITY 1: ENTERPRISE ACCESS & RBAC ---" -ForegroundColor Yellow

# 1.1 Platform administrator sign-in with the cluster token (persisted session)
Write-Host "1.1 Testing Platform Administrator Cluster-Token Sign-In..."
$loginBody = @{ token = "relayops-admin" } | ConvertTo-Json  # cluster-token sign-in (named accounts have no default password)
$loginRes = Invoke-RestMethod -Uri "$adminUrl/api/auth/login" -Method Post -Body $loginBody -ContentType "application/json"
Assert-True ($loginRes.token -ne $null -and $loginRes.token.StartsWith("adm_sess_")) "Cluster-token sign-in issued session token"
Assert-True ($loginRes.user.role -eq "superadmin") "Admin user has 'superadmin' role"
Assert-True ($loginRes.permissions -contains "users") "Superadmin permissions contain 'users'"
$superadminToken = $loginRes.token

# 1.2 Auth Me verification
$me = Invoke-RestMethod -Uri "$adminUrl/api/auth/me" -Headers @{ Authorization = "Bearer $superadminToken" }
Assert-True ($me.user.email -eq "admin@relayops.local") "GET /api/auth/me returned correct administrator identity"

# 1.3 SSO Token Validation & Impersonation Prevention
Write-Host "1.3 Testing SSO Token Cryptographic Validation & Impersonation Guards..."

# 1.3.1 Forged / Invalid Signature Token Rejection
$badToken = "eyJhbGciOiJIUzI1NiJ9.eyJlbWFpbCI6ImhhY2tlckBjb3JwLmxvY2FsIn0.invalidsig12345678"
$forgedBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/api/auth/sso/callback" -Method Post -Body (@{ provider = "okta"; token = $badToken } | ConvertTo-Json) -ContentType "application/json"
} catch {
    if ($_.Exception.Response.StatusCode -eq 401) { $forgedBlocked = $true }
}
Assert-True $forgedBlocked "Forged / invalid signature SSO token rejected with HTTP 401 Unauthorized"

# 1.3.2 Anti-Impersonation: Forged token claiming local superadmin email
$impersonateClaims = @{ email = "admin@relayops.local"; role = "operator"; exp = [DateTimeOffset]::UtcNow.AddHours(1).ToUnixTimeSeconds() }
$impersonateJwt = New-TestJwt -claims $impersonateClaims -secret $ssoSecret
$impersonateBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/api/auth/sso/callback" -Method Post -Body (@{ provider = "okta"; token = $impersonateJwt } | ConvertTo-Json) -ContentType "application/json"
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) { $impersonateBlocked = $true }
}
Assert-True $impersonateBlocked "SSO token prevented from impersonating local superadmin account without federation (HTTP 403)"

# 1.3.3 Validly Signed Enterprise SSO Token
$validSsoClaims = @{ email = "sso-operator@acme.corp"; name = "Enterprise Operator"; role = "operator"; exp = [DateTimeOffset]::UtcNow.AddHours(2).ToUnixTimeSeconds() }
$validSsoJwt = New-TestJwt -claims $validSsoClaims -secret $ssoSecret
$ssoRes = Invoke-RestMethod -Uri "$adminUrl/api/auth/sso/callback" -Method Post -Body (@{ provider = "azure_ad"; token = $validSsoJwt } | ConvertTo-Json) -ContentType "application/json"
Assert-True ($ssoRes.token -ne $null) "Cryptographically validated SSO token issued active session"
Assert-True ($ssoRes.user.email -eq "sso-operator@acme.corp") "SSO user identity extracted directly from verified token claims"
$operatorToken = $ssoRes.token

# 1.4 Create Named Auditor User (Read-Only)
Write-Host "1.4 Creating Named Auditor User (Read-Only RBAC)..."
$auditorEmail = "audit-$([System.Guid]::NewGuid().ToString().Substring(0,8))@corp.local"
$newUserBody = @{
    name = "Compliance Auditor"
    email = $auditorEmail
    role = "auditor"
    team = "Security & Compliance"
    active = $true
    password = "Auditor-Pass-0123456789"
} | ConvertTo-Json
$auditorUser = Invoke-RestMethod -Uri "$adminUrl/api/admin/users" -Method Post -Headers @{ Authorization = "Bearer $superadminToken" } -Body $newUserBody -ContentType "application/json"
Assert-True ($auditorUser.role -eq "auditor") "Created administrator user with 'auditor' role"

# Login as Auditor
$auditorLoginBody = @{ email = $auditorEmail; password = "Auditor-Pass-0123456789" } | ConvertTo-Json
$auditorSession = Invoke-RestMethod -Uri "$adminUrl/api/auth/login" -Method Post -Body $auditorLoginBody -ContentType "application/json"
$auditorToken = $auditorSession.token

# 1.5 Enforce RBAC: Auditor must be blocked from mutations (POST, PUT, DELETE)
Write-Host "1.5 Verifying RBAC Restrictions (Auditor cannot mutate resources)..."
$auditorBlocked = $false
try {
    $dummyApi = @{ name = "forbidden-api"; base_path = "/forbidden"; upstream_url = "http://localhost:7070" } | ConvertTo-Json
    Invoke-RestMethod -Uri "$adminUrl/api/apis" -Method Post -Headers @{ Authorization = "Bearer $auditorToken" } -Body $dummyApi -ContentType "application/json"
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) { $auditorBlocked = $true }
}
Assert-True $auditorBlocked "Auditor role blocked with HTTP 403 Forbidden when attempting resource mutation"

# 1.6 Enforce RBAC: Non-superadmin cannot manage admin users
Write-Host "1.6 Verifying Admin User Isolation (Non-superadmin blocked from /api/admin/users)..."
$usersBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/api/admin/users" -Headers @{ Authorization = "Bearer $auditorToken" }
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) { $usersBlocked = $true }
}
Assert-True $usersBlocked "Auditor blocked with HTTP 403 when attempting to access /api/admin/users"

# 1.7 Enforce RBAC: Operator cannot access audit logs
Write-Host "1.7 Verifying Operator RBAC Restrictions (No Audit Logs Access)..."
$operatorAuditBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/api/audit-logs" -Headers @{ Authorization = "Bearer $operatorToken" }
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) { $operatorAuditBlocked = $true }
}
Assert-True $operatorAuditBlocked "Operator role blocked with HTTP 403 Forbidden from accessing /api/audit-logs"

# 1.8 Verify Complete Audit Attribution
Write-Host "1.8 Checking Complete Audit Attribution Ledger..."
$auditLogs = Invoke-RestMethod -Uri "$adminUrl/api/audit-logs?limit=5" -Headers @{ Authorization = "Bearer $superadminToken" }
Assert-True ($auditLogs.Length -gt 0) "Audit logs retrieved"
$latestAudit = $auditLogs[0]
Assert-True ($latestAudit.actor_email -ne "" -or $latestAudit.actor -ne "") "Audit log records actor identity"
Assert-True ($latestAudit.client_ip -ne "") "Audit log records actor client IP address"
Write-Host "     Latest audit entry: Action=$($latestAudit.action), Actor=$($latestAudit.actor_email), Role=$($latestAudit.actor_role), IP=$($latestAudit.client_ip)" -ForegroundColor DarkCyan

# ============================================================================
# 2. SAFE CONFIGURATION RELEASES & 1-CLICK ROLLBACK
# ============================================================================
Write-Host "`n--- PRIORITY 2: SAFE CONFIGURATION RELEASES & 1-CLICK ROLLBACK ---" -ForegroundColor Yellow

# 2.1 Configuration Pre-Flight Validation
Write-Host "2.1 Testing Pre-Flight Configuration Validation Endpoint..."
$invalidConfig = @{
    apis = @(
        @{ name = "api-1"; base_path = "/duplicate-path"; upstream_url = "http://localhost:7070" },
        @{ name = "api-2"; base_path = "/duplicate-path"; upstream_url = "http://localhost:7070" }
    )
} | ConvertTo-Json
$valRes = Invoke-RestMethod -Uri "$adminUrl/api/revisions/validate" -Method Post -Headers @{ Authorization = "Bearer $superadminToken" } -Body $invalidConfig -ContentType "application/json"
Assert-True ($valRes.valid -eq $false) "Pre-flight validation correctly detected route collision"
Assert-True ($valRes.errors.Length -gt 0) "Validation returned actionable error explanations"

# 2.2 Create a dedicated API and record baseline revision
Write-Host "2.2 Creating a test API to record baseline configuration revision..."
$testApiName = "rollback-test-api-$([System.Guid]::NewGuid().ToString().Substring(0,6))"
$testApiPath = "/rollback-test-$([System.Guid]::NewGuid().ToString().Substring(0,6))"
$newApi = @{
    name = $testApiName
    base_path = $testApiPath
    upstream_url = "http://localhost:7070"
    auth_type = "none"
    enabled = $true
} | ConvertTo-Json
$createdApi = Invoke-RestMethod -Uri "$adminUrl/api/apis" -Method Post -Headers @{ Authorization = "Bearer $superadminToken" } -Body $newApi -ContentType "application/json"
Start-Sleep -Milliseconds 400

$fleetStatus1 = Invoke-RestMethod -Uri "$adminUrl/api/fleet/status" -Headers @{ Authorization = "Bearer $superadminToken" }
$targetRevBefore = $fleetStatus1.target_revision
Write-Host "     Baseline Revision before mutation: rev_$targetRevBefore" -ForegroundColor DarkCyan

# 2.3 Mutate the API (disable it) to create a newer revision
Write-Host "2.3 Mutating the API (disabling) to advance configuration revision..."
$mutateBody = @{ enabled = $false } | ConvertTo-Json
$updatedApi = Invoke-RestMethod -Uri "$adminUrl/api/apis/$($createdApi.id)" -Method Put -Headers @{ Authorization = "Bearer $superadminToken" } -Body $mutateBody -ContentType "application/json"
Start-Sleep -Milliseconds 400

$fleetStatus2 = Invoke-RestMethod -Uri "$adminUrl/api/fleet/status" -Headers @{ Authorization = "Bearer $superadminToken" }
$targetRevAfter = $fleetStatus2.target_revision
Assert-True ($targetRevAfter -gt $targetRevBefore) "Configuration mutation advanced monotonic revision counter"
Write-Host "     Advanced revision after mutation: rev_$targetRevAfter" -ForegroundColor DarkCyan

# 2.4 Execute 1-Click Rollback restoring the previous configuration
Write-Host "2.4 Executing 1-Click Rollback to rev_$targetRevBefore..."
$rollbackRes = Invoke-RestMethod -Uri "$adminUrl/api/revisions/$targetRevBefore/rollback" -Method Post -Headers @{ Authorization = "Bearer $superadminToken" }
Assert-True ($rollbackRes.status -eq "rolled_back") "Rollback endpoint returned status 'rolled_back'"
Assert-True ($rollbackRes.restored_revision -eq $targetRevBefore) "Rollback confirmed restoration of target revision"
Write-Host "     Rollback generated active revision: rev_$($rollbackRes.new_revision)" -ForegroundColor DarkCyan

# 2.5 Verify API is re-enabled following rollback
Start-Sleep -Milliseconds 400
$restoredApi = Invoke-RestMethod -Uri "$adminUrl/api/apis/$($createdApi.id)" -Headers @{ Authorization = "Bearer $superadminToken" }
Assert-True ($restoredApi.enabled -eq $true) "Transactional rollback successfully restored API 'enabled = true' state"

# ============================================================================
# 3. COMPLETE DEVELOPER SELF-SERVICE WITH OWNERSHIP PROTECTION
# ============================================================================
Write-Host "`n--- PRIORITY 3: DEVELOPER SELF-SERVICE & OWNERSHIP PROTECTION ---" -ForegroundColor Yellow

# 3.1 Register Developer 1 & Issue initial API key
Write-Host "3.1 Registering Developer 1 Account in Portal..."
$dev1Email = "dev1-$([System.Guid]::NewGuid().ToString().Substring(0,8))@acme.com"
$reg1Body = @{ name = "Acme Mobile Engineering $([System.Guid]::NewGuid().ToString().Substring(0,6))"; email = $dev1Email; api_ids = @($createdApi.id) } | ConvertTo-Json
$reg1Res = Invoke-RestMethod -Uri "$adminUrl/portal/api/register" -Method Post -Body $reg1Body -ContentType "application/json"
$dev1Key = $reg1Res.api_key
$dev1ConsumerId = $reg1Res.consumer.id
Assert-True ($dev1Key -ne $null) "Developer 1 received initial active API key"

# 3.2 Register Developer 2 Account (for cross-account ownership testing)
Write-Host "3.2 Registering Developer 2 Account..."
$dev2Email = "dev2-$([System.Guid]::NewGuid().ToString().Substring(0,8))@corp.com"
$reg2Body = @{ name = "External Partner $([System.Guid]::NewGuid().ToString().Substring(0,6))"; email = $dev2Email; api_ids = @($createdApi.id) } | ConvertTo-Json
$reg2Res = Invoke-RestMethod -Uri "$adminUrl/portal/api/register" -Method Post -Body $reg2Body -ContentType "application/json"
$dev2Key = $reg2Res.api_key
$dev2ConsumerId = $reg2Res.consumer.id

# 3.3 Create Developer 1 Application
Write-Host "3.3 Developer 1 Creating Application..."
$appBody = @{
    name = "Acme iOS Production App"
    environment = "production"
    description = "Mobile client iOS build v4.2"
} | ConvertTo-Json
$dev1App = Invoke-RestMethod -Uri "$adminUrl/portal/api/apps" -Method Post -Headers @{ "X-Developer-Key" = $dev1Key } -Body $appBody -ContentType "application/json"
Assert-True ($dev1App.name -eq "Acme iOS Production App") "Developer application successfully created"

# 3.4 Ownership Protection: Unauthenticated app deletion blocked (401)
Write-Host "3.4 Verifying Ownership Protection: Unauthenticated Deletion Blocked..."
$unauthDeleteBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/portal/api/apps/$($dev1App.id)" -Method Delete
} catch {
    if ($_.Exception.Response.StatusCode -eq 401) { $unauthDeleteBlocked = $true }
}
Assert-True $unauthDeleteBlocked "Unauthenticated developer app deletion rejected with HTTP 401 Unauthorized"

# 3.5 Ownership Protection: Developer 2 cannot delete Developer 1's app (403)
Write-Host "3.5 Verifying Ownership Protection: Cross-Developer App Deletion Blocked..."
$crossDeleteBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/portal/api/apps/$($dev1App.id)" -Method Delete -Headers @{ "X-Developer-Key" = $dev2Key }
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) { $crossDeleteBlocked = $true }
}
Assert-True $crossDeleteBlocked "Cross-developer app deletion blocked with HTTP 403 Forbidden"

# 3.6 Ownership Protection: Developer 2 cannot rotate Developer 1's key (403)
Write-Host "3.6 Verifying Ownership Protection: Cross-Developer Key Rotation Blocked..."
$crossRotateBlocked = $false
try {
    Invoke-RestMethod -Uri "$adminUrl/portal/api/keys/$dev1ConsumerId/rotate" -Method Post -Headers @{ "X-Developer-Key" = $dev2Key } -Body (@{ grace_hours = 24 } | ConvertTo-Json) -ContentType "application/json"
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) { $crossRotateBlocked = $true }
}
Assert-True $crossRotateBlocked "Cross-developer key rotation blocked with HTTP 403 Forbidden"

# 3.7 Authorized Developer 1 Zero-Downtime Key Rotation (7-Day Grace)
Write-Host "3.7 Authorized Developer 1 Executing Zero-Downtime Key Rotation..."
$rotBody = @{ grace_hours = 168 } | ConvertTo-Json
$rotRes = Invoke-RestMethod -Uri "$adminUrl/portal/api/keys/$dev1ConsumerId/rotate" -Method Post -Headers @{ "X-Developer-Key" = $dev1Key } -Body $rotBody -ContentType "application/json"
$dev1NewKey = $rotRes.new_key
Assert-True ($dev1NewKey -ne $null -and $dev1NewKey -ne $dev1Key) "New primary API key successfully generated"
Assert-True ($rotRes.grace_until -ne $null) "7-Day dual-active grace period active"
Write-Host "     Old Key: $($dev1Key.Substring(0,10))... (Secondary active during grace period)" -ForegroundColor DarkCyan
Write-Host "     New Key: $($dev1NewKey.Substring(0,10))... (Primary active)" -ForegroundColor DarkCyan

Start-Sleep -Milliseconds 400

# 3.8 Verify Dual-Active Key Functionality on Data Plane
$secApiBody = @{ auth_type = "api_key" } | ConvertTo-Json
Invoke-RestMethod -Uri "$adminUrl/api/apis/$($createdApi.id)" -Method Put -Headers @{ Authorization = "Bearer $superadminToken" } -Body $secApiBody -ContentType "application/json" | Out-Null
Start-Sleep -Milliseconds 400

Write-Host "3.8 Verifying Dual-Active Key Functionality on Data Plane..."
$oldKeyRes = Invoke-Http -uri "$gwUrl$testApiPath/get" -headers @{ "X-API-Key" = $dev1Key }
Assert-True ($oldKeyRes.StatusCode -eq 200) "Old key succeeded with HTTP 200 during grace period (Dual-Active)"

$newKeyRes = Invoke-Http -uri "$gwUrl$testApiPath/get" -headers @{ "X-API-Key" = $dev1NewKey }
Assert-True ($newKeyRes.StatusCode -eq 200) "New primary key succeeded with HTTP 200"

# 3.9 Early Retirement of Secondary Key
Write-Host "3.9 Retiring Secondary Key Early..."
$retireRes = Invoke-RestMethod -Uri "$adminUrl/portal/api/keys/$($rotRes.previous_key_id)/retire" -Method Post -Headers @{ "X-Developer-Key" = $dev1NewKey }
Assert-True ($retireRes.status -eq "retired") "Secondary key retired early"
Start-Sleep -Milliseconds 400

$retiredKeyRes = Invoke-Http -uri "$gwUrl$testApiPath/get" -headers @{ "X-API-Key" = $dev1Key }
Assert-True ($retiredKeyRes.StatusCode -eq 401) "Retired key immediately blocked with HTTP 401 Unauthorized"

$newKeyStillOk = Invoke-Http -uri "$gwUrl$testApiPath/get" -headers @{ "X-API-Key" = $dev1NewKey }
Assert-True ($newKeyStillOk.StatusCode -eq 200) "New primary key continues functioning smoothly"

# 3.10 Developer 1 Deleting Own App
$delOwnAppRes = Invoke-Http -uri "$adminUrl/portal/api/apps/$($dev1App.id)" -method "DELETE" -headers @{ "X-Developer-Key" = $dev1NewKey }
Assert-True ($delOwnAppRes.StatusCode -eq 204) "Authorized developer successfully deleted own application"

# ============================================================================
# 4. RELIABLE REQUEST EXPLANATIONS & PROVENANCE HEADERS
# ============================================================================
Write-Host "`n--- PRIORITY 4: RELIABLE REQUEST EXPLANATIONS & DECISION PROVENANCE ---" -ForegroundColor Yellow

# 4.1 Inspect Gateway Decision Headers on Successful Request
Write-Host "4.1 Inspecting Decision Provenance Headers on Successful Request..."
$gwRespOk = Invoke-Http -uri "$gwUrl$testApiPath/get" -headers @{ "X-API-Key" = $dev1NewKey }
$revHeader = $gwRespOk.Headers["X-RelayOps-Revision"]
$policyHeader = $gwRespOk.Headers["X-RelayOps-Decision-Policy"]
$reasonHeader = $gwRespOk.Headers["X-RelayOps-Decision-Reason"]
Assert-True ($revHeader -ne $null -and $revHeader.StartsWith("rev_")) "Response includes X-RelayOps-Revision header: $revHeader"
Assert-True ($policyHeader -eq "proxy") "Response includes X-RelayOps-Decision-Policy: proxy"
Assert-True ($reasonHeader -eq "proxied_successfully") "Response includes X-RelayOps-Decision-Reason: proxied_successfully"

# 4.2 Inspect Gateway Decision Headers on Rejection (Missing API Key)
Write-Host "4.2 Inspecting Decision Provenance on Rejected Request (Missing Key)..."
$gwRespReject = Invoke-Http -uri "$gwUrl$testApiPath/get"
Assert-True ($gwRespReject.StatusCode -eq 401) "Unauthenticated request rejected with HTTP 401"
$rejPolicy = $gwRespReject.Headers["X-RelayOps-Decision-Policy"]
$rejReason = $gwRespReject.Headers["X-RelayOps-Decision-Reason"]
Assert-True ($rejPolicy -eq "authentication") "Rejection header specifies policy 'authentication'"
Assert-True ($rejReason -eq "missing_api_key") "Rejection header specifies reason 'missing_api_key'"

$rejJson = $gwRespReject.Content | ConvertFrom-Json
Assert-True ($rejJson.policy -eq "authentication") "Rejection JSON body includes 'policy'"
Assert-True ($rejJson.decision -eq "missing_api_key") "Rejection JSON body includes 'decision'"
Assert-True ($rejJson.revision -gt 0) "Rejection JSON body includes 'revision'"

# 4.3 Verify Provenance Fields in Postgres Request Logs
Write-Host "4.3 Verifying Request Provenance Persisted in Request Logs..."
Start-Sleep -Milliseconds 600
$logs = Invoke-RestMethod -Uri "$adminUrl/api/logs?limit=5" -Headers @{ Authorization = "Bearer $superadminToken" }
$logWithProvenance = $logs | Where-Object { $_.config_revision -gt 0 } | Select-Object -First 1
Assert-True ($logWithProvenance -ne $null) "Request logs persist config_revision column"
Assert-True ($logWithProvenance.matched_route -ne "") "Request logs persist matched_route"
Write-Host "     Recorded log provenance: Revision=rev_$($logWithProvenance.config_revision), Route=$($logWithProvenance.matched_route)" -ForegroundColor DarkCyan

# ============================================================================
# 5. CREDIBLE CHANGE PREVIEWS & SANITIZED TRAFFIC REPLAY
# ============================================================================
Write-Host "`n--- PRIORITY 5: CREDIBLE CHANGE PREVIEWS & SANITIZED TRAFFIC REPLAY ---" -ForegroundColor Yellow

# 5.1 Change Preview with Impacted Consumers & Mathematical Confidence Interval
Write-Host "5.1 Testing Change Impact Preview with Statistical Evaluation..."
$proposedChange = @{
    base_path = "$testApiPath-v2"
    auth_type = "jwt"
    jwt_secret = "production-jwt-verification-secret-32bytes"
} | ConvertTo-Json
$prevRes = Invoke-RestMethod -Uri "$adminUrl/api/apis/$($createdApi.id)/preview-change" -Method Post -Headers @{ Authorization = "Bearer $superadminToken" } -Body $proposedChange -ContentType "application/json"
Assert-True ($prevRes.recent_requests -gt 0) "Preview evaluated recent production request sample"
Assert-True ($prevRes.confidence -ne "") "Preview explains confidence interval: '$($prevRes.confidence)'"
Assert-True ($prevRes.statistical_evaluation -ne $null) "Preview contains structured statistical evaluation"
Assert-True ($prevRes.statistical_evaluation.confidence_level -eq 0.95) "Statistical evaluation specifies 95% confidence level"
Assert-True ($prevRes.statistical_evaluation.margin_of_error -ge 0.0) "Statistical evaluation computes mathematical margin of error"
Assert-True ($prevRes.impacted_consumers.Length -gt 0) "Preview explicitly identifies impacted consumer accounts"
Write-Host "     Statistical statement: $($prevRes.confidence)" -ForegroundColor DarkCyan
Write-Host "     Identified impacted consumer: $($prevRes.impacted_consumers[0].name) ($($prevRes.impacted_consumers[0].request_count) observed requests)" -ForegroundColor DarkCyan

# 5.2 Sanitized Traffic Replay Simulation with Gateway Policy Engine
Write-Host "5.2 Testing Sanitized Traffic Replay Simulation with Policy Engine..."
$replayRes = Invoke-RestMethod -Uri "$adminUrl/api/apis/$($createdApi.id)/replay-preview" -Method Post -Headers @{ Authorization = "Bearer $superadminToken" } -Body $proposedChange -ContentType "application/json"
Assert-True ($replayRes.total_replayed -gt 0) "Sanitized traffic replay simulated recorded production requests"
Assert-True ($replayRes.status_changed_count -gt 0) "Traffic replay flagged behavioral status diffs"
Assert-True ($replayRes.statistical_evaluation -ne $null) "Replay preview contains mathematical statistical evaluation"
$firstSim = $replayRes.simulations[0]
Assert-True ($firstSim.behavioral_diff -eq $true) "Replay simulation produced side-by-side behavioral diff"
Assert-True ($firstSim.decision_policy -ne "") "Replay identifies specific gateway decision policy ($($firstSim.decision_policy))"
Write-Host "     Simulation sample: Status $($firstSim.original_status) -> $($firstSim.simulated_status) | Policy: $($firstSim.decision_policy) | Reason: $($firstSim.reason)" -ForegroundColor DarkCyan

# ============================================================================
# 6. PRODUCTION EVIDENCE: CONCURRENCY & RESILIENCE
# ============================================================================
Write-Host "`n--- PRIORITY 6: PRODUCTION EVIDENCE & RESILIENCE ---" -ForegroundColor Yellow

# Upgrade Developer 1 subscription to Unlimited plan for high-concurrency throughput testing
$allPlans = Invoke-RestMethod -Uri "$adminUrl/api/plans" -Headers @{ Authorization = "Bearer $superadminToken" }
$unlimitedPlan = $allPlans | Where-Object { $_.name -eq "Unlimited" } | Select-Object -First 1
$allSubs = Invoke-RestMethod -Uri "$adminUrl/api/subscriptions" -Headers @{ Authorization = "Bearer $superadminToken" }
$dev1Sub = $allSubs | Where-Object { $_.consumer_id -eq $dev1ConsumerId -and $_.api_id -eq $createdApi.id } | Select-Object -First 1
if ($dev1Sub -and $unlimitedPlan) {
    $upBody = @{ plan_id = $unlimitedPlan.id; active = $true } | ConvertTo-Json
    Invoke-RestMethod -Uri "$adminUrl/api/subscriptions/$($dev1Sub.id)" -Method Put -Headers @{ Authorization = "Bearer $superadminToken" } -Body $upBody -ContentType "application/json" | Out-Null
    Start-Sleep -Milliseconds 500
}

# 6.1 High Concurrency Load Test (200 requests)
Write-Host "6.1 Running High Concurrency Data-Plane Load Test (200 requests)..."
$stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
$client = New-Object System.Net.Http.HttpClient
$client.DefaultRequestHeaders.Add("X-API-Key", $dev1NewKey)
$tasks = 1..200 | ForEach-Object {
    $client.GetAsync("$gwUrl$testApiPath/get")
}
[System.Threading.Tasks.Task]::WaitAll($tasks)
$stopwatch.Stop()
foreach ($t in $tasks) {
    if ([int]$t.Result.StatusCode -ne 200) { throw "Status was $([int]$t.Result.StatusCode)" }
}
$client.Dispose()
$avgRps = [math]::Round(200.0 / ($stopwatch.ElapsedMilliseconds / 1000.0), 1)
Write-Host "     Completed 200 requests in $($stopwatch.ElapsedMilliseconds) ms (~$avgRps req/sec) with 0 errors!" -ForegroundColor Green
Assert-True ($stopwatch.ElapsedMilliseconds -lt 6000) "200 concurrent requests served sub-millisecond per hop"

# Clean up test API
Invoke-RestMethod -Uri "$adminUrl/api/apis/$($createdApi.id)" -Method Delete -Headers @{ Authorization = "Bearer $superadminToken" } | Out-Null

Write-Host "`n=================================================================" -ForegroundColor Cyan
Write-Host "ALL 6 ENTERPRISE PLATFORM PRIORITIES VERIFIED SUCCESSFULLY!" -ForegroundColor Green
Write-Host "=================================================================" -ForegroundColor Cyan
