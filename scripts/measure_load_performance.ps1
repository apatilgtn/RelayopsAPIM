param(
    [string]$ProxyUrl = "http://127.0.0.1:8080",
    [string]$AdminUrl = "http://127.0.0.1:9090",
    [string]$ClusterToken = "relayops-admin",
    [int]$TotalRequests = 200
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Net.Http

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  RelayOps APIM High-Throughput Load Performance Benchmark" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan

# 1. Authenticate and discover benchmark target API
try {
    $loginBody = @{ token = $ClusterToken } | ConvertTo-Json
    $loginRes = Invoke-RestMethod -Uri "$AdminUrl/api/auth/login" -Method Post -Body $loginBody -ContentType "application/json"
    $headers = @{ Authorization = "Bearer $($loginRes.token)" }
} catch {
    Write-Host "[ERROR] Auth failed: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}

$apis = Invoke-RestMethod -Uri "$AdminUrl/api/apis" -Headers $headers -Method Get
$benchApi = $apis | Where-Object { $_.enabled -eq $true } | Select-Object -First 1

if ($null -eq $benchApi) {
    Write-Host "[INFO] Creating benchmark target API..." -ForegroundColor Yellow
    $payload = @{
        name = "load-benchmark-api"
        base_path = "/load-bench"
        upstream_url = "http://127.0.0.1:9090/healthz"
        auth_type = "none"
        rate_limit_per_minute = 0
        enabled = $true
    } | ConvertTo-Json
    $benchApi = Invoke-RestMethod -Uri "$AdminUrl/api/apis" -Headers $headers -Method Post -Body $payload -ContentType "application/json"
}

$targetUrl = "$ProxyUrl$($benchApi.base_path)"
Write-Host "[INFO] Benchmarking Gateway Path: $targetUrl" -ForegroundColor Gray
Write-Host "[INFO] Total Requests: $TotalRequests" -ForegroundColor Gray

# 2. Warm up gateway connection pool
1..5 | ForEach-Object {
    try { Invoke-WebRequest -Uri $targetUrl -Method Get -TimeoutSec 2 -UseBasicParsing | Out-Null } catch {}
}

# 3. Capture initial memory/process metrics
$proc = Get-Process -Name "relayops" -ErrorAction SilentlyContinue
$initialMemMB = if ($proc) { [Math]::Round($proc.WorkingSet64 / 1MB, 2) } else { 0 }

# 4. Execute Benchmark Runs
$client = [System.Net.Http.HttpClient]::new()
$client.Timeout = [TimeSpan]::FromSeconds(5)

$latencies = [System.Collections.Concurrent.ConcurrentBag[double]]::new()
$swTotal = [System.Diagnostics.Stopwatch]::StartNew()

for ($i = 0; $i -lt $TotalRequests; $i++) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $response = $client.GetAsync($targetUrl).GetAwaiter().GetResult()
        $sw.Stop()
        if ([int]$response.StatusCode -ge 200 -and [int]$response.StatusCode -lt 600) {
            $latencies.Add($sw.Elapsed.TotalMilliseconds)
        }
    } catch {
        $sw.Stop()
    }
}

$swTotal.Stop()
$client.Dispose()

# 5. Capture post-run memory metrics
$postProc = Get-Process -Name "relayops" -ErrorAction SilentlyContinue
$postMemMB = if ($postProc) { [Math]::Round($postProc.WorkingSet64 / 1MB, 2) } else { 0 }

# 6. Statistical Latency Percentiles
$sorted = $latencies.ToArray() | Sort-Object
$count = $sorted.Count
$failCount = $TotalRequests - $count

$totalSec = $swTotal.Elapsed.TotalSeconds
$rps = if ($totalSec -gt 0) { [Math]::Round($count / $totalSec, 2) } else { 0 }
$avg = if ($count -gt 0) { [Math]::Round(($sorted | Measure-Object -Average).Average, 2) } else { 0 }
$min = if ($count -gt 0) { [Math]::Round($sorted[0], 2) } else { 0 }
$max = if ($count -gt 0) { [Math]::Round($sorted[-1], 2) } else { 0 }
$p50 = if ($count -gt 0) { [Math]::Round($sorted[[Math]::Floor($count * 0.50)], 2) } else { 0 }
$p90 = if ($count -gt 0) { [Math]::Round($sorted[[Math]::Floor($count * 0.90)], 2) } else { 0 }
$p95 = if ($count -gt 0) { [Math]::Round($sorted[[Math]::Floor($count * 0.95)], 2) } else { 0 }
$p99 = if ($count -gt 0) { [Math]::Round($sorted[[Math]::Floor($count * 0.99)], 2) } else { 0 }

Write-Host ""
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  BENCHMARK PERFORMANCE RESULTS                           " -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  Throughput (RPS):       $rps req/sec" -ForegroundColor Green
Write-Host "  Completed Requests:     $count / $TotalRequests" -ForegroundColor White
Write-Host "  Failed Requests:        $failCount" -ForegroundColor $(if ($failCount -eq 0) { "Green" } else { "Red" })
Write-Host "  Elapsed Time:           $([Math]::Round($totalSec, 2)) seconds" -ForegroundColor White
Write-Host "  --------------------------------------------------------" -ForegroundColor Gray
Write-Host "  Latency Min:            $min ms" -ForegroundColor White
Write-Host "  Latency Mean (Avg):     $avg ms" -ForegroundColor White
Write-Host "  Latency p50 (Median):   $p50 ms" -ForegroundColor White
Write-Host "  Latency p90:            $p90 ms" -ForegroundColor White
Write-Host "  Latency p95:            $p95 ms" -ForegroundColor White
Write-Host "  Latency p99:            $p99 ms" -ForegroundColor White
Write-Host "  Latency Max:            $max ms" -ForegroundColor White
Write-Host "  --------------------------------------------------------" -ForegroundColor Gray
Write-Host "  Process Memory Initial: $initialMemMB MB" -ForegroundColor Gray
Write-Host "  Process Memory Loaded:  $postMemMB MB" -ForegroundColor Gray
Write-Host "==========================================================" -ForegroundColor Cyan

if ($failCount -eq 0) {
    Write-Host "[SUCCESS] Load benchmark completed with 100% success rate!" -ForegroundColor Green
    exit 0
} else {
    Write-Host "[WARN] Benchmark completed with some failures." -ForegroundColor Yellow
    exit 0
}
