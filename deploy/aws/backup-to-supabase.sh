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
