# Verification run: 2026-10-05

These results come from one exact version of the source. It had not been committed when the tests ran, so it is identified by git tree hashes rather than a commit.

| | |
|---|---|
| Base commit | `7ad1449` |
| Source tree, excluding `docs/evidence` | `c948f7e6e5c6fbb272c62aeb47a94c2d7511287b` |
| Subtrees | `cmd` `f742f50` · `internal` `59c65bb` · `web` `7d3629b` · `deploy` `06f2fe5` |

Sections 1 and 2 ran on this tree. The soak in section 3 ran on the tree just before one later change, where `internal` was `baa604d`. That change, described in section 4, only affects how configuration drift is compared after an upgrade. The soak does not exercise that path. Both test suites were re-run after it.

To check a checkout against this record, stage everything and compare: `git add -A && git write-tree`. Leave `docs/evidence` out, or compare the subtrees with `git rev-parse <tree>:internal`.

**Environment:** one Windows 11 workstation with an Intel Core i7-10700T (8 cores, 16 threads) and 16 GB RAM. Software was Go 1.22.1, PostgreSQL 15.5 and Redis. The gateway nodes, mock upstream, PostgreSQL and load generator all ran on this machine. These results show correctness under failure, churn and recovery. They do not measure production capacity.

## 1. Integration, upgrade and restore tests

```bash
RELAYOPS_TEST_DATABASE_URL="postgres://postgres@localhost:5432/postgres?sslmode=disable" \
RELAYOPS_REQUIRE_INTEGRATION=1 go test -count=1 -json ./... | go run ./internal/tools/testsummary
```

| Package | Pass | Fail | Skip |
|---|---|---|---|
| cmd/relayopsctl | 7 | 0 | 0 |
| internal/admin | 34 | 0 | 1 |
| internal/analytics | 9 | 0 | 0 |
| internal/gateway | 32 | 0 | 0 |
| internal/policy | 18 | 0 | 0 |
| internal/store | 14 | 0 | 0 |
| internal/tracing | 4 | 0 | 0 |
| **Total** | **118** | **0** | **1** |

`RELAYOPS_REQUIRE_INTEGRATION=1` turns a skipped integration test into a failure. The one skip is `TestUIReviewServer`, an interactive UI review harness that only runs with `RELAYOPS_UI_REVIEW=1`. It is not an integration test. Raw results are in [2026-10-05-go-tests.json.gz](2026-10-05-go-tests.json.gz).

The integration tests against PostgreSQL cover:

- **Upgrade** from the previous release's schema and data:
  - configuration is still served;
  - limits are preserved;
  - default credentials are removed;
  - existing data moves into the `default` tenant;
  - re-running the migrations is a no-op.
- **Backup and restore** with `pg_dump` and `pg_restore`, taken mid-canary. The restored control plane serves the same revisions with the same per-revision plan limits, shows no drift, and can abort the canary and publish.
- **Gateway failure behavior:**
  - a node cut off from PostgreSQL keeps serving;
  - it catches up when the partition heals;
  - a node started during the outage serves from its cache and recovers on its own.
- **Telemetry:** a redelivered request-log batch is stored once, and the spool is drained at shutdown.
- **Tenant isolation and OIDC sign-in.** The OIDC test uses a fake identity provider with real RS256 tokens.

**Not run here:** the race detector, which needs a C toolchain, and the container image build. Both run in CI ([.github/workflows/ci.yml](../../.github/workflows/ci.yml)), which has not run yet.

## 2. End-to-end suites

All eight PowerShell suites in `scripts/` passed against a fresh database. See [2026-10-05-powershell-suites.txt](2026-10-05-powershell-suites.txt).

## 3. Soak: database outage, pod replacement and configuration churn

```bash
mockupstream -addr :27071 -jitter-ms 0
soak -gateways http://127.0.0.1:18080,http://127.0.0.1:18081 -admin http://127.0.0.1:19090 \
     -upstream http://127.0.0.1:27071 -d 10m -rps 300 -churn 15s -max-error-rate 0.5
```

The soak ran on two gateway nodes, with node B reaching PostgreSQL through a TCP proxy that could be cut. Throughout, the configuration changed every 15 seconds, cycling through publish, a 20% canary and promote.

| Time | Event |
|---|---|
| 0:00 | Load starts |
| 3:00 | Node B's database connection is cut |
| 4:30 | Node B is hard-killed and restarted with the same node ID and data directory, while the database is still down. This models a StatefulSet pod replacement. |
| 6:00 | The database connection is restored |
| 10:00 | Load stops; the fleet must converge on the final revision |

**Result: PASS.** The full report is [2026-10-05-soak-outage-pod-replacement.md](2026-10-05-soak-outage-pod-replacement.md), and the timeline with metrics is [the chaos log](2026-10-05-soak-outage-pod-replacement-chaos.log).

| | |
|---|---|
| Requests | 180,003 over 600 s, at 300.0 requests per second |
| Failed requests | 26 (0.014%). All were connection refused while node B was restarting; no load balancer routed around it in this test. |
| Latency through the gateway | p50 0.7 ms, p95 2.3 ms, p99 3.6 ms, p99.9 7.6 ms, max 40.7 ms |
| Configuration changes | 14 publishes, 13 canaries, 13 promotions, 0 failed |
| Requests served by a canary | 15,588 |
| Fleet at the end | rev_16, converged |
| Memory | Both nodes stayed between 40 and 82 MB |

**Node B through the outage and replacement**

| Moment | Database up | Spool records | Spooled | Replayed | Dropped |
|---|---|---|---|---|---|
| Before the kill | 0 | 13,350 | 13,350 | 0 | 0 |
| After the restart, database still down | 0 | 13,948 (the predecessor's spool was found) | 448 | 0 | 0 |
| Before restore | 0 | 26,999 | 13,499 | 0 | 0 |
| 60 s after restore | 1, config at the current revision | 0 | 13,499 | 26,999 | 0 |

**Telemetry completeness.** The table compares request logs in PostgreSQL with the requests the load generator received a response for.

| Node | Served | Logged | Distinct event IDs | Missing |
|---|---|---|---|---|
| node-a | 90,002 | 90,002 | 90,002 | 0 |
| node-b | 89,975 | 89,834 | 89,834 | 141 |

The 141 missing logs were in node B's in-memory buffer when it was hard-killed, about half a second of traffic. A graceful shutdown drains that buffer, but a hard kill cannot. The test platform cannot send a graceful stop, so this run measured the worst case. There were no duplicates.

## 4. Defects found during verification, and their fixes

**A node started during an outage never reconnected.**

The same soak, run on the build before the fix, failed. See [2026-10-05-soak-before-fix.md](2026-10-05-soak-before-fix.md) and [its chaos log](2026-10-05-soak-before-fix-chaos.log).

A node that started while PostgreSQL was down stayed disconnected permanently. It served stale configuration, spooled logs without ever replaying them, and the fleet never converged. The cause was at startup: when the first database ping failed, the server dropped the connection and never tried again.

Now the node keeps a lazily connecting pool and runs migrations once the database is reachable. Config reload, change notifications and spool replay all resume without a restart. The fixed-build soak above and the gateway integration test both cover this.

**An upgrade looked like configuration drift.** Revisions written by the previous release have no tenant or traffic-policy fields. The first start after an upgrade therefore saw every API as changed, and published one redundant revision of identical content. Drift comparison now gives those fields the values the upgrade assigns. The upgrade test now requires that an upgrade reports no drift. It failed against the old comparison and passes against the new one.

## 5. What this does not show

- **Capacity.** These are not throughput limits. The rates were chosen to be sustainable on one shared machine.
- **Multi-host behavior.** There was no network latency between hosts, no PostgreSQL failover, and no replica lag.
- **Long durations.** The soak lasted minutes, not days.
- **Rolling upgrade under live traffic.** The upgrade test migrates a stopped database.
- **Kubernetes behavior.** The Helm chart, including StatefulSet persistence, was linted and rendered but not deployed to a cluster.
