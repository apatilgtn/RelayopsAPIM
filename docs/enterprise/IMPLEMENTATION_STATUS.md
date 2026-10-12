# Enterprise readiness delivery

This delivery implements the guided HTTP/OpenAPI release and consumer journeys and prepares the AWS reliability, release-security and pilot work. It does not certify availability, recovery guarantees or an independent assessment.

## Available in the product

- **Release workspace** is a primary console entry. API detail and Releases offer **Start safe release**. Saved tenant-scoped drafts remain separate from live configuration; versions reject conflicting edits. Review uses declarative plan hashes and the current fleet baseline. Candidate deployment and draft/deployment linking commit together. Retried launches return the same candidate.
- Required suites, immutable run inputs, environment and credential versions, exact revision, expiry, completeness and skipped tests are checked by one server gate evaluator. Verification and both promotion paths use it; promotion repeats the check inside its transaction. A newer unfinished run cannot fall back to older passing evidence. Unconfigured gates block promotion. Legacy overrides cannot bypass these checks.
- Promotion requires an explicit authenticated platform-administrator review. Promotion, deployment status and passport/evidence seal commit together. Concurrent duplicate promotions return the same passport. Evidence downloads use the sealed decision, rather than reevaluating later test results.
- AI qualification requires a completed qualifying server evaluation. Unknown evaluation IDs and external attestations cannot qualify a route. Average latency is labeled as average; AI costs are labeled estimates.
- **Get connected** carries the published API through requirements, plan, application registration, policy-based access, an explicit first request and usage. Pending approval stays pending. Generated keys are shown once and kept out of browser persistence. Completion requires a successful authorized response attributed to the selected API by the gateway.
- Production mode adds actionable startup/preflight rejection for insecure credentials, trust/signing, transport and recovery storage settings. Gateway cache writes are atomic and use durable configured storage. Existing local installations default to development.
- Audit records are preserved by default. Platform-only archival exports create receipts; deletion requires configured retention, exact archive digest confirmation and an audited transaction.

## Prepared operational deliverables

- [AWS reference](../../deploy/aws/ENTERPRISE_REFERENCE.md): a private two-AZ VM foundation, public TLS API load balancer, internal TLS control plane, restricted listeners, retained encrypted gateway disks and production Compose profiles. Existing database and secret providers are retained. AMI configuration, credentials, trust, storage mounts and shared Redis must be supplied and verified by the operator.
- [Failure drills](FAILURE_DRILLS.md): isolated, explicitly authorized harness, workload/acceptance manifest, reversible network interruption, node replacement and rolling upgrade procedures. Provider failover and full database-plus-secret restore require reviewed provider recovery procedures. Dry-run output is not a passing recovery result.
- [Release security](RELEASE_SECURITY.md): CI vulnerability/container scanning, SBOM/dependency inventory, checksums and signed draft releases, verification instructions, proposed patch/support policies and independent assessment inputs. Configure the protected enterprise-release environment and administrators before relying on its approval gate.
- [Pilot package](PILOT_PACKAGE.md): prerequisites, measured-limit templates, known limitations, support/escalation ownership and outcome/permission templates. Owners and measured commitments remain to be assigned.

## Upgrade considerations

1. Back up the database and review migrations 025–027. The single-canary unique index intentionally rejects an existing database with multiple active canaries; resolve that ambiguity before migration.
2. Prepare replacement administrator/node credentials, TLS, signing/trust keys and persistent gateway cache/spool storage **before** switching an existing installation to production. Follow the AWS migration procedure and run `relayops preflight` with the actual runtime environment.
3. Production regional relay mode is rejected until its persistent signed recovery path is supported. Use direct gateway connections to the redundant control plane; development relay workflows remain available.
4. Classic and APIOps promotion now require configured passing gates and `reviewed:true` from a platform administrator. Old automation that relied on missing gates, actor fields or overrides must be updated. Fleet-wide release semantics remain.
5. Historic releases without a sealed decision cannot produce a newly claimed authoritative promotion bundle. Create a new qualified release for a sealed bundle.
6. Consumers re-enter their key after a browser refresh; plaintext is deliberately not restored from local storage. Existing application metadata can remain persisted without credentials.

## Verification and remaining release gates

Local verification outputs are under `artifacts/enterprise-*`. The full browser suite passed 31 tests; the final guided rerun passed all three cases, additionally exercising abort and the navigation-race correction. Frontend checks passed 20 tests. Linux executables and the distroless container built; container preflight accepted development and rejected insecure production. Helm lint/render and Go static analysis completed.

The initial all-package run hit three-minute limits in the database-heavy and WebAssembly packages, and exposed a concurrency-test fixture with an invalid timeout. The fixture was corrected. Administrator, store and gateway packages subsequently passed with twelve-minute limits (226.903s, 223.079s and 301.110s). Final outputs are retained alongside the original failures. The new abort browser case also exposed and corrected stale asynchronous rendering and the distinction between an explicit abort and an automatic rollback.

Race verification is still a release requirement: this Windows Go environment could not build its race runtime. CI retains the required race run with PostgreSQL and Redis. CI vulnerability scanning, SBOM/signature generation and protected release review must execute against the actual release commit; local packaging is not a scanner or signature result. AWS provisioning and failure injection, provider recovery proof including secrets, independent security assessment and design-partner pilots have **not** been executed by this implementation.

## Reviewed UI

- [Desktop release review](../../artifacts/enterprise-release-desktop.png)
- [Phone release review](../../artifacts/enterprise-release-mobile.png)
- [Phone consumer connection](../../artifacts/enterprise-consumer-mobile.png)
