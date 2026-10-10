# RelayOps soak test

**Result: PASS**

| | |
|---|---|
| Started | 2026-10-05T14:43:07+11:00 |
| Duration | 600 s |
| Gateway nodes | 2 (http://127.0.0.1:18080, http://127.0.0.1:18081) |
| Target / achieved rate | 300 / 300.0 requests per second |
| Requests | 180003 |
| Status codes | 200: 179977 |
| Transport errors | 26 |
| Failed requests | 0.014% |
| Latency through the gateway p50 / p95 / p99 / p99.9 / max | 0.7 / 2.3 / 3.6 / 7.6 / 40.7 ms |
| Upstream called directly p50 / p99 (baseline) | 0.0 / 0.7 ms |
| Revisions published / canaries / promotions | 14 / 13 / 13 |
| Requests served by a canary | 15588 |
| Final revision, fleet converged | rev_16, true |
| Failed configuration changes | 0 |

Latency is measured end to end by the load generator on the same machine as the
gateway, the mock upstream and PostgreSQL.

## Sample errors

```
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
Get "http://127.0.0.1:18081/soak-1791171786/echo": dial tcp 127.0.0.1:18081: connectex: No connection could be made because the target machine actively refused it.
```
