# RelayOps deployment: AWS compute and hosted Supabase

Prepared 5 October 2026. This is a deployment checklist, not a completed deployment. No cloud resources have been created or paid for.

## Selected AWS target

- Account ID supplied by the user: `282583951405`.
- Account ARN supplied by the user: `arn:aws:account::282583951405:account`.
- Region: Asia Pacific (Sydney), `ap-southeast-2`.
- Local AWS identity check returned "Unable to locate credentials". These account details identify the target but do not grant deployment access; the account identity has not yet been independently verified.
- Credit balance, expiry and eligible services remain to be checked after authentication.

## Initial architecture

- One Linux EC2 instance running the existing RelayOps container and an HTTPS reverse proxy. Start with 2 vCPU / 4 GB RAM for a low-traffic pilot, then measure resource use; this is not a capacity guarantee.
- Hosted Supabase PostgreSQL in a region close to EC2. Keep RelayOps authentication and authorization; adopting Supabase Auth is not required.
- Private Supabase Storage buckets for agreed file types. Application upload/download support needs implementation; the current product has no Supabase/S3 storage adapter.
- Optional local Redis for quotas/rate limits. Keep it private. Multiple gateway nodes need shared Redis when using distributed limits.
- Persistent encrypted EBS for local configuration recovery and request-log spooling. Object storage does not replace the local outage-recovery disk.
- Console hostname routes to container port 9090; API hostname routes to container port 8080. Only HTTPS is publicly exposed. The developer portal remains part of the existing embedded application.

A single EC2 instance is a pilot topology, not high availability. Independent standard and canary gateway processes are needed to demonstrate real canary isolation.

## Decisions required

1. Authenticate AWS access to the selected account; confirm available credit balance, expiry and eligible services.
2. Supabase organization/project and region, or confirmation that a new project is needed.
3. Domain and access to its DNS settings.
4. Whether to migrate existing RelayOps users/configuration/history or start with an empty installation.
5. Storage scope: backups, API documentation/specification attachments, or other customer files; expected size and retention.

Supply credentials through protected local configuration or a secrets manager, not chat, source control or frontend JavaScript.

## Supabase setup

Connection verified on 5 October 2026 using a read-only transaction: project `moxleagjsaagypnwpgyp`, host `aws-0-ap-southeast-2.pooler.supabase.com`, session port `5432`, database `postgres`, login `postgres.moxleagjsaagypnwpgyp`, PostgreSQL `17.6`. Client-to-pooler TLS 1.3 was confirmed with `psql` connection information. `pg_stat_ssl` describes the pooler's backend connection, not the client TLS connection. No schema changes, migrations or application configuration changes were made. The supplied password was used transiently and is not recorded in this document. Rotate it before deployment because it was shared in chat. Full application compatibility remains untested.

1. Create a dedicated staging project before production. Use stable PostgreSQL rather than optional beta database engines.
2. Copy the exact PostgreSQL connection details from the project's Connect panel. Use a direct connection if EC2 has the required network connectivity; otherwise use the Supavisor **session** pooler on port 5432. Do not use the transaction pooler on port 6543: RelayOps uses LISTEN and session advisory locks.
3. Require TLS, preferably `sslmode=verify-full` with the project CA certificate mounted and referenced through `sslrootcert`. URL-encode special characters in passwords. Confirm certificate/hostname compatibility against the actual project.
4. Keep RelayOps tables off the public Data API. For this backend-only deployment, disable the Data API unless a separately designed use case needs it. Restrict grants for anon/authenticated roles and future objects, and apply RLS to any exposed tables. Do not invent Supabase Auth policies for RelayOps's existing identity model.
5. Separate migration privileges from runtime privileges where feasible, checking the current startup migration path before restricting its account. Do not overwrite Supabase-managed auth/storage schemas when migrating.
6. Verify migrations, notifications, revision locking, connection recovery and the pool budget on staging. The current application allows up to 20 pool connections per process; account for every gateway and Supabase's own connections before adding nodes.
7. Create private Storage buckets only after deciding their purpose. Keep generated S3 credentials on the server: they bypass Storage RLS. An adapter must enforce RelayOps tenant/role permissions, validate uploads, use scoped object paths and issue short-lived signed download links.

## AWS setup

1. Confirm credit coverage in Billing, enable MFA, and set budget alerts and cost anomaly monitoring. Alerts are not a hard spending cap; charges may accrue after credits expire or are exhausted.
2. Provision one EC2 instance, persistent encrypted EBS and a narrowly scoped instance role. Prefer Systems Manager access; restrict SSH if it is needed.
3. Allow ports 80/443 to the HTTPS proxy. Do not expose 9090, 8080 or Redis directly. Use a public subnet initially to avoid an unnecessary NAT gateway; no ALB/EKS is needed for this pilot.
4. Install the container runtime, establish persistent directories with permissions suitable for the image's non-root user, and configure restart policies.
5. Before building the image, add a reviewed `.dockerignore`: the current Dockerfile copies the build context and there is no `.dockerignore`. Exclude local secrets, `.env` files, database directories, backups, generated binaries and test artifacts; Git ignore rules alone do not protect Docker build contexts.
6. Build/test the image, store it in a private registry if needed, and pass runtime secrets securely. Use a unique administrator token; never deploy the default token.
7. Configure DNS and automatic HTTPS. Preserve streaming/SSE behavior through the proxy. Retain and monitor local cache/spool storage, log retention and disk usage.

## Existing supported runtime configuration

These names already exist in the product; values below are placeholders, not working credentials:

```dotenv
RELAYOPS_DATABASE_URL=<exact Supabase direct/session connection URL with TLS>
RELAYOPS_ADMIN_TOKEN=<unique long random secret>
RELAYOPS_PUBLIC_URL=https://console.example.com
RELAYOPS_PROXY_ADDR=:8080
RELAYOPS_ADMIN_ADDR=:9090
RELAYOPS_NODE_ID=aws-primary-01
RELAYOPS_NODE_GROUP=default
RELAYOPS_CANARY=false
RELAYOPS_REDIS_URL=redis://redis:6379/0
RELAYOPS_LOG_RETENTION_HOURS=72
RELAYOPS_LOG_SPOOL_MB=256
```

For a single-node deployment without Redis, explicitly set `RELAYOPS_REDIS_URL=disabled`; limits then use in-memory state. No Supabase Storage settings are currently implemented. Proposed storage configuration must be added alongside the actual adapter and authorization checks.

## Migration, start and acceptance

1. Fix the known analytics test fixture failure and run relevant frontend/Go checks. Keep external OIDC disabled until the recorded browser-state binding and email-verification issues are addressed.
2. Build and verify the container. Docker CLI exists locally, but the daemon was unavailable during this review; no fresh image build was verified.
3. Back up the existing PostgreSQL data and prove a staging restore. Migrate only RelayOps-owned objects/data; retain user password hashes and configuration deliberately. Existing sessions may need revocation if hostnames/secrets change.
4. Test Supabase connectivity and application migrations in staging. Existing integration tests that create/drop databases must not be assumed to work against a hosted project; run the appropriate isolated checks instead.
5. Start the application with the protected environment and persistent volumes. Verify readiness before exposing it through DNS/HTTPS.
6. Exercise email/password and administrator sign-in, signup/approval, catalog/workbench, API proxying, SSE, configuration planning/apply, release locks and notifications, logs and analytics. Use isolated test data.
7. Test restart and temporary database unavailability, restore from backup, storage authorization and signed-link expiry once storage is implemented. Record which checks used the real Supabase project.
8. Inspect actual AWS/Supabase costs and retention after an initial representative workload before increasing traffic or adding gateways.

## High availability

### Stage 1: one host, no single gateway process (no extra AWS cost)

`deploy/aws/compose.ha.yaml` runs the existing container as the control
plane (`RELAYOPS_ROLE=control-plane`: console, admin API, node API,
rollouts). Two gateway-only nodes (`gateway-a`, `gateway-b`) serve API
traffic. `Caddyfile.ha` balances API traffic across the gateways and takes a
gateway out of rotation when its `/readyz` check fails. It retries a request
on the other gateway when the connection fails before any response.

```sh
# once: add a node token to .env
echo "RELAYOPS_DATAPLANE_TOKEN=$(openssl rand -hex 32)" >> .env
docker compose -f compose.yaml -f compose.ha.yaml up -d
# every release:
./rolling-update.sh
```

What this covers:

- A gateway crash or restart: the other gateway keeps serving.
- Deploys: `rolling-update.sh` updates the control plane, then replaces one
  gateway at a time, waiting until each is ready.
- A control-plane outage or restart: the gateways keep serving their last
  signed configuration and spool request logs until it returns.

Not covered: losing the EC2 instance or its availability zone. The
instance also needs memory for three RelayOps processes. `t3.small` (2 GiB)
is enough for the pilot, but use `t3.medium` with production traffic.

### Stage 2: two hosts (additional cost, needs approval)

For instance and availability-zone failures:

- Run the gateways on two instances in different availability zones, behind
  an Application or Network Load Balancer that health-checks `:9091/readyz`.
- Run the control plane on one of them, or a third small instance. The
  database stays in Supabase.
- Redis, used for distributed rate limits, moves to ElastiCache, or to
  Redis on the control-plane host. If Redis is unreachable, gateways fall
  back to local limits.

Approximate added cost in `ap-southeast-2`: one more `t3.small` (about
USD 20/month) plus an ALB (about USD 20–25/month plus LCU charges).
Optionally add ElastiCache `cache.t4g.micro` (about USD 15/month). The
CloudFormation stack would gain a second instance, a target group and a
load balancer. Gateways need no database access (`RELAYOPS_ROLE=gateway`),
so the database credentials stay on the control-plane host.

## Cost boundaries

AWS credits apply only to eligible AWS charges, subject to their terms. Hosted Supabase is separately billed. Supabase Free can support an early demo but has small database/storage limits and inactivity pausing. Pro starts at US$25/month; additional compute/projects/add-ons and usage may increase that amount. Its spend cap is not a universal invoice cap. Do not present credits or either plan's starting price as a guaranteed total RelayOps deployment cost.

### Database egress

Supabase bills egress: bytes the database sends to clients. Free includes 5 GB per billing cycle.

**What happened.** Builds before commit `7089152` (9 October 2026) read the full configuration snapshot every 3 seconds in the auto-rollback supervisor. An idle node pulled about 2.1 GB a day, and that exceeded the Free quota in early October.

**Measured after the fix**, on one idle combined node, through a byte-counting proxy:

| Source | Egress |
| --- | --- |
| Supervisor (one evaluation every 3 s, about 2 KB each) | about 53 MB a day |
| Config resync (every `RELAYOPS_RESYNC_SECONDS`, 300 s by default, one snapshot each) | about 20–25 MB a day at about 70 KB per snapshot |
| Drift adopter sweep (every 5 minutes) | about 20 MB a day |

**Keeping egress down on a metered database:**

- **Run one control-plane node.** Every control-plane node runs the supervisor, the drift adopter and its own resync.
- **Slow the supervisor.** Set `RELAYOPS_AUTO_ROLLBACK_INTERVAL_SECONDS=15`, which cuts supervisor egress by 5x. Rollback detection becomes up to 15 seconds slower.
- **Close idle console tabs.** The Logs page reloads 200 rows every 10 seconds while it is visible.
- **Sample request logs with real traffic.** Set `RELAYOPS_LOG_SAMPLE_RATE`, for example 0.1. Errors are always kept.
- **Keep tests local.** Do not point E2E, soak or verification runs at the hosted project.
- **Run gateways as gateway-only nodes** (`RELAYOPS_ROLE=gateway`). They have no database connection, so they add no egress beyond what their control plane reads for them.

The CI test `TestIntegrationAutoRollbackEvaluationEgressBudget` fails if one supervisor evaluation pulls more than 5 KB. With the old snapshot read it measured about 177 KB.

## Official references

- Supabase connections: https://supabase.com/docs/guides/database/connecting-to-postgres
- Data API security: https://supabase.com/docs/guides/api/securing-your-api
- Storage credentials: https://supabase.com/docs/guides/storage/s3/authentication
- Private buckets: https://supabase.com/docs/guides/storage/buckets/fundamentals
- Supabase pricing: https://supabase.com/pricing
- Supabase cost controls: https://supabase.com/docs/guides/platform/cost-control
- AWS credit terms: https://aws.amazon.com/awscredits/
- AWS budgets: https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-managing-costs.html
