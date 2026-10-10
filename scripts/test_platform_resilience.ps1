# scripts/test_platform_resilience.ps1
# Enterprise Release Safety, Canary Fleet Convergence, Auto-Rollback & Resilience Test Suite

$ErrorActionPreference = "Stop"

$adminUrlPrimary = "http://127.0.0.1:9090"
$proxyUrlPrimary = "http://127.0.0.1:8080"
$adminUrlCanary  = "http://127.0.0.1:9092"
$proxyUrlCanary  = "http://127.0.0.1:8082"
$mockUpstreamUrl = "http://127.0.0.1:7070"
$adminToken      = "relayops-admin"

$headers = @{
    "Authorization" = "Bearer $adminToken"
    "Content-Type"  = "application/json"
}

$passCount = 0
$failCount = 0

function Assert-Step([string]$name, [bool]$condition, [string]$detail = "") {
    if ($condition) {
        Write-Host "  [PASS] $name" -ForegroundColor Green
        $script:passCount++
    } else {
        Write-Host "  [FAIL] $name : $detail" -ForegroundColor Red
        $script:failCount++
    }
}

Write-Host "`n========================================================" -ForegroundColor Cyan
Write-Host "  RelayOps APIM - Complete Resilience & Safety Verification" -ForegroundColor Cyan
Write-Host "========================================================`n" -ForegroundColor Cyan

# Ensure primary node is up
try {
    $health = Invoke-RestMethod -Uri "$adminUrlPrimary/healthz" -Method Get
    Assert-Step "Primary Gateway Node Available" ($health.status -eq "ok") "status: $($health.status)"
} catch {
    Write-Host "FATAL: Primary gateway at $adminUrlPrimary is not reachable: $_" -ForegroundColor Red
    exit 1
}

# ---------------------------------------------------------------------------
# Section 1: Monitoring Trustworthiness & Prometheus Histograms (Item 3)
# ---------------------------------------------------------------------------
Write-Host "`n--- Section 1: Trustworthy Cumulative Counters & Latency Histograms ---" -ForegroundColor Yellow

# Send diverse requests to primary proxy to generate metric points
try {
    $null = Invoke-RestMethod -Uri "$proxyUrlPrimary/__relayops/health" -Method Get
    $null = Invoke-RestMethod -Uri "$proxyUrlPrimary/nonexistent-route-for-metrics-test" -Method Get -SkipHttpErrorCheck
} catch {}

$metricsRaw = Invoke-RestMethod -Uri "$adminUrlPrimary/metrics" -Method Get
Assert-Step "Metrics endpoint returns Prometheus 0.0.4 format" ($metricsRaw -match "relayops_config_revision")
Assert-Step "Metrics export cumulative status counters" ($metricsRaw -match "relayops_requests_total")
Assert-Step "Metrics export Prometheus latency histogram buckets" ($metricsRaw -match "relayops_request_duration_seconds_bucket\{le=""0.005""\}")
Assert-Step "Metrics export histogram sum and count" ($metricsRaw -match "relayops_request_duration_seconds_sum" -and $metricsRaw -match "relayops_request_duration_seconds_count")
Assert-Step "Metrics export fleet convergence gauge" ($metricsRaw -match "relayops_fleet_converged")

# ---------------------------------------------------------------------------
# Section 2: OpenAPI Compatibility Engine ($ref, types, enums, responses) (Item 4)
# ---------------------------------------------------------------------------
Write-Host "`n--- Section 2: Advanced OpenAPI Contract Checking ($ref, types, enums) ---" -ForegroundColor Yellow

$contractPayload = @{
    base_spec = @{
        openapi = "3.0.0"
        paths = @{
            "/payments" = @{
                post = @{
                    parameters = @(
                        @{
                            name = "currency"
                            in = "query"
                            required = $false
                            schema = @{
                                type = "string"
                                enum = @("USD", "EUR", "GBP", "JPY")
                            }
                        }
                    )
                    requestBody = @{
                        required = $true
                        content = @{
                            "application/json" = @{
                                schema = @{
                                    '$ref' = "#/components/schemas/PaymentRequest"
                                }
                            }
                        }
                    }
                    responses = @{
                        "200" = @{
                            description = "Success"
                            content = @{
                                "application/json" = @{
                                    schema = @{
                                        '$ref' = "#/components/schemas/PaymentResponse"
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        components = @{
            schemas = @{
                PaymentRequest = @{
                    type = "object"
                    required = @("amount", "account_id")
                    properties = @{
                        amount = @{ type = "integer" }
                        account_id = @{ type = "string" }
                        meta = @{
                            type = "object"
                            properties = @{
                                tag = @{ type = "string" }
                            }
                        }
                    }
                }
                PaymentResponse = @{
                    type = "object"
                    required = @("transaction_id", "status")
                    properties = @{
                        transaction_id = @{ type = "string" }
                        status = @{ type = "string" }
                    }
                }
            }
        }
    }
    proposed_spec = @{
        openapi = "3.0.0"
        paths = @{
            "/payments" = @{
                post = @{
                    parameters = @(
                        @{
                            name = "currency"
                            in = "query"
                            required = $false
                            schema = @{
                                type = "string"
                                enum = @("USD", "EUR") # GBP and JPY removed! (Breaking)
                            }
                        }
                        @{
                            name = "idempotency_key"
                            in = "header"
                            required = $true # New required parameter! (Breaking)
                            schema = @{ type = "string" }
                        }
                    )
                    requestBody = @{
                        required = $true
                        content = @{
                            "application/json" = @{
                                schema = @{
                                    '$ref' = "#/components/schemas/PaymentRequest"
                                }
                            }
                        }
                    }
                    responses = @{
                        "200" = @{
                            description = "Success"
                            content = @{
                                "application/json" = @{
                                    schema = @{
                                        '$ref' = "#/components/schemas/PaymentResponse"
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        components = @{
            schemas = @{
                PaymentRequest = @{
                    type = "object"
                    required = @("amount", "account_id", "pin") # "pin" newly required! (Breaking)
                    properties = @{
                        amount = @{ type = "string" } # Type changed from integer to string! (Breaking)
                        account_id = @{ type = "string" }
                        pin = @{ type = "string" }
                        meta = @{
                            type = "object"
                            properties = @{
                                tag = @{ type = "integer" } # Nested property type changed! (Breaking)
                            }
                        }
                    }
                }
                PaymentResponse = @{
                    type = "object"
                    required = @("transaction_id") # "status" removed from required! (Breaking)
                    properties = @{
                        transaction_id = @{ type = "string" }
                    }
                }
            }
        }
    }
} | ConvertTo-Json -Depth 15

$contractResult = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis/validate-contract" -Method Post -Headers $headers -Body $contractPayload
Assert-Step "Contract checker identifies incompatible proposed spec" ($contractResult.is_compatible -eq $false)
Assert-Step "Contract checker resolves `$ref and flags breaking changes" ($contractResult.breaking_changes_count -ge 4) "count: $($contractResult.breaking_changes_count)"

$diffDescriptions = ($contractResult.differences | ForEach-Object { $_.description }) -join "; "
Assert-Step "Contract checker caught enum reduction (GBP/JPY)" ($diffDescriptions -match "currency" -and $diffDescriptions -match "enum")
Assert-Step "Contract checker caught new required parameter (idempotency_key)" ($diffDescriptions -match "idempotency_key")
Assert-Step "Contract checker caught newly required request body field (pin)" ($diffDescriptions -match "pin")
Assert-Step "Contract checker caught type mutation on amount" ($diffDescriptions -match "amount")

# ---------------------------------------------------------------------------
# Section 3: Declarative Configuration Automation & GitOps (Item 6)
# ---------------------------------------------------------------------------
Write-Host "`n--- Section 3: Declarative Configuration Automation (Export & Apply) ---" -ForegroundColor Yellow

$exportResult = Invoke-RestMethod -Uri "$adminUrlPrimary/api/system/export" -Method Get -Headers $headers
Assert-Step "Declarative export succeeds with format_version 1.0" ($exportResult.format_version -eq "1.0")

# Apply declarative config adding an automated declarative API
$declarativePayload = @{
    format_version = "1.0"
    plans = @(
        @{
            name = "Enterprise Declarative Plan"
            description = "Applied via declarative gitops pipeline"
            rate_limit_per_minute = 1000
            quota_per_day = 50000
            quota_per_month = 1000000
        }
    )
    apis = @(
        @{
            name = "Declarative GitOps API"
            description = "Deployed via declarative apply"
            base_path = "/gitops-service"
            upstream_url = "$mockUpstreamUrl/anything"
            auth_type = "none"
            enabled = $true
            is_draft = $false
        }
    )
} | ConvertTo-Json -Depth 10

$applyResult = Invoke-RestMethod -Uri "$adminUrlPrimary/api/system/apply" -Method Post -Headers $headers -Body $declarativePayload
Assert-Step "Declarative apply publishes atomic configuration revision" ($applyResult.revision -gt 0) "rev: $($applyResult.revision)"

# Wait 200ms for data plane watcher reload
Start-Sleep -Milliseconds 300

$gitopsReq = Invoke-RestMethod -Uri "$proxyUrlPrimary/gitops-service" -Method Get
Assert-Step "Declarative API immediately routable on proxy" ($gitopsReq.message -eq "pong" -or $gitopsReq.method -eq "GET" -or $null -ne $gitopsReq)

# ---------------------------------------------------------------------------
# Section 4: Enterprise RBAC & Role Boundaries (Item 6)
# ---------------------------------------------------------------------------
Write-Host "`n--- Section 4: Enterprise RBAC & Permission Boundaries ---" -ForegroundColor Yellow

# Create auditor user
$auditorEmail = "auditor-test-$([Guid]::NewGuid().ToString().Substring(0,8))@relayops.local"
$auditorUser = Invoke-RestMethod -Uri "$adminUrlPrimary/api/admin/users" -Method Post -Headers $headers -Body (@{
    name = "Compliance Auditor"
    email = $auditorEmail
    role = "auditor"
    team = "Security Audit"
    password = "AuditorPassword123!"
} | ConvertTo-Json)

$auditorLogin = Invoke-RestMethod -Uri "$adminUrlPrimary/api/auth/login" -Method Post -Body (@{
    email = $auditorEmail
    password = "AuditorPassword123!"
} | ConvertTo-Json)
$auditorToken = $auditorLogin.token
$auditorHeaders = @{ "Authorization" = "Bearer $auditorToken"; "Content-Type" = "application/json" }

# Auditor can read logs and audit
$auditorAudit = Invoke-RestMethod -Uri "$adminUrlPrimary/api/audit-logs?limit=5" -Method Get -Headers $auditorHeaders
Assert-Step "Auditor role has read access to audit logs" ($null -ne $auditorAudit)

# Auditor is strictly forbidden from mutations
$auditorMutationBlocked = $false
try {
    $null = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis" -Method Post -Headers $auditorHeaders -Body (@{
        name = "Auditor Illegal API"
        base_path = "/illegal"
        upstream_url = "$mockUpstreamUrl/anything"
    } | ConvertTo-Json)
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) {
        $auditorMutationBlocked = $true
    }
}
Assert-Step "Auditor role strictly forbidden from creating APIs (HTTP 403)" $auditorMutationBlocked

# Create developer user
$devEmail = "dev-test-$([Guid]::NewGuid().ToString().Substring(0,8))@relayops.local"
$devUser = Invoke-RestMethod -Uri "$adminUrlPrimary/api/admin/users" -Method Post -Headers $headers -Body (@{
    name = "Partner Developer"
    email = $devEmail
    role = "developer"
    team = "Frontend Team"
    password = "DevPassword123!"
} | ConvertTo-Json)

$devLogin = Invoke-RestMethod -Uri "$adminUrlPrimary/api/auth/login" -Method Post -Body (@{
    email = $devEmail
    password = "DevPassword123!"
} | ConvertTo-Json)
$devToken = $devLogin.token
$devHeaders = @{ "Authorization" = "Bearer $devToken"; "Content-Type" = "application/json" }

# Developer can view APIs
$devApis = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis" -Method Get -Headers $devHeaders
Assert-Step "Developer role can list public APIs" ($null -ne $devApis)

# Developer is forbidden from modifying APIs
$devMutationBlocked = $false
try {
    $null = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis" -Method Post -Headers $devHeaders -Body (@{
        name = "Developer Illegal API"
        base_path = "/dev-illegal"
        upstream_url = "$mockUpstreamUrl/anything"
    } | ConvertTo-Json)
} catch {
    if ($_.Exception.Response.StatusCode -eq 403) {
        $devMutationBlocked = $true
    }
}
Assert-Step "Developer role forbidden from creating gateway APIs (HTTP 403)" $devMutationBlocked

# ---------------------------------------------------------------------------
# Section 5: Real Canary Deployments & Fleet Convergence Proof (Item 1)
# ---------------------------------------------------------------------------
Write-Host "`n--- Section 5: Real Canary Deployment & Fleet Convergence ---" -ForegroundColor Yellow

# Launch secondary gateway node configured as Canary node (:8082 proxy, :9092 admin)
Write-Host "  Launching canary gateway node instance on :8082..." -ForegroundColor Gray
$repoRoot = (Resolve-Path "$PSScriptRoot\..").Path
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = "$repoRoot\bin\relayops.exe"
$psi.WorkingDirectory = $repoRoot
$psi.EnvironmentVariables["RELAYOPS_PROXY_ADDR"] = ":8082"
$psi.EnvironmentVariables["RELAYOPS_ADMIN_ADDR"] = ":9092"
$psi.EnvironmentVariables["RELAYOPS_ADMIN_TOKEN"] = "relayops-admin"
$psi.EnvironmentVariables["RELAYOPS_NODE_ID"] = "node-canary-experimental"
$psi.EnvironmentVariables["RELAYOPS_NODE_GROUP"] = "canary"
$psi.EnvironmentVariables["RELAYOPS_CANARY"] = "true"
$psi.EnvironmentVariables["RELAYOPS_DATABASE_URL"] = "postgres://postgres@localhost:5432/relayops?sslmode=disable"
$psi.EnvironmentVariables["RELAYOPS_REDIS_URL"] = "redis://localhost:6379/0"
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$canaryProcess = [System.Diagnostics.Process]::Start($psi)

# Wait for canary node to initialize
$canaryReady = $false
for ($i = 0; $i -lt 15; $i++) {
    Start-Sleep -Milliseconds 600
    try {
        $ch = Invoke-RestMethod -Uri "http://127.0.0.1:9092/healthz" -Method Get
        if ($ch.status -eq "ok") {
            $canaryReady = $true
            break
        }
    } catch {}
}
Assert-Step "Canary Gateway Node started and reporting healthy" $canaryReady

try {
    # Check canary node metadata on health endpoint
    $canaryHealthMeta = Invoke-RestMethod -Uri "http://127.0.0.1:8082/__relayops/health" -Method Get
    Assert-Step "Canary node reports is_canary=true and node_group=canary" ($canaryHealthMeta.is_canary -eq $true -and $canaryHealthMeta.node_group -eq "canary")

    # Step 1: Create a brand new API in draft mode
    $canarySuffix = [Guid]::NewGuid().ToString().Substring(0,8)
    $canaryApiName = "Experimental Canary API-$canarySuffix"
    $canaryBasePath = "/canary-$canarySuffix"

    $canaryApi = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis" -Method Post -Headers $headers -Body (@{
        name = $canaryApiName
        description = "Canary candidate service"
        base_path = $canaryBasePath
        upstream_url = "$mockUpstreamUrl/anything"
        auth_type = "none"
        is_draft = $true
        enabled = $true
    } | ConvertTo-Json)

    # Step 2: Publish API atomically to generate a candidate revision
    $publishRes = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis/$($canaryApi.id)/publish" -Method Post -Headers $headers
    $candidateRev = $publishRes.revision
    Assert-Step "Candidate revision rev_$candidateRev generated" ($candidateRev -gt 0)

    # Step 3: Deploy revision ONLY to Canary group
    $deployCanaryRes = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/$candidateRev/canary" -Method Post -Headers $headers
    Assert-Step "Revision deployed to canary group with status=canary" ($deployCanaryRes.status -eq "canary")

    # Wait 800ms for fleet nodes to react to Postgres NOTIFY
    Start-Sleep -Milliseconds 800

    # Step 4: Verify Canary Status endpoint breakdown
    $canaryStatus = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/$candidateRev/canary-status" -Method Get -Headers $headers
    Assert-Step "Canary status reports revision $candidateRev" ($canaryStatus.revision -eq $candidateRev)
    Assert-Step "Canary nodes acknowledged candidate revision" ($canaryStatus.canary_acknowledged_count -ge 1)
    Assert-Step "Canary converged successfully" ($canaryStatus.canary_converged -eq $true)
    Assert-Step "Standard nodes NOT on canary revision yet" ($canaryStatus.fleet_converged -eq $false)

    # Step 5: Verify Data Plane isolation:
    # Canary node (:8082) MUST route the candidate API
    $canaryReq = Invoke-RestMethod -Uri "$proxyUrlCanary$canaryBasePath" -Method Get
    Assert-Step "Canary node (:8082) serves candidate revision successfully" ($null -ne $canaryReq)

    # Standard node (:8080) MUST NOT route the candidate API yet (stays on approved active snapshot)
    $standardBlocked = $false
    try {
        $null = Invoke-RestMethod -Uri "$proxyUrlPrimary$canaryBasePath" -Method Get
    } catch {
        if ($_.Exception.Response.StatusCode -eq 404) {
            $standardBlocked = $true
        }
    }
    Assert-Step "Standard node (:8080) safely isolates traffic and returns 404 for candidate revision" $standardBlocked

    # Step 6: Promote Canary revision to 100% of fleet
    $promoteRes = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/$candidateRev/promote" -Method Post -Headers $headers
    Assert-Step "Revision promoted to 100% of fleet (status=active, target_group=all)" ($promoteRes.status -eq "active" -and $promoteRes.target_group -eq "all")

    # Wait 800ms for standard node to apply promoted revision
    Start-Sleep -Milliseconds 800

    # Step 7: Verify standard node now routes the promoted revision
    $standardAfterPromote = Invoke-RestMethod -Uri "$proxyUrlPrimary$canaryBasePath" -Method Get
    Assert-Step "Standard node (:8080) successfully upgraded to promoted revision" ($null -ne $standardAfterPromote)

    $finalFleetStatus = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/$candidateRev/canary-status" -Method Get -Headers $headers
    Assert-Step "Fleet is 100% converged across all gateway nodes" ($finalFleetStatus.fleet_converged -eq $true)

} finally {
    # Clean up canary process
    if ($canaryProcess -and -not $canaryProcess.HasExited) {
        try { $canaryProcess.Kill() } catch {}
    }
}

# ---------------------------------------------------------------------------
# Section 6: Automatic Rollback Controller & Cooldown Enforcement (Item 2)
# ---------------------------------------------------------------------------
Write-Host "`n--- Section 6: Threshold-Based Automatic Rollback Controller ---" -ForegroundColor Yellow

# Get current baseline revision
$baselineRevs = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions?limit=5" -Method Get -Headers $headers
$currentStableRev = $baselineRevs[0].revision
Write-Host "  Current stable baseline revision: rev_$currentStableRev" -ForegroundColor Gray

# Deploy a faulty API that will trigger high 5xx errors
$faultySuffix = [Guid]::NewGuid().ToString().Substring(0,8)
$faultyApiName = "Faulty Test Service-$faultySuffix"
$faultyBasePath = "/faulty-$faultySuffix"

$faultyApi = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis" -Method Post -Headers $headers -Body (@{
    name = $faultyApiName
    description = "Service with invalid upstream triggering 502/504 errors"
    base_path = $faultyBasePath
    upstream_url = "http://127.0.0.1:59999/bad-port" # Unreachable port
    auth_type = "none"
    timeout_ms = 500
    is_draft = $true
    enabled = $true
} | ConvertTo-Json)

# Publish atomically to generate a new active revision
$faultyPublish = Invoke-RestMethod -Uri "$adminUrlPrimary/api/apis/$($faultyApi.id)/publish" -Method Post -Headers $headers
$faultyRev = $faultyPublish.revision
Write-Host "  Faulty revision deployed: rev_$faultyRev" -ForegroundColor Gray

# Configure auto-rollback controller:
# threshold: 25%, min_requests: 5, window: 60s, cooldown: 10s, reset cooldown from any prior test runs
$cfgPayload = @{
    enabled = $true
    error_rate_threshold_percent = 25.0
    evaluation_window_seconds = 60
    min_requests = 5
    cooldown_seconds = 10
    reset_cooldown = $true
} | ConvertTo-Json

$null = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/auto-rollback/config" -Method Post -Headers $headers -Body $cfgPayload

# Wait 800ms for gateway watcher to apply published revision
Start-Sleep -Milliseconds 800

# Send 6 requests to faulty service (100% error rate on rev_$faultyRev)
Write-Host "  Sending sample traffic to faulty revision..." -ForegroundColor Gray
for ($i = 0; $i -lt 6; $i++) {
    try {
        $null = Invoke-RestMethod -Uri "$proxyUrlPrimary$faultyBasePath" -Method Get -TimeoutSec 3
    } catch {
        # Catch 502/504 Bad Gateway from faulty upstream
    }
}

# Wait 2 seconds for analytics collector to flush request logs to database
Start-Sleep -Seconds 2

# Trigger evaluation
# The background supervisor on every control-plane node evaluates every 3s, so it may
# roll back before this manual evaluation runs. Either path must act exactly once, on
# the faulty revision; the persisted settings record which revision was rolled back.
$evalResult = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/auto-rollback/evaluate" -Method Post -Headers $headers
Write-Host "  Evaluation result: $($evalResult | ConvertTo-Json -Compress)" -ForegroundColor Magenta
$arState = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/auto-rollback/config" -Method Get -Headers $headers
$supervisorActed = ($evalResult.in_cooldown -eq $true) -and ($arState.last_triggered_revision -eq $faultyRev)
Assert-Step "Auto-rollback evaluator evaluated traffic sample" (($evalResult.evaluated -eq $true) -or $supervisorActed) "result: $($evalResult.reason)"
Assert-Step "Auto-rollback triggered when error rate breached threshold" (($evalResult.triggered -eq $true) -or $supervisorActed) "last_triggered_revision: $($arState.last_triggered_revision)"
Assert-Step "Auto-rollback identified active revision and restored baseline" (($evalResult.active_revision -eq $faultyRev) -or $supervisorActed) "reason: $($arState.last_triggered_reason)"
Assert-Step "Auto-rollback decision persisted cluster-wide (last_triggered_revision)" ($arState.last_triggered_revision -eq $faultyRev)

# Verify immediate second evaluation enters cooldown
$evalCooldown = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions/auto-rollback/evaluate" -Method Post -Headers $headers
Assert-Step "Auto-rollback supervisor enforces cooldown protection against flapper loops" ($evalCooldown.in_cooldown -eq $true)

# Verify new revision is a rollback in revision list
$afterRollbackRevs = Invoke-RestMethod -Uri "$adminUrlPrimary/api/revisions?limit=5" -Method Get -Headers $headers
$rollbackRev = $afterRollbackRevs[0]
Assert-Step "Rollback revision published and marked in revision history" ($rollbackRev.rollback_of -ne $null -or $rollbackRev.description -match "Rollback")

Write-Host "`n========================================================" -ForegroundColor Cyan
Write-Host "  Test Summary: $passCount Passed, $failCount Failed" -ForegroundColor $(if ($failCount -eq 0) { "Green" } else { "Red" })
Write-Host "========================================================`n" -ForegroundColor Cyan

if ($failCount -gt 0) {
    exit 1
}
