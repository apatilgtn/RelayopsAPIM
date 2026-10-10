# Restore drill (11 October 2026)

Proves the nightly production backup can be restored and served. Script:
[`deploy/aws/restore-drill.sh`](../../deploy/aws/restore-drill.sh), run on the
production host with the backup credentials from Vault. It restores into a
throwaway PostgreSQL container on an isolated Docker network and boots a
separate control plane bound to that network only. Production is only read.

## Result: passed

| Check | Result |
|---|---|
| Backup | `relayops/2026/10/10/20261010T163949Z.dump`, 3.8 MB, about 7 hours old |
| Restore | `pg_restore` in 2 s. One expected message: `schema "public" already exists` |
| Configuration | Identical to production: 1 tenant, 49 admin users, 20 APIs, 8 plans, 33 consumers, 41 API keys, 19 subscriptions |
| History | Behind production by the backup's age: revisions 226 / 228, audit entries 720 / 727, request logs 39,325 / 45,226 |
| Schema | Latest migration `024_mcp_catalog` |
| Service | A RelayOps control plane on the restored data reported healthy (revision 230, 14 routes) and served all 20 APIs |
| Time | 19 s from download to serving |

## What this does not cover

- **Encrypted secrets.** Vault secrets (provider API keys, for example) are
  backed up as ciphertext encrypted with the source Supabase project's key.
  Restoring into the same project restores them; restoring into a new project
  does not, and those secrets must be re-entered.
- **Recovery point.** Backups run daily, so up to a day of request logs and
  audit history can be lost. Configuration changes are also kept in
  configuration revisions, which the gateways cache.
- **Scale.** The database is small (3.8 MB compressed); restore time grows
  with request-log volume.

## First attempt

The first run restored nothing: the drill started `pg_restore` while the
PostgreSQL image was restarting after its first-time setup, and the script
still exited successfully. The drill now waits for the real server, compares
row counts with production, and fails unless the restored control plane serves
the restored APIs.
