# RelayOps production Vault configuration

Verified and deployed on 5 October 2026 in project `moxleagjsaagypnwpgyp`.

## Stored now

- Encrypted Vault secrets: `relayops.env.RELAYOPS_ADMIN_TOKEN` and `relayops.env.RELAYOPS_SECRET_NVIDIA_API_KEY`.
- Seven ordinary private settings in `relayops_private.runtime_settings`: public console URL, log retention, spool size, S3 endpoint/region and the two intended bucket names.
- Database connection remains an encrypted AWS Parameter Store bootstrap value. The existing administrator token also remains there as a recovery fallback; removing it would affect startup while Vault is unavailable.

No secret values are recorded here. The two Vault values were verified to differ from their ciphertext, and client roles lack access to the decrypted view or private configuration schema.

## Adding Storage credentials

Generate the S3 credential pair in the project's Storage S3 settings, then add these **exact names** in Supabase Vault:

| Vault name | Value |
| --- | --- |
| `relayops.env.SUPABASE_S3_ACCESS_KEY_ID` | Generated Storage Access Key ID |
| `relayops.env.SUPABASE_S3_SECRET_ACCESS_KEY` | Generated Storage Secret Access Key |

Both Storage keys are now stored in Vault. The private relayops-backups and relayops-documents buckets are connected; the first database backup, separate Vault ciphertext backup and API specification snapshot were uploaded and downloaded successfully. The downloaded database backup restored into an isolated PostgreSQL 17 container with matching record counts (47 APIs, 49 users, 31 consumers, 8 plans, 7 settings). The temporary restore container was removed. Vault ciphertext was downloaded but not restored; matching encryption context remains required. Automated recurring backup scheduling is not yet configured.

The backend reads namespaced secrets and private settings at startup when `RELAYOPS_VAULT_ENABLED=true`. Changes need a process restart. Values enter process memory; the loader does not write a decrypted file or expose a browser API. Arbitrary environment overrides, including the database connection and node identity, are rejected. A supplied secure administrator environment fallback preserves recovery startup when Vault cannot be read; it does not guarantee every Vault-dependent external integration works through a cold-start outage.

## Operations

`relayops-secrets store` accepts `{ "secrets": {...}, "settings": {...} }` JSON through stdin and writes parameterized values transactionally. Do not place secrets in command arguments or commit filled-in files. It prints counts only.

`relayops-secrets run -- COMMAND [ARG...]` loads hosted values into a child process, for example running the Storage backup script on the EC2 host from `/opt/relayops`. It does not offer a plaintext export operation. Use the protected database bootstrap environment/working directory.

Vault setup uses `deploy/aws/vault-setup.sql`, matching the actual installed four-argument create_secret and five-argument update_secret signatures. Internal Supabase cryptographic functions are not modified.

This integration covers runtime environment secrets. Existing per-API JWT fields remain in their protected API configuration; converting those fields to Vault references is a separate migration.

## Verification

- Vault loader tests passed: reject bootstrap overrides without changing environment, separate secret/settings placement, reject invalid values, resolve existing secret references and suppress secret-bearing database errors.
- `go test ./...` passed; the full database integration suite was not enabled in this invocation.
- Actual Supabase write/read verification: 4 encrypted secrets including 2 Storage keys, 7 private settings; anonymous Vault schema/view and authenticated private-schema access denied.
- EC2 startup logged `hosted configuration loaded` with 11 values, followed by database readiness.
- Public HTTPS health remained healthy at revision 176 with 38 routes. Administrator sign-in and migrated gateway traffic passed; all 47 APIs retained.
- Storage backup script was extended for private settings and a separate encrypted Vault metadata backup. Matching encryption context is needed for secret restoration; cross-project restore is not assumed. Storage upload/download and isolated application-schema restore passed on 5 October 2026. The app remains healthy at revision 176.

Reference: https://supabase.com/docs/guides/database/vault
