# Capacity benchmark: upstream baseline, combined mode and split mode on one host.
#
# Builds relayops, mockupstream and bench into -WorkDir, creates a fresh database,
# and runs each scenario with cmd/bench. Results: <WorkDir>\results\*.json.
#
#   pwsh scripts/bench-capacity.ps1 -WorkDir C:\tmp\relayops-bench
#
# Uses its own ports, database and Redis server (tools\redis) so it does not touch a running instance.
param(
  [Parameter(Mandatory = $true)][string]$WorkDir,
  [string]$PgAdminUrl = 'postgres://postgres@localhost:5432/postgres?sslmode=disable',
  [string]$Database = 'relayops_bench',
  [int]$RedisPort = 6390,
  [int]$ProxyPort = 18080,
  [int]$AdminPort = 19090,
  [int]$EdgePort = 18081,
  [int]$EdgeStatusPort = 19092,
  [int]$UpstreamPort = 27070,
  [string]$Rates = '1000,2000,4000,6000,8000,12000,16000',
  [string]$Concurrency = '64,256',
  [string]$Step = '15s',
  [string[]]$Only = @()
)
$ErrorActionPreference = 'Stop'
# -File passes '-Only a,b' as one string.
$Only = @($Only | ForEach-Object { $_ -split ',' } | Where-Object { $_ })
$repo = Split-Path $PSScriptRoot -Parent
New-Item -ItemType Directory -Force "$WorkDir\results", "$WorkDir\cp", "$WorkDir\edge" | Out-Null
$token = 'bench-admin-' + [guid]::NewGuid().ToString('N')
$dpToken = 'bench-dataplane-' + [guid]::NewGuid().ToString('N')
$dbUrl = $PgAdminUrl -replace '/postgres\?', "/$Database`?"
$RedisUrl = "redis://127.0.0.1:$RedisPort/0"
$procs = @()

function Build {
  Push-Location $repo
  try {
    foreach ($c in 'relayops', 'mockupstream', 'bench') { go build -o "$WorkDir\$c.exe" "./cmd/$c"; if ($LASTEXITCODE) { throw "build $c failed" } }
  } finally { Pop-Location }
}

function Start-Node([string]$name, [string]$dir, [hashtable]$envs) {
  $saved = @{}
  foreach ($k in $envs.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $envs[$k]) }
  try {
    $p = Start-Process "$WorkDir\relayops.exe" -WorkingDirectory $dir -PassThru -WindowStyle Hidden `
      -RedirectStandardOutput "$WorkDir\$name.out.log" -RedirectStandardError "$WorkDir\$name.err.log"
  } finally {
    foreach ($k in $envs.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
  }
  $script:procs += $p
  return $p
}

function Wait-Url([string]$url) {
  for ($i = 0; $i -lt 60; $i++) {
    try { if ((Invoke-WebRequest $url -UseBasicParsing -TimeoutSec 2).StatusCode -lt 500) { return } } catch { Start-Sleep -Milliseconds 500 }
  }
  throw "$url did not come up"
}

function Stop-Nodes { foreach ($p in $script:procs) { if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force } }; $script:procs = @(); Start-Sleep 2 }

function Api($method, $path, $body) {
  $p = @{ Method = $method; Uri = "http://127.0.0.1:$AdminPort$path"; Headers = @{ Authorization = "Bearer $token" }; ContentType = 'application/json' }
  if ($null -ne $body) { $p.Body = $body | ConvertTo-Json -Depth 8 }
  Invoke-RestMethod @p
}

function Bench([string]$label, [string]$url, [string]$key, [string]$note) {
  if ($Only.Count -and $Only -notcontains $label) { return }
  Write-Host "== $label" -ForegroundColor Cyan
  $benchArgs = @('-url', $url, '-rates', $Rates, '-concurrency', $Concurrency, '-step', $Step, '-label', $label, '-note', $note, '-out', "$WorkDir\results\$label")
  if ($key) { $benchArgs += @('-key', $key) }
  & "$WorkDir\bench.exe" @benchArgs
}

function Common-Env {
  @{ RELAYOPS_DATABASE_URL = $dbUrl; RELAYOPS_REDIS_URL = $RedisUrl; RELAYOPS_ADMIN_TOKEN = $token
     RELAYOPS_ADMIN_ADDR = ":$AdminPort"; RELAYOPS_PROXY_ADDR = ":$ProxyPort"; RELAYOPS_NODE_ID = 'bench-cp'; RELAYOPS_DATAPLANE_TOKEN = $dpToken }
}

try {
  Build
  psql -q -d $PgAdminUrl -c "DROP DATABASE IF EXISTS $Database" -c "CREATE DATABASE $Database" | Out-Null
  # A private Redis (no persistence) for the distributed rate limiter.
  $script:redis = Start-Process "$repo\tools\redis\redis-server.exe" -ArgumentList '--port', $RedisPort, '--save', '""', '--appendonly', 'no' -PassThru -WindowStyle Hidden
  $script:mock = Start-Process "$WorkDir\mockupstream.exe" -ArgumentList '-addr', ":$UpstreamPort", '-jitter-ms', '0' -PassThru -WindowStyle Hidden
  Wait-Url "http://127.0.0.1:$UpstreamPort/"

  Bench 'upstream-direct' "http://127.0.0.1:$UpstreamPort/get" '' 'mock upstream without the gateway (load-generator and upstream ceiling)'

  # Combined mode: one process with proxy + control plane.
  $envAll = Common-Env
  Start-Node 'combined' "$WorkDir\cp" $envAll | Out-Null
  Wait-Url "http://127.0.0.1:$AdminPort/healthz"
  $open = Api POST '/api/apis' @{ name = 'bench-open'; base_path = '/bench-open'; upstream_url = "http://127.0.0.1:$UpstreamPort"; strip_path = $true }
  $sec = Api POST '/api/apis' @{ name = 'bench-key'; base_path = '/bench-key'; upstream_url = "http://127.0.0.1:$UpstreamPort"; strip_path = $true; auth_type = 'api_key' }
  $plan = Api POST '/api/plans' @{ name = 'bench-unlimited'; description = 'benchmark'; rate_limit_per_minute = 100000000 }
  $consumer = Api POST '/api/consumers' @{ name = 'bench'; email = 'bench@relayops.local' }
  $key = (Api POST "/api/consumers/$($consumer.id)/keys" @{ name = 'bench' }).key
  $sub = Api POST '/api/subscriptions' @{ consumer_id = $consumer.id; api_id = $sec.id; plan_id = $plan.id }
  if ($sub.status -ne 'approved') { Api POST "/api/subscriptions/$($sub.id)/approve" @{} | Out-Null }
  Start-Sleep 3
  Bench 'combined-open' "http://127.0.0.1:$ProxyPort/bench-open/get" '' 'combined mode, no auth, every request logged'
  Bench 'combined-apikey' "http://127.0.0.1:$ProxyPort/bench-key/get" $key 'combined mode, API key + subscription + Redis rate limit, every request logged'
  Stop-Nodes

  $envSampled = Common-Env; $envSampled.RELAYOPS_LOG_SAMPLE_RATE = '0.05'
  Start-Node 'combined-sampled' "$WorkDir\cp" $envSampled | Out-Null
  Wait-Url "http://127.0.0.1:$AdminPort/healthz"; Start-Sleep 3
  Bench 'combined-apikey-sampled' "http://127.0.0.1:$ProxyPort/bench-key/get" $key 'combined mode, API key + Redis rate limit, 5% of successes logged'
  Stop-Nodes

  # Split mode: control plane without a proxy, plus a gateway-only node with no database.
  $envCP = Common-Env; $envCP.RELAYOPS_ROLE = 'control-plane'; $envCP.RELAYOPS_STATUS_ADDR = ':19093'
  Start-Node 'control-plane' "$WorkDir\cp" $envCP | Out-Null
  Wait-Url "http://127.0.0.1:$AdminPort/healthz"
  Start-Node 'gateway' "$WorkDir\edge" @{
    RELAYOPS_ROLE = 'gateway'; RELAYOPS_CONTROL_PLANE_URL = "http://127.0.0.1:$AdminPort"; RELAYOPS_DATAPLANE_TOKEN = $dpToken
    RELAYOPS_DATAPLANE_ALLOW_HTTP = 'true'; RELAYOPS_PROXY_ADDR = ":$EdgePort"; RELAYOPS_STATUS_ADDR = ":$EdgeStatusPort"
    RELAYOPS_NODE_ID = 'bench-edge'; RELAYOPS_REDIS_URL = $RedisUrl; RELAYOPS_LOG_SAMPLE_RATE = '0.05'; RELAYOPS_DATABASE_URL = 'postgres://gateway-has-no-database.invalid/none'
  } | Out-Null
  Wait-Url "http://127.0.0.1:$EdgeStatusPort/readyz"; Start-Sleep 3
  Bench 'split-gateway-apikey-sampled' "http://127.0.0.1:$EdgePort/bench-key/get" $key 'gateway-only node (no DB), API key + Redis rate limit, 5% logged via node API'
} finally {
  Stop-Nodes
  foreach ($p in $script:mock, $script:redis) { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force } }
  psql -q -d $PgAdminUrl -c "DROP DATABASE IF EXISTS $Database" 2>$null | Out-Null
}
