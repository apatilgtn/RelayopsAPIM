# Gateway and control-plane separation

Date: 9 October 2026. Status: Phases 1 and 2 implemented; Phase 3 partly implemented (sampling and live traffic); Phase 4 is a proposal.

## Why

Before this change, every RelayOps node ran the gateway and the control plane in one process, and **every gateway node held PostgreSQL credentials**. That causes four problems:

- **Placement.** A gateway cannot run in an untrusted network, at the edge or in another region unless the database is reachable from there too.
- **Scaling.** Database connections grow with the number of gateways: each node holds a `LISTEN` connection, writes request logs and records acknowledgements.
- **Attack surface.** Every gateway also serves the admin API, console, portal, MCP endpoint and SCIM.
- **Comparison.** The enterprise products RelayOps is compared with all separate the two planes:

| Product | How gateways get configuration |
| --- | --- |
| Kong | Hybrid mode: the control plane pushes config to database-less gateways |
| Tyk | MDCB: gateways sync from a central control plane |
| Apigee | Hybrid: the runtime pulls from the Google-managed management plane |
| Azure API Management | Self-hosted gateway pulls from the Azure control plane |

## What a gateway needed the database for

| # | Dependency | Before | Phase 1 |
| --- | --- | --- | --- |
| 1 | Configuration snapshot (revision + keys + subscriptions) | `LoadSnapshotDataForNode` on every change | `GET /dataplane/v1/config` long-poll |
| 2 | Change feed | `LISTEN relayops_config` (a held connection) | The same long-poll, keyed by a configuration fingerprint |
| 3 | Revision acknowledgement (the only liveness signal) | `node_acknowledgements` upsert | `POST /dataplane/v1/ack`, plus a 30 s heartbeat |
| 4 | Request logs | `COPY` into `request_logs`, with an on-disk spool during outages | `POST /dataplane/v1/logs` (gzip), with the same spool |
| 5 | Log retention purge | Ran on every node | Control plane only |
| 6 | AI budget lookup, reserve, settle and release | Inline per request | `POST /dataplane/v1/ai-budget/*`, still failing closed |
| 7 | TCP stream services | 15 s poll | Delivered with the configuration |
| — | Co-hosted jobs: auto-rollback supervisor, Test Studio runner, drift adopter, admin | Ran on every node | Control plane only |

Rate limits (Redis or in-memory), JWKS, health checks and circuit breakers never used the database.

## Architecture

```
                  control-plane pods (RELAYOPS_ROLE=control-plane)              PostgreSQL
  console · portal · admin API · node API (/dataplane/v1/*) :9090  ◀────────▶  (only these pods
  auto-rollback supervisor · Test Studio · drift adopter · retention             hold the DSN)
                         ▲
                         │  HTTPS + node token
                         │  config long-poll · acks/heartbeats · log batches · AI budget ledger
                         │
  gateway pods (RELAYOPS_ROLE=gateway): proxy :8080, status :9091 (/metrics /healthz /readyz)
  no DSN, no admin listener; last-known-good cache and log spool cover control-plane outages
```

`RELAYOPS_ROLE` selects one of three roles:

| Role | Runs | Database |
| --- | --- | --- |
| `all` (default) | Gateway and control plane, unchanged | Yes |
| `control-plane` | Everything except the proxy listener and upstream health checks. It keeps an in-process snapshot for the admin features that read it (overview, release safety, replay) but does not acknowledge it, so it never counts as a gateway in fleet status. | Yes |
| `gateway` | Proxy plus a status listener | **No** |

## Node API

The node API is mounted on the control plane's admin listener under `/dataplane/v1/`, and only when `RELAYOPS_DATAPLANE_TOKEN` is set. Admin RBAC does not apply to it. Every request carries:

- `Authorization: Bearer <node token>`, compared in constant time
- `X-RelayOps-Node-Id`, `X-RelayOps-Node-Group` and `X-RelayOps-Node-Canary`

| Method and path | Purpose |
| --- | --- |
| `GET /config?wait=N` | Returns `{fingerprint, issued_at, data: SnapshotData, streams}`, gzip-compressed when the client accepts it. With `If-None-Match` equal to the current fingerprint, the request is held for up to N seconds (capped at 30). It returns `304` if nothing changed, or `200` as soon as something did. |
| `POST /ack` | Body is a `NodeAck`, written to `node_acknowledgements`. Gateways resend it every 30 s as a heartbeat, which also fixes idle nodes disappearing from fleet status after 15 minutes. |
| `POST /logs` | A gzip JSON batch of `RequestLog`. Every record needs `log_id` and `node_id`, so retries are idempotent. |
| `POST /ai-budget/{lookup,reserve,settle,release}` | Proxies the store ledger. `404 not_found` maps to `store.ErrNotFound` and `409 budget_exceeded` to `store.ErrBudgetExceeded`. Any other failure surfaces to the gateway, which keeps returning 502 `ai_budget_unavailable` rather than skipping budget checks. |

**Change detection.**

- **Fingerprint.** The fingerprint is a SHA-256 over the canonical JSON of the snapshot and stream services. Keys are sorted by hash and subscriptions by consumer and API, because the access-state queries have no `ORDER BY`. Any change to a revision, a key, a consumer's status or a subscription therefore changes the fingerprint.
- **Notifications.** The control plane follows `LISTEN relayops_config` itself and wakes waiting gateways on each notification, after a 100 ms debounce.
- **Caching.** Computed snapshots are shared by node group and canary flag. A cached snapshot is reused for at most 15 s, because key rotation grace periods expire without a NOTIFY.
- **Wire format.** The client sends the bare fingerprint as `If-None-Match`. The server accepts it with or without ETag quotes.

**Measured latency.** On the local integration test, a change is served by a gateway-only node in about 210 ms. Most of that is the two debounce delays.

## Security model

- **What the snapshot contains:** API definitions, including HS256 JWT secrets resolved at apply time, plus API key **hashes**, consumer status and subscription grants. It contains no plaintext API keys and no database credentials. `${secret:NAME}` references in upstream headers are resolved on the gateway at request time from the gateway's own environment, so those values never travel in the snapshot.
- **Transport:** gateways refuse `http://` control-plane URLs unless `RELAYOPS_DATAPLANE_ALLOW_HTTP=true`. That setting is meant for local development, the compose network, or an in-cluster service with a mesh providing mTLS. Across networks, use HTTPS: set `RELAYOPS_ADMIN_TLS_CERT_FILE` and `RELAYOPS_ADMIN_TLS_KEY_FILE` on the control plane, or terminate TLS in front of it.
- **Authentication:** per-node credentials (Phase 2, below), or the Phase 1 shared node token of at least 32 characters. A leaked shared token lets the holder read configuration and write acknowledgements, request logs and budget settlements for any node. A leaked per-node token does the same only as that node, and can be revoked on its own. Neither grants admin API access.
- **Disk:** the last-known-good cache is written with mode `0600` (it was `0644`). With signature verification on, it is the signed envelope and is verified again before a cold start.

## Node trust (Phase 2)

**Per-node credentials.**

- **Issuing.** A superadmin issues a token for one node ID with `POST /api/admin/dataplane/credentials` and the body `{"node_id": "...", "description": "...", "expires_in_days": 90}`. The token (`rlnode_…`) is returned once. Only its SHA-256 hash is stored, in the `dataplane_node_credentials` table (migration 018).
- **Scope.** A credential is accepted only from the node it was issued to, so `X-RelayOps-Node-Id` must match. It can acknowledge and send logs only as that node; anything else gets `403 node_mismatch`.
- **Lifecycle.**
  - `DELETE /api/admin/dataplane/credentials/{id}` revokes a credential, effective on the node's next call.
  - Expired credentials are refused.
  - `GET /api/admin/dataplane/credentials` lists each credential with its last-seen time and address.
- **Enabling.** The node API is enabled on every `control-plane` node, and on combined nodes with `RELAYOPS_DATAPLANE_API=true` or a shared token. Without `RELAYOPS_DATAPLANE_TOKEN`, only per-node credentials are accepted.

**Signed configuration.**

- **Payload.** The payload travels as raw JSON. Its fingerprint is the SHA-256 of those exact bytes, so gateways verify what was signed rather than a re-serialisation.
- **Signing.** With `RELAYOPS_DATAPLANE_SIGNING_KEY` (an Ed25519 seed from `relayops dataplane-keygen`), the control plane signs `fingerprint + issued_at` and adds `key_id` and `signature`. The public key is logged at startup.
- **Verification.** Gateways with `RELAYOPS_DATAPLANE_VERIFY_KEYS` refuse configuration that is unsigned, signed by an unknown key, or altered after signing. List several keys to rotate without downtime.
- **Replays.** A gateway refuses an envelope issued more than one minute before the one it is serving, which blocks a replay of an old configuration.
- **Cold start.** The on-disk cache is the signed envelope. A tampered cache is refused on a cold start rather than served.

**mTLS (optional).**

- **Control plane.** `RELAYOPS_ADMIN_CLIENT_CA_FILE` makes the admin listener request client certificates and verify them against that CA. Browsers without a certificate still connect. `RELAYOPS_DATAPLANE_REQUIRE_CLIENT_CERT=true` then requires a verified certificate for the node API; it needs admin TLS to be configured.
- **Gateways.** Set `RELAYOPS_DATAPLANE_CLIENT_CERT_FILE` and `RELAYOPS_DATAPLANE_CLIENT_KEY_FILE` to present a certificate, and `RELAYOPS_DATAPLANE_CA_FILE` to trust a private CA.
- **Not checked.** The certificate's subject is not compared with the node ID; the per-node token provides node identity.

**Not done in Phase 2:** a console view of credentials and per-node sync health (the API above provides the data).

## Analytics volume (Phase 3, first step)

**Why sampling rather than rollups first.** The original plan was per-minute rollups. However, auto-rollback, canary status, the analytics summary and their tests all read `request_logs`. A separate rollup store would split the source of truth between gateway-written rollups and raw rows. Weighted sampling cuts write volume while keeping a single table and exact error counts.

**Sampling.**

- `RELAYOPS_LOG_SAMPLE_RATE` (default 1) is the fraction of successful requests written to `request_logs`.
- Errors (status 400 or above, or a recorded error) are always written, with weight 1.
- Each kept success carries `sample_weight = 1/rate` (migration 019).
- Selection hashes the log ID, so it is deterministic.
- Live dashboards and Prometheus metrics still see every request.
- `relayops_request_logs_sampled_out_total` counts what was not written.

**Weighted readers.**

- **Auto-rollback and canary status** (`RevisionErrorStatsBetween`), the analytics summary (totals, 4xx/5xx, bytes, tokens, weighted mean latency, status classes, top APIs, consumers and models), and the release-safety population count all sum `sample_weight`.
- **Error counts stay exact**, because errors are never sampled. The request total is an unbiased estimate, so `min_requests` and the error rate behave as before at reasonable rates.
- **Known bias:** latency percentiles are computed over persisted rows. With sampling, errors are over-represented in them.
- **Unweighted views:** the logs view, request diagnosis, traffic replay, consumer impact and portal usage read raw rows. With sampling they see every error but only a sample of successes.

**Live traffic in split mode.** Gateway-only nodes forward their one-second ticks to `POST /dataplane/v1/ticks`, keeping only the latest and dropping rather than queueing. The control plane republishes them to console subscribers with the same per-tenant filtering, so the live view shows real traffic again.

## Regional relays (Phase 4)

`RELAYOPS_ROLE=relay` serves the node API to one region's gateways on `RELAYOPS_ADMIN_ADDR`, using the admin TLS certificate files. Gateways set their `RELAYOPS_CONTROL_PLANE_URL` to the relay. The relay itself uses `RELAYOPS_CONTROL_PLANE_URL` and its own node credential to reach the control plane.

- **Configuration:** one long-poll per node group to the control plane, shared by all the region's gateways, which long-poll the relay instead. Envelopes are passed through byte for byte, so the signature still holds and gateways with verify keys still check it. The relay cannot alter configuration; it also drops any envelope whose payload does not match its fingerprint.
- **Everything else** (acknowledgements, logs, ticks, AI budget) is proxied with the gateway's own credentials. The control plane authenticates and attributes each gateway itself, and the relay never appears in fleet status.
- **Gateway authentication:** the relay checks each gateway's credentials with the control plane (`GET /dataplane/v1/whoami`) and caches the answer for 60 seconds. A credential used as another node is refused (403).
- **Partitions:** while the control plane is unreachable, the relay keeps serving its cached configuration to gateways it verified in the last 24 hours. Gateways it has never verified get 503. Revocation reaches the relay within the 60-second cache window once the control plane is reachable.
- **Status:** `/healthz` and `/metrics` (groups followed, upstream syncs and errors, configurations served).

Verified in `internal/dataplane/relay_integration_test.go`: change propagation through the relay, fleet attribution, logs, refusal of unknown or mismatched credentials, and serving through a control-plane outage.

## Failure behaviour

| Event | Gateway-only node |
| --- | --- |
| Control plane down | Keeps serving its current snapshot. The long-poll reconnects with backoff from 1 s to 30 s. Request logs go to the on-disk spool, capped by `RELAYOPS_LOG_SPOOL_MB`. |
| Gateway restarts while the control plane is down | Cold-starts from `data/last_known_good_config.<node>.json` |
| Control plane returns | The long-poll picks up any changes. The spool replays in order, at least once, with duplicates removed by `log_id`. |
| AI route while the ledger is unreachable | `502 ai_budget_unavailable`, the same fail-closed behaviour as combined mode |
| Control plane database down | The node API answers `503`. Gateways behave as if the control plane were down. |

## Deployment

- **Helm** (`deploy/helm/relayops`):
  - `mode: combined` (the default) renders as before. The only additions are a `relayops.io/serves-proxy` pod label and the matching proxy Service selector.
  - `mode: split` adds a `-control-plane` Deployment, which is the only workload with the DSN, and points the admin Service at it. Gateway and canary pods run `RELAYOPS_ROLE=gateway` and expose `:9091`.
  - Split mode also generates and keeps a `dataplane-token` and a shared `runner-secret`, so Test Studio can call gateways. It adds a `-gateway-status` Service, and a second ServiceMonitor when monitoring is enabled.
  - In split mode the control plane sets `RELAYOPS_GATEWAY_URL` to the proxy Service, for the workbench, MCP `invoke_api` and Test Studio.
- **Compose:** `RELAYOPS_DATAPLANE_TOKEN=<32+ chars> docker compose --profile split up -d` adds `gateway-edge` on ports 8081 and 9091.

## Verification (Phase 1)

| Check | Result |
| --- | --- |
| Unit tests | Fingerprint independent of row order and changed by a revoked key; node token required, and admin token rejected on the node API; API not mounted without a token; long-poll holds (304), accepts the bare fingerprint, and wakes on change; remote source load, watch and ack; ledger error mapping; gzip log batches; role validation |
| Integration test (`internal/dataplane/split_integration_test.go`) | A real control plane and store with a gateway node that has no database. Covers change propagation, an idle gateway not reloading, fleet status, log delivery, serving through an outage, cold start from cache, and duplicate-free spool replay. Passed 3 out of 3 runs. |
| Binaries end to end on isolated ports | A new API was served by the gateway-only node 249 ms after creation. Logs persisted, fleet status listed only the gateway, the status endpoints worked, and the node API returned 401 without a token. The gateway kept serving through a control-plane restart. |
| `helm lint` and `helm template` | Both modes lint. Combined-mode output is unchanged apart from the label and selector. In split mode the gateway pods have no DSN. |
| Full suite | At Phase 1, all packages passed except three failures already present on the base commit. Those were fixed separately (OIDC binding check order, an APIOps test writing to the shared database, `go vet` in `cmd/verify_supabase`). |
| Phase 2 (`internal/dataplane/trust_integration_test.go`, `dataplane_test.go`) | A per-node credential serves its node; using it as another node, or acknowledging for one, returns 403; an unconfigured shared token is refused. Signed changes propagate. A gateway trusting a different key refuses the configuration. Cold start verifies the signed cache, and a tampered cache is refused. Revocation returns 401 on the next call. Unit tests cover signature, tamper, re-fingerprint, untrusted key, key rotation, unsigned envelopes and replay. |

Not done yet: a soak or latency comparison between split and combined mode (`cmd/soak` works unchanged against gateway-only nodes), a real Kubernetes install, and TLS between the planes.

## Roadmap

1. **Phase 1: gateway-only mode over the node API.** Done.
2. **Phase 2: node trust.** Done: per-node credentials, signed configuration and cache, replay refusal, optional mTLS (see "Node trust"). Remaining: the console view of credentials and per-node sync health.
3. **Phase 3: analytics off the transactional path.** Partly done (see "Analytics volume").
   - Done: weighted sampling of successful requests; live traffic from gateway-only nodes on the console; every request log exported as OTLP log records (`RELAYOPS_LOG_EXPORT=otlp`) so full analytics can live in an observability backend while PostgreSQL keeps the sampled rows.
   - Remaining: `request_logs` partitioning (converting the existing table needs a planned migration on large installs) and per-minute rollups if the sampled table proves insufficient.
4. **Phase 4: multi-region.**
   - Regional relays: done (`RELAYOPS_ROLE=relay`, see "Regional relays").
   - Per-region Redis.
   - Control-plane leader election through a Kubernetes Lease: done (`RELAYOPS_LEADER_ELECTION=kubernetes`, verified on kind with failover in 24 s).

## Open questions

- AI budget calls now add a network round trip per AI request on gateway-only nodes. Should gateways hold short-lived local budget leases instead (reserve a block, settle in batches)? This is a Phase 3 candidate.
- With several control-plane replicas, a gateway's ticks reach only the replica its request lands on, so a console connected to another replica sees less traffic. Should ticks fan out through PostgreSQL NOTIFY, or through Redis when it is configured?
- Should the node API move to its own listener port, so it can be exposed to remote gateways without exposing the console?
