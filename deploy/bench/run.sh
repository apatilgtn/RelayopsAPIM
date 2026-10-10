#!/usr/bin/env bash
# Same-host comparison of RelayOps, Kong and Tyk with identical policies.
#   deploy/bench/run.sh                       # all gateways, both scenarios
#   GATEWAYS="relayops kong" deploy/bench/run.sh
#   RATES=1000,2000,4000 STEP=10s deploy/bench/run.sh
# Results: deploy/bench/results/<gateway>-<scenario>.json, summary in
# deploy/bench/results/summary.md.
set -euo pipefail
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: keep container paths as written
cd "$(dirname "$0")"

GATEWAYS=${GATEWAYS:-"relayops relayops-sampled kong tyk"}
RATES=${RATES:-1000,2000,4000,8000,12000,16000}
STEP=${STEP:-15s}
CONC=${CONC:-64,256}
KONG_KEY=bench-key-0123456789abcdef
DC="docker compose"

echo "== building tools image"
mkdir -p .build results
for c in relayops mockupstream bench; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags=-s -o ".build/$c" "../../cmd/$c"
done
$DC build -q upstream
$DC up -d redis postgres upstream

# tool runs a command in the tools container on the bench network.
tool() { $DC run --rm -T --entrypoint "$1" bench "${@:2}"; }

wait_for() {
  for _ in $(seq 1 60); do
    if tool wget -q -O /dev/null "$1" 2>/dev/null; then return 0; fi
    sleep 1
  done
  echo "timed out waiting for $1" >&2
  return 1
}

post() { # url json [header...]
  local url=$1 body=$2; shift 2
  local args=(-q -O - --header "Content-Type: application/json" --post-data "$body")
  for h in "$@"; do args+=(--header "$h"); done
  tool wget "${args[@]}" "$url"
}

provision_relayops() {
  local admin=http://relayops:9090 auth="Authorization: Bearer bench-admin-token"
  wait_for "$admin/healthz"
  post "$admin/api/apis" '{"name":"open","base_path":"/open","upstream_url":"http://upstream:7070","strip_path":true}' "$auth" >/dev/null
  local api plan consumer
  api=$(post "$admin/api/apis" '{"name":"key","base_path":"/key","upstream_url":"http://upstream:7070","strip_path":true,"auth_type":"api_key"}' "$auth" | sed -E 's/.*"id":"([^"]+)".*/\1/' | head -c 36)
  plan=$(post "$admin/api/plans" '{"name":"bench","rate_limit_per_minute":100000000}' "$auth" | sed -E 's/.*"id":"([^"]+)".*/\1/' | head -c 36)
  consumer=$(post "$admin/api/consumers" '{"name":"bench","email":"bench@example.com"}' "$auth" | sed -E 's/.*"id":"([^"]+)".*/\1/' | head -c 36)
  KEY=$(post "$admin/api/consumers/$consumer/keys" '{"name":"bench"}' "$auth" | sed -E 's/.*"key":"([^"]+)".*/\1/')
  post "$admin/api/subscriptions" "{\"consumer_id\":\"$consumer\",\"api_id\":\"$api\",\"plan_id\":\"$plan\"}" "$auth" >/dev/null
  sleep 3 # config propagation
}

provision_tyk() {
  wait_for "http://tyk:8080/hello"
  KEY=$(post "http://tyk:8080/tyk/keys/create" \
    '{"rate":100000000,"per":60,"quota_max":-1,"org_id":"default","access_rights":{"key":{"api_id":"key","api_name":"key","versions":["Default"]}}}' \
    "x-tyk-authorization: bench-tyk-secret" | sed -E 's/.*"key":"([^"]+)".*/\1/')
}

provision_kong() {
  wait_for "http://kong:8080/open/get"
  KEY=$KONG_KEY
}

# verify_auth fails the run unless the key route refuses requests without a
# key and accepts the provisioned one (so no gateway skips the policy).
verify_auth() { # gateway-host key
  local host=$1 key=$2 code
  code=$(tool sh -c "wget -S -O /dev/null http://$host:8080/key/get 2>&1 | awk '/^ *HTTP\//{print \$2}' | tail -1")
  case "$code" in 401|403) ;; *) echo "$host: /key without a key returned '$code', want 401/403" >&2; exit 1 ;; esac
  code=$(tool sh -c "wget -S -O /dev/null --header 'X-API-Key: $key' http://$host:8080/key/get 2>&1 | awk '/^ *HTTP\//{print \$2}' | tail -1")
  [ "$code" = 200 ] || { echo "$host: /key with the key returned '$code', want 200" >&2; exit 1; }
  echo "   $host: key route refuses anonymous calls and accepts the key"
}

bench() { # gateway-host scenario label key
  local host=$1 scenario=$2 label=$3 key=${4:-}
  local args=(-url "http://$host:8080/$scenario/get" -rates "$RATES" -concurrency "$CONC" -step "$STEP" -label "$label" -out "/results/$label")
  [ -n "$key" ] && args+=(-key "$key")
  echo "== $label"
  $DC run --rm -T bench "${args[@]}"
}

for gw in $GATEWAYS; do
  svc=$gw
  export RELAYOPS_LOG_SAMPLE_RATE=1
  if [ "$gw" = relayops-sampled ]; then svc=relayops; export RELAYOPS_LOG_SAMPLE_RATE=0.05; fi
  if [ "$svc" = relayops ]; then # every RelayOps run starts from an empty database
    $DC rm -sf postgres >/dev/null && $DC up -d postgres >/dev/null && sleep 4
  fi
  $DC --profile gateway up -d --force-recreate "$svc"
  KEY=""
  "provision_${svc}"
  verify_auth "$svc" "$KEY"
  bench "$svc" open "$gw-open"
  bench "$svc" key "$gw-key" "$KEY"
  $DC --profile gateway rm -sf "$svc" >/dev/null
done

python3 summarize.py results > results/summary.md 2>/dev/null || python summarize.py results > results/summary.md
cat results/summary.md
$DC --profile gateway --profile tools down >/dev/null
