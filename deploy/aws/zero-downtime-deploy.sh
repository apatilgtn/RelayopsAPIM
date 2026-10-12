#!/usr/bin/env bash
# Replace the RelayOps binaries without an outage (single host).
#
#   zero-downtime-deploy.sh BUCKET ARCHIVE SHA256
#
# 1. Fetch and verify the release, build the new image (the running node is
#    untouched).
# 2. Start a standby node on the new version; abort if it is not healthy.
#    Caddy (Caddyfile, lb_policy first) now has two healthy backends.
# 3. Recreate the primary on the new version. While it restarts, Caddy
#    retries requests on the standby. Wait until the primary is healthy.
# 4. Stop the standby. Traffic is back on the primary alone.
#
# On any failure before step 3 the primary keeps running the old version and
# the previous binaries are restored on disk. Keeps .env, compose and Caddy.
set -euo pipefail
umask 077
bucket=$1; archive=$2; sha=$3
cd /opt/relayops
dc() { docker compose "$@"; }
standby() { docker compose --profile standby "$@"; }

if grep -qiE '^RELAYOPS_ENV=production' .env; then
  echo "ABORT: .env sets RELAYOPS_ENV=production; this single-host layout fails the production preflight"; exit 1
fi

healthy() { # container port path
  for _ in $(seq 1 40); do
    if docker run --rm --network "container:$1" curlimages/curl:8.10.1 -sf "http://127.0.0.1:$2$3" >/dev/null 2>&1; then return 0; fi
    sleep 3
  done
  return 1
}
restore_previous() {
  echo "restoring previous binaries"
  cp -p previous/relayops previous/relayopsctl previous/mockupstream previous/relayops-secrets .
  docker build -q -t relayops:release . >/dev/null
}

echo "== 1. fetch $archive"
aws s3 cp "s3://$bucket/$archive" /opt/relayops/bin-update.tar.gz --region ap-southeast-2 --only-show-errors
printf '%s  %s\n' "$sha" /opt/relayops/bin-update.tar.gz | sha256sum --check -
mkdir -p previous
cp -p relayops relayopsctl mockupstream relayops-secrets previous/
tar -xzf bin-update.tar.gz -C /opt/relayops
chmod 0755 relayops relayopsctl mockupstream relayops-secrets
rm -f bin-update.tar.gz
docker build -q -t relayops:release . >/dev/null

echo "== 2. standby on the new version"
standby rm -sf relayops-standby >/dev/null 2>&1 || true
standby up -d --no-deps relayops-standby
if ! healthy relayops-relayops-standby-1 9090 /healthz || ! healthy relayops-relayops-standby-1 8080 /__relayops/health; then
  echo "ABORT: the new version is not healthy; the primary was not touched"
  standby logs --no-color --tail 30 relayops-standby | sed -E 's/(token|password|key)=[^ ]+/\1=***/Ig'
  standby rm -sf relayops-standby >/dev/null 2>&1 || true
  restore_previous
  exit 1
fi
sleep 5 # let Caddy's active health check see the standby

echo "== 3. primary"
dc up -d --no-deps --force-recreate relayops
if ! healthy relayops-relayops-1 9090 /healthz || ! healthy relayops-relayops-1 8080 /__relayops/health; then
  echo "PRIMARY UNHEALTHY on the new version; the standby keeps serving. Investigate before stopping it."
  dc logs --no-color --tail 30 relayops | sed -E 's/(token|password|key)=[^ ]+/\1=***/Ig'
  exit 1
fi
sleep 5 # let Caddy see the primary healthy again before the standby goes

echo "== 4. stop the standby"
standby stop relayops-standby >/dev/null
standby rm -f relayops-standby >/dev/null

echo "== result"
dc ps --format '{{.Service}} {{.Status}}'
dc logs --no-color --tail 60 relayops | grep -E 'database ready|APIM started|migrat|ERROR|fatal' | tail -6
