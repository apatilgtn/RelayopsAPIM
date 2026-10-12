# Isolated AWS failure proof — execution pending

Provision the private two-AZ reference first. Record version/digest, instance
types, AZs, database and secret provider, Redis topology, rate-limit policy,
payload/concurrency, log sampling and duration. Approve numeric pass/fail criteria
**before** injection. Keep a separate lab account/VPC where possible.

The prepared harness is dry-run by default:

    python scripts/enterprise/aws-drill.py scripts/enterprise/lab.example.json redis-loss --output result.json

Execution is a separate operational action. It requires a populated manifest,
`--execute --ack-isolated-lab`, IAM/SSM authorization, and an exact `RelayOpsLab`
tag on every instance and, for replacement, on the retained volume and target
group. Never grant a drill identity production permissions. Store the probe
credential in `RELAYOPS_DRILL_API_KEY`; reports contain no key or response payload.
SSM output stays in the authorized AWS log, not the report.

| Scenario | Injection and evidence | Pass/fail review |
|---|---|---|
| Database interruption/reconnection | Timed outbound database TCP rejection on both control planes | Serving/error envelope, reconnect and convergence; this is not DB provider failover |
| Redis loss | Timed rejection to shared limiter from both gateways | Expected fail-open/fail-closed policy separately from transport failures; limits after reconnect |
| Control-plane isolation | Timed rejection to all internal control LB addresses | Signed last-known-good serving, log spool/replay and no policy drift |
| Gateway replacement | Fence old instance, move retained data EBS to same-AZ spare, recover node secrets, preflight and register spare | Actual host replacement; spool loss/duplicates, fencing and convergence |
| Rolling upgrade | Replace one gateway at a time with approved image digest | Errors/latency, version convergence, rollback and continuous limiting/authentication |
| Complete recovery | Authorized provider recovery runbook on fresh isolated host | Database AND secrets, then authorized upstream call; row counts alone cannot pass |
| Provider database failover | Existing provider's separately approved failover | Explicitly unsupported by TCP drill; attach provider evidence before certification |

Prerequisites: Linux/systemd/iptables and SSM, all current endpoint IPs (refresh
DNS before each drill), spare in the original volume's AZ, retained volume labeled
`relayops-data`, production runtime at `/opt/relayops`. The replacement helper
`/opt/relayops/restore-node-secrets` must rotate/restore the node credential and
trust through the existing authorized secret provider. It is provider-owned and
must be reviewed before execution. The old node stays stopped; explicitly review
cleanup/reversion. Cross-AZ replacement needs snapshot restore, not reattachment.

The complete-recovery helper `/opt/relayops/recover-lab --json` is an authorized,
provider-specific operational dependency. It must restore into a new database,
recover/re-encrypt secrets through that provider, start the restored installation,
call a protected upstream successfully, and return:

    {"fresh_database":true,"secrets_restored":true,"authorized_upstream_success":true,"rto_seconds":123,"rpo_seconds":45}

Do not install a helper that merely emits booleans. Attach database restore logs,
secret-owner approval, key/reference identity checks and actual request IDs.
Supabase Vault ciphertext copied to another project may not be decryptable without
its original key; use authorized provider recovery. Do not silently replace it.

The harness measures mean/p50/p95/p99, statuses and request IDs. It reports
`measured_pending_review`, never blanket success. Reconcile successful request IDs
with persisted logs after the spool drains (sampling must be 1); detect duplicates
and count absent IDs as loss. Measure RTO from injection to sustained approved
service, RPO from the latest recovered committed marker, and convergence from
exact node acknowledgements. Unknown fields remain null, not zero. Expected
401/403/429 rejections need a separate expected-status workload.

Store criteria, redacted ledger, environment/version, measured limits, pass/fail
disposition and reviewer. Do not publish availability, RTO/RPO, zero-loss or
zero-disruption claims until measurements and support ownership exist. Local
integration tests do not certify AWS behavior.
