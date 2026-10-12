# Bounded pilot package — no customer outcomes claimed

Entry conditions: noncritical HTTP/OpenAPI workload, one active fleet-wide
canary, named platform administrator, published subscription policy, verified
release, passed production preflight, private control-plane connectivity,
authorized secret recovery, reviewed capacity envelope and escalation contacts.
Existing database and secret providers remain in use. AI cost figures are
estimates until authoritative pricing, conservative reservation and durable
settlement reconciliation are implemented. Kubernetes is not certified here.

Before inviting a design partner, fill in this operational agreement:

| Item | Required value |
|---|---|
| Pilot/customer and authorized workload | Unassigned |
| Start, end and stop conditions | Unassigned |
| Primary support owner and backup | Unassigned |
| Hours and incident contact channel | Unassigned |
| Security escalation and disclosure owner | Unassigned |
| Database, Redis and secret-provider owner | Unassigned |
| Measured request rate, payload/concurrency limits | Unmeasured |
| Measured latency/error envelope | Unmeasured |
| Recovery and rollback evidence | Pending AWS drills |
| Availability/RTO/RPO commitment | None until measured and owned |

Measure each session with timestamps and evidence IDs: discovery→registration;
registration→approval; first successful authorized gateway request; draft→safe
release; stale/unsafe changes blocked; canary abort and rollback convergence;
recovery duration and lost/duplicated logs. Separate usability failures, expected
policy rejections and infrastructure faults. Record numerator/denominator for
completion rates, workload, version, environment and observation period.

Stop on unexplained authorization success, cross-tenant exposure, missing audit
events, uncontrolled spend, unbounded data loss, or recovery outside the agreed
pilot budget. Escalate to the assigned owner, preserve evidence and revert through
the tested procedure. Publish customer names, quotes or outcomes only with their
explicit written permission. Do not advertise contractual reliability based on
synthetic tests or an incomplete restore.
