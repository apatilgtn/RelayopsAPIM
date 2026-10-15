# Certificate management

Status: phase 1 implemented (14 October 2026). Phase 2 is a proposal.

## Problem

RelayOps checked certificates well but did not manage them:

- Certificates were read from files once at startup. A renewal needed a restart.
- Nothing issued certificates. The AWS reference deployment relied on Caddy for Let's Encrypt.
- Upstream (backend) TLS trusted only the system roots. There was no private CA, no client certificate for upstream mTLS, and no SNI override.
- Nothing reported expiry. A lapsed certificate was found by callers.
- Client certificates were never checked for revocation.
- `DynamicCertManager.AddClientCA` wrote to a map that was never created, so a call would panic. The per-tenant pools it filled were never read.

## Goals

1. Rotate any certificate RelayOps reads without a restart or dropped connections.
2. Issue and renew public certificates with ACME.
3. Let each API trust a private CA and present a client certificate to its upstream.
4. Report every certificate's expiry: as metrics, per gateway node fleet-wide, as an Orbit signal and as a webhook alert.
5. Reject revoked client certificates (CRL).
6. Keep private keys out of the database, revisions, exports, logs and Orbit.

Non-goals for phase 1: storing certificates in Postgres, a console page for uploading them, and OCSP.

## Design

### 1. Listener certificates and hot reload (`internal/gateway/tls.go`)

`DynamicCertManager` holds an immutable `certSet` behind an `atomic.Pointer`: the default certificate, certificates by hostname (exact and `*.` wildcard), the client CA pool, a client CA pool per hostname, and the CRLs. Handshakes read the current set without locking. A reload builds a complete new set and swaps it in only when everything parses. A bad file therefore keeps the previous certificates, logs the error and increments `relayops_tls_reloads_total{result="error"}`.

`TLSConfig()` returns a config whose `GetConfigForClient` picks the client CA pool for the SNI hostname. When client authentication is on, `VerifyConnection` checks the CRLs.

Files (unchanged names, plus two):

| Variable | Use |
|---|---|
| `RELAYOPS_TLS_CERT_FILE`, `RELAYOPS_TLS_KEY_FILE` | Default gateway certificate |
| `RELAYOPS_TLS_CERTS_DIR` | `<host>.crt` + `<host>.key` per hostname (`_wildcard.example.com` for `*.example.com`). **New:** `<host>.client-ca.pem` is the client CA for that hostname and replaces the global one |
| `RELAYOPS_TLS_CLIENT_CA_FILE` | Global client CA |
| `RELAYOPS_TLS_CLIENT_CRL_FILE` | **New.** One or more CRLs (PEM or DER), reloaded like certificates |
| `RELAYOPS_TLS_RELOAD_SECONDS` | **New.** How often files are re-read (default 30, `0` disables polling). `SIGHUP` reloads immediately |

The per-hostname client CA replaces the half-built per-tenant pools. Hostnames are what the TLS layer sees, so this is the correct key. Mapping hostnames to tenants belongs to phase 2.

The admin listener (`RELAYOPS_ADMIN_TLS_*`, `RELAYOPS_ADMIN_CLIENT_CA_FILE`) and the relay listener use the same manager and reload the same way. A gateway-only node's client certificate for the control plane (`RELAYOPS_DATAPLANE_CLIENT_*`) is read through `GetClientCertificate` and re-read when the files change.

Reload compares file contents (SHA-256), not modification times. Kubernetes Secret volumes swap a symlink, and cert-manager rewrites files in place, so content comparison catches both.

### 2. ACME (`internal/certs/acme.go`)

`golang.org/x/crypto/acme/autocert` (already a dependency) issues and renews certificates for an explicit list of hostnames.

| Variable | Use |
|---|---|
| `RELAYOPS_ACME_DOMAINS` | Comma-separated hostnames. Nothing else is ever requested, so a client cannot make the gateway order certificates for arbitrary names |
| `RELAYOPS_ACME_EMAIL` | Account contact |
| `RELAYOPS_ACME_ACCEPT_TOS` | Must be `true`: accepting the CA's terms is the operator's decision |
| `RELAYOPS_ACME_DIRECTORY_URL` | Default Let's Encrypt production. Use the staging URL to test, or a private ACME CA (step-ca, Vault PKI, Smallstep) |
| `RELAYOPS_ACME_CACHE_DIR` | Default `$RELAYOPS_DATA_DIR/acme` (mode 0700) |
| `RELAYOPS_ACME_CACHE_KEY` | Base64 32-byte key. When set on a node with a database, certificates and the account key are cached in Postgres (`acme_cache`), encrypted with AES-256-GCM, so every node shares them and the CA's rate limits are respected |
| `RELAYOPS_ACME_HTTP_ADDR` | Optional HTTP-01 listener such as `:80`. Without it, TLS-ALPN-01 runs on the gateway TLS listener, which must be reachable on port 443 |

Selection order for a handshake: an ACME TLS-ALPN challenge → a certificate from files (exact, then wildcard) → ACME for a listed hostname → the default certificate. Certificates from files take precedence, so an operator can always override.

Nodes sharing the database cache take a 10-minute lease in `acme_order_leases` before ordering a hostname, so only one orders it. The others serve the certificate from the cache once it is issued. Challenge responses (TLS-ALPN-01 certificates and HTTP-01 tokens) are cached too, so any node can answer the CA's validation. Renewals are coordinated by autocert through the cache.

A TLS-ALPN-01 validation is answered with a configuration that requests no client certificate, so ACME works where client certificates are otherwise required.

Gateway-only nodes have no database. They use the directory cache, which should be on durable storage (they already need it for the spool). With several such nodes behind one load balancer, use HTTP-01 on a shared directory or a single issuing node: a validation reaching a node that did not place the order fails.

### 3. Upstream TLS (`traffic_policy.upstream_tls`)

```json
"traffic_policy": {
  "upstream_tls": {
    "ca": "${secret:BILLING_CA}",
    "client_certificate": "${secret:BILLING_CLIENT_CERT}",
    "client_key": "${secret:BILLING_CLIENT_KEY}",
    "server_name": "billing.internal",
    "min_version": "1.3"
  }
}
```

- **Secret references only.** Every PEM value must be a `${secret:NAME}` reference. Validation rejects literal PEM, so keys never enter the database, revisions, exports or Orbit context.
- **Resolved on the gateway.** Values come from `RELAYOPS_SECRET_<NAME>_FILE` (a path, read at each reload, so a rotated file is used without a restart), `RELAYOPS_SECRET_<NAME>` or `<NAME>`. The `_FILE` form is explicit and set by the operator, so an API definition cannot point the gateway at an arbitrary file.
- **`ca`** replaces the system roots for that API. **`client_certificate` and `client_key`** go together. **`server_name`** overrides SNI and the name verified. **`min_version`** is `1.2` (default) or `1.3`. There is deliberately no "skip verification" option.
- **Revision isolation.** The resolved TLS configuration belongs to the route in a snapshot, not to the shared target state. A canary that changes `upstream_tls` affects only the canary cohort.
- **Fail closed.** If a secret is missing or does not parse, the API stays routed and requests get `502 upstream_tls_unavailable`. The gateway never falls back to default trust, and the reload reports the error.
- **Transports.** Transports are keyed by the SHA-256 of the resolved material, so identical settings share connection pools. A rotation produces a new key, and transports no longer used by any loaded snapshot are closed. Health checks use the same TLS settings as traffic.

### 4. Expiry inventory and alerts

- **`gateway.(*Gateway).Certificates()`** lists every certificate the node holds: listener, per-host, client CAs, ACME, upstream CAs and client certificates, and the node's own control-plane client certificate. For each it gives the source, name, subject, DNS names, issuer, serial, `not_before`, `not_after` and SHA-256 fingerprint. It never includes key material.
- **Prometheus:**
  - `relayops_tls_certificate_not_after_timestamp_seconds{source,listener,name,api_id,role,serial,fingerprint_sha256}`
  - `relayops_tls_reloads_total{listener,result}`
  - `relayops_upstream_tls_errors_total{api_id}`

  Example alert: `relayops_tls_certificate_not_after_timestamp_seconds - time() < 14*86400`.
- **Fleet.** Each node sends its certificate list with an acknowledgement only when the list changes, and otherwise every 10 minutes. The list is trimmed to 48 KiB, soonest expiry first. Recording it is best effort: a failed report never fails the acknowledgement or costs the node its place in fleet status. Reports are stored in `node_certificates` and served at `GET /api/system/certificates`, which is platform-only. A node is listed while it keeps acknowledging. The report has one row per place a certificate is used, and the summary counts distinct certificates.
- **Orbit signal `certificates`:** critical when a certificate has expired, warning within 14 days, info within 30.
- **Webhook alert `certificate_expiring`:** one per certificate and severity per day, naming every place the certificate is used. Replicas claim alerts atomically in `certificate_alerts`, so several control planes never send the same alert twice.

### 5. Client certificate revocation

CRLs from `RELAYOPS_TLS_CLIENT_CRL_FILE` are matched to issuers by subject and verified against the issuer's key before use. A client certificate whose serial is listed is refused during the handshake. An expired CRL (`NextUpdate` in the past) is still enforced and counted in `relayops_tls_crl_stale`, so a stale list fails safe instead of admitting revoked certificates. This applies to mTLS terminated by the gateway. When an ingress terminates mTLS (`RELAYOPS_TRUSTED_PROXY_CIDRS`), the ingress must check revocation.

## Security notes

- Private keys are never stored in Postgres except the encrypted ACME cache, never sent to the control plane, and never reported.
- Reload errors name the file, never its contents.
- ACME orders only the configured hostnames.
- Upstream TLS cannot disable verification.

## Phase 2 (proposal)

- **Certificates as managed resources:** `certificates` with hostname, tenant, certificate chain and a secret reference (Vault PKI, AWS Secrets Manager, Supabase Vault) for the key. Admin API, console page and declarative support, live like API keys rather than revisioned.
- **Per-tenant custom domains** with ACME issuance on demand, after a DNS ownership check.
- **OCSP:** stapling for served certificates, and checks for client certificates.
- **A cert-manager `Issuer` example** for the Helm chart, and an operator.
