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

Write-Host "=== TEST 1: Developer Portal & Catalog ===" -ForegroundColor Cyan
$cat = Invoke-RestMethod "$A/portal/api/catalog"
Check "portal catalog lists APIs" ($cat.apis.Count -ge 1)
$hasNvidia = $cat.apis | Where-Object name -eq 'nvidia-nim'
Check "nvidia-nim AI API listed in catalog" ([bool]$hasNvidia)

# Developer self-service registration
$devName = "ai-app-dev-" + ([guid]::NewGuid().ToString().Substring(0,8))
$reg = Invoke-RestMethod -Method POST -Uri "$A/portal/api/register" -ContentType 'application/json' `
  -Body (@{ name = $devName; email = "$devName@app.io"; api_ids = @($hasNvidia.id) } | ConvertTo-Json)
Check "developer registration created API key" ([bool]$reg.api_key -and $reg.api_key.StartsWith("rk_"))
$devKey = $reg.api_key
Start-Sleep -Milliseconds 400

Write-Host "`n=== TEST 2: NVIDIA AI Gateway with Secret Injection ===" -ForegroundColor Cyan
# Call /ai/nvidia/models through RelayOps gateway using our client API key!
# The gateway transparently injects Bearer ${secret:NVIDIA_API_KEY} upstream.
$modelsResp = Gw '/ai/nvidia/models' @{ 'X-API-Key' = $devKey }
Check "NVIDIA models proxy successful (200)" ($modelsResp.s -eq 200)
$modelsData = $modelsResp.b | ConvertFrom-Json
Check "NVIDIA returned models list (>20 models)" ($modelsData.data.Count -gt 20)

# Check quota headers returned from Redis
Check "Redis Quota-Day header returned" ([bool]$modelsResp.h['X-Quota-Day-Limit'])
Check "Redis RateLimit header returned" ([bool]$modelsResp.h['X-RateLimit-Limit'])

# Execute a fast chat completion with Llama 3.2 Vision
$chatBody = @{
  model = "meta/llama-3.2-11b-vision-instruct"
  messages = @(
    @{ role = "user"; content = "Say 'RelayOps is running' in exactly 3 words." }
  )
  temperature = 0.1
  max_tokens = 20
}
$chatResp = Gw '/ai/nvidia/chat/completions' @{ 'X-API-Key' = $devKey } $chatBody
Check "NVIDIA chat completion successful (200)" ($chatResp.s -eq 200)
$chatData = $chatResp.b | ConvertFrom-Json
$content = $chatData.choices[0].message.content
Write-Host "AI Response: $content" -ForegroundColor Yellow
Check "AI completion has content" ([bool]$content)

Write-Host "`n=== TEST 3: Redis Cluster Rate Limiting & Quotas ===" -ForegroundColor Cyan
$redisKeys = & .\tools\redis\redis-cli.exe keys "relayops:*"
Check "Redis rate limit or quota keys exist" ($redisKeys.Count -gt 0)

Write-Host "`n=== TEST 4: OpenAPI 3.0 Spec Import ===" -ForegroundColor Cyan
$sampleOpenAPI = @"
openapi: 3.0.0
info:
  title: Petstore Logistics API
  version: 2.1.0
  description: Logistics and fulfillment service
servers:
  - url: http://localhost:7070
paths:
  /shipments:
    get:
      summary: List all active shipments
    post:
      summary: Create new shipment
"@
$existingApis = Adm GET '/api/apis'
foreach ($item in $existingApis) {
  if ($item.name -eq 'petstore-logistics-api') {
    Adm DELETE "/api/apis/$($item.id)" | Out-Null
  }
}
Start-Sleep -Milliseconds 300
$imported = Adm POST '/api/apis/import-openapi' $sampleOpenAPI
Check "OpenAPI import succeeded" ($imported.name -eq 'petstore-logistics-api')
Check "OpenAPI base path derived" ($imported.base_path -eq '/petstore-logistics-api')
Start-Sleep -Milliseconds 400

# Call the imported API
$testImp = Gw '/petstore-logistics-api/shipments'
Check "Imported API route is live and callable (200)" ($testImp.s -eq 200)

Write-Host "`n=== TEST 5: FinOps & LLM Token Logging ===" -ForegroundColor Cyan
Start-Sleep -Seconds 2
$aiLogs = Adm GET '/api/logs?q=nvidia&limit=10'
Check "AI chat request logged with model name" (@($aiLogs | Where-Object { $_.model -match 'mistral|llama' }).Count -ge 1)

$summary = Adm GET '/api/analytics/summary?window=15m'
Check "Analytics summary tracked tokens" ($summary.total_tokens -gt 0)
Write-Host "Total tokens metered: $($summary.total_tokens)" -ForegroundColor Green

# Cleanup test resources
Adm DELETE "/api/apis/$($imported.id)" | Out-Null
Adm DELETE "/api/consumers/$($reg.consumer.id)" | Out-Null

Write-Host ""
if ($fail) { Write-Host "$fail check(s) FAILED" -ForegroundColor Red; exit 1 }
else { Write-Host "ALL ADVANCED FEATURE CHECKS PASSED!" -ForegroundColor Green }
