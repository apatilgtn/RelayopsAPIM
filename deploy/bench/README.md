# Gateway comparison harness

Runs RelayOps, Kong (3.9, DB-less) and Tyk OSS (5.8) one at a time, behind
the same upstream and the same two policies, and measures them with the same
load generator ([`cmd/bench`](../../cmd/bench/main.go)).

| Scenario | Policy on every gateway |
|---|---|
| `open` | No authentication: routing and proxying only |
| `key` | API-key authentication, plus a distributed rate limit checked in Redis on every request (limit set high enough never to trip) |

The script checks that each gateway's `key` route refuses requests without a
key and accepts the provisioned key before measuring. A gateway that skipped
the policy would otherwise look fast.

RelayOps runs twice:

- **`relayops`**: every request is written to PostgreSQL.
- **`relayops-sampled`**: 5% of successful requests are logged; errors are
  always logged.

Kong and Tyk run with their default logging off (no access log, no Tyk
analytics). Compare `relayops-sampled` with them for like-for-like
overhead, and `relayops` for the cost of a full audit trail.

## Run

```sh
deploy/bench/run.sh                                   # everything (~25 min)
GATEWAYS="relayops kong" RATES=2000,8000 STEP=30s deploy/bench/run.sh
```

Requirements: Docker with Compose, Go (to build the tools image) and
Python 3. Results go to `deploy/bench/results/`: one JSON file per
gateway and scenario, plus `summary.md`.

Open loop: fixed request rates. Latency is measured from each request's
scheduled send time, so queueing counts. A rate is *sustained* when p99 ≤
50 ms and errors ≤ 0.1%. Closed loop: 64 and 256 workers sending
back-to-back (peak throughput).

## Fairness

- **Isolation.** CPU sets keep the components apart: the gateway gets cores
  0-3, the upstream 4-5, Redis 6, PostgreSQL 7, and the load generator
  8-15. Override them with `GATEWAY_CPUS`, `UPSTREAM_CPUS`, `REDIS_CPUS`,
  `POSTGRES_CPUS` and `BENCH_CPUS`.
- **Container networking for everyone.** All components run as containers
  on one Docker network, so no gateway gets a cheaper network path.
- **Comparable defaults.** Kong runs 4 nginx workers (matching its 4 cores),
  and Go-based RelayOps and Tyk use the cores they are given. Nothing else
  is tuned.
- **Reporting.** Publish results together with the hardware, the Docker
  version, the image tags and the exact command.

## On dedicated hardware

For numbers worth publishing, run the script on a quiet Linux host with at
least 16 cores, for example an AWS `c7i.4xlarge`. Nothing else should run
on the host. Repeat each run three times, and report the median and the
spread.

A laptop gives only indicative numbers, because background load moves p99.
To separate the load generator from the gateway, run the stack on one
machine and point `cmd/bench` at it from another.
