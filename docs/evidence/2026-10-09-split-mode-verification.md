# Split-mode verification: load, TLS/mTLS and Kubernetes

Date: 9 October 2026. Machine: one Windows 11 developer workstation. The load generator, gateways, control plane, mock upstream and PostgreSQL 15 all ran on this machine. These are functional and comparative results, not capacity limits.

## Load: combined vs split (`cmd/soak`, 90 s, 300 req/s, publish → canary → promote every 20 s)

| | Combined node | Gateway-only node + control plane |
| --- | --- | --- |
| Requests / failed | 27,000 / 0 | 27,000 / 0 |
| Gateway latency p50 / p95 / p99 / p99.9 / max | 0.5 / 0.7 / 1.1 / 2.1 / 5.6 ms | 0.5 / 0.7 / 1.1 / 1.6 / 3.0 ms |
| Upstream direct p50 / p99 | 0.0 / 0.6 ms | 0.0 / 0.6 ms |
| Revisions / canaries / promotions | 2 / 1 / 1 | 2 / 1 / 1 |
| Requests served by the canary | 1,210 | 1,157 |
| Fleet converged on the final revision | yes | yes |

**The first split run failed convergence, and it was a fleet-status bug, not a split-mode bug.** The combined node from the previous run had stopped, but it kept counting for 15 minutes because fleet status considered any acknowledgement in the last 15 minutes. That run's tail latency was also wider: p99.9 6.8 ms and max 83 ms.

**The fix.** Nodes now heartbeat every 30 seconds, so the liveness window is 2 minutes (`store.NodeLivenessWindow`), with the test `TestIntegrationFleetStatusDropsSilentNodes`. The rerun in the table passed. Any node that is scaled down or replaced used to hold the fleet "not converged" for 15 minutes.

## TLS and mTLS between the planes (real binaries)

- **Setup.** A test CA issued a server certificate for the control plane and a client certificate for the gateway. The control plane ran with `RELAYOPS_ADMIN_TLS_CERT_FILE`, `RELAYOPS_ADMIN_TLS_KEY_FILE`, `RELAYOPS_ADMIN_CLIENT_CA_FILE` and `RELAYOPS_DATAPLANE_REQUIRE_CLIENT_CERT=true`.
- **Gateway with a certificate.** It used `RELAYOPS_CONTROL_PLANE_URL=https://…`, `RELAYOPS_DATAPLANE_CA_FILE` and the client certificate and key. It loaded configuration and reported `/readyz` as ready.
- **Gateway without a client certificate.** It was refused with `401 client_certificate_required`. It had no cache, so it exited at startup.
- **A client without a certificate**, such as a browser, still completes the TLS handshake with the admin listener.

## Kubernetes (kind, `mode: split`)

**Setup.**

- Single-node kind cluster with its own kubeconfig. The operator's kubectl context was not changed.
- Image built from the repository Dockerfile: 45.7 MB.
- In-cluster `postgres:16-alpine`.
- Installed with `helm install … --set mode=split --set gateway.replicas=2 --set controlPlane.replicas=1 --set logSampleRate=0.5 --wait`.

**Results.**

| Check | Result |
| --- | --- |
| Pods ready | 2 gateway pods, 1 control-plane pod, all `Running` |
| Gateway pod environment | No `RELAYOPS_DATABASE_URL`; has `RELAYOPS_ROLE=gateway`, the control plane URL and the node token from the chart Secret |
| Control-plane pod environment | Has the database DSN |
| Fleet status | The 2 gateway pods only (the control plane does not acknowledge), converged |
| `/api/system/runtime` | `role: control-plane`, `log_sample_rate: 0.5`, node API enabled |
| New API through the proxy Service | Served 481 ms after `POST /api/apis` |
| Traffic during `kubectl rollout restart` of the control plane | 167 of 167 requests returned 200 |
| Request logs | 85 rows for about 170 requests, consistent with 50% sampling, written by the control plane |

The cluster was deleted after the run.

**Not covered:**

- multiple nodes, or a managed Kubernetes service
- an ingress with TLS in front of the control plane
- long-duration soak
- `persistence.enabled` with a StatefulSet
