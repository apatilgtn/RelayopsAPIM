# Capacity benchmark: 9 October 2026

How much traffic one RelayOps gateway node handles, measured with
[`cmd/bench`](../../cmd/bench/main.go) through
[`scripts/bench-capacity.ps1`](../../scripts/bench-capacity.ps1). Treat these
figures as a **lower bound**. Everything ran on one shared developer laptop,
so the gateway competed for CPU and memory with the load generator, the
upstream, PostgreSQL, Redis, Docker Desktop and the IDE.

## Setup

| | |
|---|---|
| Host | Intel Core i7-10700T (8 cores / 16 threads, 2.0 GHz base, 35 W), 16 GB RAM, Windows 11 |
| Build | RelayOps `0110a68`, Go 1.26.0 |
| Colocated | PostgreSQL 15.5, Redis 5.0.14 (Windows port, no persistence), mock upstream (`cmd/mockupstream`, no added latency), load generator |
| Request | `GET` through the gateway to the mock upstream, ~0.5 KB JSON response, HTTP/1.1 keep-alive |
| Open loop | Fixed request rate for 15 s after a 3 s warm-up. Latency is measured from each request's scheduled send time, so queueing counts (no coordinated omission). A step is *sustained* when p99 ≤ 50 ms and errors ≤ 0.1%. |
| Closed loop | 64 or 256 workers sending back-to-back for 15 s (maximum throughput) |

Scenarios:

- **upstream-direct**: load generator to mock upstream, no gateway. This is the
  ceiling set by the host, generator and upstream.
- **combined-open**: one process with proxy and control plane, no
  authentication, every request logged to PostgreSQL.
- **combined-apikey**: API key, subscription check and distributed (Redis)
  rate limit, every request logged.
- **combined-apikey-sampled**: same, with `RELAYOPS_LOG_SAMPLE_RATE=0.05`
  (errors always kept).
- **split-gateway-apikey-sampled**: a gateway-only node (`RELAYOPS_ROLE=gateway`,
  no database credentials). It gets signed config from a separate
  control-plane process and ships its 5% log sample over the node API.

## Results

Two runs. Run 2 repeated the API-key scenarios from a clean checkout of `0110a68`.

### Latency at fixed rates (open loop): p50 / p99 in ms

| Scenario | 1,000 rps | 2,000 rps | 4,000 rps | 6,000 rps |
|---|---|---|---|---|
| upstream-direct | 0.28 / 1.0 | 0.30 / 1.5 | 0.32 / 3.3 | 0.40 / 21 |
| combined-open | 0.75 / 8.8 | 0.76 / 15.5 | 0.88 / 17.8 | 1.03 / 36.5 |
| combined-apikey (run 1) | 1.38 / 19.5 | 0.89 / 20.8 | 1.74 / 49.1 | ✗ (p99 557) |
| combined-apikey (run 2) | 0.87 / 12.2 | 0.91 / 13.2 | ✗ (p99 3,095) | |
| combined-apikey-sampled (run 1) | 0.69 / 3.9 | 0.73 / 13.4 | ✗ (p99 61.7) | |
| combined-apikey-sampled (run 2) | 0.84 / 4.2 | 0.88 / 12.5 | ✗ (p99 473) | |
| split-gateway-apikey-sampled (run 1) | 0.77 / 18.2 | 0.78 / 12.5 | 0.91 / 25.0 | ✗ (p99 193) |
| split-gateway-apikey-sampled (run 2) | 0.85 / 4.3 | 0.88 / 8.2 | ✗ (p99 456, 1.6% refused connections) | |

The mock upstream alone sustained 12,000 rps with p99 5.5 ms.

### Maximum throughput (closed loop, 64 workers)

| Scenario | Run 1 | Run 2 |
|---|---|---|
| upstream-direct | 37,048 rps | |
| combined-open | 8,644 rps (p99 41 ms) | |
| combined-apikey | 7,348 rps (p99 45 ms) | 6,824 rps (p99 40 ms) |
| combined-apikey-sampled | 9,842 rps (p99 27 ms) | 8,006 rps (p99 27 ms) |
| split-gateway-apikey-sampled | 11,295 rps (p99 22 ms) | 8,558 rps (p99 24 ms) |

With 256 workers, throughput stayed flat or dropped slightly and p99 rose to
80–150 ms. The node was saturated.

## What this shows

1. **Every run sustained 2,000 rps on one node, with the full policy chain.**
   At that rate the chain was API key, subscription, Redis rate limit and
   request logging. Median latency stayed under 1 ms and p99 at or under
   21 ms (8–13 ms in run 2). In about half the runs, 4,000 rps was also
   sustained. Above that the shared host saturates: p99 grows to hundreds of
   milliseconds, and once Windows' accept backlog overflows, connections are
   refused.
2. **Peak throughput is 7,000–11,000 rps per node on this laptop.**
   Production hardware without colocated load generation, database and
   upstream should do better. That has not been measured yet.
3. **Sampling request logs raises capacity by 10–35%.** With 5% sampling and
   errors always kept, closed-loop throughput was 8,000–9,800 rps against
   6,800–7,300 rps with every request logged. Persisting every request costs
   the gateway work, even with COPY batching.
4. **Split mode costs nothing on the request path.** A gateway-only node with
   no database matched or beat the combined process at every rate. Config
   arrives asynchronously, and logs leave in background batches.
5. **This is not a competitor comparison.** Published numbers for Kong, Tyk
   and others come from different hardware, payloads and plugin chains. A
   fair comparison needs the same host and an equivalent policy chain.

## Reproduce

```powershell
pwsh scripts/bench-capacity.ps1 -WorkDir C:\tmp\relayops-bench
# a subset:
pwsh scripts/bench-capacity.ps1 -WorkDir C:\tmp\relayops-bench -Only combined-apikey,split-gateway-apikey-sampled
```

The script builds the binaries and creates its own database
(`relayops_bench`). It starts a private Redis on port 6390, uses ports
18080/18081/19090/19092/19093/27070, and cleans up afterwards. Per-step JSON
reports land in `<WorkDir>\results`.

Next measurement: dedicated Linux hosts (gateway, upstream and load generator
on separate machines), plus the effect of WASM plugins and MCP inspection on
latency.
