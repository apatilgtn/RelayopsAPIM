# RelayOps APIM · Disaster Recovery & Operational Reliability Runbook

This document defines authoritative disaster recovery (DR) protocols, automated backup schedules, failure mitigation procedures, and operational verification standards for RelayOps API Management deployments in production.

---

## 1. Reliability Objectives & Service Level Indicators

| Metric | Target | Rationale |
| :--- | :--- | :--- |
| **Recovery Time Objective (RTO)** | **< 5 minutes** | Time required to restore control plane operations following total primary failure. |
| **Recovery Point Objective (RPO)** | **< 60 seconds** | Maximum allowable data loss window under database crash. Replicated WAL streams and snapshots ensure near-zero transaction loss. |
| **Data Plane Availability** | **99.99%** | Gateway nodes serve traffic continuously even during total control plane or database outages via local cache recovery. |
| **Rolling Upgrade Disruption** | **0 dropped requests** | Sequential canary rolling upgrades guarantee uninterrupted consumer traffic. |

---

## 2. Automated Scheduled Backups

### Production Cron Schedule (Linux / Container)
Deploy as root or postgres cron job (`/etc/cron.d/relayops-backup`):
```cron
# Every 15 minutes: Incremental WAL archive snapshot
*/15 * * * * postgres /usr/local/bin/relayops-backup-snapshot.sh --incremental >> /var/log/relayops/backup.log 2>&1

# Daily at 02:00 UTC: Full database dump & snapshot verification
0 2 * * * postgres /usr/local/bin/relayops-backup-snapshot.sh --full >> /var/log/relayops/backup.log 2>&1
```

### Windows PowerShell Scheduled Task
Run via Windows Task Scheduler (`scripts/backup_and_recovery.ps1`):
```powershell
powershell.exe -ExecutionPolicy Bypass -File "C:\Devops-Projects\RelayOpsAPIM\scripts\backup_and_recovery.ps1" -BackupDir "C:\RelayOpsBackups"
```

---

## 3. Gateway Cache Recovery Mode (Database Outage Survivability)

RelayOps data plane nodes maintain a local, immutable configuration snapshot on disk at:
`C:\RelayOpsAPIM\cache\snapshot.json` (or `/var/lib/relayops/cache/snapshot.json` on Linux).

### Failover Behavior During Database Crash:
1. **Detection:** When the PostgreSQL connection pool fails (e.g., network partition or database host termination), the gateway flags itself as `DEGRADED_LOCAL_CACHE`.
2. **Local Cache Execution:** The gateway evaluates routes, rate limits, and authentication policies using the local cached snapshot.
3. **Traffic Continuity:** Proxied requests succeed normally. Consumers receive the `X-RelayOps-Degraded: local-cache` response header for auditability.
4. **Automatic Reconnection:** As soon as PostgreSQL recovers, the gateway reconnects, updates its snapshot, and clears the degraded state with zero restart required.

---

## 4. Disaster Recovery Procedure (Step-by-Step Restoration)

When recovering an environment after complete storage or host loss:

### Step 1: Provision Clean Database & Run Base Schema
```bash
# Export standard environment variables
export DATABASE_URL="postgres://relayops:password@db-host:5432/relayops?sslmode=require"

# Migrate schema to current release level
relayops migrate
```

### Step 2: Restore from Latest Valid Snapshot
```bash
# Decompress and verify checksum of the latest archive
tar -xzf /backups/relayops-backup-2026-10-05.tar.gz -C /tmp/restore/
sha256sum -c /tmp/restore/checksum.sha256

# Restore Postgres database
pg_restore -d "$DATABASE_URL" --clean --if-exists /tmp/restore/relayops_db.dump
```

### Step 3: Verify Integrity & Force Cluster Sync
```bash
# Validate that APIs, plans, subscriptions, and keys are active
relayopsctl health --deep

# Issue cluster sync notification to data plane instances
relayopsctl cluster notify-sync
```

### Step 4: Health Check Verification
Execute HTTP health probe against all active nodes:
```bash
curl -f http://127.0.0.1:9090/healthz || exit 1
```

---

## 5. Zero-Downtime Rolling Upgrades

To upgrade RelayOps binary versions across a cluster:
1. **Canary Node Upgrade:** Take Gateway Node 2 out of load balancer rotation. Update binary and restart.
2. **Canary Verification:** Run test traffic against Gateway Node 2 on `:8082` to verify 200 OK responses.
3. **Re-add to Cluster:** Re-add Gateway Node 2 into load balancer rotation.
4. **Primary Node Upgrade:** Drain and upgrade Gateway Node 1 on `:8080`.
5. **Zero Request Drop:** Consumers experience zero 5xx errors or dropped connections throughout the process.

---

## 6. Incident Escalation & Post-Mortem

- **Alerting Channels:** PagerDuty / OpsGenie connected to `/healthz` and `/metrics`.
- **Log Retention:** Telemetry logs retained for 90 days; audit logs retained for 365 days.
- **Runbook Review Cycle:** Exercised quarterly using `scripts/backup_and_recovery.ps1` and `scripts/test_platform_resilience.ps1`.
