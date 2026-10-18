// k6 gateway load/latency benchmark.
// Drives traffic through the RelayOps gateway to a mock upstream and records
// throughput, latency percentiles and error rate. Measures gateway overhead
// (the upstream path /status/200 adds no artificial latency).
//
// Local run:  k6 run test/load/gateway.js
// Override:   TARGET=http://host:8080/bench/status/200 k6 run test/load/gateway.js
import http from "k6/http";
import { check } from "k6";

const TARGET = __ENV.TARGET || "http://127.0.0.1:8080/bench/status/200";

export const options = {
  scenarios: {
    ramp: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: "15s", target: 50 }, // warm up to 50 virtual users
        { duration: "30s", target: 50 }, // hold
        { duration: "10s", target: 0 }, // ramp down
      ],
      gracefulRampDown: "5s",
    },
  },
  thresholds: {
    // Report-friendly guardrails; a healthy gateway clears these comfortably.
    http_req_failed: ["rate<0.01"], // <1% errors
    http_req_duration: ["p(95)<500"], // p95 under 500ms
  },
};

export default function () {
  const res = http.get(TARGET);
  check(res, { "status is 200": (r) => r.status === 200 });
}
