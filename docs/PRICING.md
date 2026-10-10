# RelayOps pricing

Invoice SKUs for selling RelayOps itself. These are not per-call meters and not
the prices API owners charge their own consumers (`plans.price_monthly_usd`).

Release safety (revisions, Test Studio, canary, evidence-gated promote, auto-rollback)
is included in every paid edition.

| Edition | Invoice price | Included |
| --- | --- | --- |
| Pilot | $2,500 per production gateway / month | 1 production gateway, 1 tenant, email support, 90-day term |
| Team | $4,000 / month | Up to 3 gateways, 3 tenants, Studio CI, SSO, audit export, 8×5 support |
| Business | $8,000 / month | Up to 25 gateways and tenants, AI Gateway & APIOps preview, named CSM, 4-hour sev-1, on-prem license |

Quote above Business. There is no self-serve card checkout in this release.

## How to enable an edition

Set `RELAYOPS_EDITION` to `pilot`, `team`, or `business` on the control plane.
Optional overrides: `RELAYOPS_LICENSE_MAX_GATEWAYS`, `RELAYOPS_LICENSE_MAX_TENANTS`.

Preview surfaces (AI Gateway and APIOps) are included by default in `business` edition, or can be explicitly toggled:

- `RELAYOPS_PREVIEW_AI=true`
- `RELAYOPS_PREVIEW_APIOPS=true`

An unset edition is unlimited so existing installs keep working. Caps apply only
when an edition (or an override) is set.
