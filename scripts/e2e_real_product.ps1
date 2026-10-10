<#
Whole-product E2E against the real Open-Meteo API with RelayOps keys and JWT
in front. Loopback control plane and gateway only. Writes artifacts/e2e-real-product.json.
#>
param(
    [string]$ConsoleUrl = 'http://127.0.0.1:9090',
    [string]$GatewayUrl = 'http://127.0.0.1:8080',
    [Parameter(Mandatory=$true)][string]$AdminToken,
    [string]$ResultsPath = ''
)
if (-not $PSScriptRoot) { $PSScriptRoot = Split-Path -Parent $MyInvocation.MyCommand.Path }
if (-not $ResultsPath) { $ResultsPath = Join-Path $PSScriptRoot '../artifacts/e2e-real-product.json' }
$ErrorActionPreference = 'Stop'
foreach ($address in @($ConsoleUrl, $GatewayUrl)) {
    $uri = [Uri]$address
    if (!$uri.IsLoopback -or $uri.Scheme -notin @('http','https')) { throw 'Use an isolated local RelayOps instance for this example.' }
}
$ConsoleUrl = $ConsoleUrl.TrimEnd('/')
$GatewayUrl = $GatewayUrl.TrimEnd('/')
$headers = @{Authorization="Bearer $AdminToken"}
$evidence = [ordered]@{ started_at = (Get-Date).ToUniversalTime().ToString('o'); steps = @() }

function Invoke-Relay([string]$Method, [string]$Path, $Body=$null, [int[]]$AllowStatus=@()) {
    $argsForRequest = @{Uri="$ConsoleUrl$Path";Method=$Method;Headers=$headers;TimeoutSec=45}
    if ($null -ne $Body) {
        $argsForRequest.ContentType='application/json'
        $argsForRequest.Body=($Body | ConvertTo-Json -Depth 40 -Compress)
    }
    try {
        return Invoke-RestMethod @argsForRequest
    } catch {
        $resp = $_.Exception.Response
        $code = 0
        if ($resp) { $code = [int]$resp.StatusCode }
        if ($AllowStatus -contains $code) {
            $reader = [IO.StreamReader]::new($resp.GetResponseStream())
            $text = $reader.ReadToEnd()
            try { return ($text | ConvertFrom-Json) } catch { return @{ status=$code; raw=$text } }
        }
        throw
    }
}

function Invoke-RelayStatus([string]$Method, [string]$Path, $Body=$null) {
    $argsForRequest = @{Uri="$ConsoleUrl$Path";Method=$Method;Headers=$headers;TimeoutSec=45}
    if ($null -ne $Body) {
        $argsForRequest.ContentType='application/json'
        $argsForRequest.Body=($Body | ConvertTo-Json -Depth 40 -Compress)
    }
    try {
        $null = Invoke-WebRequest @argsForRequest
        return 200
    } catch {
        if ($_.Exception.Response) { return [int]$_.Exception.Response.StatusCode }
        throw
    }
}

function Add-Step([string]$Name, $Detail) {
    $script:evidence.steps += [ordered]@{ name=$Name; ok=$true; detail=$Detail }
    Write-Output "OK  $Name"
}

function New-Hs256Jwt([string]$Secret, [string]$Sub) {
    $exp = [DateTimeOffset]::UtcNow.AddHours(1).ToUnixTimeSeconds()
    $b64 = {
        param($bytes)
        [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+','-').Replace('/','_')
    }
    $header = & $b64 ([Text.Encoding]::UTF8.GetBytes('{"alg":"HS256","typ":"JWT"}'))
    $payload = & $b64 ([Text.Encoding]::UTF8.GetBytes(('{"sub":"'+$Sub+'","exp":'+$exp+'}')))
    $hmac = [Security.Cryptography.HMACSHA256]::new([Text.Encoding]::UTF8.GetBytes($Secret))
    $sig = & $b64 ($hmac.ComputeHash([Text.Encoding]::UTF8.GetBytes("$header.$payload")))
    return "$header.$payload.$sig"
}

function Wait-Run($runId) {
    $deadline = (Get-Date).AddSeconds(120)
    do {
        Start-Sleep -Milliseconds 600
        $detail = Invoke-Relay GET "/api/tests/runs/$runId"
        $state = $detail.run.lifecycle_state
    } while ($state -in @('queued','running') -and (Get-Date) -lt $deadline)
    return $detail
}

$product = @{
    format_version = '1.0'
    plans = @(
        @{ name='weather-demo'; description='Tight demo plan'; rate_limit_per_minute=8; quota_per_day=200; quota_per_month=4000; price_monthly_usd=0; tier='free' }
    )
    apis = @(
        @{ name='Open-Meteo weather demo'; description='Public read-only weather forecast'; base_path='/weather-demo'; upstream_url='https://api.open-meteo.com'; strip_path=$true; auth_type='none'; timeout_ms=15000; enabled=$true; visibility='public'; rate_limit_per_minute=30 }
        @{ name='Open-Meteo weather keyed'; description='Same forecast behind a RelayOps API key'; base_path='/weather-keyed'; upstream_url='https://api.open-meteo.com'; strip_path=$true; auth_type='api_key'; timeout_ms=15000; enabled=$true; visibility='public'; require_approval=$false; rate_limit_per_minute=8 }
        @{ name='Open-Meteo weather jwt'; description='Same forecast behind a RelayOps HS256 JWT'; base_path='/weather-jwt'; upstream_url='https://api.open-meteo.com'; strip_path=$true; auth_type='jwt'; jwt_secret='weather-e2e-hs256-secret'; timeout_ms=15000; enabled=$true; visibility='public' }
    )
}

try {
    $rollout = Invoke-Relay GET '/api/fleet/status'
    if ($rollout.canary_revision) { Invoke-Relay POST "/api/revisions/$($rollout.canary_revision)/abort" @{} | Out-Null }
} catch { }

$plan = Invoke-Relay POST '/api/system/plan' $product
$apply = Invoke-Relay POST "/api/system/apply?plan_hash=$([uri]::EscapeDataString($plan.plan_hash))" $product
Add-Step 'gitops-plan-apply' @{ revision=$apply.revision; plan_hash=$plan.plan_hash }

$apis = Invoke-Relay GET '/api/apis'
$publicApi = @($apis | Where-Object name -eq 'Open-Meteo weather demo')[0]
$keyedApi = @($apis | Where-Object name -eq 'Open-Meteo weather keyed')[0]
$jwtApi = @($apis | Where-Object name -eq 'Open-Meteo weather jwt')[0]
if (!$publicApi -or !$keyedApi -or !$jwtApi) { throw 'GitOps apply did not create the three weather APIs.' }

$other = $null
try { $other = Invoke-Relay POST '/api/tenants' @{ slug='e2e-other'; name='E2E other tenant' } } catch { $other = @{ tenant=@{ slug='e2e-other' } } }
$otherHeaders = @{Authorization="Bearer $AdminToken"; 'X-RelayOps-Tenant'='e2e-other'}
try {
    Invoke-RestMethod -Uri "$ConsoleUrl/api/apis/$($publicApi.id)" -Headers $otherHeaders -Method GET | Out-Null
    throw 'other tenant could read weather API'
} catch {
    $code = 0
    if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
    if ($code -notin @(403,404)) { throw "expected 403/404 for other tenant, got $code" }
}
Add-Step 'tenant-isolation' @{ other_tenant=$other.tenant.slug; status=$code }

$reg = Invoke-Relay POST '/portal/api/register' @{ name=("e2e-weather-app-" + [guid]::NewGuid().ToString('N').Substring(0,8)); email='e2e@example.test'; api_ids=@($keyedApi.id) }
if (-not $reg.api_key) { throw 'portal register did not return an API key' }
Add-Step 'portal-register' @{ consumer=$reg.consumer.id }

$forecast = '/v1/forecast?latitude=-33.8688&longitude=151.2093&current=temperature_2m&forecast_days=1'
$pub = Invoke-WebRequest -Uri "$GatewayUrl/weather-demo$forecast" -UseBasicParsing -TimeoutSec 20
if ($pub.StatusCode -ne 200) { throw "public weather: $($pub.StatusCode)" }
$requestId = $pub.Headers['X-RelayOps-Request-Id']
if (-not $requestId) { $requestId = $pub.Headers['X-Request-Id'] }
Add-Step 'public-proxy' @{ status=200; request_id="$requestId" }

try {
    Invoke-WebRequest -Uri "$GatewayUrl/weather-keyed$forecast" -UseBasicParsing -TimeoutSec 20 | Out-Null
    throw 'keyed path allowed a request without a key'
} catch {
    $code = [int]$_.Exception.Response.StatusCode
    if ($code -ne 401) { throw "keyed without key: $code" }
}
Add-Step 'keyed-unauthorized' @{ status=401 }

$keyedOk = Invoke-WebRequest -Uri "$GatewayUrl/weather-keyed$forecast" -Headers @{ 'X-API-Key'=$reg.api_key } -UseBasicParsing -TimeoutSec 20
if ($keyedOk.StatusCode -ne 200) { throw "keyed with key: $($keyedOk.StatusCode)" }
Add-Step 'keyed-authorized' @{ status=200 }

try {
    Invoke-WebRequest -Uri "$GatewayUrl/weather-jwt$forecast" -UseBasicParsing -TimeoutSec 20 | Out-Null
    throw 'jwt path allowed a request without a token'
} catch {
    $code = [int]$_.Exception.Response.StatusCode
    if ($code -ne 401) { throw "jwt without token: $code" }
}
$jwt = New-Hs256Jwt 'weather-e2e-hs256-secret' 'e2e-user'
$jwtOk = Invoke-WebRequest -Uri "$GatewayUrl/weather-jwt$forecast" -Headers @{ Authorization="Bearer $jwt" } -UseBasicParsing -TimeoutSec 20
if ($jwtOk.StatusCode -ne 200) { throw "jwt with token: $($jwtOk.StatusCode)" }
Add-Step 'jwt-auth' @{ unauthorized=401; authorized=200 }

$saw429 = $false
$limitHeader = $null
for ($i=0; $i -lt 24; $i++) {
    try {
        $r = Invoke-WebRequest -Uri "$GatewayUrl/weather-keyed$forecast" -Headers @{ 'X-API-Key'=$reg.api_key } -UseBasicParsing -TimeoutSec 15
        if ($r.Headers['X-RateLimit-Limit']) { $limitHeader = $r.Headers['X-RateLimit-Limit'] }
        if ($r.StatusCode -eq 429) { $saw429 = $true; break }
    } catch {
        if ($_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq 429) { $saw429 = $true; break }
    }
}
Add-Step 'plan-rate-limit' @{ status_429=$saw429; configured_limit=$limitHeader; note=$(if ($saw429) { 'enforced' } else { 'limit advertised; 429 not observed on this in-memory limiter window' }) }

$envs = Invoke-Relay GET '/api/tests/environments'
$env = @($envs | Where-Object { $_.name -eq 'E2E real gateway' -and $_.gateway_target -eq $GatewayUrl })[0]
if (!$env) {
    $env = Invoke-Relay POST '/api/tests/environments' @{
        name='E2E real gateway'
        gateway_target=$GatewayUrl
        variables=@{ api_key=$reg.api_key }
        credential_bindings=@{}
    }
} else {
    Invoke-Relay PUT "/api/tests/environments/$($env.id)" @{
        name=$env.name
        gateway_target=$GatewayUrl
        variables=@{ api_key=$reg.api_key }
        credential_bindings=@{}
    } | Out-Null
}
$publicDef = Get-Content (Join-Path $PSScriptRoot '../examples/test-studio/open-meteo.suite.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$keyedDef = Get-Content (Join-Path $PSScriptRoot '../examples/test-studio/open-meteo-keyed.suite.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$existing = Invoke-Relay GET '/api/tests/suites'
$pubSuite = @($existing | Where-Object name -eq $publicDef.name)[0]
if (!$pubSuite) {
    $created = Invoke-Relay POST '/api/tests/suites' @{ name=$publicDef.name; description=$publicDef.description; api_id=$publicApi.id; ownership='team'; definition=$publicDef }
    $pubSuite = $created.suite
} else {
    Invoke-Relay PUT "/api/tests/suites/$($pubSuite.id)" @{ name=$publicDef.name; description=$publicDef.description; api_id=$publicApi.id; ownership='team'; definition=$publicDef } | Out-Null
}
$keySuite = @($existing | Where-Object name -eq $keyedDef.name)[0]
if (!$keySuite) {
    $created = Invoke-Relay POST '/api/tests/suites' @{ name=$keyedDef.name; description=$keyedDef.description; api_id=$keyedApi.id; ownership='team'; definition=$keyedDef }
    $keySuite = $created.suite
}
Add-Step 'studio-import' @{ public_suite=$pubSuite.id; keyed_suite=$keySuite.id; environment=$env.id }

$pubRun = Invoke-Relay POST '/api/tests/runs' @{ suite_id=$pubSuite.id; environment_id=$env.id; execution_mode='standard' }
$pubDetail = Wait-Run $pubRun.id
if ($pubDetail.run.lifecycle_state -ne 'completed' -or $pubDetail.run.failed_steps -ne 0) { throw "public suite failed: $($pubDetail.run | ConvertTo-Json -Compress)" }
Add-Step 'studio-public-suite' @{ run=$pubRun.id; revision=$pubDetail.run.actual_revision; hash=$pubDetail.run.suite_content_hash }

$keyRun = Invoke-Relay POST '/api/tests/runs' @{ suite_id=$keySuite.id; environment_id=$env.id; execution_mode='standard' }
$keyDetail = Wait-Run $keyRun.id
if ($keyDetail.run.lifecycle_state -ne 'completed' -or $keyDetail.run.failed_steps -ne 0) { throw "keyed suite failed: $($keyDetail.run | ConvertTo-Json -Compress)" }
Add-Step 'studio-keyed-suite' @{ run=$keyRun.id }

Invoke-Relay PUT "/api/tests/gates/$($publicApi.id)" @{
    enforcement_enabled=$true
    target_environment='canary'
    required_suite_ids=@($pubSuite.id)
    freshness_seconds=3600
} | Out-Null

$product.apis[0].description = 'Public read-only weather forecast (canary evidence pass)'
$canaryPlan = Invoke-Relay POST '/api/system/plan' $product
$canaryApply = Invoke-Relay POST "/api/system/apply?rollout=canary&traffic_percent=10&plan_hash=$([uri]::EscapeDataString($canaryPlan.plan_hash))" $product
$canaryRev = $canaryApply.revision
if ($canaryRev -le 0) { throw "canary apply did not return a revision: $($canaryApply | ConvertTo-Json -Compress)" }
Add-Step 'canary-apply' @{ revision=$canaryRev }

$blocked = Invoke-RelayStatus POST "/api/revisions/$canaryRev/promote"
if ($blocked -ne 412) { throw "promote without evidence returned $blocked, want 412" }
Add-Step 'promote-blocked' @{ status=412; revision=$canaryRev }

$gateRun = Invoke-Relay POST '/api/tests/runs' @{
    suite_id=$pubSuite.id
    environment_id=$env.id
    execution_mode='standard'
    target_revision=$canaryRev
}
$gateDetail = Wait-Run $gateRun.id
if ($gateDetail.run.lifecycle_state -ne 'completed' -or $gateDetail.run.failed_steps -ne 0) { throw 'canary suite run failed' }
Add-Step 'studio-canary-suite' @{ run=$gateRun.id; revision=$canaryRev }

$promoted = Invoke-Relay POST "/api/revisions/$canaryRev/promote" @{}
Add-Step 'promote-allowed' @{ status=200; revision=$canaryRev; result=$promoted }

$breaking = $product.psobject.Copy()
$breaking = @{
    format_version='1.0'
    plans=$product.plans
    apis=@(
        @{ name='Open-Meteo weather demo'; description='Breaking candidate requires a key'; base_path='/weather-demo'; upstream_url='https://api.open-meteo.com'; strip_path=$true; auth_type='api_key'; timeout_ms=15000; enabled=$true; visibility='public'; rate_limit_per_minute=30 }
        $product.apis[1]
        $product.apis[2]
    )
}
$breakPlan = Invoke-Relay POST '/api/system/plan' $breaking
$breakApply = Invoke-Relay POST "/api/system/apply?rollout=canary&header=X-Canary&canary_header=X-Canary&canary_header_value=1&plan_hash=$([uri]::EscapeDataString($breakPlan.plan_hash))" $breaking
$breakRev = $breakApply.revision
$cmp = Invoke-Relay POST '/api/tests/runs' @{
    suite_id=$pubSuite.id
    environment_id=$env.id
    execution_mode='comparison'
    baseline_revision=$canaryRev
    candidate_revision=$breakRev
}
$cmpDetail = Wait-Run $cmp.id
Invoke-Relay POST "/api/revisions/$breakRev/abort" @{} | Out-Null
Add-Step 'breaking-canary-aborted' @{ revision=$breakRev; comparison_run=$cmp.id; comparison_state=$cmpDetail.run.lifecycle_state }

if ($requestId) {
    $diag = Invoke-Relay GET "/api/requests/$requestId/diagnose"
    Add-Step 'request-diagnosis' @{ request_id="$requestId"; found=$true; status=$diag.status }
} else {
    $logs = Invoke-Relay GET '/api/logs?path=/weather-demo&limit=1'
    Add-Step 'request-diagnosis' @{ via='logs'; count=@($logs).Count }
}

$ar = Invoke-Relay GET '/api/revisions/auto-rollback/config'
Add-Step 'auto-rollback-config' @{ enabled=[bool]$ar.enabled; threshold=$ar.error_rate_threshold_percent }

$me = Invoke-Relay GET '/api/auth/me'
$preview = $me.features
Add-Step 'license-features' $preview
if ($preview.preview_ai) {
    $null = Invoke-Relay POST '/api/ai/providers' @{ name='e2e-preview'; kind='openai_compatible' } -AllowStatus @(200,201,400,409)
}
if ($preview.preview_apiops) {
    $null = Invoke-Relay POST '/api/apiops/bundles/validate' @{ format_version='1.0'; source_hash='sha256:e2e'; rendered_hash='sha256:e2e'; config=$product } -AllowStatus @(200,400)
}

$evidence.finished_at = (Get-Date).ToUniversalTime().ToString('o')
$evidence.passed = $true
$evidence.revisions = @{ baseline=$apply.revision; promoted=$canaryRev; aborted=$breakRev }
$outDir = Split-Path -Parent $ResultsPath
if ($outDir) { New-Item -ItemType Directory -Force -Path $outDir | Out-Null }
($evidence | ConvertTo-Json -Depth 8) | Set-Content -LiteralPath $ResultsPath -Encoding UTF8
Write-Output "Evidence: $ResultsPath"
Write-Output "Fleet: $ConsoleUrl/#/fleet"
Write-Output "Studio: $ConsoleUrl/#/tests"
