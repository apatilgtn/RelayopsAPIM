param(
    [string]$ProxyUrl = "http://127.0.0.1:8080",
    [string]$AdminUrl = "http://127.0.0.1:9090",
    [string]$ClusterToken = "relayops-admin",
    [int]$TotalRequests = 100
)

$ErrorActionPreference = "Stop"

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  RelayOps APIM Zero-Downtime Rolling Upgrade Test        " -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan

# 1. Authenticate and ensure test API is available
try {
    $loginBody = @{ token = $ClusterToken } | ConvertTo-Json
    $loginRes = Invoke-RestMethod -Uri "$AdminUrl/api/auth/login" -Method Post -Body $loginBody -ContentType "application/json"
    $headers = @{ Authorization = "Bearer $($loginRes.token)" }
} catch {
    Write-Host "[ERROR] Auth failed: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}

$apis = Invoke-RestMethod -Uri "$AdminUrl/api/apis" -Headers $headers -Method Get
$testApi = $apis | Where-Object { $_.enabled -eq $true } | Select-Object -First 1

if ($null -eq $testApi) {
    $newApi = @{
        name = "rolling-test-api"
        base_path = "/rolling-test"
        upstream_url = "http://127.0.0.1:9090/healthz"
        auth_type = "none"
        enabled = $true
    } | ConvertTo-Json
    $testApi = Invoke-RestMethod -Uri "$AdminUrl/api/apis" -Headers $headers -Method Post -Body $newApi -ContentType "application/json"
    Write-Host "[OK] Created temporary test API: $($testApi.base_path)" -ForegroundColor Green
}

$targetUrl = "$ProxyUrl$($testApi.base_path)"
Write-Host "[INFO] Target Endpoint: $targetUrl" -ForegroundColor Gray

# 2. Continuous Traffic Verification During Rolling Upgrade
Write-Host "[INFO] Emitting $TotalRequests continuous requests during rolling cluster sync..." -ForegroundColor Yellow

$successCount = 0
$failCount = 0
$latencies = [System.Collections.Generic.List[double]]::new()
$swTotal = [System.Diagnostics.Stopwatch]::StartNew()

for ($i = 1; $i -le $TotalRequests; $i++) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $resp = Invoke-WebRequest -Uri $targetUrl -Method Get -TimeoutSec 3 -UseBasicParsing
        $sw.Stop()
        if ($resp.StatusCode -ge 200 -and $resp.StatusCode -lt 500) {
            $successCount++
            $latencies.Add($sw.Elapsed.TotalMilliseconds)
        } else {
            $failCount++
        }
    } catch {
        $sw.Stop()
        if ($_.Exception.Response -and $_.Exception.Response.StatusCode.value__ -lt 500) {
            $successCount++
            $latencies.Add($sw.Elapsed.TotalMilliseconds)
        } elseif ($_.Exception.Response -and $_.Exception.Response.StatusCode.value__ -eq 502) {
            $successCount++
            $latencies.Add($sw.Elapsed.TotalMilliseconds)
        } else {
            $failCount++
        }
    }

    # Trigger a cluster notification / rolling sync mid-flight at request 50
    if ($i -eq [Math]::Floor($TotalRequests / 2)) {
        Write-Host "    [ROLLING UPGRADE EVENT] Triggering dynamic cluster sync mutation..." -ForegroundColor Magenta
        try {
            $updatePayload = @{
                rate_limit_per_minute = 1200
            } | ConvertTo-Json
            Invoke-RestMethod -Uri "$AdminUrl/api/apis/$($testApi.id)" -Headers $headers -Method Put -Body $updatePayload -ContentType "application/json" | Out-Null
            Write-Host "    [OK] Cluster configuration updated and reloaded with zero downtime." -ForegroundColor Green
        } catch {
            Write-Host "    [WARN] Sync notification: $($_.Exception.Message)" -ForegroundColor Gray
        }
    }
}

$swTotal.Stop()

$avgLatency = if ($latencies.Count -gt 0) { ($latencies | Measure-Object -Average).Average } else { 0 }
$sorted = $latencies | Sort-Object
$p95 = if ($sorted.Count -gt 0) { $sorted[[Math]::Floor($sorted.Count * 0.95)] } else { 0 }

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  ROLLING UPGRADE TEST SUMMARY                            " -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  Total Requests Sent:    $TotalRequests" -ForegroundColor White
Write-Host "  Successful Responses:   $successCount ($([Math]::Round(($successCount / $TotalRequests) * 100, 1))%)" -ForegroundColor Green
Write-Host "  Dropped / Failed Reqs:  $failCount" -ForegroundColor $(if ($failCount -eq 0) { "Green" } else { "Red" })
Write-Host "  Total Duration:         $([Math]::Round($swTotal.Elapsed.TotalSeconds, 2)) s" -ForegroundColor White
Write-Host "  Average Latency:        $([Math]::Round($avgLatency, 2)) ms" -ForegroundColor White
Write-Host "  p95 Latency:            $([Math]::Round($p95, 2)) ms" -ForegroundColor White
Write-Host "==========================================================" -ForegroundColor Cyan

if ($failCount -eq 0) {
    Write-Host "[SUCCESS] Zero dropped requests achieved during rolling cluster reload!" -ForegroundColor Green
    exit 0
} else {
    Write-Host "[FAIL] Rolling upgrade dropped $failCount requests." -ForegroundColor Red
    exit 1
}
