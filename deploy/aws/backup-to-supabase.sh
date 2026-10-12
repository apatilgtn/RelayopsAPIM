#!/usr/bin/env bash
# Required: Docker, AWS CLI v2, a private bucket and Supabase-generated S3 credentials.
# This backs up RelayOps public/private objects and separate Vault ciphertext.
# Do not run for another application sharing public without narrowing the scope first.
set -euo pipefail
umask 077
: "${RELAYOPS_DATABASE_URL:?Set the PostgreSQL connection URL}"
: "${SUPABASE_S3_ENDPOINT:?Copy the Storage S3 endpoint from Supabase}"
: "${SUPABASE_S3_ACCESS_KEY_ID:?Set server-side Storage S3 access key}"
: "${SUPABASE_S3_SECRET_ACCESS_KEY:?Set server-side Storage S3 secret key}"
: "${SUPABASE_STORAGE_BUCKET:?Set the PRIVATE backup bucket name}"
: "${SUPABASE_DOCUMENTS_BUCKET:?Set the PRIVATE API document bucket name}"
backup_dir=$(mktemp -d)
trap 'rm -f -- "$backup_dir/relayops.dump" "$backup_dir/vault-encrypted.dump" "$backup_dir/api-documents.json"; rmdir -- "$backup_dir"' EXIT
# Pass the connection URL into the container through its environment.
export PGDATABASE="$RELAYOPS_DATABASE_URL"
docker run --rm --env PGDATABASE postgres:17 bash -c 'exec pg_dump --dbname="$PGDATABASE" "$@"' -- \
  --format=custom --schema=public --schema=relayops_private --no-owner --no-acl > "$backup_dir/relayops.dump"
test -s "$backup_dir/relayops.dump"
# Keep ciphertext backup separate. Restoration requires the matching Vault
# encryption context; do not assume this decrypts in another Supabase project.
docker run --rm --env PGDATABASE postgres:17 bash -c 'exec pg_dump --dbname="$PGDATABASE" "$@"' -- \
  --format=custom --data-only --table=vault.secrets --no-owner --no-acl > "$backup_dir/vault-encrypted.dump"
docker run --rm --env PGDATABASE postgres:17 bash -c 'exec psql --dbname="$PGDATABASE" "$@"' -- -X -tA --set=ON_ERROR_STOP=1 \
  --command="SELECT coalesce(jsonb_agg(jsonb_build_object('api_id', id, 'tenant_id', tenant_id, 'name', name, 'openapi_spec', openapi_spec)), '[]'::jsonb) FROM apis WHERE openapi_spec IS NOT NULL AND openapi_spec <> '{}'::jsonb;" \
  > "$backup_dir/api-documents.json"
object_key="relayops/$(date -u +%Y/%m/%d)/$(date -u +%Y%m%dT%H%M%SZ).dump"
# Scope these credentials to this upload; never change the AWS deployment profile.
AWS_ACCESS_KEY_ID="$SUPABASE_S3_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$SUPABASE_S3_SECRET_ACCESS_KEY" \
AWS_SESSION_TOKEN='' AWS_DEFAULT_REGION=ap-southeast-2 \
AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required \
aws --endpoint-url "$SUPABASE_S3_ENDPOINT" s3 cp \
  "$backup_dir/relayops.dump" "s3://$SUPABASE_STORAGE_BUCKET/$object_key" --only-show-errors
printf 'Uploaded database backup to private bucket: %s\n' "$object_key"
AWS_ACCESS_KEY_ID="$SUPABASE_S3_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$SUPABASE_S3_SECRET_ACCESS_KEY" \
AWS_SESSION_TOKEN='' AWS_DEFAULT_REGION=ap-southeast-2 \
AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required \
aws --endpoint-url "$SUPABASE_S3_ENDPOINT" s3 cp \
  "$backup_dir/vault-encrypted.dump" "s3://$SUPABASE_STORAGE_BUCKET/$object_key.vault-encrypted.dump" --only-show-errors
AWS_ACCESS_KEY_ID="$SUPABASE_S3_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$SUPABASE_S3_SECRET_ACCESS_KEY" \
AWS_SESSION_TOKEN='' AWS_DEFAULT_REGION=ap-southeast-2 \
AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required \
aws --endpoint-url "$SUPABASE_S3_ENDPOINT" s3 cp \
  "$backup_dir/api-documents.json" "s3://$SUPABASE_DOCUMENTS_BUCKET/$object_key.json" \
  --content-type application/json --only-show-errors
printf 'Uploaded API specification snapshot to private documents bucket\n'

# Retention: runs only after this backup uploaded (set -e). Deletes backups
# older than RELAYOPS_BACKUP_RETENTION_DAYS (default 30) but always keeps the
# newest RELAYOPS_BACKUP_KEEP_MIN (default 7), so a stalled schedule never
# leaves zero backups. Only keys this script writes are considered.
# RELAYOPS_BACKUP_PRUNE_DRY_RUN=1 lists what would be deleted.
s3() {
  AWS_ACCESS_KEY_ID="$SUPABASE_S3_ACCESS_KEY_ID" \
  AWS_SECRET_ACCESS_KEY="$SUPABASE_S3_SECRET_ACCESS_KEY" \
  AWS_SESSION_TOKEN='' AWS_DEFAULT_REGION=ap-southeast-2 \
  AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required \
  aws --endpoint-url "$SUPABASE_S3_ENDPOINT" "$@"
}
retention_days=${RELAYOPS_BACKUP_RETENTION_DAYS:-30}
keep_min=${RELAYOPS_BACKUP_KEEP_MIN:-7}
case "$retention_days$keep_min" in *[!0-9]*) echo "backup retention settings must be whole numbers" >&2; exit 1 ;; esac
if [ "$retention_days" -lt 7 ] || [ "$keep_min" -lt 1 ]; then
  echo "refusing to prune: retention must be at least 7 days and keep at least 1 backup" >&2; exit 1
fi
cutoff=$(date -u -d "-${retention_days} days" +%Y%m%d)
prune() { # bucket
  local bucket=$1 keys stamps protected removed=0 key stamp
  keys=$(s3 s3 ls "s3://$bucket/relayops/" --recursive | awk '{print $4}' |
    grep -E '^relayops/[0-9]{4}/[0-9]{2}/[0-9]{2}/[0-9]{8}T[0-9]{6}Z\.dump(\.vault-encrypted\.dump|\.json)?$' || true)
  stamps=$(printf '%s\n' "$keys" | grep -oE '[0-9]{8}T[0-9]{6}Z' | sort -u -r)
  protected=$(printf '%s\n' "$stamps" | head -n "$keep_min")
  for key in $keys; do
    stamp=$(printf '%s' "$key" | grep -oE '[0-9]{8}T[0-9]{6}Z')
    [ "${stamp:0:8}" -lt "$cutoff" ] || continue
    printf '%s\n' "$protected" | grep -qx "$stamp" && continue
    if [ "${RELAYOPS_BACKUP_PRUNE_DRY_RUN:-0}" = 1 ]; then
      printf 'would delete %s/%s\n' "$bucket" "$key"
    else
      s3 s3 rm "s3://$bucket/$key" --only-show-errors
    fi
    removed=$((removed + 1))
  done
  printf 'Retention (%s days, keep newest %s): %s old object(s) %s in %s\n' "$retention_days" "$keep_min" "$removed" \
    "$([ "${RELAYOPS_BACKUP_PRUNE_DRY_RUN:-0}" = 1 ] && echo 'would be removed' || echo removed)" "$bucket"
}
prune "$SUPABASE_STORAGE_BUCKET"
prune "$SUPABASE_DOCUMENTS_BUCKET"
