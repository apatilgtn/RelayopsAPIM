$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$AdminBase = 'http://localhost:9090'; $GatewayBase = 'http://localhost:8080'
$H = @{ Authorization = 'Bearer relayops-admin' }
function Adm($m, $p, $b) {
  if ($b) { Invoke-RestMethod -Method $m -Uri "$AdminBase$p" -Headers $H -ContentType 'application/json' -Body ($b | ConvertTo-Json -Depth 5) }
  else { Invoke-RestMethod -Method $m -Uri "$AdminBase$p" -Headers $H }
}
function Gw($p, $hdr = @{}) {
  try {
    $r = Invoke-WebRequest -Uri "$GatewayBase$p" -Headers $hdr -UseBasicParsing
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
function Check($name, $cond) { if ($cond) { Write-Host "PASS  $name" -ForegroundColor Green } else { Write-Host "FAIL  $name" -ForegroundColor Red; $script:fail++ } }
$fail = 0

function Items($x) { if ($null -eq $x) { return @() }; if ($x.value) { return $x.value }; return @($x) }

# pre-clean any leftover resources from previous runs
$targets = @('demo', 'orders', 'slow', 'dead')
$rawApis = Adm GET '/api/apis'
foreach ($item in $rawApis) {
  if ($targets -contains $item.name) {
    Adm DELETE "/api/apis/$($item.id)" | Out-Null
  }
}
$rawConsumers = Adm GET '/api/consumers'
foreach ($item in $rawConsumers) {
  if ($item.name -eq 'acme') { Adm DELETE "/api/consumers/$($item.id)" | Out-Null }
}
$rawPlans = Adm GET '/api/plans'
foreach ($item in $rawPlans) {
  if ($item.name -eq 'Tiny') { Adm DELETE "/api/plans/$($item.id)" | Out-Null }
}
Start-Sleep -Milliseconds 400

# unauthenticated admin call is rejected
try { Invoke-RestMethod "$AdminBase/api/apis" | Out-Null; Check 'admin auth required' $false } catch { Check 'admin auth required' ($_.Exception.Response.StatusCode -eq 401) }

# 1. open API
$open = Adm POST '/api/apis' @{ name = 'demo'; base_path = '/demo'; upstream_url = 'http://localhost:7070'; description = 'open demo API' }
Start-Sleep -Milliseconds 300
$r = Gw '/demo/hello?x=1'
$body = $r.b | ConvertFrom-Json
Check 'open API proxies (200)' ($r.s -eq 200)
Check 'path stripped (/hello)' ($body.path -eq '/hello')
Check 'X-Request-ID returned' ([bool]$r.h['X-Request-ID'])
Check 'X-Forwarded-For set upstream' ([bool]$body.headers.'X-Forwarded-For')
Check 'unknown path -> 404' ((Gw '/nope').s -eq 404)

# 2. API key protected API + consumer + plan
$plans = Adm GET '/api/plans'
$tiny = Adm POST '/api/plans' @{ name = 'Tiny'; description = 'test'; rate_limit_per_minute = 5 }
$sec = Adm POST '/api/apis' @{ name = 'orders'; base_path = '/orders'; upstream_url = 'http://localhost:7070/v1'; auth_type = 'api_key'; request_headers = @{ 'X-Env' = 'test' } }
$c = Adm POST '/api/consumers' @{ name = 'acme'; email = 'dev@acme.io' }
$k = Adm POST "/api/consumers/$($c.id)/keys" @{ name = 'prod' }
Start-Sleep -Milliseconds 300
Check 'no key -> 401' ((Gw '/orders/list').s -eq 401)
Check 'bad key -> 401' ((Gw '/orders/list' @{ 'X-API-Key' = 'rk_bad' }).s -eq 401)
Check 'valid key, not subscribed -> 403' ((Gw '/orders/list' @{ 'X-API-Key' = $k.key }).s -eq 403)

$sub = Adm POST '/api/subscriptions' @{ consumer_id = $c.id; api_id = $sec.id; plan_id = $tiny.id }
Start-Sleep -Milliseconds 300
$r = Gw '/orders/list' @{ 'X-API-Key' = $k.key }
$body = $r.b | ConvertFrom-Json
Check 'subscribed -> 200 (hot reload, no restart)' ($r.s -eq 200)
Check 'upstream base path joined (/v1/list)' ($body.path -eq '/v1/list')
Check 'API key stripped from upstream' (-not $body.headers.'X-Api-Key')
Check 'consumer header injected' ($body.headers.'X-Consumer-Name' -eq 'acme')
Check 'custom header injected' ($body.headers.'X-Env' -eq 'test')
Check 'rate limit headers' ($r.h['X-RateLimit-Limit'] -eq '5')

$codes = 1..6 | ForEach-Object { (Gw '/orders/list' @{ 'X-API-Key' = $k.key }).s }
Check "plan limit enforced -> 429 (got $($codes -join ','))" ($codes -contains 429)

# plan upgrade takes effect immediately
$unl = $plans | Where-Object name -eq 'Unlimited'
Adm PUT "/api/subscriptions/$($sub.id)" @{ plan_id = $unl.id; active = $true } | Out-Null
Start-Sleep -Milliseconds 300
Check 'plan upgrade lifts limit instantly' ((Gw '/orders/list' @{ 'X-API-Key' = $k.key }).s -eq 200)

# revoke key
Adm POST "/api/keys/$($k.id)/revoke" | Out-Null
Start-Sleep -Milliseconds 300
Check 'revoked key rejected instantly' ((Gw '/orders/list' @{ 'X-API-Key' = $k.key }).s -eq 401)

# 3. direct SQL change propagates via NOTIFY
psql -h localhost -U postgres -w -d relayops -q -c "UPDATE apis SET enabled=false WHERE name='demo';" | Out-Null
Start-Sleep -Milliseconds 400
Check 'direct SQL disable -> 404 via LISTEN/NOTIFY' ((Gw '/demo/hello').s -eq 404)
psql -h localhost -U postgres -w -d relayops -q -c "UPDATE apis SET enabled=true WHERE name='demo';" | Out-Null
Start-Sleep -Milliseconds 400
Check 'direct SQL re-enable -> 200' ((Gw '/demo/hello').s -eq 200)

# 4. JWT
$secret = 'super-secret-signing-key-123'
$jwtApi = Adm POST '/api/apis' @{ name = 'billing'; base_path = '/billing'; upstream_url = 'http://localhost:7070'; auth_type = 'jwt'; jwt_secret = $secret }
function B64u([byte[]]$b) { [Convert]::ToBase64String($b).TrimEnd('=').Replace('+', '-').Replace('/', '_') }
function Jwt($claims, $key) {
  $h = B64u ([Text.Encoding]::UTF8.GetBytes('{"alg":"HS256","typ":"JWT"}'))
  $p = B64u ([Text.Encoding]::UTF8.GetBytes(($claims | ConvertTo-Json -Compress)))
  $mac = New-Object Security.Cryptography.HMACSHA256 (,[Text.Encoding]::UTF8.GetBytes($key))
  "$h.$p." + (B64u $mac.ComputeHash([Text.Encoding]::UTF8.GetBytes("$h.$p")))
}
$exp = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + 300
Start-Sleep -Milliseconds 300
$r = Gw '/billing/invoices' @{ Authorization = 'Bearer ' + (Jwt @{ sub = 'user-42'; exp = $exp } $secret) }
Check 'valid JWT -> 200' ($r.s -eq 200)
Check 'JWT sub forwarded' ((($r.b | ConvertFrom-Json).headers.'X-Consumer-Name') -eq 'user-42')
Check 'wrong-signature JWT -> 401' ((Gw '/billing/invoices' @{ Authorization = 'Bearer ' + (Jwt @{ sub = 'x'; exp = $exp } 'wrong-key-wrong-key') }).s -eq 401)
Check 'expired JWT -> 401' ((Gw '/billing/invoices' @{ Authorization = 'Bearer ' + (Jwt @{ sub = 'x'; exp = 1000 } $secret) }).s -eq 401)

# 5. upstream errors / timeouts
Adm POST '/api/apis' @{ name = 'slow'; base_path = '/slow'; upstream_url = 'http://localhost:7070'; timeout_ms = 300 } | Out-Null
Adm POST '/api/apis' @{ name = 'dead'; base_path = '/dead'; upstream_url = 'http://localhost:7999' } | Out-Null
Start-Sleep -Milliseconds 300
Check 'upstream timeout -> 504' ((Gw '/slow/delay/1000').s -eq 504)
Check 'upstream down -> 502' ((Gw '/dead/x').s -eq 502)
Check 'upstream 500 passed through' ((Gw '/demo/status/500').s -eq 500)

# 6. streaming (SSE through the gateway)
$r = Gw '/demo/stream'
Check 'SSE stream proxied (10 events)' (([regex]::Matches($r.b, 'tick')).Count -eq 10)

# 7. validation + conflict
try { Adm POST '/api/apis' @{ name = 'demo'; base_path = '/demo2'; upstream_url = 'http://x' } | Out-Null; Check 'duplicate name -> 409' $false } catch { Check 'duplicate name -> 409' ($_.Exception.Response.StatusCode -eq 409) }
try { Adm POST '/api/apis' @{ name = 'bad'; base_path = 'nope'; upstream_url = 'http://x' } | Out-Null; Check 'invalid base_path -> 400' $false } catch { Check 'invalid base_path -> 400' ($_.Exception.Response.StatusCode -eq 400) }

# 8. logs + analytics persisted in Postgres
Start-Sleep -Seconds 2
$logs = Adm GET '/api/logs?limit=500'
Check "request logs persisted ($($logs.Count) rows)" ($logs.Count -ge 20)
$errLogs = Adm GET '/api/logs?status=5xx'; $errLogs = @($errLogs)
Check "5xx filter works ($($errLogs.Count) rows)" ($errLogs.Count -ge 3 -and @($errLogs | Where-Object { $_.status -lt 500 }).Count -eq 0)
$s = Adm GET '/api/analytics/summary?window=15m'
Check "analytics summary (total=$($s.total), p95=$([math]::Round($s.p95_latency_ms,1))ms)" ($s.total -ge 20 -and $s.top_apis.Count -ge 3)
$ov = Adm GET '/api/overview'
Check "overview (config v$($ov.gateway.config_version), routes=$($ov.gateway.routes))" ($ov.gateway.routes -ge 5)

# cleanup test-only resources (leave demo + orders for the user)
Adm DELETE "/api/apis/$($jwtApi.id)" | Out-Null
foreach ($a in (Items (Adm GET '/api/apis'))) { if ($a.name -in 'slow', 'dead') { Adm DELETE "/api/apis/$($a.id)" | Out-Null } }
Adm DELETE "/api/plans/$($tiny.id)" | Out-Null

Write-Host ""
if ($fail) { Write-Host "$fail check(s) FAILED" -ForegroundColor Red; exit 1 } else { Write-Host "ALL CHECKS PASSED" -ForegroundColor Green }
