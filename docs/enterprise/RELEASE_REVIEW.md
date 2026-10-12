# Enterprise release review — draft

Do not publish until a platform administrator records approval for this exact
commit and the required correctness, database/race, browser and packaging jobs
pass. Scanning or signatures alone do not establish enterprise readiness.

- [ ] Exact tag, commit, binary checksum, SBOM and signature verified.
- [ ] High/critical vulnerability findings resolved; any exception has an owner,
  expiry, impact assessment and explicit security approval in the release record.
- [ ] AWS production preflight and isolated two-AZ failure scenarios measured.
- [ ] Provider-specific database failover result recorded separately.
- [ ] Fresh-environment recovery includes secrets and authenticated upstream access.
- [ ] Independent assessment completed and findings disposition approved.
- [ ] Pilot outcomes, limits, support owner and escalation path recorded.
- [ ] Known limitations and migration/rollback procedure reviewed.

The shipped materials prepare these milestones. They do not certify that an
assessment, AWS drill, pilot, or operational service commitment has occurred.
