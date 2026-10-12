# AWS virtual-machine production reference — certification pending

Use `enterprise-reference.json` to prepare four private hosts in two existing
availability zones. Its public HTTPS load balancer accepts API traffic only;
the control-plane load balancer is internal. No SSH ingress or public instance
addresses are created. VPN/private connectivity is required for administrators.
Provide ACM certificates, existing VPC/subnets, a maintained golden AMI and a
restricted SSM instance profile. The AMI must include Docker Compose, the verified
RelayOps runtime, and the CloudWatch/SSM agents. Provisioning incurs AWS charges
and is a separate operational approval.

The template creates hosts, security groups, durable encrypted data volumes and
load balancers. It intentionally does not copy secrets into CloudFormation
parameters or user data, replace the database provider, or claim that an empty
host is a functioning RelayOps node. Configure each host through SSM:

1. Verify the release signature and checksum. Install the production compose
   file, trust certificates and a mode-0600 `runtime.env` from the existing
   authorized secret-management system. Never put values into shell history.
2. Mount each host's retained encrypted EBS device at `/var/lib/relayops`, persist
   the mount in fstab, and give the container runtime UID write permission.
   `RELAYOPS_PERSISTENT_DATA=true` is an operator assertion, not a disk-durability
   detector. Confirm the mount with `findmnt` before starting.
3. Control planes share the existing database, signing key and secret-provider
   configuration. Set distinct node IDs, the internal TLS certificate paths,
   PostgreSQL leader election and a unique admin credential of 32+ characters.
   Configure `RELAYOPS_GATEWAY_URL` to the API HTTPS load balancer for Test Studio.
4. Issue a distinct node credential for each gateway using the existing node
   credential administration API. Configure the internal HTTPS control-plane DNS,
   CA trust, public verification key, persistent recovery directory and shared
   authenticated TLS Redis. Bootstrap in the private network before production
   traffic is enabled. Never use the shared dataplane token in production.
5. Set `PRIVATE_IP` and `RELAYOPS_IMAGE` (verified digest) in compose's `.env`.
   Run `docker compose -f compose.production.yaml --profile control-plane run --rm
   control-plane preflight` (or profile/service `gateway`). Resolve every issue.
   Then start the selected profile. Only the private service port is published;
   status listeners remain on host loopback. Configure load-balancer DNS and
   health probes, then verify both hosts acknowledge the same signed revision.

For an existing deployment, prepare replacement admin and per-node credentials,
signing/trust keys, TLS and durable mounts **before** setting `RELAYOPS_ENV=production`.
Revoke old shared/default credentials after both gateways have acknowledged the
new configuration. Development compose remains an explicitly local reference.

The database and secret provider retain their current recovery guarantees. An
external managed Redis endpoint is required; single-host Redis is not a redundant
distributed limiter. Provider-specific database failover is a separate drill from
a TCP interruption. Replacing a host requires fencing the old node, retaining or
restoring its EBS volume, rotating its credential, restoring secret access and
checking spool replay before increasing traffic. Never run two hosts with the
same recovery volume/node identity.

Certification requires the scenarios and measurements in
`../../docs/enterprise/FAILURE_DRILLS.md`, real AWS execution, independent
assessment and named operational ownership. Availability, RTO and RPO are
unmeasured until those results are recorded. Kubernetes parity follows later.
