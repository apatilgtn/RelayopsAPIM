#!/usr/bin/env bash
# Database-only restore drill. Secret recovery and complete DR remain unproven.
#
# Run on the RelayOps host as:  ./relayops-secrets run -- bash restore-drill.sh
# (the backup storage credentials come from Vault, as for the backup itself).
#
# It downloads the newest database dump, restores it into a throwaway
# PostgreSQL container on an isolated Docker network, compares row counts with
# production (read-only), boots a separate RelayOps control plane on the
# restored data (bound to localhost only), checks it serves the restored APIs,
# and removes everything. Production is only read, never written.
set -euo pipefail
umask 077
: "${RELAYOPS_DATABASE_URL:?}" "${SUPABASE_S3_ENDPOINT:?}" "${SUPABASE_S3_ACCESS_KEY_ID:?}"
: "${SUPABASE_S3_SECRET_ACCESS_KEY:?}" "${SUPABASE_STORAGE_BUCKET:?}"

work=$(mktemp -d)
net=relayops-drill-$$
pg=relayops-drill-pg-$$
app=relayops-drill-app-$$
cleanup() {
  docker rm -f "$app" "$pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf -- "$work"
}
trap cleanup EXIT
started=$(date +%s)
s3() {
  AWS_ACCESS_KEY_ID="$SUPABASE_S3_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$SUPABASE_S3_SECRET_ACCESS_KEY" \
  AWS_SESSION_TOKEN='' AWS_DEFAULT_REGION=ap-southeast-2 \
  AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required \
  aws --endpoint-url "$SUPABASE_S3_ENDPOINT" "$@"
}

echo "== 1. latest backup"
key=$(s3 s3 ls "s3://$SUPABASE_STORAGE_BUCKET/relayops/" --recursive | awk '{print $4}' \
  | grep -E '\.dump$' | grep -v 'vault-encrypted' | sort | tail -1)
[ -n "$key" ] || { echo "no backup found"; exit 1; }
echo "backup: $key"
s3 s3 cp "s3://$SUPABASE_STORAGE_BUCKET/$key" "$work/relayops.dump" --only-show-errors
s3 s3 cp "s3://$SUPABASE_STORAGE_BUCKET/$key.vault-encrypted.dump" "$work/vault.dump" --only-show-errors || echo "(no vault ciphertext file)"
ls -l "$work" | awk 'NR>1 {print "  " $5 " bytes  " $9}'

echo "== 2. restore into an isolated PostgreSQL"
docker network create --internal "$net" >/dev/null
docker run -d --name "$pg" --network "$net" -e POSTGRES_PASSWORD=drill -e POSTGRES_DB=relayops postgres:17 >/dev/null
# The image starts a temporary server for first-time setup, stops it, then
# starts the real one: wait for the second "ready" message.
for _ in $(seq 1 90); do
  [ "$(docker logs "$pg" 2>&1 | grep -c 'ready to accept connections')" -ge 2 ] && break
  sleep 1
done
docker exec "$pg" psql -U postgres -d relayops -tAc 'SELECT 1' >/dev/null
docker cp "$work/relayops.dump" "$pg:/tmp/relayops.dump"
restore_start=$(date +%s)
set +e
docker exec "$pg" pg_restore -U postgres -d relayops --no-owner --no-acl /tmp/relayops.dump > "$work/restore.log" 2>&1
rc=$?
set -e
echo "pg_restore exit $rc in $(( $(date +%s) - restore_start ))s; $(grep -c 'error:' "$work/restore.log" || true) error line(s)"
grep 'error:' "$work/restore.log" | sed -E 's/^.*error: //' | sort | uniq -c | head -10 || true

echo "== 3. row counts: restored vs production now"
tables="tenants admin_users apis plans consumers api_keys subscriptions config_revisions audit_logs request_logs mcp_catalog test_suites apiops_environments"
printf '%-20s %10s %10s\n' table restored production
for t in $tables; do
  r=$(docker exec "$pg" psql -U postgres -d relayops -tAc "SELECT count(*) FROM public.$t" 2>/dev/null || echo "-")
  p=$(docker run --rm --env PGDATABASE="$RELAYOPS_DATABASE_URL" postgres:17 \
    bash -c 'exec psql --dbname="$PGDATABASE" -tAc "$1"' -- "SELECT count(*) FROM public.$t" 2>/dev/null || echo "-")
  printf '%-20s %10s %10s\n' "$t" "$r" "$p"
done
restored_apis=$(docker exec "$pg" psql -U postgres -d relayops -tAc 'SELECT count(*) FROM public.apis' 2>/dev/null || echo 0)
if [ "${restored_apis:-0}" -lt 1 ]; then
  echo "DRILL FAILED: the restored database has no APIs"; exit 1
fi
echo "latest revision restored: $(docker exec "$pg" psql -U postgres -d relayops -tAc 'SELECT max(revision) FROM config_revisions')"
echo "migration version restored: $(docker exec "$pg" psql -U postgres -d relayops -tAc 'SELECT max(version) FROM schema_migrations' 2>/dev/null || echo '?')"
if [ -s "$work/vault.dump" ]; then
  echo "vault ciphertext dump: $(docker exec -i "$pg" pg_restore --list < "$work/vault.dump" 2>/dev/null | grep -c 'TABLE DATA') table(s) of encrypted secrets (decryptable only with the source project's Vault key)"
fi

echo "== 4. boot RelayOps on the restored data"
drill_token=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
docker run -d --name "$app" --network "$net" \
  -e RELAYOPS_DATABASE_URL="postgres://postgres:drill@$pg:5432/relayops?sslmode=disable" \
  -e RELAYOPS_ADMIN_TOKEN="$drill_token" -e RELAYOPS_ROLE=control-plane -e RELAYOPS_REDIS_URL=disabled \
  -e RELAYOPS_VAULT_ENABLED=false -e RELAYOPS_NODE_ID=restore-drill \
  relayops:release >/dev/null
ok=""
for _ in $(seq 1 60); do
  if docker exec "$pg" sh -c "true" && docker run --rm --network "$net" curlimages/curl:8.10.1 -sf "http://$app:9090/healthz" >"$work/health.json" 2>/dev/null; then ok=1; break; fi
  sleep 2
done
if [ -z "$ok" ]; then echo "drill instance did not become healthy"; docker logs --tail 30 "$app" 2>&1 | sed -E 's/(token|password)=[^ ]+/\1=***/Ig'; exit 1; fi
echo "healthz: $(cat "$work/health.json")"
apis=$(docker run --rm --network "$net" curlimages/curl:8.10.1 -sf -H "Authorization: Bearer $drill_token" "http://$app:9090/api/apis" | grep -o '"base_path"' | wc -l)
echo "APIs served by the restored control plane: $apis (restored database: $restored_apis)"
if [ "$apis" -ne "$restored_apis" ]; then
  echo "DRILL FAILED: the restored control plane does not serve the restored APIs"; exit 1
fi
echo "DRILL PASSED"
echo "== drill finished in $(( $(date +%s) - started ))s (download, restore, verify, boot)"

printf "\nDATABASE-ONLY RESULT: complete recovery requires restored secrets and authenticated upstream access.\n"
