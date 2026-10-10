#!/usr/bin/env bash
set -euo pipefail
umask 077
artifact_bucket=${1:?Private deployment bucket required}
console_domain=${2:?Console hostname required}
api_domain=${3:?Gateway hostname required}
archive_sha256=${4:?Release checksum required}
region=ap-southeast-2
mkdir -p /opt/relayops /usr/local/lib/docker/cli-plugins
cd /usr/local/lib/docker/cli-plugins
curl --fail --silent --show-error --location \
  https://github.com/docker/compose/releases/download/v2.40.3/docker-compose-linux-x86_64 \
  --output docker-compose-linux-x86_64
curl --fail --silent --show-error --location \
  https://github.com/docker/compose/releases/download/v2.40.3/docker-compose-linux-x86_64.sha256 \
  --output docker-compose-linux-x86_64.sha256
sha256sum --check docker-compose-linux-x86_64.sha256
install -m 0755 docker-compose-linux-x86_64 docker-compose
cd /opt/relayops
aws s3 cp "s3://$artifact_bucket/relayops-release.tar.gz" /opt/relayops/release.tar.gz --region "$region" --only-show-errors
printf '%s  %s\n' "$archive_sha256" /opt/relayops/release.tar.gz | sha256sum --check -
tar -xzf /opt/relayops/release.tar.gz
chmod 0755 relayops relayopsctl mockupstream relayops-secrets backup-to-supabase.sh
mkdir -p data
chown 65532:65532 data
# Parameter values are captured directly into a protected file, never echoed.
database_url=$(aws ssm get-parameter --name /relayops/production/database-url --with-decryption --query Parameter.Value --output text --region "$region")
admin_token=$(aws ssm get-parameter --name /relayops/production/admin-token --with-decryption --query Parameter.Value --output text --region "$region")
printf 'RELAYOPS_DATABASE_URL=%s\nRELAYOPS_ADMIN_TOKEN=%s\nRELAYOPS_VAULT_ENABLED=true\nRELAYOPS_PUBLIC_URL=https://%s\nRELAYOPS_LOG_RETENTION_HOURS=72\nRELAYOPS_LOG_SPOOL_MB=256\nCONSOLE_DOMAIN=%s\nAPI_DOMAIN=%s\n' \
  "$database_url" "$admin_token" "$console_domain" "$console_domain" "$api_domain" > .env
unset database_url admin_token
chmod 0600 .env
docker build -t relayops:release .
docker compose up -d
docker compose ps
