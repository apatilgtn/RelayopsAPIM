# RelayOps disaster recovery runbook

Recovery guarantees are not certified. The AWS virtual-machine reference and
failure ledger in [enterprise/FAILURE_DRILLS.md](enterprise/FAILURE_DRILLS.md)
are authoritative for the new certification effort.

There is no measured 99.99% availability, sub-five-minute RTO, sub-minute RPO,
zero-log-loss or zero-disruption upgrade commitment. Database connection recovery,
provider failover and a complete fresh-environment restore are distinct tests.
Nightly full dumps have a potential data-loss window approaching their backup
interval; they do not establish a sub-minute RPO. Provider PITR/WAL recovery must
be configured, authorized and measured separately.

Existing AWS backup tooling retains the current database and secret-storage
integrations. deploy/aws/restore-drill.sh proves a database-only restore. Its
Vault ciphertext copy does not prove that secrets can be recovered into a new
provider project. Complete recovery additionally needs authorized secret
rehydration and a successful authenticated gateway-to-upstream request.

Production gateways keep signed last-known-good snapshots and request-log spools
under the explicitly mounted RELAYOPS_DATA_DIR. Cache files are named
last_known_good_config.<node-id>.json. Node identities/directories must be unique.
Persistence is an operator assertion verified during replacement/recovery drills,
not inferred from a successful write probe.

Before recovery, fence the failed node, preserve its volume and logs, assign the
incident/secret/database owners, and record the recovery start marker. Restore
into an isolated environment first, verify release signature, trust, credentials,
database, secret access and production preflight. Measure errors, convergence and
spool replay. Switch production traffic only after administrator review of the
evidence and rollback path.
