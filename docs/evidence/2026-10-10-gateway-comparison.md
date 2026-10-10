# Gateway comparison: RelayOps, Kong and Tyk (10 October 2026)

This is one run of [`deploy/bench`](../../deploy/bench/README.md) on a
developer laptop. It shows where RelayOps stands, and it is **not a
publishable benchmark**: that needs dedicated Linux hardware and repeated
runs (see the harness README).

## Setup

| | |
|---|---|
| Host | Intel Core i7-10700T (8 cores / 16 threads, 35 W), Windows 11, Docker Desktop 28.3 (WSL 2 VM with 16 vCPUs and 8 GB); other applications running |
| Gateways | RelayOps `888b94c` (Go 1.26), Kong 3.9 (DB-less, 4 nginx workers), Tyk OSS 5.8.1 |
| CPU sets | gateway cores 0-3; upstream 4-5; Redis 6; PostgreSQL 7; load generator 8-15 |
| Upstream | `cmd/mockupstream`, no added latency, ~0.5 KB JSON |
| `open` | No authentication |
| `key` | API key, plus a Redis-backed rate limit checked on every request. Before measuring, each gateway was verified to refuse a request without a key and accept the provisioned key. |
| RelayOps logging | `relayops`: every request written to PostgreSQL. `relayops-sampled`: 5% of successful requests, all errors. Kong and Tyk ran without access logs or analytics. |
| Load | Open loop at 1,000 / 2,000 / 4,000 / 8,000 rps, 15 s per step (latency from scheduled send time). Then closed loop with 64 and 256 workers. |

## Results

| Gateway / scenario | Max sustained (p99 ≤ 50 ms) | p50 / p99 ms @ 1,000 | @ 2,000 | @ 4,000 | Peak (closed loop) |
|---|---|---|---|---|---|
| Kong, open | 2,000 rps | 1.22 / 3.9 | 1.09 / 4.6 | ✗ (p99 63) | **14,732 rps** (p99 64) |
| Tyk, open | **4,000 rps** | 1.46 / 6.1 | 1.32 / 6.3 | 1.26 / 21.0 | 10,080 rps (p99 24) |
| RelayOps sampled, open | 2,000 rps | 1.42 / 3.4 | 1.34 / 10.8 | ✗ (p99 50.4) | 7,875 rps (p99 30) |
| RelayOps, open | 2,000 rps | 1.45 / 5.5 | 1.37 / 15.2 | ✗ (p99 318) | 5,517 rps (p99 128) |
| Kong, key | 1,000 rps | 4.99 / 45.4 | ✗ (p99 546) | | 1,647 rps (p99 128) |
| Tyk, key | **4,000 rps** | 1.67 / 15.3 | 1.49 / 16.2 | 1.45 / 26.0 | **6,782 rps** (p99 35) |
| RelayOps sampled, key | 2,000 rps | 1.86 / 5.6 | 1.79 / 8.5 | ✗ (p99 52) | 5,652 rps (p99 36) |
| RelayOps, key | 2,000 rps | 2.12 / 16.4 | 2.19 / 41.5 | ✗ (p99 546) | 4,500 rps (p99 52) |

## What this shows

1. **Raw proxying: RelayOps trails both.** On 4 cores, Kong's nginx core
   peaks at about 1.9× and Tyk at about 1.3× RelayOps with sampled logging.
   This is the main performance gap to work on. Profiling the open path
   (header handling, decision trail, per-request allocations) comes next.
2. **With API keys and a distributed rate limit, RelayOps is mid-pack.**
   That is the configuration real APIs run. RelayOps (sampled) reached 5.7k
   rps, about 3.4× Kong's 1.6k, whose Redis rate-limiting plugin makes a
   blocking Redis round trip per request. Tyk reached 6.8k, ahead of
   RelayOps by about 20%.
3. **Logging every request costs 20–30% of capacity.** RelayOps can write
   each request to PostgreSQL (the full audit trail its release safety and
   replay features use). That is slower than sampling, and the PostgreSQL
   core became the limit. Kong and Tyk did no equivalent work in this run.
4. **Median latency is close across all three**, at 1.1–2.2 ms below
   saturation. The differences are in tail latency and in the saturation
   point.
5. **Run-to-run variance on this laptop is large.** In particular, p99
   values at the saturation step swing by several times between runs (see
   [the earlier capacity runs](2026-10-09-capacity-benchmark.md)). Treat
   differences under about 25% as noise.

## Reproduce

```sh
deploy/bench/run.sh
```

To publish a claim, run the harness three times on an isolated 16-core
Linux host (for example `c7i.4xlarge`), report medians with min/max, and
include this page's setup table.
