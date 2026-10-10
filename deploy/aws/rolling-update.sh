#!/usr/bin/env bash
# Zero-downtime update for the HA layout (compose.ha.yaml): rebuild the
# image, update the control plane (gateways keep serving their cached
# config meanwhile), then replace the gateways one at a time, waiting until
# each is ready before touching the next.
set -euo pipefail
cd "$(dirname "$0")"
dc() { docker compose -f compose.yaml -f compose.ha.yaml "$@"; }

ready() { # service
  for _ in $(seq 1 60); do
    if docker run --rm --network relayops_default curlimages/curl:8.10.1 -fsS "http://$1:9091/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "$1 did not become ready" >&2
  return 1
}

docker build -q -t relayops:release . >/dev/null
docker tag relayops:release relayops:local

echo "== control plane"
dc up -d --no-deps relayops
for _ in $(seq 1 60); do
  if docker run --rm --network relayops_default curlimages/curl:8.10.1 -fsS http://relayops:9090/healthz >/dev/null 2>&1; then break; fi
  sleep 2
done

for gw in gateway-a gateway-b; do
  echo "== $gw"
  dc up -d --no-deps --force-recreate "$gw"
  ready "$gw"
done
dc ps --format '{{.Service}} {{.Status}}'
