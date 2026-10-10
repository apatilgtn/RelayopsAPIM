# Builds every plugin in plugins/wasm into plugins/wasm/dist/<name>.wasm.
#   pwsh plugins/wasm/build.ps1                 # all plugins
#   pwsh plugins/wasm/build.ps1 pii-redactor
param([string[]]$Names)
$ErrorActionPreference = 'Stop'
$repo = Resolve-Path "$PSScriptRoot\..\.."
$out = Join-Path $PSScriptRoot 'dist'
New-Item -ItemType Directory -Force $out | Out-Null
if (-not $Names) { $Names = Get-ChildItem $PSScriptRoot -Directory | Where-Object { $_.Name -notin 'dist', 'internal' } | ForEach-Object Name }
$saved = $env:GOOS, $env:GOARCH
try {
  $env:GOOS = 'wasip1'; $env:GOARCH = 'wasm'
  Push-Location $repo
  foreach ($n in $Names) {
    go build -buildmode=c-shared -trimpath -ldflags=-s -o "$out\$n.wasm" "./plugins/wasm/$n"
    if ($LASTEXITCODE) { throw "build $n failed" }
    "built $out\$n.wasm"
  }
} finally {
  Pop-Location
  $env:GOOS, $env:GOARCH = $saved
}
