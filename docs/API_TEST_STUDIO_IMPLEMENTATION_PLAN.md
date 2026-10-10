# RelayOps API Test Studio — implementation plan

Status: proposal, 5 October 2026. No implementation or deployment is authorized by this planning request. Local and AWS applications remain stopped.

## 1. Product outcome and boundaries

Let developers create repeatable API tests, operators compare releases, and administrators require trustworthy test evidence before promotion. Extend RelayOps's embedded Go/HTML/CSS/JavaScript product rather than creating a separate SaaS or replacing its authentication.

First supported surface: HTTP REST/JSON against registered RelayOps APIs through approved gateway targets. Tests may exercise other response formats with status/header/body-text assertions. Arbitrary external URL testing, JavaScript execution, scheduled monitors, mocks, load generation, GraphQL-specific tooling and gRPC are deferred.

The distinction is release-aware testing: identify the actual revision, consumer and gateway decision, compare baseline and candidate, then attach evidence to a release. Existing policy replay is simulation, not upstream response replay, and stays separately labelled.

## 2. Existing foundation and gaps

- `web/static/portal.js`: API workbench and OpenAPI operation selection.
- `internal/admin/server.go`: portalTry, authentication, tenant scopes, request logs, comparison and release handlers.
- `internal/admin/portal.go`: gateway address configuration and request validation helpers.
- `internal/policy/evaluator.go`: shared live/simulation decision engine.
- `internal/gateway/snapshot.go`: caller-cohort selection; no trusted arbitrary revision selector.
- `internal/gateway/gateway.go`: request diagnostics and cohort metadata.
- `internal/store/rollout.go`: durable rollout and atomic promotion.
- `internal/config/hosted.go`: current runtime Vault loader; not a tenant testing-secret manager.
- Existing CLI, Go tests, database fixtures and embedded design system can be extended.

Gaps: saved suites, versioned assertions, environment/credential references, durable jobs, test result retention, deterministic release targeting, and transactional promotion gates.

Before exposing Studio to multiple tenants, finish tenant-aware live events and ensure all new routes use explicit scope and capabilities. Harden browser-bound OIDC state and verified-email handling before relying on enterprise SSO.

## 3. UI and user journeys

Add Test Studio to the console Manage group and preserve existing hash routes. Proposed routes: #/tests, #/tests/suites/{id}, #/tests/runs/{id}. Portal users retain a separate permission-scoped workbench with Save as test and My test suites when allowed.

Suite workspace: left request tree; central method/path builder with Params, Headers, Body, Authentication and Assertions tabs; right/bottom response, assertion results and gateway diagnosis. Keep endpoint selection separate from environment selection and credential selection. Environment identity is always visible next to Run.

Run screen: queued/running/completed/cancelled/interrupted state, progress, passed/failed/skipped assertions, duration, actual revision and a reason for unavailable diagnostics. Failures show expected versus actual values with masking. A connection failure is a run error, not an HTTP assertion failure; skipped assertions are not passes.

Canary comparison: baseline and candidate columns, side-by-side assertion changes, response diff with ignored paths, timing measurements and gate outcome. Functional timing measurements are not advertised as load benchmarks.

Canary overview gains a Tests panel linking the latest eligible run, failed required assertions and gate reasons. Configure optional gates before enabling enforcement; read-only users see evidence without mutation controls.

All views use Inter, existing green/neutral tokens, local SVG icons, keyboard navigation, labelled fields, focus-trapped dialogs and reduced motion. Cover populated, empty, loading, cancellation, worker outage and database-unavailable states at desktop/tablet/mobile sizes.

## 4. Data model

Use versioned immutable run inputs; avoid interpreting results against a suite that changed after execution.

| Entity | Essential data |
|---|---|
| test_suites | tenant_id, API association, name, ownership, visibility, current version, timestamps |
| test_suite_versions | immutable request/assertion definitions, schema version, content hash, author |
| test_environments | tenant_id, approved gateway target, public variables, credential bindings, revision counter |
| test_credentials | tenant/owner scope, provider reference, credential version, use permissions; no plaintext secret |
| test_runs | tenant, suite/version/hash, environment/version, actor, mode, lifecycle state, timestamps, cancellation, immutable inputs, expiry |
| test_run_steps | request identity, expected/observed revision, HTTP/timing summary, assertion outcomes, trace/log references, redacted preview |
| test_run_artifacts | optional private object key, digest, bytes, content type, retention, deletion state |
| test_jobs | run_id, claim lease, heartbeat, attempt, worker identity, next availability |
| test_gate_policies | tenant/API or release scope, required suite hashes, environment, freshness, version, override rules |
| test_gate_evidence | run references, verified target fingerprint, computed eligibility and reasons |

Use UUIDs and composite tenant relationships to prevent cross-tenant references. Index tenant/time, suite/time and queue eligibility. Terminal run evidence is immutable apart from retention cleanup.

Implement migrations through the repository's existing migration runner, using the next available number. Enable RLS and revoke anonymous/client grants on hosted tables; Go server authorization remains mandatory because its database role can bypass RLS. Do not expose Vault or Storage metadata writes through the Data API.

Inline previews are redacted and capped. Optional large artifacts use private Storage with short-lived authorized retrieval; local filesystem persistence remains available for installations without Supabase. Do not require Storage for small suites/runs.

## 5. Proposed API contracts

These are new endpoints, not current capabilities.

| Operation | Endpoint |
|---|---|
| List/create suites | GET/POST /api/tests/suites |
| Read/update/archive suite | GET/PATCH/DELETE /api/tests/suites/{id} |
| Read version | GET /api/tests/suites/{id}/versions/{version} |
| List/create/update environments | GET/POST /api/tests/environments; PATCH /api/tests/environments/{id} |
| Create asynchronous run | POST /api/tests/runs |
| List/read results | GET /api/tests/runs; GET /api/tests/runs/{id} |
| Cancel run | POST /api/tests/runs/{id}/cancel |
| Baseline/candidate comparison | GET /api/tests/runs/{id}/comparison |
| Validate/preview import | POST /api/tests/import/preview |
| Apply reviewed import | POST /api/tests/import/apply |
| Export portable suite | GET /api/tests/suites/{id}/export |
| Gate policy | GET/PUT /api/tests/gates/{scope} |
| Release evidence | GET /api/revisions/{id}/test-evidence |

Run creation returns 202 with run_id, status and result URL. Require an idempotency key to prevent accidental duplicate runs. Suite edits use a version precondition and return 409 on conflicts. Use bounded pagination and existing error envelopes. Progress may initially use polling; tenant-filtered events follow when available. Portal routes get a dedicated ownership-authorized adapter rather than opening all administrative endpoints to developers.

## 6. Runner and assertion engine

Add `internal/testingstudio` for validation, interpolation, assertions, HTTP execution, comparisons and evidence evaluation. Add storage methods and explicit administration handlers. Start with a worker inside the existing Go process behind a feature flag; an optional dedicated worker command can follow without changing run contracts.

Persist a job before returning success. Claim it with a short transaction using row locks/SKIP LOCKED and a heartbeat lease. Do not hold a database transaction during network I/O. Enforce global and per-tenant bounded concurrency. Write step results durably and provide cooperative cancellation.

Snapshot suite, environment metadata, credential version references and target selection when the run is accepted. Resolve secrets only when executing, never in exported definitions, queued plaintext or browser responses. A credential rotation/deletion invalidates or interrupts affected work rather than silently changing its identity.

First assertions: exact/range HTTP status, header presence/equality, content type, JSON path equality/existence/type, body text matching, response-time threshold and OpenAPI response validation for an explicitly supported version/subset. Publish unsupported schema features; do not silently pass them. Prefer bounded JSON-pointer selection in v1 over a scripting language.

Run requests sequentially within a suite first. Add explicit response-variable extraction with scoped lifetimes, masked secret-like values and no unrestricted expression evaluator. Missing variables and invalid assertions fail preflight before requests are sent.

Initial configurable limits: 100 requests per suite, 1 MiB request body, 5 MiB response read cap, 16 KiB stored redacted preview, 30-second request timeout, 10-minute run limit, 2 concurrent runs per tenant and 4 globally. Treat these as protective starting defaults, not validated capacity claims. Distinguish deadline, cancellation, truncation and assertion failure.

HTTP timeouts and request budgets apply to redirects and DNS/connect stages. Never automatically retry mutating requests. On crash/lease expiry, mark in-flight requests uncertain and interrupt the run; do not promise exactly-once HTTP delivery. Resume only explicitly safe work and ensure duplicate result writes are idempotent.

## 7. Network, credentials and execution permissions

Default target is a registered API through an administrator-approved gateway. No user-controlled absolute URL or upstream bypass. Normalize and validate path/Host headers; reject authority overrides and encoded traversal. Revalidate destinations after DNS resolution and on redirects; block metadata/link-local destinations. Approved private gateways are allowed via explicit administrator registration, not blanket access to private networks. Disable automatic redirects initially.

Create dedicated test capabilities: read, author, execute, manage environments, manage gates and override gates. Proposed role defaults: developer owns/executes own suites with authorized credentials; operator runs shared suites and reviews releases; auditor reads redacted results only; admin manages tenant definitions/gates; superadmin manages platform policy. Enforce API subscriptions and credential ownership independently of role.

Do not mint another consumer's production key for testing. Use authorized test consumer fixtures and sandbox data. Backend policy simulation covers revoked/unsubscribed scenarios separately; real identity tests require explicitly bound test identities.

Use a tenant-aware secret-reference adapter. Supabase Vault is one optional provider; existing runtime environment secret loading is not sufficient for scoped Studio credentials. Credentials must not appear in logs, traces, previews, exports or generated curl commands. Redact sensitive headers, configured JSON fields and returned tokens before persistence.

Production environment classification and allowed methods must be explicit. Read-only comparison is the default, but HTTP GET does not guarantee harmless backend behavior; API owners declare approved operations. Mutating scenarios require approved sandbox targets/data, and any baseline/candidate double execution is shown before Run. Avoid arbitrary scripts in the initial release.

## 8. Trustworthy revision comparison

Current percentage cohorts cannot deterministically select both releases. Implement an internal authenticated execution context that selects only the current baseline or current candidate for a permitted API. Strip any user-supplied testing selector at public ingress. Never create an unauthenticated force-revision header.

Candidate routing still performs normal consumer authentication, subscriptions and policies. Add observed revision/node/cohort and target fingerprint to a trustworthy runner result, not merely a client-spoofable response header. At first support the colocated runner/gateway; remote targets need authenticated transport and attestation before their results qualify for gates.

If the requested snapshot is absent, a node is not synchronized, a canary is aborted/promoted or the target changes mid-run, report unavailable/stale targeting; never substitute the current release and label it candidate. Recheck the release fingerprint before final evidence eligibility.

Compare status, selected headers, contract and JSON with configured ignore paths for timestamps/IDs. Store ignored fields and tolerance rules in the suite version so they are reviewable. A pass is limited to those assertions and observed requests, not an assertion of overall production health.

## 9. Promotion gates

Begin advisory: show passed, failed, missing, stale, interrupted or wrong-target evidence. Enforcement is explicitly enabled per policy after evidence is dependable. Require zero failed required assertions and no skipped required checks; absence of evidence never becomes a pass.

Eligibility includes exact candidate/baseline fingerprint, suite hashes, environment and credential versions, required scope coverage, freshness and a terminal successful run. A later suite/environment/gate change invalidates eligibility. Test traffic is tagged to separate synthetic reporting; whether it affects rollback evaluation is explicit and separately validated rather than silently altered.

Enforce promotion on the server in the same transaction as checking rollout state and committing promotion. Cover UI, direct API and any other promotion path. Avoid the time-of-check/time-of-use gap of trusting a browser badge. An authorized exception requires a reason and immutable audit entry; do not automatically promote or rollback based solely on a synthetic test failure in v1.

## 10. Delivery phases and acceptance

1. **Foundation and contracts:** finalize identities, capabilities, target policy, immutable model, limits and feature flags. Accept when tenant isolation, validation and migration/upgrade tests pass.
2. **Saved suites and UI:** OpenAPI/manual request creation, environments, visual assertions, versioned save and secret references. Accept when users can author without exposing secrets; unsupported inputs are reported.
3. **Durable execution:** queued runner, results, cancellation, timeouts and restart handling. Accept when results survive restart, unsafe requests are not auto-repeated and limits hold.
4. **Gateway-aware diagnosis:** actual revision/consumer/decision attribution and isolated test identities. Accept when requested and served revision are verifiably matched and public spoofing cannot select a revision.
5. **Release comparison:** controlled baseline/candidate runs, field differences, coverage and stale-state detection. Accept when known regressions are caught and missing evidence is explicit.
6. **Canary gates:** advisory panel, opt-in enforcement, transactional checks and audited overrides. Accept when direct API promotion cannot bypass a required gate and suite/release changes invalidate evidence.
7. **Portability:** selected Postman collection import, OpenAPI import, portable export and `relayopsctl test run` with useful exit codes. Accept with import compatibility reports, credential exclusion and CI examples. Do not claim full Postman compatibility.
8. **Operational release:** cleanup, private artifact retention, metrics, documentation, multi-host recovery and real capacity measurements. Accept with restore drills, resource budgets and a pilot operating guide.

Phases 1–3 are the usable testing MVP. Phases 4–6 deliver the distinguishing release-safety feature. Establish estimates after phase 1; calendar promises depend on scope and engineering capacity.

## 11. Verification matrix and operations

Unit tests: assertion semantics, interpolation, escaping/redaction, destination validation, comparison tolerances, fingerprints and gate eligibility.

Integration tests with isolated PostgreSQL/Redis: version conflicts, tenant ownership, queue leases, cancellation, duplicate result handling, deletion, migration and backup restore. Gate evidence remains tamper-resistant and tenant-scoped.

End-to-end: valid request, missing credentials, expired test identity, unsubscribed identity, quota scenario in isolation, upstream timeout, oversized body, mismatched revision, aborted candidate, baseline contract regression, passing gate and audited override.

Failure/security tests: DNS changes, redirect escape, metadata targets, secret-bearing responses, viewer permissions, worker crash during a write, database outage, stale lease and concurrent promote/update. No such test runs against production data by default.

UI: keyboard/focus, responsive view at 1440/1024/390, 200% zoom, no clipping, unavailable diagnostics and a cancelled run. Extend the existing JS test/CI steps and run DB-enabled Go/race checks rather than counting skipped integrations as passes.

Retention defaults: 7 days of detailed redacted results/artifacts and 30 days of compact summaries, configurable. Retain gate-linked compact evidence as needed by the audit policy. Track artifact bytes/database growth, queue depth, duration, interrupted runs, truncation and cleanup failures. Define tombstone/retry cleanup rather than orphaning private blobs.

Existing deployment remains a small single VM. Bounded workers share resources with the gateway, so profile impact before production concurrency increases. Supabase Free database/file quotas are finite: cap retention and skip raw body persistence by default. Scheduling and performance tests require separate resource/cost planning.

## 12. Implementation file map

Proposed additions: `internal/testingstudio/{definitions,validation,runner,assertions,comparison,evidence,secrets}.go`, `internal/store/testingstudio*.go`, repository-numbered migrations, `internal/admin/testingstudio*.go`, `web/static/test-studio.js`, and dedicated tests. Extend existing console navigation/design system, portal workbench, gateway trusted execution hook, promotion transaction and CLI commands. Keep schema and endpoints backwards compatible and preserve existing working-tree changes.

Feature flags separately control Studio visibility/execution, revision comparison and enforced gates. Disablement stops new work but preserves readable prior evidence. Ship through local isolated fixtures first, then a controlled pilot deployment only when the user asks to restart/deploy.
