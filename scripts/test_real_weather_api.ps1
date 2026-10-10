<#
Creates a reusable example in an isolated LOCAL RelayOps instance and executes
three read-only requests through its gateway to the real Open-Meteo API.
No provider API key is required. The administrator token is never written to disk.
#>
param(
    [string]$ConsoleUrl = 'http://127.0.0.1:9199',
    [string]$GatewayUrl = 'http://127.0.0.1:8189',
    [Parameter(Mandatory=$true)][string]$AdminToken,
    [string]$ResultsPath = (Join-Path $PSScriptRoot '../artifacts/real-weather-api-results.json')
)
$ErrorActionPreference = 'Stop'
foreach ($address in @($ConsoleUrl, $GatewayUrl)) {
    $uri = [Uri]$address
    if (!$uri.IsLoopback -or $uri.Scheme -notin @('http','https')) { throw 'Use an isolated local RelayOps instance for this example.' }
}
$ConsoleUrl = $ConsoleUrl.TrimEnd('/')
$GatewayUrl = $GatewayUrl.TrimEnd('/')
$headers = @{Authorization="Bearer $AdminToken"}
function Invoke-Relay([string]$Method, [string]$Path, $Body=$null) {
    $argsForRequest = @{Uri="$ConsoleUrl$Path";Method=$Method;Headers=$headers;TimeoutSec=30}
    if ($null -ne $Body) {
        $argsForRequest.ContentType='application/json'
        $argsForRequest.Body=($Body | ConvertTo-Json -Depth 30 -Compress)
    }
    Invoke-RestMethod @argsForRequest
}

$existingAPIs=Invoke-Relay GET '/api/apis'
$api = $existingAPIs | Where-Object name -eq 'Open-Meteo weather demo' | Select-Object -First 1
if (!$api) {
    $api=Invoke-Relay POST '/api/apis' @{
        name='Open-Meteo weather demo';description='Read-only weather API verification example';
        base_path='/weather-demo';upstream_url='https://api.open-meteo.com';strip_path=$true;
        auth_type='none';timeout_ms=15000;enabled=$true;rate_limit_per_minute=30
    }
}
if ($api.upstream_url -ne 'https://api.open-meteo.com' -or $api.base_path -ne '/weather-demo') { throw 'The existing example API has a different configuration; refusing to modify it.' }
$existingEnvironments=Invoke-Relay GET '/api/tests/environments'
$environment = $existingEnvironments | Where-Object { $_.name -eq 'Real API local gateway' -and $_.gateway_target -eq $GatewayUrl } | Select-Object -First 1
if (!$environment) { $environment=Invoke-Relay POST '/api/tests/environments' @{name='Real API local gateway';gateway_target=$GatewayUrl;variables=@{};credential_bindings=@{}} }
$definition=Get-Content (Join-Path $PSScriptRoot '../examples/test-studio/open-meteo.suite.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$existingSuites=Invoke-Relay GET '/api/tests/suites'
$suite = $existingSuites | Where-Object name -eq $definition.name | Select-Object -First 1
if (!$suite) {
    $created=Invoke-Relay POST '/api/tests/suites' @{name=$definition.name;description=$definition.description;api_id=$api.id;ownership='team';definition=$definition}
    $suite=$created.suite
    if (!$suite) { $suite=$created }
} else {
    if ($suite.api_id -ne $api.id) { throw 'The existing suite belongs to a different API.' }
    Invoke-Relay PUT "/api/tests/suites/$($suite.id)" @{name=$definition.name;description=$definition.description;api_id=$api.id;ownership='team';definition=$definition} | Out-Null
}
$run=Invoke-Relay POST '/api/tests/runs' @{suite_id=$suite.id;environment_id=$environment.id;execution_mode='standard'}
$deadline=(Get-Date).AddSeconds(90)
do {
    Start-Sleep -Milliseconds 500
    $detail=Invoke-Relay GET "/api/tests/runs/$($run.id)"
} while ($detail.run.lifecycle_state -in @('queued','running') -and (Get-Date) -lt $deadline)
$outputDirectory=Split-Path -Parent $ResultsPath
if ($outputDirectory) { New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null }
$detail | ConvertTo-Json -Depth 40 | Set-Content -LiteralPath $ResultsPath -Encoding UTF8
$summary=@($detail.steps | ForEach-Object {
    $assertions=@($_.assertion_results)
    $failed=@($assertions | Where-Object { !$_.passed }).Count
    [PSCustomObject]@{Test=$_.request_name;HTTP=$_.status_code;Assertions=$assertions.Count;Passed=($assertions.Count-$failed);Failed=$failed;Milliseconds=[Math]::Round($_.duration_ms,1);Revision=$_.observed_revision}
})
$summary | Format-Table -AutoSize
Write-Output "Studio run: $ConsoleUrl/#/tests/run-$($run.id)"
Write-Output "Evidence: $ResultsPath"
if ($detail.run.lifecycle_state -ne 'completed' -or $detail.run.failed_steps -ne 0 -or $detail.steps.Count -ne 3) { throw 'Real API verification did not pass; inspect the recorded assertions.' }
