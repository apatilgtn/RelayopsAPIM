# Security, release integrity and support policy

The CI workflow now requires Go vulnerability analysis, high/critical container
scanning, a software dependency inventory, SPDX SBOM, race/database tests,
browser journeys and image/chart packaging. A tagged build prepares a **draft**
release only after all jobs pass. Configure the `enterprise-release` GitHub
environment with required platform-administrator reviewers and protected tags
before enabling it. Restrict workflow changes through branch protection. Pin
third-party actions to reviewed immutable commits in the release environment;
the repository's current versioned action references need periodic review.

Verify a downloaded release before unpacking or executing it:

```sh
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/OWNER/REPOSITORY/.github/workflows/ci.yml@refs/tags/VERSION" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
sha256sum -c SHA256SUMS
```

Replace OWNER/REPOSITORY/VERSION with the approved repository and exact tag; do
not use a permissive certificate identity pattern. Retain the SBOM, vulnerability
report, signature bundle, checksums, test reports and release passport together.
Deployment scripts must use the reviewed image digest, never a mutable `latest`.
Signing failure prevents a signed draft; it must never fall back to unsigned.

Tool references: [Sigstore verification](https://docs.sigstore.dev/quickstart/quickstart-ci/),
[Syft SBOM action](https://github.com/anchore/sbom-action),
[Trivy vulnerability scanning](https://github.com/aquasecurity/trivy/blob/main/docs/guide/scanner/vulnerability.md).

## Proposed support policy — activate only with named ownership

Support the current minor release and preceding minor for 90 days after the
new minor is published. Patch releases contain security and correctness fixes;
unsupported versions require upgrade. Publish a version/date support matrix
with each release. This policy is a release requirement, not an existing SLA.

Security triage targets after ownership is assigned: critical issues within one
business day, high within three business days; mitigation or patch targets of
72 hours and 14 days respectively. Document exceptions, customer impact,
workarounds and revised dates. These are proposed internal response targets,
not customer availability commitments. Assign a security contact and private
disclosure channel before accepting pilots. Do not invent a contact address.

## Audit archive and retention

Audit records are preserved by default. A platform administrator exports up to
10,000 oldest records using `GET /api/audit/archives/export?before=RFC3339`.
The download contains action/identity metadata and omits sensitive payloads and
configuration. Its digest covers the canonical JSON `records` array. Store it in
the approved immutable archive and verify the digest. Repeat after each batch
only if an explicit retention purge is intended; an export without purge returns
the same oldest records.

Deletion requires **both** `RELAYOPS_AUDIT_RETENTION_DAYS` set to a positive
integer and a platform superadministrator POST to
`/api/audit/archives/{archive_id}/purge` with
`{"archived":true,"digest":"sha256:..."}`. All exported records must be older
than retention. Only that exact exported ID set is deleted. A purge audit and
receipt update commit atomically; replay is idempotent. The caller's archival
confirmation is an explicit assertion; RelayOps does not independently verify
the destination archive. Back up the database and archive receipts as well.

## Independent assessment package

Provide the assessor: architecture and trust boundaries; role/tenant matrix;
HTTP/admin/node API inventory; authentication and secret recovery procedure;
SBOM/dependency inventory; scanner output and exceptions; gate/race/browser
results; redacted passports/audit sample; deployment and failure measurements;
known limitations; test credentials for an isolated environment; and rules of
engagement with a named owner. Commissioning and completion are external
milestones. Internal tests cannot mark the independent assessment as passed.
